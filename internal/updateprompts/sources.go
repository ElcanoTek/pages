// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package updateprompts

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/versions"
)

// MaxInstructions bounds the user request, in UTF-8 bytes.
const MaxInstructions = 20000

// MaxSources bounds the declared source bindings.
const MaxSources = 20

// UpdateTypes are the accepted update_type values.
var UpdateTypes = []string{"auto", "data", "layout"}

// jsWS is JavaScript's /\s/ class body (Go's \s is ASCII-only and omits \v).
const jsWS = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// jsRE compiles a JavaScript pattern, rewriting \s to JavaScript's whitespace
// set. Patterns here never use \s inside a character class except where the
// caller spells jsWS out.
func jsRE(pattern string) *regexp.Regexp {
	return regexp.MustCompile(strings.ReplaceAll(pattern, `\s`, "["+jsWS+"]"))
}

var (
	forbiddenKey   = regexp.MustCompile(`(?i)(?:password|passphrase|secret|api[_-]?key|authorization|credential|access[_-]?token|refresh[_-]?token)`)
	forbiddenValue = jsRE(`(?i)(?:\bBearer\s+[A-Za-z0-9._~+/-]{8,}|\bpgs_[A-Za-z0-9_-]{12,}|\bsk-[A-Za-z0-9_=-]{12,})`)
)

// AssertCredentialFree refuses a credential-shaped value or a credential-like
// key anywhere in value (decoded JSON: string, []any, *jsonv.Object or
// map[string]any). path names the root in the error ("" ⇒ "$").
func AssertCredentialFree(value any, path string) error {
	if path == "" {
		path = "$"
	}
	return assertCredentialFree(value, path, 0)
}

func assertCredentialFree(value any, path string, depth int) error {
	if depth > 30 {
		return apierror.BadRequest("update instructions are nested too deeply", "update_instructions_invalid", nil)
	}
	switch v := value.(type) {
	case string:
		if forbiddenValue.MatchString(v) {
			return apierror.BadRequest("credential-shaped value is forbidden at "+path, "update_credentials_forbidden", nil)
		}
	case []any:
		for i, item := range v {
			if err := assertCredentialFree(item, fmt.Sprintf("%s[%d]", path, i), depth+1); err != nil {
				return err
			}
		}
	case []string:
		for i, item := range v {
			if err := assertCredentialFree(item, fmt.Sprintf("%s[%d]", path, i), depth+1); err != nil {
				return err
			}
		}
	default:
		obj := asObject(value)
		if obj == nil {
			return nil
		}
		var err error
		obj.Range(func(key string, item any) bool {
			if forbiddenKey.MatchString(key) {
				err = apierror.BadRequest(fmt.Sprintf("credential-like field is forbidden at %s.%s", path, key), "update_credentials_forbidden", nil)
				return false
			}
			err = assertCredentialFree(item, path+"."+key, depth+1)
			return err == nil
		})
		return err
	}
	return nil
}

// asObject views a decoded JSON object (jsonv or a plain map) as *jsonv.Object.
func asObject(v any) *jsonv.Object {
	switch o := v.(type) {
	case *jsonv.Object:
		return o
	case map[string]any:
		return jsonv.FromMap(o)
	}
	return nil
}

// NormalizeInstructions trims the request and bounds and screens it.
func NormalizeInstructions(value any) (string, error) {
	s, ok := value.(string)
	if !ok || jsstr.Trim(s) == "" {
		return "", apierror.BadRequest("instructions are required", "update_instructions_required", nil)
	}
	normalized := jsstr.Trim(s)
	if len(normalized) > MaxInstructions {
		return "", apierror.BadRequest(fmt.Sprintf("instructions must be at most %d UTF-8 bytes", MaxInstructions), "update_instructions_too_large", nil)
	}
	if err := AssertCredentialFree(normalized, ""); err != nil {
		return "", err
	}
	return normalized, nil
}

// Binding fields are rendered as LINES of a section the executing agent reads as
// authority, so a value containing a line break could forge its own section
// header ("OUT OF SCOPE", "REQUIRED WORKFLOW", ...). Every field here is a single
// identifier or one line of detail, so control characters are simply refused —
// the cheap structural defence, on top of quoting the free-text field at render.
var unsafeLine = regexp.MustCompile(`[\x00-\x1f\x7f\x{2028}\x{2029}]`)

// RequirementIdentifier is the server/tool name shape a strict scheduler
// accepts. mcp_server and required_tools are copied verbatim into EXECUTION
// REQUIREMENTS, which fleet parses strictly: it refuses any server or tool name
// outside this pattern and dead-letters the task at dispatch (for example a
// binding naming two servers joined with "+"). Refuse it here, at preparation,
// where the caller can still fix it, instead of in a run weeks later.
var RequirementIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,200}$`)

func sourcesInvalid(msg string) error {
	return apierror.BadRequest(msg, "update_sources_invalid", nil)
}

func boundedString(value any, field string, maxLen int) (string, error) {
	s, ok := value.(string)
	if !ok || jsstr.Trim(s) == "" {
		return "", sourcesInvalid(field + " is required")
	}
	normalized := jsstr.Trim(s)
	if jsstr.UTF16Len(normalized) > maxLen {
		return "", sourcesInvalid(fmt.Sprintf("%s must be at most %d characters", field, maxLen))
	}
	if unsafeLine.MatchString(normalized) {
		return "", sourcesInvalid(field + " must not contain line breaks or control characters")
	}
	return normalized, nil
}

func requirementName(value, field string) (string, error) {
	if !RequirementIdentifier.MatchString(value) {
		return "", sourcesInvalid(field + " must be one MCP server or tool name (letters, digits, '_', '.', '-'); " +
			"bind each server as its own source instead of combining names")
	}
	return value, nil
}

func safeLine(value any) bool {
	s, ok := value.(string)
	return ok && jsstr.Trim(s) != "" && !unsafeLine.MatchString(s)
}

// Partition says a source is retrieved by enumerating a range, not by picking
// the newest file.
type Partition struct {
	By     string `json:"by"`
	Format string `json:"format,omitempty"`
	Since  string `json:"since,omitempty"`
	Until  string `json:"until,omitempty"`
}

// Source is one normalized source binding, in the JavaScript key order.
type Source struct {
	SourceID              string     `json:"source_id"`
	MCPServer             string     `json:"mcp_server"`
	Account               string     `json:"account,omitempty"`
	RequiredTools         []string   `json:"required_tools,omitempty"`
	Path                  string     `json:"path,omitempty"`
	Partition             *Partition `json:"partition,omitempty"`
	RetrievalInstructions string     `json:"retrieval_instructions,omitempty"`
}

// isNullish is JavaScript `v === undefined || v === null` for a decoded
// value (versions.Null counts as null).
func isNullish(v any) bool { return v == nil || v == versions.Null }

func lookup(obj *jsonv.Object, key string) (any, bool) {
	v, ok := obj.Get(key)
	if !ok || isNullish(v) {
		return nil, false
	}
	return v, true
}

func asArray(v any) ([]any, bool) {
	switch a := v.(type) {
	case []any:
		return a, true
	case []string:
		out := make([]any, len(a))
		for i, s := range a {
			out[i] = s
		}
		return out, true
	case []map[string]any:
		out := make([]any, len(a))
		for i, m := range a {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// NormalizeSources validates the OPTIONAL caller-declared source bindings
// (decoded JSON). nil/null ⇒ nil. The vocabulary intentionally matches the
// `workflow.sources` shape client bundles already author (source_id /
// mcp_server / account / required_tools / retrieval_instructions), so a caller
// has one way to say "this data comes from that connector" rather than burying
// it in prose the executing agent has to re-derive. Names only: the same
// credential screen as `instructions` applies, so a secret value can never
// arrive through this field either.
func NormalizeSources(value any) ([]Source, error) {
	if isNullish(value) {
		return nil, nil
	}
	if s, ok := value.([]Source); ok {
		value = sourcesToJSON(s)
	}
	list, ok := asArray(value)
	if !ok || len(list) == 0 {
		return nil, sourcesInvalid("sources must be a non-empty array when provided")
	}
	if len(list) > MaxSources {
		return nil, sourcesInvalid(fmt.Sprintf("sources must contain at most %d entries", MaxSources))
	}
	if err := AssertCredentialFree(list, "$.sources"); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]Source, 0, len(list))
	for index, raw := range list {
		entry := asObject(raw)
		if entry == nil {
			return nil, sourcesInvalid(fmt.Sprintf("sources[%d] must be an object", index))
		}
		field := func(name string) string { return fmt.Sprintf("sources[%d].%s", index, name) }
		sourceID, err := boundedString(entry.Value("source_id"), field("source_id"), 120)
		if err != nil {
			return nil, err
		}
		if seen[sourceID] {
			return nil, sourcesInvalid(fmt.Sprintf("sources[%d].source_id is duplicated", index))
		}
		seen[sourceID] = true
		server, err := boundedString(entry.Value("mcp_server"), field("mcp_server"), 120)
		if err != nil {
			return nil, err
		}
		if server, err = requirementName(server, field("mcp_server")); err != nil {
			return nil, err
		}
		n := Source{SourceID: sourceID, MCPServer: server}
		if v, ok := lookup(entry, "account"); ok {
			if n.Account, err = boundedString(v, field("account"), 120); err != nil {
				return nil, err
			}
		}
		if v, ok := lookup(entry, "required_tools"); ok {
			tools, isArr := asArray(v)
			if !isArr || len(tools) == 0 {
				return nil, sourcesInvalid(fmt.Sprintf("sources[%d].required_tools must be a non-empty array when provided", index))
			}
			for ti, tool := range tools {
				name := fmt.Sprintf("sources[%d].required_tools[%d]", index, ti)
				t, err := boundedString(tool, name, 200)
				if err != nil {
					return nil, err
				}
				if t, err = requirementName(t, name); err != nil {
					return nil, err
				}
				n.RequiredTools = append(n.RequiredTools, t)
			}
		}
		if v, ok := lookup(entry, "path"); ok {
			if n.Path, err = boundedString(v, field("path"), 500); err != nil {
				return nil, err
			}
		}
		// A partitioned source is retrieved by enumerating a range, not by
		// picking the newest file. Saying so structurally is the difference
		// between an agent that reads six daily files and one that reads the
		// newest and publishes a sixth of the data — the shape of the source has
		// to survive the handoff to whatever scheduler runs this prompt weeks
		// later.
		if v, ok := lookup(entry, "partition"); ok {
			partition := asObject(v)
			if partition == nil {
				return nil, sourcesInvalid(fmt.Sprintf("sources[%d].partition must be an object", index))
			}
			by, err := boundedString(partition.Value("by"), field("partition.by"), 40)
			if err != nil {
				return nil, err
			}
			if by != "date" {
				return nil, sourcesInvalid(fmt.Sprintf(`sources[%d].partition.by must be "date"`, index))
			}
			p := &Partition{By: by}
			for _, f := range []struct {
				key string
				dst *string
			}{{"format", &p.Format}, {"since", &p.Since}, {"until", &p.Until}} {
				if pv, ok := lookup(partition, f.key); ok {
					if *f.dst, err = boundedString(pv, field("partition."+f.key), 40); err != nil {
						return nil, err
					}
				}
			}
			n.Partition = p
		}
		if v, ok := lookup(entry, "retrieval_instructions"); ok {
			if n.RetrievalInstructions, err = boundedString(v, field("retrieval_instructions"), 2000); err != nil {
				return nil, err
			}
		}
		out = append(out, n)
	}
	return out, nil
}

func sourcesToJSON(sources []Source) []any {
	out := make([]any, len(sources))
	for i, s := range sources {
		o := jsonv.ObjectOf("source_id", s.SourceID, "mcp_server", s.MCPServer)
		if s.Account != "" {
			o.Set("account", s.Account)
		}
		if s.RequiredTools != nil {
			tools := make([]any, len(s.RequiredTools))
			for j, t := range s.RequiredTools {
				tools[j] = t
			}
			o.Set("required_tools", tools)
		}
		if s.Path != "" {
			o.Set("path", s.Path)
		}
		if s.Partition != nil {
			p := jsonv.ObjectOf("by", s.Partition.By)
			if s.Partition.Format != "" {
				p.Set("format", s.Partition.Format)
			}
			if s.Partition.Since != "" {
				p.Set("since", s.Partition.Since)
			}
			if s.Partition.Until != "" {
				p.Set("until", s.Partition.Until)
			}
			o.Set("partition", p)
		}
		if s.RetrievalInstructions != "" {
			o.Set("retrieval_instructions", s.RetrievalInstructions)
		}
		out[i] = o
	}
	return out
}

// SourcesFromWorkflow lifts bindings out of the legacy `workflow` object older
// clients still send to the configure_page_refresh compatibility tool. That
// shape already carries source_id/mcp_server per source, so those callers get
// the same hard bindings as a modern caller instead of only a serialized blob
// buried in the instructions. Entries without both identifiers are skipped
// rather than guessed at; extra workflow fields (date windows, row minimums)
// stay in the serialized contract where they already worked. nil when nothing
// can be lifted faithfully.
func SourcesFromWorkflow(workflow any) []Source {
	obj := asObject(workflow)
	if obj == nil {
		return nil
	}
	raw, ok := asArray(obj.Value("sources"))
	if !ok || len(raw) == 0 {
		return nil
	}
	if len(raw) > MaxSources {
		raw = raw[:MaxSources]
	}
	var lifted []Source
	for _, e := range raw {
		entry := asObject(e)
		if entry == nil {
			continue
		}
		// A legacy payload must not fail this read-only preparation, so an
		// unusable field is dropped rather than raised — but it is never
		// rendered either: the same one-line rule as NormalizeSources applies.
		if !safeLine(entry.Value("source_id")) || !safeLine(entry.Value("mcp_server")) {
			continue
		}
		server := jsstr.Trim(entry.Value("mcp_server").(string))
		// Same rule as NormalizeSources, but a legacy read must not fail. It must
		// not drop just this entry either: the rendered bindings would then
		// declare that source out of scope while the serialized workflow still
		// requires it, and an unattended run could publish without it. A server
		// name a scheduler cannot parse (e.g. "fast_io + fastio_helpers") means
		// the lift cannot be faithful, so lift nothing and leave every source to
		// the serialized workflow, as for a workflow that names no servers.
		if !RequirementIdentifier.MatchString(server) {
			return nil
		}
		picked := Source{SourceID: jsstr.Trim(entry.Value("source_id").(string)), MCPServer: server}
		if safeLine(entry.Value("account")) {
			picked.Account = jsstr.Trim(entry.Value("account").(string))
		}
		if tools, ok := asArray(entry.Value("required_tools")); ok {
			for _, t := range tools {
				if !safeLine(t) {
					continue
				}
				if name := jsstr.Trim(t.(string)); RequirementIdentifier.MatchString(name) {
					picked.RequiredTools = append(picked.RequiredTools, name)
				}
			}
		}
		if safeLine(entry.Value("retrieval_instructions")) {
			picked.RetrievalInstructions = jsstr.Trim(entry.Value("retrieval_instructions").(string))
		}
		lifted = append(lifted, picked)
	}
	if len(lifted) == 0 {
		return nil
	}
	// Duplicate source_ids in a legacy payload must not fail the compatibility
	// read; keep the first occurrence of each.
	seen := map[string]bool{}
	out := lifted[:0]
	for _, s := range lifted {
		if seen[s.SourceID] {
			continue
		}
		seen[s.SourceID] = true
		out = append(out, s)
	}
	return out
}

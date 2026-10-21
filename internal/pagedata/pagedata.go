// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package pagedata is safe parsing and materialization for Pages-managed
// dashboard data (lib/page-data.js). It deliberately owns no database
// behaviour: it turns immutable HTML into a validated contract plus source
// offsets, and can replace only the contents of the managed script blocks. The
// version state machine owns locking and publishing.
//
// JSON values use the internal/jsonv model (nil, bool, float64, string, []any,
// *jsonv.Object). Callers must parse client JSON with jsonv.Parse: key order
// is stored verbatim in the page, and only *jsonv.Object preserves it.
package pagedata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/jsonv"
)

// Block ids and types.
const (
	SchemaID       = "pages-data-schema"
	DataID         = "pages-data"
	ConfigSchemaID = "pages-config-schema"
	ConfigID       = "pages-config"
	ExampleID      = "pages-data-example"
	SchemaType     = "application/schema+json"
	DataType       = "application/json"
)

const dataPlaceholder = "__PAGES_DATA_CONTENT__"

// ── managed block pairs ──────────────────────────────────────────────────────
// A managed pair is a schema block plus the payload block it validates. The
// payload is the only thing a write may replace; the schema is design-time and
// immutable for the life of the deployed bytes.
//
//	data   — the refresh payload (an envelope). Replaced by update_page_data.
//	config — per-instance deploy-time settings (a bare object). Replaced only
//	         by update_page_config. Optional: a page with no config pair is an
//	         ordinary managed page.
//
// One entry is not a pair: example is a lone payload with no schema of its own,
// because it is example DATA and validates against the data schema already in
// the document. It exists only so the library can show a design populated
// instead of empty, and materialization always deletes it.

type blockEntry struct {
	key, label, schemaID, payloadID string
	payloadOnly, required           bool
	missingCode                     string
}

// Spec says which managed blocks a document must, may, or may not carry.
type Spec struct{ entries []blockEntry }

var (
	dataPair   = blockEntry{key: "data", label: "data", schemaID: SchemaID, payloadID: DataID}
	configPair = blockEntry{key: "config", label: "config", schemaID: ConfigSchemaID, payloadID: ConfigID}
)

func with(e blockEntry, required bool, code string) blockEntry {
	e.required, e.missingCode = required, code
	return e
}

// PageSpec: data required, config optional (present only on template-derived
// pages).
var PageSpec = Spec{entries: []blockEntry{
	with(dataPair, true, "page_not_data_managed"),
	with(configPair, false, "page_not_template_managed"),
}}

// TemplateSpec: both pairs required, plus the optional preview-only example.
// Its shipped payloads are the reference config and the empty-state envelope,
// which is what makes a template a real page that can be validated against its
// own schemas at registration.
var TemplateSpec = Spec{entries: []blockEntry{
	with(dataPair, true, "template_contract_invalid"),
	with(configPair, true, "template_contract_invalid"),
	{key: "example", label: "example data", payloadID: ExampleID, payloadOnly: true, required: false, missingCode: "template_contract_invalid"},
}}

// envBytes reads a positive safe-integer setting at call time.
func envBytes(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f != math.Trunc(f) || f <= 0 || f > 9007199254740991 {
		return fallback
	}
	return int(f)
}

// MaxSchemaBytes is PAGES_DATA_SCHEMA_MAX_BYTES (default 256 KiB).
func MaxSchemaBytes() int { return envBytes("PAGES_DATA_SCHEMA_MAX_BYTES", 256*1024) }

// MaxConfigBytes is PAGES_DATA_CONFIG_MAX_BYTES (default 256 KiB). Config is
// per-campaign identity, channels and registries — kilobytes, not the megabyte
// of rows a refresh carries; a tighter ceiling keeps a runaway config from
// being smuggled in as "settings".
func MaxConfigBytes() int { return envBytes("PAGES_DATA_CONFIG_MAX_BYTES", 256*1024) }

// MaxTemplateBytes is PAGES_DATA_TEMPLATE_MAX_BYTES (default 2 MiB), the
// ceiling on a whole managed document.
func MaxTemplateBytes() int { return envBytes("PAGES_DATA_TEMPLATE_MAX_BYTES", 2*1024*1024) }

// MaxDataBytes is the managed-data payload budget (datalimits.MaxDataBytes).
func MaxDataBytes() int { return datalimits.MaxDataBytes() }

func maxSchemaNodes() int { return envBytes("PAGES_DATA_SCHEMA_MAX_NODES", 10000) }
func maxSchemaDepth() int { return envBytes("PAGES_DATA_SCHEMA_MAX_DEPTH", 100) }

// FutureToleranceMs is PAGES_SOURCE_FUTURE_TOLERANCE_MS (default five minutes):
// how far in the future a source_as_of may be.
func FutureToleranceMs() int { return envBytes("PAGES_SOURCE_FUTURE_TOLERANCE_MS", 5*60*1000) }

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func contractInvalid(message string, details any) *apierror.Error {
	return apierror.Conflict(message, "data_contract_invalid", details)
}

func isPlainObject(v any) bool { _, ok := v.(*jsonv.Object); return ok }

// ── canonical JSON and hashes ────────────────────────────────────────────────

// CanonicalJSON is the deterministic serialization semantic hashes are taken
// over: object keys sorted (by UTF-16 code units, as JavaScript sorts),
// everything else exactly as JSON.stringify writes it.
func CanonicalJSON(v any) (string, error) {
	var b strings.Builder
	if err := canonical(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func canonical(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool, string:
		b.WriteString(jsonv.Stringify(x))
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return apierror.BadRequest("data must contain only finite JSON numbers", "data_contract_invalid", nil)
		}
		b.WriteString(jsonv.FormatNumber(x))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := canonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case *jsonv.Object:
		keys := x.Keys()
		jsonv.SortStrings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			jsonv.WriteString(b, k)
			b.WriteByte(':')
			if err := canonical(b, x.Value(k)); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		n, err := jsonv.Normalize(v)
		if err != nil {
			return apierror.BadRequest("data must contain only JSON values", "data_contract_invalid", nil)
		}
		return canonical(b, n)
	}
	return nil
}

// SemanticHash is the lowercase hex SHA-256 of CanonicalJSON(v). Stored
// data_sha256 / schema_sha256 / config_sha256 values are compared against it,
// so it must equal the JS hash for the same value.
func SemanticHash(v any) (string, error) {
	s, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return sha256Hex(s), nil
}

// EscapedJSON is datalimits.EscapedJSON (JSON.stringify made safe inside a
// <script> element), re-exported because lib/page-data.js exported it too.
func EscapedJSON(v any) string { return datalimits.EscapedJSON(v) }

// EscapedEnvelopeJSON is EscapedJSON under the name the JS used for envelopes.
func EscapedEnvelopeJSON(v any) string { return datalimits.EscapedJSON(v) }

func mustHash(v any) string {
	h, err := SemanticHash(v)
	if err != nil {
		// Every caller hashes a value that already passed CanonicalJSON.
		panic(err)
	}
	return h
}

// ── compiled-schema cache ────────────────────────────────────────────────────
// ParseManagedHTML runs on every managed-data read and write; compiling a
// near-max-size schema per call is free CPU for a loop-calling agent.
// Validators are stateless, so compile once per schema content and reuse,
// keyed by the semantic hash, bounded LRU. Keyed by content only: the label
// changes error wording, never validity.

const compileCacheMax = 64

// CacheStats reports the compiled-schema cache.
type CacheStats struct {
	Size   int `json:"size"`
	Hits   int `json:"hits"`
	Misses int `json:"misses"`
}

var cache = struct {
	sync.Mutex
	order  []string
	byKey  map[string]*Validator
	hits   int
	misses int
}{byKey: map[string]*Validator{}}

// CompileCacheStats returns the cache's size and counters.
func CompileCacheStats() CacheStats {
	cache.Lock()
	defer cache.Unlock()
	return CacheStats{Size: len(cache.byKey), Hits: cache.hits, Misses: cache.misses}
}

func compileSchemaCached(schema any, label string) (*Validator, error) {
	key, err := SemanticHash(schema)
	if err != nil {
		return nil, err
	}
	cache.Lock()
	if v, ok := cache.byKey[key]; ok {
		cache.hits++
		for i, k := range cache.order {
			if k == key {
				cache.order = append(cache.order[:i:i], cache.order[i+1:]...)
				break
			}
		}
		cache.order = append(cache.order, key)
		cache.Unlock()
		return v, nil
	}
	cache.misses++
	cache.Unlock()

	v, err := compileSchema(schema, label)
	if err != nil {
		return nil, err
	}
	cache.Lock()
	if _, ok := cache.byKey[key]; !ok {
		cache.byKey[key] = v
		cache.order = append(cache.order, key)
		for len(cache.order) > compileCacheMax {
			delete(cache.byKey, cache.order[0])
			cache.order = cache.order[1:]
		}
	}
	cache.Unlock()
	return v, nil
}

func compileSchema(schema any, label string) (*Validator, error) {
	if err := assertSelfContainedSchema(schema, label); err != nil {
		return nil, err
	}
	root := schema.(*jsonv.Object)
	if err := validateMeta(root); err != nil {
		return nil, contractInvalid(fmt.Sprintf("the %s schema is not a valid self-contained JSON Schema 2020-12 document", label), nil)
	}
	v, err := compileValidator(root)
	if err != nil {
		return nil, contractInvalid(fmt.Sprintf("the %s schema is not a valid self-contained JSON Schema 2020-12 document", label), nil)
	}
	return v, nil
}

// CompileSchema compiles a managed schema (self-containment, ReDoS screen,
// meta-schema and Ajv strict-mode checks), through the shared cache. label is
// "data" or "config" and only changes error wording.
func CompileSchema(schema any, label string) (*Validator, error) {
	if label == "" {
		label = "data"
	}
	return compileSchemaCached(schema, label)
}

func assertSelfContainedSchema(schema any, label string) error {
	root, ok := schema.(*jsonv.Object)
	if !ok {
		return contractInvalid(fmt.Sprintf("the %s schema root must be an object", label), nil)
	}
	switch root.Value("$schema") {
	case "https://json-schema.org/draft/2020-12/schema", "https://json-schema.org/draft/2020-12/schema#":
	default:
		return contractInvalid(fmt.Sprintf("the %s schema must declare JSON Schema draft 2020-12 in $schema", label), nil)
	}
	if root.Value("type") != "object" {
		return contractInvalid(fmt.Sprintf(`the %s schema root must declare type "object"`, label), nil)
	}
	nodes := 0
	maxNodes, maxDepth := maxSchemaNodes(), maxSchemaDepth()
	var visit func(v any, depth int) error
	visit = func(v any, depth int) error {
		switch x := v.(type) {
		case []any:
			if depth > maxDepth {
				return contractInvalid(fmt.Sprintf("the %s schema is too deeply nested", label), nil)
			}
			nodes++
			if nodes > maxNodes {
				return contractInvalid(fmt.Sprintf("the %s schema is too complex", label), nil)
			}
			for _, c := range x {
				if err := visit(c, depth+1); err != nil {
					return err
				}
			}
		case *jsonv.Object:
			if depth > maxDepth {
				return contractInvalid(fmt.Sprintf("the %s schema is too deeply nested", label), nil)
			}
			nodes++
			if nodes > maxNodes {
				return contractInvalid(fmt.Sprintf("the %s schema is too complex", label), nil)
			}
			for _, key := range x.Keys() {
				child := x.Value(key)
				if s, isStr := child.(string); isStr {
					if (key == "$ref" || key == "$dynamicRef" || key == "$recursiveRef") && s != "" && !strings.HasPrefix(s, "#") {
						return contractInvalid("external schema references are not allowed", jsonv.ObjectOf("keyword", key))
					}
					if key == "pattern" && !IsSafePattern(s) {
						return contractInvalid(fmt.Sprintf("the %s schema uses a pattern that could backtrack catastrophically; simplify the regex", label),
							jsonv.ObjectOf("keyword", "pattern"))
					}
				}
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(root, 0)
}

// BoundedError is one validation error as the API reports it.
type BoundedError struct {
	InstancePath string `json:"instance_path"`
	SchemaPath   string `json:"schema_path"`
	Keyword      string `json:"keyword"`
	Message      string `json:"message"`
}

// ValidationDetails is the detail object of a schema refusal: at most 12
// errors, each field bounded, and the true total.
type ValidationDetails struct {
	ValidationErrors []BoundedError `json:"validation_errors"`
	TotalErrors      int            `json:"total_errors"`
}

func clip16(s string, n int) string {
	if jsonv.UTF16Len(s) <= n {
		return s
	}
	return jsonv.SliceUTF16(s, 0, n)
}

func boundedValidationErrors(errs []ValidationError) ValidationDetails {
	out := ValidationDetails{ValidationErrors: []BoundedError{}, TotalErrors: len(errs)}
	for i, e := range errs {
		if i >= 12 {
			break
		}
		msg := e.Message
		if msg == "" {
			msg = "validation failed"
		}
		out.ValidationErrors = append(out.ValidationErrors, BoundedError{
			InstancePath: clip16(e.InstancePath, 300),
			SchemaPath:   clip16(e.SchemaPath, 300),
			Keyword:      clip16(e.Keyword, 100),
			Message:      clip16(msg, 300),
		})
	}
	return out
}

// ── timestamps ───────────────────────────────────────────────────────────────

var rfc3339Re = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$`)

func normalizeRFC3339(value any, field, errorCode string, currentContract bool) (string, error) {
	fail := func(msg string) (string, error) {
		if currentContract {
			return "", contractInvalid(msg, nil)
		}
		return "", apierror.BadRequest(msg, errorCode, nil)
	}
	s, ok := value.(string)
	if !ok || !rfc3339Re.MatchString(s) {
		return fail(field + " must be an RFC3339 timestamp with a timezone")
	}
	ms, ok := jsDateParse(s)
	if !ok {
		return fail(field + " must be a valid RFC3339 timestamp")
	}
	return jsISOString(ms), nil
}

// NormalizeSourceAsOf validates a caller's source_as_of (an RFC3339 timestamp
// with a timezone, not materially in the future) and returns its canonical
// toISOString form.
func NormalizeSourceAsOf(value any, now time.Time) (string, error) {
	normalized, err := normalizeRFC3339(value, "source_as_of", "source_as_of_invalid", false)
	if err != nil {
		return "", err
	}
	ms, _ := jsDateParse(normalized)
	tol := FutureToleranceMs()
	if ms > timeMs(now)+float64(tol) {
		return "", apierror.BadRequest(fmt.Sprintf("source_as_of cannot be more than %dms in the future", tol), "source_in_future", nil)
	}
	return normalized, nil
}

func assertEnvelope(v any) error {
	env, ok := v.(*jsonv.Object)
	if !ok {
		return contractInvalid("#pages-data must contain an object envelope", nil)
	}
	keys := env.Keys()
	sort.Strings(keys)
	if strings.Join(keys, "\x00") != "contract_version\x00data\x00refreshed_at\x00source_as_of" {
		return contractInvalid("#pages-data must contain exactly contract_version, refreshed_at, source_as_of, and data", nil)
	}
	if env.Value("contract_version") != 1.0 {
		return contractInvalid("#pages-data contract_version must be 1", nil)
	}
	if _, err := normalizeRFC3339(env.Value("refreshed_at"), "refreshed_at", "data_contract_invalid", true); err != nil {
		return err
	}
	if _, err := normalizeRFC3339(env.Value("source_as_of"), "source_as_of", "data_contract_invalid", true); err != nil {
		return err
	}
	if !isPlainObject(env.Value("data")) {
		return contractInvalid("#pages-data data must be an object", nil)
	}
	return nil
}

// assertRepresentableNumbers refuses integers beyond IEEE-754 exact range.
// JSON number literals have arbitrary precision; every parser in this stack
// turns them into doubles, so 9007199254740993 is stored as ...992 and a
// 70-digit concatenated "count" satisfies {"type":"integer"}. Called on writes
// only, deliberately: reads revalidate the stored payload, and a page already
// carrying such a number must stay readable.
func assertRepresentableNumbers(v any, errorCode, label, path string) error {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) && math.Abs(x) > 9007199254740991 {
			where := path
			if where == "" {
				where = "the root"
			}
			return apierror.BadRequest(
				fmt.Sprintf("%s carries an integer at %s beyond IEEE-754 exact range (|value| > 9007199254740991), so it cannot round-trip without silently changing. ", label, where)+
					"If this is a count, it is almost certainly a summing bug — check for string concatenation. "+
					"If it is an identifier, send it as a string.",
				errorCode, nil)
		}
	case []any:
		for i, e := range x {
			if err := assertRepresentableNumbers(e, errorCode, label, path+"/"+strconv.Itoa(i)); err != nil {
				return err
			}
		}
	case *jsonv.Object:
		for _, k := range x.Keys() {
			if err := assertRepresentableNumbers(x.Value(k), errorCode, label, path+"/"+k); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePayload(v *Validator, value any, errorCode, label string, maxBytes int) (string, error) {
	if !isPlainObject(value) {
		return "", apierror.BadRequest(label+" must be an object", errorCode, ValidationDetails{
			ValidationErrors: []BoundedError{{InstancePath: "", SchemaPath: "#/type", Keyword: "type", Message: "must be object"}},
			TotalErrors:      1,
		})
	}
	serialized, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	if label == "data" {
		if err := datalimits.AssertDomainSize(len(serialized), "payload", errorCode); err != nil {
			return "", err
		}
	}
	if len(serialized) > maxBytes {
		return "", apierror.BadRequest(fmt.Sprintf("%s exceeds the %d-byte limit", label, maxBytes), errorCode, nil)
	}
	if errs := v.Validate(value); len(errs) > 0 {
		return "", apierror.BadRequest(label+" does not satisfy the page's JSON Schema", errorCode, boundedValidationErrors(errs))
	}
	return serialized, nil
}

// validateData returns the payload's canonical JSON, which the caller hashes
// for data_sha256 instead of serializing it a second time.
func validateData(v *Validator, data any, errorCode string) (string, error) {
	return validatePayload(v, data, errorCode, "data", MaxDataBytes())
}

func validateConfig(v *Validator, config any, errorCode string) error {
	_, err := validatePayload(v, config, errorCode, "config", MaxConfigBytes())
	return err
}

// assertCurrentPayload re-labels a failure on content ALREADY in the
// document: that is a broken contract (nothing the caller sent), not a
// rejected proposal.
func assertCurrentPayload(err error, message string) error {
	if err == nil {
		return nil
	}
	if e, ok := apierror.As(err); ok && e.Code == "data_contract_invalid" {
		return contractInvalid(message, e.Details)
	}
	return err
}

// ── reading a managed document ───────────────────────────────────────────────

type foundPair struct {
	entry        blockEntry
	schemaBlock  *Block
	payloadBlock *Block
}

func findBlocks(html string, spec Spec) (map[string]*foundPair, error) {
	if html == "" {
		return nil, contractInvalid("published HTML is empty", nil)
	}
	if limit := MaxTemplateBytes(); len(html) > limit {
		return nil, contractInvalid(fmt.Sprintf("published HTML exceeds the %d-byte managed-data limit", limit), nil)
	}
	matches := map[string][]*Block{}
	for _, e := range spec.entries {
		if e.schemaID != "" {
			matches[e.schemaID] = nil
		}
		matches[e.payloadID] = nil
	}
	if err := walkHTML(html, matches); err != nil {
		return nil, err
	}
	found := map[string]*foundPair{}
	for _, e := range spec.entries {
		payloads := matches[e.payloadID]
		if e.payloadOnly {
			switch {
			case len(payloads) == 0:
				if e.required {
					return nil, apierror.Conflict(fmt.Sprintf("page is missing its #%s block", e.payloadID), e.missingCode, nil)
				}
				found[e.key] = nil
				continue
			case len(payloads) != 1:
				return nil, contractInvalid(fmt.Sprintf("a managed page allows at most one #%s block", e.payloadID), nil)
			case payloads[0].Type != DataType:
				return nil, contractInvalid(fmt.Sprintf(`#%s must use type="%s"`, e.payloadID, DataType), nil)
			}
			found[e.key] = &foundPair{entry: e, payloadBlock: payloads[0]}
			continue
		}
		schemas := matches[e.schemaID]
		if len(schemas) == 0 && len(payloads) == 0 {
			if e.required {
				return nil, apierror.Conflict(
					fmt.Sprintf("page is not %s-managed; add exactly one #%s block and one #%s block", e.label, e.schemaID, e.payloadID),
					e.missingCode, nil)
			}
			found[e.key] = nil
			continue
		}
		if len(schemas) != 1 || len(payloads) != 1 {
			return nil, contractInvalid(fmt.Sprintf("managed pages require exactly one #%s block and one #%s block", e.schemaID, e.payloadID), nil)
		}
		if schemas[0].Type != SchemaType {
			return nil, contractInvalid(fmt.Sprintf(`#%s must use type="%s"`, e.schemaID, SchemaType), nil)
		}
		if payloads[0].Type != DataType {
			return nil, contractInvalid(fmt.Sprintf(`#%s must use type="%s"`, e.payloadID, DataType), nil)
		}
		found[e.key] = &foundPair{entry: e, schemaBlock: schemas[0], payloadBlock: payloads[0]}
	}
	return found, nil
}

func parseJSONBlock(text, blockName string, maxBytes int) (any, error) {
	if len(text) > maxBytes {
		return nil, contractInvalid(fmt.Sprintf("#%s exceeds its %d-byte limit", blockName, maxBytes), nil)
	}
	v, err := jsonv.ParseString(text)
	if err != nil {
		return nil, contractInvalid(fmt.Sprintf("#%s must contain valid JSON", blockName), nil)
	}
	return v, nil
}

// Managed is a parsed managed document. The data pair's fields keep their
// historical names; the config pair and the example block are nil when absent.
type Managed struct {
	HTML         string
	Schema       *jsonv.Object
	Envelope     *jsonv.Object
	Config       *jsonv.Object
	ConfigSchema *jsonv.Object
	Example      *jsonv.Object

	DataSHA256         string
	SchemaSHA256       string
	ConfigSHA256       string // "" when the page has no config (JS null)
	ConfigSchemaSHA256 string // "" when the page has no config (JS null)
	TemplateSHA256     string

	DataBlock    *Block
	ConfigBlock  *Block
	ExampleBlock *Block

	Validate       *Validator
	ValidateConfig *Validator
}

// Data is the envelope's data object.
func (m *Managed) Data() *jsonv.Object { return m.Envelope.Value("data").(*jsonv.Object) }

// SourceAsOf is the envelope's source_as_of stamp.
func (m *Managed) SourceAsOf() string { s, _ := m.Envelope.Value("source_as_of").(string); return s }

// RefreshedAt is the envelope's refreshed_at stamp.
func (m *Managed) RefreshedAt() string { s, _ := m.Envelope.Value("refreshed_at").(string); return s }

// DataUsage is the current payload's size against the managed-data limit
// (issue #109).
func (m *Managed) DataUsage() datalimits.Usage { return envelopeUsage(m.Envelope) }

func envelopeUsage(env *jsonv.Object) datalimits.Usage {
	payload := len(jsonv.Stringify(env.Value("data")))
	return datalimits.UsageOf(payload, len(datalimits.EscapedJSON(env)))
}

// ParseManaged reads a managed document against spec.
func ParseManaged(html string, spec Spec) (*Managed, error) {
	found, err := findBlocks(html, spec)
	if err != nil {
		return nil, err
	}
	data := found["data"]
	dataBlock := data.payloadBlock
	schemaV, err := parseJSONBlock(html[data.schemaBlock.ContentStart:data.schemaBlock.ContentEnd], SchemaID, MaxSchemaBytes())
	if err != nil {
		return nil, err
	}
	envV, err := parseJSONBlock(html[dataBlock.ContentStart:dataBlock.ContentEnd], DataID, MaxDataBytes())
	if err != nil {
		return nil, err
	}
	validate, err := compileSchemaCached(schemaV, "data")
	if err != nil {
		return nil, err
	}
	if err := assertEnvelope(envV); err != nil {
		return nil, err
	}
	envelope := envV.(*jsonv.Object)
	schema := schemaV.(*jsonv.Object)
	canonicalData, err := validateData(validate, envelope.Value("data"), "data_contract_invalid")
	if err := assertCurrentPayload(err, fmt.Sprintf("the current #%s payload does not satisfy its schema", DataID)); err != nil {
		return nil, err
	}

	m := &Managed{HTML: html, Schema: schema, Envelope: envelope, DataBlock: dataBlock, Validate: validate}
	if cfg := found["config"]; cfg != nil {
		m.ConfigBlock = cfg.payloadBlock
		csV, err := parseJSONBlock(html[cfg.schemaBlock.ContentStart:cfg.schemaBlock.ContentEnd], ConfigSchemaID, MaxSchemaBytes())
		if err != nil {
			return nil, err
		}
		cV, err := parseJSONBlock(html[m.ConfigBlock.ContentStart:m.ConfigBlock.ContentEnd], ConfigID, MaxConfigBytes())
		if err != nil {
			return nil, err
		}
		m.ValidateConfig, err = compileSchemaCached(csV, "config")
		if err != nil {
			return nil, err
		}
		if err := assertCurrentPayload(validateConfig(m.ValidateConfig, cV, "data_contract_invalid"),
			fmt.Sprintf("the current #%s payload does not satisfy its schema", ConfigID)); err != nil {
			return nil, err
		}
		m.ConfigSchema = csV.(*jsonv.Object)
		m.Config = cV.(*jsonv.Object)
	}
	// Example data is preview-only, but it is still validated: a design whose
	// example does not satisfy its own data schema would render a preview no
	// real refresh could reproduce, which is worse than no preview at all.
	if ex := found["example"]; ex != nil {
		m.ExampleBlock = ex.payloadBlock
		exV, err := parseJSONBlock(html[m.ExampleBlock.ContentStart:m.ExampleBlock.ContentEnd], ExampleID, MaxDataBytes())
		if err != nil {
			return nil, err
		}
		_, err = validateData(validate, exV, "data_contract_invalid")
		if err := assertCurrentPayload(err, fmt.Sprintf("the #%s payload does not satisfy the #%s schema", ExampleID, SchemaID)); err != nil {
			return nil, err
		}
		m.Example = exV.(*jsonv.Object)
	}

	m.DataSHA256 = sha256Hex(canonicalData)
	m.SchemaSHA256 = mustHash(schema)
	if m.Config != nil {
		m.ConfigSHA256 = mustHash(m.Config)
		m.ConfigSchemaSHA256 = mustHash(m.ConfigSchema)
	}
	m.TemplateSHA256 = sha256Hex(templateIdentity(html, dataBlock))
	return m, nil
}

// ParseManagedHTML is ParseManaged with the page spec.
func ParseManagedHTML(html string) (*Managed, error) { return ParseManaged(html, PageSpec) }

// templateIdentity is "everything about these bytes except the refresh
// payload". Only the DATA block is elided. Config stays inline on purpose: to a
// data update, config is part of the immutable template it is pouring rows
// into, so editing config MUST change this hash, or update_page_data's (data,
// template, source) dedupe could match a version from before the config change
// and silently republish the old config. For a page with no config block this
// is byte-for-byte the original formula, so template_sha256 is stable for
// every page already deployed.
func templateIdentity(html string, dataBlock *Block) string {
	return html[:dataBlock.ContentStart] + dataPlaceholder + html[dataBlock.ContentEnd:]
}

// ── inferring a config schema ────────────────────────────────────────────────
// A page and a template are the same artifact used two ways, and the only thing
// that used to stand between them was 4 KB of hand-written JSON Schema. So a
// page may ship #pages-config with no schema block and get one derived from the
// values. It pins types and rejects unknown keys — what catches a typo'd key or
// a number sent as a string — and is deliberately NOT clever: no formats, enums
// or patterns, because a guess that rejects a legitimate future value is worse
// than a loose schema. Deterministic, so redeploying the same file dedupes.

const inferMaxDepth = 12

func inferSchema(value any, depth int) *jsonv.Object {
	if depth >= inferMaxDepth {
		return jsonv.NewObject()
	}
	switch v := value.(type) {
	case nil:
		return jsonv.NewObject() // unknowable — never constrain it to null
	case bool:
		return jsonv.ObjectOf("type", "boolean")
	case float64:
		if v == math.Trunc(v) && !math.IsInf(v, 0) {
			return jsonv.ObjectOf("type", "integer")
		}
		return jsonv.ObjectOf("type", "number")
	case string:
		return jsonv.ObjectOf("type", "string")
	case []any:
		if len(v) == 0 {
			return jsonv.ObjectOf("type", "array")
		}
		// One items schema only when every element agrees.
		var first *jsonv.Object
		distinct := map[string]bool{}
		for _, e := range v {
			shape := inferSchema(e, depth+1)
			key := jsonv.Stringify(shape)
			if !distinct[key] && first == nil {
				first = shape
			}
			distinct[key] = true
		}
		if len(distinct) == 1 {
			return jsonv.ObjectOf("type", "array", "items", first)
		}
		return jsonv.ObjectOf("type", "array")
	case *jsonv.Object:
		keys := v.Keys()
		if len(keys) == 0 {
			// additionalProperties:false with no properties would reject everything.
			return jsonv.ObjectOf("type", "object")
		}
		shapes := make([]*jsonv.Object, len(keys))
		distinct := map[string]bool{}
		allObjects := true
		for i, k := range keys {
			shapes[i] = inferSchema(v.Value(k), depth+1)
			distinct[jsonv.Stringify(shapes[i])] = true
			if o, ok := v.Value(k).(*jsonv.Object); !ok || o.Len() == 0 {
				allObjects = false
			}
		}
		// A DICTIONARY, not a record: a map keyed by deal code must not have its
		// current keys frozen. Recognised narrowly — two or more keys whose
		// values are all objects of the identical shape — so {sspLabel,
		// dspLabel} stays a record and keeps its typo protection.
		if len(keys) >= 2 && allObjects && len(distinct) == 1 {
			return jsonv.ObjectOf("type", "object", "additionalProperties", shapes[0])
		}
		properties := jsonv.NewObject()
		var required []any
		for i, k := range keys {
			properties.Set(k, shapes[i])
			// A null value reads as "not set", so those keys are optional.
			if v.Value(k) != nil {
				required = append(required, k)
			}
		}
		schema := jsonv.ObjectOf("type", "object", "additionalProperties", false, "properties", properties)
		if len(required) > 0 {
			schema.Set("required", required)
		}
		return schema
	}
	return jsonv.NewObject()
}

// InferConfigSchema derives a #pages-config-schema from a config object.
func InferConfigSchema(config any) (*jsonv.Object, error) {
	schema := jsonv.ObjectOf("$schema", "https://json-schema.org/draft/2020-12/schema")
	inferred := inferSchema(config, 0)
	for _, k := range inferred.Keys() {
		schema.Set(k, inferred.Value(k))
	}
	if schema.Value("type") != "object" {
		return nil, contractInvalid(fmt.Sprintf("#%s must contain a JSON object so its schema can be derived", ConfigID), nil)
	}
	// Self-check: a generated schema that does not accept the values it was
	// generated from would fail the deploy it is supposed to enable.
	v, err := compileSchemaCached(schema, "config")
	if err != nil {
		return nil, err
	}
	if errs := v.Validate(config); len(errs) > 0 {
		if len(errs) > 5 {
			errs = errs[:5]
		}
		return nil, contractInvalid(fmt.Sprintf("could not derive a #%s for this config", ConfigSchemaID), jsonv.ObjectOf("errors", toAnySlice(errs)))
	}
	return schema, nil
}

func toAnySlice(errs []ValidationError) []any {
	out := make([]any, len(errs))
	for i, e := range errs {
		out[i] = e
	}
	return out
}

// EnsuredHTML is EnsureConfigSchema's result.
type EnsuredHTML struct {
	HTML      string
	Generated bool
	Schema    *jsonv.Object // the derived schema, nil when nothing was generated
}

// EnsureConfigSchema is the ONE place that tolerates a page carrying a config
// payload without its schema: it inserts a derived #pages-config-schema
// immediately before #pages-config. Stored HTML always has the complete pair,
// so ParseManaged keeps its strict both-or-neither rule.
func EnsureConfigSchema(html string) (EnsuredHTML, error) {
	unchanged := EnsuredHTML{HTML: html}
	if !strings.Contains(html, ConfigID) || strings.Contains(html, ConfigSchemaID) {
		return unchanged, nil
	}
	matches := map[string][]*Block{ConfigID: nil, ConfigSchemaID: nil}
	if err := walkHTML(html, matches); err != nil {
		return EnsuredHTML{}, err
	}
	if len(matches[ConfigSchemaID]) > 0 || len(matches[ConfigID]) == 0 {
		return unchanged, nil
	}
	if len(matches[ConfigID]) != 1 {
		return EnsuredHTML{}, contractInvalid(fmt.Sprintf("managed pages allow exactly one #%s block", ConfigID), nil)
	}
	block := matches[ConfigID][0]
	if block.Type != DataType {
		return EnsuredHTML{}, contractInvalid(fmt.Sprintf(`#%s must use type="%s"`, ConfigID, DataType), nil)
	}
	config, err := parseJSONBlock(html[block.ContentStart:block.ContentEnd], ConfigID, MaxConfigBytes())
	if err != nil {
		return EnsuredHTML{}, err
	}
	schema, err := InferConfigSchema(config)
	if err != nil {
		return EnsuredHTML{}, err
	}
	encoded := datalimits.EscapedJSON(schema)
	if limit := MaxSchemaBytes(); len(encoded) > limit {
		return EnsuredHTML{}, contractInvalid(fmt.Sprintf("the derived #%s exceeds its %d-byte limit", ConfigSchemaID, limit), nil)
	}
	inserted := fmt.Sprintf(`<script type="%s" id="%s">%s</script>`+"\n", SchemaType, ConfigSchemaID, encoded)
	return EnsuredHTML{HTML: html[:block.ElementStart] + inserted + html[block.ElementStart:], Generated: true, Schema: schema}, nil
}

// ── materialization ──────────────────────────────────────────────────────────

type splice struct {
	start, end int
	text       string
	isData     bool
}

// spliceBlocks replaces ranges in DESCENDING offset order, so every
// not-yet-applied range — computed against the original html — stays valid.
func spliceBlocks(html string, reps []splice) string {
	sorted := append([]splice(nil), reps...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].start > sorted[j].start })
	out := html
	for _, r := range sorted {
		out = out[:r.start] + r.text + out[r.end:]
	}
	return out
}

// BlockUpdate is what a write replaces. A field whose Set flag is false leaves
// that block's bytes untouched, which is what makes a config update provably
// data-preserving and a data update provably config-preserving. Envelope is a
// complete preserved envelope (moving data between layouts keeps its
// freshness stamps); it excludes Data.
type BlockUpdate struct {
	Data        any
	SetData     bool
	Config      any
	SetConfig   bool
	Envelope    any
	SetEnvelope bool
}

// MaterializeOptions carries the new data's source timestamp and the clock
// (zero Now means time.Now()).
type MaterializeOptions struct {
	SourceAsOf any
	Now        time.Time
}

// Materialized is the document a write produces and its identity hashes.
type Materialized struct {
	HTML               string
	Envelope           *jsonv.Object
	Config             *jsonv.Object
	DataSHA256         string
	SchemaSHA256       string
	ConfigSHA256       string
	ConfigSchemaSHA256 string
	TemplateSHA256     string
}

// DataUsage is the written payload's size against the managed-data limit
// (issue #109).
func (m *Materialized) DataUsage() datalimits.Usage { return envelopeUsage(m.Envelope) }

// MaterializeBlocks writes new payloads into the managed blocks of a parsed
// document. Example data is a property of the TEMPLATE, never of what the
// template produces, so the example element is always deleted.
func MaterializeBlocks(m *Managed, u BlockUpdate, opts MaterializeOptions) (*Materialized, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	var reps []splice
	envelope := m.Envelope
	dataSHA := m.DataSHA256
	configOut := m.Config
	configSHA := m.ConfigSHA256

	data, setData := u.Data, u.SetData
	if u.SetEnvelope {
		if setData {
			return nil, apierror.BadRequest("provide data or a preserved envelope, not both", "data_validation_failed", nil)
		}
		if err := assertEnvelope(u.Envelope); err != nil {
			return nil, err
		}
		data, setData = u.Envelope.(*jsonv.Object).Value("data"), true
	}
	if setData {
		if err := assertRepresentableNumbers(data, "data_validation_failed", "data", ""); err != nil {
			return nil, err
		}
		canonicalData, err := validateData(m.Validate, data, "data_validation_failed")
		if err != nil {
			return nil, err
		}
		if u.SetEnvelope {
			envelope = u.Envelope.(*jsonv.Object)
		} else {
			sourceAsOf, err := NormalizeSourceAsOf(opts.SourceAsOf, now)
			if err != nil {
				return nil, err
			}
			envelope = jsonv.ObjectOf("contract_version", 1.0, "refreshed_at", jsISOString(timeMs(now)), "source_as_of", sourceAsOf, "data", data)
		}
		encoded := datalimits.EscapedJSON(envelope)
		if err := datalimits.AssertDomainSize(len(encoded), "envelope", ""); err != nil {
			return nil, err
		}
		reps = append(reps, splice{start: m.DataBlock.ContentStart, end: m.DataBlock.ContentEnd, text: encoded, isData: true})
		dataSHA = sha256Hex(canonicalData)
	}

	if u.SetConfig {
		if m.ConfigBlock == nil || m.ValidateConfig == nil {
			return nil, apierror.Conflict(fmt.Sprintf("page has no #%s block to update; it was not built from a template", ConfigID), "page_not_template_managed", nil)
		}
		if err := assertRepresentableNumbers(u.Config, "config_validation_failed", "config", ""); err != nil {
			return nil, err
		}
		if err := validateConfig(m.ValidateConfig, u.Config, "config_validation_failed"); err != nil {
			return nil, err
		}
		encoded := datalimits.EscapedJSON(u.Config)
		if limit := MaxConfigBytes(); len(encoded) > limit {
			return nil, apierror.BadRequest(fmt.Sprintf("materialized config exceeds the %d-byte limit", limit), "config_validation_failed", nil)
		}
		reps = append(reps, splice{start: m.ConfigBlock.ContentStart, end: m.ConfigBlock.ContentEnd, text: encoded})
		configOut = u.Config.(*jsonv.Object)
		configSHA = mustHash(u.Config)
	}

	if m.ExampleBlock != nil {
		reps = append(reps, splice{start: m.ExampleBlock.ElementStart, end: m.ExampleBlock.ElementEnd})
	}

	html := spliceBlocks(m.HTML, reps)
	if limit := MaxTemplateBytes(); len(html) > limit {
		return nil, apierror.BadRequest(fmt.Sprintf("materialized page exceeds the %d-byte limit", limit), "data_validation_failed", nil)
	}
	// The new template identity, without offset arithmetic: splice the same
	// replacements but put the placeholder where the data content goes.
	var idReps []splice
	for _, r := range reps {
		if !r.isData {
			idReps = append(idReps, r)
		}
	}
	idReps = append(idReps, splice{start: m.DataBlock.ContentStart, end: m.DataBlock.ContentEnd, text: dataPlaceholder, isData: true})
	identity := spliceBlocks(m.HTML, idReps)

	return &Materialized{
		HTML:               html,
		Envelope:           envelope,
		Config:             configOut,
		DataSHA256:         dataSHA,
		SchemaSHA256:       m.SchemaSHA256,
		ConfigSHA256:       configSHA,
		ConfigSchemaSHA256: m.ConfigSchemaSHA256,
		TemplateSHA256:     sha256Hex(identity),
	}, nil
}

// Materialize writes new data with a source timestamp.
func Materialize(m *Managed, data any, sourceAsOf any, now time.Time) (*Materialized, error) {
	return MaterializeBlocks(m, BlockUpdate{Data: data, SetData: true}, MaterializeOptions{SourceAsOf: sourceAsOf, Now: now})
}

// AssembledTemplate is AssembleTemplate's result.
type AssembledTemplate struct {
	HTML         string
	Config       *jsonv.Object
	ConfigSchema *jsonv.Object
}

// AssembleTemplate turns a PAGE that already separates design from data into
// the template form, without moving the bytes: its live data is replaced by the
// empty state a template must ship (epoch stamps, so the first real ingest can
// never be rejected as a regression), and exampleData (nil for none) is kept as
// preview-only example rows. It cannot go through MaterializeBlocks, which
// deletes the example block.
func AssembleTemplate(pageHTML string, emptyData, exampleData any) (*AssembledTemplate, error) {
	m, err := ParseManaged(pageHTML, PageSpec)
	if err != nil {
		return nil, err
	}
	if m.ConfigBlock == nil {
		return nil, apierror.Conflict(
			fmt.Sprintf("this page has no #%s block, so there is nothing to vary per instance; ", ConfigID)+
				fmt.Sprintf("separate its per-instance values into #%s first and redeploy", ConfigID),
			"page_not_template_managed", nil)
	}
	if _, err := validateData(m.Validate, emptyData, "data_validation_failed"); err != nil {
		return nil, err
	}
	if exampleData != nil {
		if _, err := validateData(m.Validate, exampleData, "data_validation_failed"); err != nil {
			return nil, err
		}
	}
	epoch := jsISOString(0)
	envelope := jsonv.ObjectOf("contract_version", 1.0, "refreshed_at", epoch, "source_as_of", epoch, "data", emptyData)
	reps := []splice{{start: m.DataBlock.ContentStart, end: m.DataBlock.ContentEnd, text: datalimits.EscapedJSON(envelope)}}
	if exampleData != nil {
		element := fmt.Sprintf("\n<script type=\"%s\" id=\"%s\">%s</script>", DataType, ExampleID, datalimits.EscapedJSON(exampleData))
		// A zero-length range right after the data element: an insertion
		// through the same machinery as a replacement.
		reps = append(reps, splice{start: m.DataBlock.ElementEnd, end: m.DataBlock.ElementEnd, text: element})
	}
	html := spliceBlocks(pageHTML, reps)
	if limit := MaxTemplateBytes(); len(html) > limit {
		return nil, apierror.BadRequest(fmt.Sprintf("the assembled template exceeds the %d-byte limit", limit), "data_validation_failed", nil)
	}
	// Prove it: the result has to satisfy the template contract on its own.
	if _, err := ParseManaged(html, TemplateSpec); err != nil {
		return nil, err
	}
	return &AssembledTemplate{HTML: html, Config: m.Config, ConfigSchema: m.ConfigSchema}, nil
}

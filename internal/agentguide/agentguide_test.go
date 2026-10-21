// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package agentguide

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	pages "github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/apierror"
)

// Ported from test/agent-guide.test.js. The tests that need the MCP tool
// table (input/output schema, renderText wiring, "the workflow names only
// tools that exist", token scopes) belong with internal/mcp and
// internal/tokens.

func get(t *testing.T, topic, section string) *Result {
	t.Helper()
	var tp, sp *string
	if topic != "" {
		tp = &topic
	}
	if section != "" {
		sp = &section
	}
	out, err := Get(tp, sp)
	if err != nil {
		t.Fatalf("Get(%q, %q): %v", topic, section, err)
	}
	return out
}

func docText(t *testing.T, topic string) string {
	t.Helper()
	b, err := fs.ReadFile(pages.Docs(), Topics[topic])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIndexListsEveryTopicWorkflowFirst(t *testing.T) {
	out := get(t, "", "")
	if out.Topic != nil || out.Section != nil {
		t.Fatal("the index has no topic or section")
	}
	var topics []string
	for _, e := range out.Entries {
		topics = append(topics, e.Topic)
	}
	if !slices.Equal(topics, []string{"workflow", "authoring", "templates", "data_updates", "motion"}) {
		t.Fatalf("index order %v", topics)
	}
	for _, topic := range TopicNames {
		if !strings.Contains(out.Content, "`"+topic+"`") {
			t.Errorf("index content misses %s", topic)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(out.ContentSHA256) {
		t.Error("content_sha256 shape")
	}
}

func TestEveryTopicSplitsIntoUniqueBoundedSections(t *testing.T) {
	for _, topic := range TopicNames {
		out := get(t, topic, "")
		if len(out.Entries) == 0 {
			t.Errorf("%s has no sections", topic)
		}
		seen := map[string]bool{}
		for _, e := range out.Entries {
			slug := *e.Section
			if seen[slug] {
				t.Errorf("%s: duplicate slug %s", topic, slug)
			}
			seen[slug] = true
			if !SectionRE.MatchString(slug) {
				t.Errorf("%s: slug %q is not slug-shaped", topic, slug)
			}
			// One read must not flood an agent's context: the largest section
			// shipped today is about 12 KB.
			if e.Bytes > 16*1024 {
				t.Errorf("%s/%s is %d bytes", topic, slug, e.Bytes)
			}
			s := get(t, topic, slug)
			if !strings.HasPrefix(s.Content, "## "+e.Title) {
				t.Errorf("%s/%s does not start at its heading", topic, slug)
			}
		}
	}
}

func TestHeadingInsideFenceIsContent(t *testing.T) {
	doc := ParseDoc("# T\n\nintro\n\n## One\n\n```md\n## not a heading\n```\n\n## Two\n\ntext\n")
	var slugs []string
	for _, s := range doc.Sections {
		slugs = append(slugs, s.Slug)
	}
	if !slices.Equal(slugs, []string{"one", "two"}) {
		t.Fatalf("slugs %v", slugs)
	}
	if !strings.Contains(doc.Sections[0].Text, "## not a heading") {
		t.Error("fenced heading lost")
	}
	if doc.Intro != "# T\n\nintro" {
		t.Errorf("intro %q", doc.Intro)
	}
	if doc.Title != "T" {
		t.Errorf("title %q", doc.Title)
	}
}

func TestParseDocDuplicateAndEmptySlugs(t *testing.T) {
	doc := ParseDoc("## Same\r\na\r\n## Same\n## “Same”\n## ***\n## `x_y` & Z's\n")
	var slugs []string
	for _, s := range doc.Sections {
		slugs = append(slugs, s.Slug)
	}
	want := []string{"same", "same-2", "same-3", "section-4", "xy-zs"}
	if !slices.Equal(slugs, want) {
		t.Fatalf("slugs %v, want %v", slugs, want)
	}
	if doc.Sections[0].Text != "## Same\na" {
		t.Errorf("CRLF not normalised: %q", doc.Sections[0].Text)
	}
}

func TestUnknownSectionAndSectionWithoutTopicRefused(t *testing.T) {
	topic, section := "workflow", "no-such-section"
	_, err := Get(&topic, &section)
	e, ok := apierror.As(err)
	if !ok || e.Status != 404 || e.Code != "guide_section_not_found" {
		t.Fatalf("got %v", err)
	}
	if d, _ := e.Details.(sectionNotFoundDetails); !slices.Contains(d.Available, "never") {
		t.Errorf("details %+v do not list the available sections", e.Details)
	}
	if e.Message != `no section "no-such-section" in topic workflow` {
		t.Errorf("message %q", e.Message)
	}
	never := "never"
	_, err = Get(nil, &never)
	if e, ok := apierror.As(err); !ok || e.Status != 400 || e.Code != "guide_topic_required" {
		t.Fatalf("got %v", err)
	}
	bogus := "README"
	_, err = Get(&bogus, nil)
	if e, ok := apierror.As(err); !ok || e.Status != 400 {
		t.Fatalf("unknown topic: %v", err)
	}
}

func TestRenderedTextLeadsWithContent(t *testing.T) {
	re := regexp.MustCompile(`get_guide · .* · Pages \d+\.\d+\.\d+ · sha256 [0-9a-f]{12}`)
	for _, args := range [][2]string{{"", ""}, {"motion", ""}, {"workflow", "never"}} {
		out := get(t, args[0], args[1])
		text := Render(out)
		if !strings.HasPrefix(text, out.Content) {
			t.Errorf("%v: text does not lead with the guide", args)
		}
		if !re.MatchString(text) {
			t.Errorf("%v: footer %q", args, text[len(out.Content):])
		}
	}
	if !strings.Contains(Render(get(t, "workflow", "never")), "Other sections in workflow: who-owns-what, ") {
		t.Error("a section's text names the topic's other sections")
	}
}

func TestEveryRenderedResponseIsBoundedAndHashed(t *testing.T) {
	reads := [][2]string{{"", ""}}
	for _, topic := range TopicNames {
		reads = append(reads, [2]string{topic, ""})
		for _, e := range get(t, topic, "").Entries {
			reads = append(reads, [2]string{topic, *e.Section})
		}
	}
	for _, args := range reads {
		out := get(t, args[0], args[1])
		sum := sha256.Sum256([]byte(out.Content))
		if out.ContentSHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%v: hash mismatch", args)
		}
		if n := len(Render(out)); n > 16*1024 {
			t.Errorf("%v renders %d bytes", args, n)
		}
	}
}

// ── tenant neutrality ────────────────────────────────────────────────────────
// Each deployment serves this text to its own agents, so it must be generic:
// no real organisation's data, endpoints or identifiers, and no internal
// product names presented as if every deployment had them. These are generic
// heuristics, not proof. Real customer names belong in a private list read
// when PAGES_PRIVATE_DENYLIST points at it (one term or /regex/flags per
// line); it must never be committed here, and failures name only the entry
// number so a CI log cannot republish it.

var (
	allowedURLHosts = []*regexp.Regexp{regexp.MustCompile(`(?i)(^|\.)example\.com$`), regexp.MustCompile(`(?i)^json-schema\.org$`)}
	syntheticIDs    = map[string]bool{"9000001": true}
	internalNames   = []*regexp.Regexp{
		regexp.MustCompile(`(?i)elcanotek\.com`), regexp.MustCompile(`(?i)elcano-pages\.com`),
		regexp.MustCompile(`\bCutlass\b`), regexp.MustCompile(`\bMOC\b`), regexp.MustCompile(`\bFleet\b`), regexp.MustCompile(`\bVictoria\b`),
	}
	secretParams = map[string]bool{
		"token": true, "access_token": true, "refresh_token": true, "id_token": true, "key": true, "api_key": true, "apikey": true,
		"secret": true, "client_secret": true, "password": true, "sig": true, "signature": true, "auth": true,
	}
	absURLRE = regexp.MustCompile("(?i)\\bhttps?://[^\\s`\"')<>\\]]+")
	// Go regexp has no lookbehind: the JS (?<![\w:/]) is checked by hand.
	relURLRE = regexp.MustCompile("(?i)//(?:(?:[a-z0-9-]+\\.)+[a-z]{2,}|\\d{1,3}(?:\\.\\d{1,3}){3}|\\[[0-9a-f:.]+\\])(?:[/:?#][^\\s`\"')<>\\]]*)?")
	emailRE  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

func isWordColonSlash(b byte) bool {
	return b == '_' || b == ':' || b == '/' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func urlFindings(text string) []string {
	var problems []string
	urls := absURLRE.FindAllString(text, -1)
	for _, loc := range relURLRE.FindAllStringIndex(text, -1) {
		if loc[0] > 0 && isWordColonSlash(text[loc[0]-1]) {
			continue
		}
		urls = append(urls, "https:"+text[loc[0]:loc[1]])
	}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			problems = append(problems, "unparseable URL "+raw)
			continue
		}
		host := u.Hostname()
		if !slices.ContainsFunc(allowedURLHosts, func(re *regexp.Regexp) bool { return re.MatchString(host) }) {
			problems = append(problems, "non-documentation host "+raw)
		}
		if u.User != nil {
			problems = append(problems, "credentials in URL "+raw)
		}
		for name := range u.Query() {
			if secretParams[strings.ReplaceAll(strings.ToLower(name), "-", "_")] {
				problems = append(problems, "secret-shaped query in "+raw)
				break
			}
		}
	}
	for _, email := range emailRE.FindAllString(text, -1) {
		if !strings.HasSuffix(strings.ToLower(email), "@example.com") {
			problems = append(problems, "non-example email "+email)
		}
	}
	return problems
}

var (
	longNumRE = regexp.MustCompile(`\d{6,}`)
	acctRE    = regexp.MustCompile(`(?i)\b(?:acct|elc)[-_ ]?\d{3,}\b`)
	storageRE = regexp.MustCompile(`s3://|\bStorage/`)
)

func isWordOrDot(b byte) bool { return b == '.' || isWordColonSlash(b) && b != ':' && b != '/' }

func idFindings(text string) []string {
	var problems []string
	for _, loc := range longNumRE.FindAllStringIndex(text, -1) {
		// (?<![\w.])\d{6,}(?![\w.]) — a maximal digit run not glued to a word
		// character or dot on either side.
		if loc[0] > 0 && isWordOrDot(text[loc[0]-1]) || loc[1] < len(text) && isWordOrDot(text[loc[1]]) {
			continue
		}
		if id := text[loc[0]:loc[1]]; !syntheticIDs[id] {
			problems = append(problems, "record-shaped id "+id)
		}
	}
	if code := acctRE.FindString(text); code != "" {
		problems = append(problems, "account or campaign code "+code)
	}
	if storageRE.MatchString(text) {
		problems = append(problems, "storage location")
	}
	return problems
}

var errBadEntry = errors.New("bad entry")

// compilePrivateTerms — one matcher per entry. A literal matches anywhere,
// case-insensitively, when not glued to another letter or digit (so Markdown
// emphasis like _Name_ still matches). A /regex/ entry may use only the i, m,
// s and u flags. A bad entry fails with its number only: the native error
// would quote the private text.
func compilePrivateTerms(lines []string) ([]*regexp.Regexp, error) {
	entryRE := regexp.MustCompile(`^/(.+)/([A-Za-z]*)$`)
	var out []*regexp.Regexp
	n := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
		re, err := func() (*regexp.Regexp, error) {
			m := entryRE.FindStringSubmatch(line)
			if strings.HasPrefix(line, "/") && m == nil {
				return nil, errBadEntry
			}
			if m != nil {
				flags := "i"
				for _, f := range m[2] {
					switch f {
					case 'i':
					case 'm', 's':
						flags += string(f)
					case 'u': // Go regexp is always Unicode-aware.
					default:
						return nil, errBadEntry
					}
				}
				return regexp.Compile("(?" + flags + ")" + m[1])
			}
			return regexp.Compile(`(?i)(?:^|[^A-Za-z0-9])` + regexp.QuoteMeta(line) + `(?:$|[^A-Za-z0-9])`)
		}()
		if err != nil {
			return nil, fmt.Errorf("private denylist entry #%d is not a valid term or /regex/flags", n)
		}
		out = append(out, re)
	}
	return out, nil
}

func privateFindings(text string, terms []*regexp.Regexp) []string {
	var out []string
	for i, re := range terms {
		if re.MatchString(text) {
			out = append(out, fmt.Sprintf("private denylist entry #%d", i+1))
		}
	}
	return out
}

func TestServedURLsPointOnlyAtDocumentationDomains(t *testing.T) {
	for _, topic := range TopicNames {
		if p := urlFindings(docText(t, topic)); len(p) > 0 {
			t.Errorf("%s: %v", Topics[topic], p)
		}
	}
}

func TestServedExamplesCarryNoRecordShapedIdentifiers(t *testing.T) {
	for _, topic := range TopicNames {
		if p := idFindings(docText(t, topic)); len(p) > 0 {
			t.Errorf("%s: %v", Topics[topic], p)
		}
	}
}

func TestServedTextNamesNoInternalDomainOrTool(t *testing.T) {
	for _, topic := range TopicNames {
		text := docText(t, topic)
		for _, re := range internalNames {
			if hit := re.FindString(text); hit != "" {
				t.Errorf("%s contains %s", Topics[topic], hit)
			}
		}
	}
}

func TestChecksFireOnInventedLeaks(t *testing.T) {
	// Every fixture is invented; none names a real organisation.
	for _, leak := range []string{
		"see https://dashboards.invented-corp.test/page",
		"see HTTPS://dashboards.invented-corp.test/page",
		"see //dashboards.invented-corp.test/page",
		"see https://demo:invented-secret@example.com/page",
		"see https://example.com/page?token=invented-secret",
		"see https://example.com/page?%74oken=invented-secret",
		"see https://example.com/page?access_token=invented-secret",
		"see //192.0.2.1/page",
		"see //[2001:db8::1]/page",
		"mail ops@invented-corp.test",
	} {
		if len(urlFindings(leak)) == 0 {
			t.Errorf("no finding for %q", leak)
		}
	}
	for _, fine := range []string{"see https://content.example.com/page", "see https://JSON-SCHEMA.org/draft", "mail OPS@EXAMPLE.COM", "a protocol-relative (`//host/path`) URL"} {
		if p := urlFindings(fine); len(p) > 0 {
			t.Errorf("%q: %v", fine, p)
		}
	}
	for _, leak := range []string{`{"dealId":"4815162"}`, "filter: ACCT00123", "filter: elc-0042", "s3://invented-bucket/x", "Storage/invented.csv"} {
		if len(idFindings(leak)) == 0 {
			t.Errorf("no finding for %q", leak)
		}
	}
	if p := idFindings(`{"dealId":"9000001","bytes":"1.048576"}`); len(p) > 0 {
		t.Errorf("synthetic ids flagged: %v", p)
	}
}

func TestPrivateDenylistMatchesDisguisesAndNeverEchoes(t *testing.T) {
	terms, err := compilePrivateTerms([]string{"# comment", "Invented Holdings", `/invent(?:ed)? ?co\b/`, "Invented Co."})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"by _Invented Holdings_.", "INVENTED HOLDINGS", "**Invented Holdings**", "from inventedco", "an Invented Co. page"} {
		if len(privateFindings(text, terms)) == 0 {
			t.Errorf("no finding for %q", text)
		}
	}
	one, _ := compilePrivateTerms([]string{"Invented Holdings"})
	if p := privateFindings("Uninvented Holdingsworth", one); len(p) > 0 {
		t.Errorf("glued match: %v", p)
	}
	for _, bad := range []string{"/InventedSecretName[/i", "/InventedSecretName/y", "/InventedSecretName/g", "/InventedSecretName/I", "/InventedSecretName"} {
		_, err := compilePrivateTerms([]string{"fine", bad})
		if err == nil || err.Error() != "private denylist entry #2 is not a valid term or /regex/flags" || strings.Contains(err.Error(), "InventedSecretName") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestNoPrivateDenylistTermInServedText(t *testing.T) {
	file := os.Getenv("PAGES_PRIVATE_DENYLIST")
	if file == "" {
		t.Skip("PAGES_PRIVATE_DENYLIST is not set (it is kept outside this public repository)")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("PAGES_PRIVATE_DENYLIST is set but the file cannot be read")
	}
	terms, err := compilePrivateTerms(strings.Split(string(b), "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(terms) == 0 {
		t.Fatal("the private denylist is empty")
	}
	for _, topic := range TopicNames {
		if p := privateFindings(docText(t, topic), terms); len(p) > 0 {
			t.Errorf("%s: %v", Topics[topic], p)
		}
	}
}

// TestMatchesJS runs lib/agent-guide.js over the same docs and compares every
// result and rendering byte for byte. It skips once the Node tree is gone.
func TestMatchesJS(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "lib", "agent-guide.js")); err != nil {
		t.Skip("lib/agent-guide.js is gone")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := `
const g = require(process.argv[1] + "/lib/agent-guide.js");
const reads = [{}];
for (const topic of g.TOPIC_NAMES) { reads.push({ topic }); for (const e of g.get({ topic }).entries) reads.push({ topic, section: e.section }); }
reads.push({ topic: "workflow", section: "no-such" });
const out = reads.map((a) => { try { const r = g.get(a); return { r, text: g.render(r) }; } catch (e) { return { status: e.status, message: e.message, code: e.code, details: e.details }; } });
process.stdout.write(JSON.stringify(out));`
	cmd := exec.CommandContext(t.Context(), node, "-e", script, root)
	jsOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	saved := ProductVersion
	t.Cleanup(func() { ProductVersion = saved })
	pkg, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct{ Version string }
	if err := json.Unmarshal(pkg, &meta); err != nil {
		t.Fatal(err)
	}
	ProductVersion = meta.Version

	type jsErr struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
		Code    string `json:"code"`
		Details any    `json:"details"`
	}
	var outs []any
	reads := [][2]string{{"", ""}}
	for _, topic := range TopicNames {
		reads = append(reads, [2]string{topic, ""})
		for _, e := range get(t, topic, "").Entries {
			reads = append(reads, [2]string{topic, *e.Section})
		}
	}
	reads = append(reads, [2]string{"workflow", "no-such"})
	for _, a := range reads {
		var tp, sp *string
		if a[0] != "" {
			tp = &a[0]
		}
		if a[1] != "" {
			sp = &a[1]
		}
		r, err := Get(tp, sp)
		if err != nil {
			e, _ := apierror.As(err)
			outs = append(outs, jsErr{e.Status, e.Message, e.Code, e.Details})
			continue
		}
		outs = append(outs, struct {
			R    *Result `json:"r"`
			Text string  `json:"text"`
		}{r, Render(r)})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(outs); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSuffix(buf.String(), "\n")
	if got != string(jsOut) {
		for i := range min(len(got), len(jsOut)) {
			if got[i] != jsOut[i] {
				lo := max(0, i-200)
				t.Fatalf("differs from JS at byte %d:\n go: %s\n js: %s", i, got[lo:min(len(got), i+200)], jsOut[lo:min(len(jsOut), i+200)])
			}
		}
		t.Fatalf("differs from JS in length: go %d, js %d", len(got), len(jsOut))
	}
}

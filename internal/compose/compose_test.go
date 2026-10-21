// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package compose

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// unit.test.js: "compose.childEnv: strips every server secret, keeps shell
// basics + child config namespaces".
func TestChildEnvStripsServerSecrets(t *testing.T) {
	env := map[string]string{
		"PATH":                "/usr/bin",
		"HOME":                "/home/dev",
		"NODE_ENV":            "development",
		"https_proxy":         "http://proxy:3128",
		"PAGES_MCP_URL":       "http://127.0.0.1:3099/mcp",
		"PAGES_MCP_TOKEN":     "pgs_childtoken",
		"CUTLASS_DIR":         "/opt/cutlass",
		"OPENROUTER_API_KEY":  "sk-or-child",
		"PAGE_COOKIE_SECRET":  "server-secret-1",
		"RAW_TOKEN_SECRET":    "server-secret-2",
		"API_TOKEN_PEPPER":    "server-secret-3",
		"ADMIN_CSRF_SECRET":   "server-secret-4",
		"DATABASE_URL":        "postgres://pages:pw@127.0.0.1/pages",
		"PGHOST":              "127.0.0.1",
		"PGPORT":              "5432",
		"AUTH_SIGNING_PUBKEY": "pubkey",
		"AUTH_COOKIE_NAME":    "elcano_auth",
		"MOC_API_KEY":         "moc-secret",
		"DEV_ADMIN_COOKIE":    "dev-cookie",
		"UNRELATED_RANDOM":    "nope",
	}
	child := ChildEnv(env)
	for _, key := range []string{"PATH", "HOME", "NODE_ENV", "https_proxy", "PAGES_MCP_URL", "PAGES_MCP_TOKEN", "CUTLASS_DIR", "OPENROUTER_API_KEY"} {
		if child[key] != env[key] {
			t.Errorf("%s did not pass through", key)
		}
	}
	for _, key := range []string{
		"PAGE_COOKIE_SECRET", "RAW_TOKEN_SECRET", "API_TOKEN_PEPPER", "ADMIN_CSRF_SECRET",
		"DATABASE_URL", "PGHOST", "PGPORT", "AUTH_SIGNING_PUBKEY", "AUTH_COOKIE_NAME",
		"MOC_API_KEY", "DEV_ADMIN_COOKIE", "UNRELATED_RANDOM",
	} {
		if _, ok := child[key]; ok {
			t.Errorf("%s must not reach the child", key)
		}
	}
}

func TestEnabledGate(t *testing.T) {
	t.Setenv("PAGES_COMPOSE", "")
	t.Setenv("CUTLASS_BIN", "/bin/true")
	t.Setenv("CUTLASS_DIR", "/tmp")
	if Enabled() {
		t.Fatal("enabled without PAGES_COMPOSE=1")
	}
	t.Setenv("PAGES_COMPOSE", "true")
	if Enabled() {
		t.Fatal("only the exact value 1 enables compose")
	}
	t.Setenv("PAGES_COMPOSE", "1")
	if !Enabled() || DriverLabel() != "spawn (/bin/true)" {
		t.Fatalf("spawn: %v %q", Enabled(), DriverLabel())
	}
	t.Setenv("CUTLASS_DIR", "")
	if Enabled() {
		t.Fatal("spawn needs CUTLASS_DIR")
	}
	t.Setenv("COMPOSE_DRIVER", "MOC")
	t.Setenv("MOC_URL", "https://moc.example.com//")
	if Enabled() {
		t.Fatal("moc needs MOC_API_KEY")
	}
	t.Setenv("MOC_API_KEY", "k")
	if !Enabled() || DriverLabel() != "moc (https://moc.example.com)" {
		t.Fatalf("moc: %v %q", Enabled(), DriverLabel())
	}
}

func TestClean(t *testing.T) {
	if got := clean("\x1b[31mred\x1b[0m\r\x00ok\tx\n\x7f"); got != "redok\tx\n" {
		t.Fatalf("clean = %q", got)
	}
}

func TestBuildTask(t *testing.T) {
	create := buildTask("make it", "northwind/q2", "", "create")
	if !strings.Contains(create, "Page title: northwind/q2\n") || !strings.Contains(create, "\nmake it\n") {
		t.Errorf("create task:\n%s", create)
	}
	revise := buildTask("fix it", "northwind/q2", "T", "revise")
	if !strings.Contains(revise, `First call get_page("northwind/q2")`) || strings.Contains(revise, "Page title") {
		t.Errorf("revise task:\n%s", revise)
	}
}

// passthrough stands in for the csrf middlewares; it records what ran.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (rec *recorder) mw(name string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.mu.Lock()
			rec.calls = append(rec.calls, name)
			rec.mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}
}

func do(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestHandlerValidation(t *testing.T) {
	rec := &recorder{}
	h := Handler(rec.mw("admin"), rec.mw("csrf"))
	cases := []struct {
		body   string
		status int
		code   string
		msg    string
	}{
		{`{}`, 400, "no_prompt", "prompt is required"},
		{`{"mode":"revise","prompt":"  "}`, 400, "no_prompt", "describe what to change"},
		{`{"prompt":"` + strings.Repeat("x", 200001) + `","slug":"a"}`, 400, "too_long", "prompt too long (200k max)"},
		{`{"prompt":"p","slug":"Bad Slug"}`, 400, "bad_slug", "slug must be a lowercase page path"},
		{`{"prompt":"p","slug":"a//b"}`, 400, "bad_slug", "slug must be a lowercase page path"},
		{`{"prompt":`, 400, "bad_json", "invalid JSON request body"},
		{`"just a string"`, 400, "bad_json", "invalid JSON request body"},
	}
	for _, c := range cases {
		w, out := do(t, h, "POST", Prefix, c.body)
		if w.Code != c.status || out["code"] != c.code || out["error"] != c.msg {
			t.Errorf("%.40s: %d %v", c.body, w.Code, out)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Errorf("content type %q", ct)
		}
		if !strings.HasPrefix(w.Header().Get("ETag"), `W/"`) {
			t.Errorf("no Express ETag")
		}
	}
	if rec.calls[0] != "admin" || rec.calls[1] != "csrf" {
		t.Errorf("middleware order %v", rec.calls)
	}

	rec.calls = nil
	w, out := do(t, h, "GET", Prefix+"/999999", "")
	if w.Code != 404 || out["code"] != "no_job" || out["error"] != "no such job" {
		t.Errorf("unknown job: %d %v", w.Code, out)
	}
	if len(rec.calls) != 1 || rec.calls[0] != "admin" {
		t.Errorf("GET must pass requireAdminJSON only: %v", rec.calls)
	}
}

func TestExpressETag(t *testing.T) {
	// The golden from test/mcp-wire.golden.json: same body, same validator.
	body := `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params for initialize"}}`
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	type rpcErr struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	w := httptest.NewRecorder()
	writeJSON(w, httptest.NewRequest("POST", "/", nil), 200, struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Error   rpcErr `json:"error"`
	}{"2.0", 1, rpcErr{-32602, "Invalid params for initialize"}})
	if w.Body.String() != body {
		t.Fatalf("body %s", w.Body.String())
	}
	if got := w.Header().Get("ETag"); got != `W/"5a-5IzRDivJCC4eLwDA2kAQzVp/iU8"` {
		t.Fatalf("etag %s", got)
	}
}

func waitDone(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, out := do(t, h, "GET", Prefix+"/"+id, "")
		if out["status"] != "running" {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job never finished")
	return nil
}

func TestSpawnDriver(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "cutlass")
	// Prints its argv and env so the test sees exactly what the child got: no
	// shell between the server and the prompt, and no server secret.
	script := "#!/bin/sh\nprintf 'arg1=%s arg2=%s\\n' \"$1\" \"$2\"\nprintf '\\033[32mgreen\\033[0m\\n'\necho \"secret=${PAGE_COOKIE_SECRET:-none} mcp=$PAGES_MCP_URL pwd=$(pwd)\" >&2\nexit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAGES_COMPOSE", "1")
	t.Setenv("COMPOSE_DRIVER", "")
	t.Setenv("CUTLASS_BIN", bin)
	t.Setenv("CUTLASS_DIR", dir)
	t.Setenv("PAGE_COOKIE_SECRET", "server-secret")
	t.Setenv("PAGES_MCP_URL", "http://127.0.0.1:1/mcp")
	rec := &recorder{}
	h := Handler(rec.mw("admin"), rec.mw("csrf"))

	w, out := do(t, h, "POST", Prefix, `{"prompt":"  build $(rm -rf /) ; it  ","slug":" Northwind/Q2 ","title":"Q2"}`)
	if w.Code != 202 || out["driver"] != "spawn" {
		t.Fatalf("start: %d %v", w.Code, out)
	}
	id, _ := out["jobId"].(string)
	done := waitDone(t, h, id)
	if done["status"] != "error" || done["code"] != float64(3) || done["slug"] != "northwind/q2" || done["title"] != "Q2" || done["driver"] != "spawn" {
		t.Fatalf("job: %v", done)
	}
	log, _ := done["log"].(string)
	for _, want := range []string{
		"▸ cutlass " + bin + " (cwd " + dir + ")",
		"arg1=-task arg2=Use the pages MCP server to create and publish a client dashboard page.",
		"build $(rm -rf /) ; it",
		"green",
		"secret=none mcp=http://127.0.0.1:1/mcp pwd=" + dir,
		"▸ cutlass exited (code 3)",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "\x1b") {
		t.Error("ANSI escapes reached the log")
	}
}

func TestSpawnDriverTimeout(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "cutlass")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOSE_DRIVER", "spawn")
	t.Setenv("CUTLASS_BIN", bin)
	t.Setenv("CUTLASS_DIR", dir)
	t.Setenv("COMPOSE_TIMEOUT_MS", "200")
	rec := &recorder{}
	h := Handler(rec.mw("admin"), rec.mw("csrf"))
	_, out := do(t, h, "POST", Prefix, `{"prompt":"p","slug":"a"}`)
	done := waitDone(t, h, out["jobId"].(string))
	log, _ := done["log"].(string)
	if done["status"] != "error" || done["code"] != nil || !strings.Contains(log, "✗ timed out after 0s — killing") || !strings.Contains(log, "(code null)") {
		t.Fatalf("job: %v", done)
	}
}

func TestMocDriver(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	var created map[string]any
	moc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "moc-key" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":"bad key"}`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/tasks":
			_ = json.NewDecoder(r.Body).Decode(&created)
			_, _ = io.WriteString(w, `{"id":42,"status":"pending"}`)
		case r.URL.Path == "/tasks/42":
			polls++
			if polls == 1 {
				w.WriteHeader(502)
				_, _ = io.WriteString(w, "upstream down")
				return
			}
			if polls < 3 {
				_, _ = io.WriteString(w, `{"status":"running"}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"SUCCESS","result":"deployed \u001b[1mnorthwind/q2\u001b[0m"}`)
		case r.URL.Path == "/logs/42":
			_, _ = io.WriteString(w, `{"title":"Compose","messages":[{"role":"assistant","content":" hi "},{"content":"x"},{"role":"user","content":"  "}],"cost":0.25,"prompt_tokens":10}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer moc.Close()
	t.Setenv("COMPOSE_DRIVER", "moc")
	t.Setenv("MOC_URL", moc.URL+"/")
	t.Setenv("MOC_API_KEY", "moc-key")
	t.Setenv("MOC_MODEL", "model-a")
	t.Setenv("MOC_TARGET_NODE_NAME", "")
	t.Setenv("COMPOSE_POLL_MS", "5")
	rec := &recorder{}
	h := Handler(rec.mw("admin"), rec.mw("csrf"))
	_, out := do(t, h, "POST", Prefix, `{"prompt":"p","slug":"northwind/q2","mode":"revise"}`)
	if out["driver"] != "moc" {
		t.Fatalf("start: %v", out)
	}
	done := waitDone(t, h, out["jobId"].(string))
	if done["status"] != "done" {
		t.Fatalf("job: %v", done)
	}
	want := strings.Join([]string{
		"▸ MOC task 42",
		"title: Compose",
		"[assistant] hi",
		"[?] x",
		"cost: $0.25 (10 in / 0 out)",
		"result: deployed northwind/q2",
		"▸ MOC task 42 succeeded",
	}, "\n")
	if done["log"] != want {
		t.Errorf("log:\n%s\nwant:\n%s", done["log"], want)
	}
	mu.Lock()
	defer mu.Unlock()
	if created["model"] != "model-a" || created["target_node_name"] != nil || !strings.HasPrefix(created["prompt"].(string), "Use the pages MCP server to AUDIT") {
		t.Errorf("created %v", created)
	}
}

func TestMocCreateFailure(t *testing.T) {
	moc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"message":"key lacks create-task"}`)
	}))
	defer moc.Close()
	t.Setenv("COMPOSE_DRIVER", "moc")
	t.Setenv("MOC_URL", moc.URL)
	t.Setenv("MOC_API_KEY", "k")
	h := Handler(func(n http.Handler) http.Handler { return n }, func(n http.Handler) http.Handler { return n })
	_, out := do(t, h, "POST", Prefix+"/", `{"prompt":"p","slug":"a"}`)
	done := waitDone(t, h, out["jobId"].(string))
	if done["status"] != "error" || done["log"] != "MOC create failed: MOC /tasks → 403 key lacks create-task" {
		t.Fatalf("job: %v", done)
	}
}

func TestFirstUTF16(t *testing.T) {
	if got := firstUTF16("ab😀c", 3); got != "ab" {
		t.Errorf("split pair: %q", got)
	}
	if got := firstUTF16("ab😀c", 4); got != "ab😀" {
		t.Errorf("whole pair: %q", got)
	}
	if utf16Len("😀é") != 3 {
		t.Error("utf16Len")
	}
}

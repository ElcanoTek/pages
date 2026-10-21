// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package uploads

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/versions"
)

func newRequest(method, target string, body io.Reader) (*http.Request, error) {
	return httptest.NewRequest(method, target, body), nil
}

type fixture struct {
	t   *testing.T
	d   *db.DB
	ctx context.Context
}

func setup(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("PAGE_COOKIE_SECRET", "uploads-test-secret-0123456789abcdef")
	t.Setenv("PAGE_UPLOAD_MAX_CHUNK_BYTES", "")
	t.Setenv("MAX_HTML_BYTES", "")
	t.Setenv("PAGES_DATA_MAX_BYTES", "")
	t.Setenv("PAGES_MCP_MAX_INLINE_DATA_BYTES", "")
	t.Setenv("PAGE_UPLOAD_TICKET_TTL_MINUTES", "")
	return &fixture{t: t, d: testdb.New(t), ctx: context.Background()}
}

func (f *fixture) agent(label string) versions.Actor {
	f.t.Helper()
	m, err := tokens.Mint(f.ctx, f.d, tokens.MintParams{Label: label})
	if err != nil {
		f.t.Fatal(err)
	}
	return versions.Actor{Actor: label, ActorType: "agent", TokenID: m.ID, IP: "127.0.0.1"}
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.d.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.d.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func ptr(s string) *string { return &s }

func startIn(slug string, content []byte) StartInput {
	return StartInput{Slug: ptr(slug), TotalBytes: len(content), ContentSHA256: SHA256(content)}
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return v
	}
}

// wantErr asserts an *apierror.Error with this status and code.
func wantErr(t *testing.T, err error, status int, code string) *apierror.Error {
	t.Helper()
	e, ok := apierror.As(err)
	if !ok || e.Status != status || e.Code != code {
		t.Fatalf("err = %v (%+v), want %d %s", err, e, status, code)
	}
	return e
}

func details(t *testing.T, e *apierror.Error) *jsonv.Object {
	t.Helper()
	o, ok := e.Details.(*jsonv.Object)
	if !ok {
		t.Fatalf("details %T", e.Details)
	}
	return o
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func largeHTML() string {
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><title>Large MCP</title></head><body><main>")
	for i := range 1800 {
		fmt.Fprintf(&b, `<section data-row="%d">Dashboard row %d: complete source data ☀</section>`, i, i)
	}
	b.WriteString("</main><script>window.dashboardReady=true;</script></body></html>")
	return b.String()
}

// deployPage is the deploy_page_upload consumer shape: the verified bytes go
// through versions inside the upload's transaction.
func deployPage(a versions.Actor, publish bool) DeployFunc {
	return func(ctx context.Context, tx pgx.Tx, s Staged) (any, error) {
		res, err := versions.CreateAndDeployWithTx(ctx, tx, versions.CreateAndDeployInput{
			DeployInput: versions.DeployInput{Slug: s.Slug, HTML: s.HTML, RenderMode: "raw", Source: "mcp", Publish: publish},
			Title:       "Northwind",
		}, a, versions.CreateAndDeployOptions{})
		if err != nil {
			return nil, err
		}
		return jsonv.ObjectOf("upload_id", s.UploadID, "created", res.Created, "version", res.Version), nil
	}
}

var keyA = strings.Repeat("a", 64)
var keyB = strings.Repeat("b", 64)

func TestChunkedUploadResumesDedupesAndCommitsOnce(t *testing.T) {
	f := setup(t)
	a := f.agent("upload-agent")
	other := f.agent("upload-other")
	html := largeHTML()
	content := []byte(html)
	if len(content) <= 100_000 {
		t.Fatal("fixture too small")
	}
	up := must[*State](t)(Start(f.ctx, f.d, startIn("mcp-large-upload", content), a))
	if *up.Slug != "mcp-large-upload" || up.Template != nil || up.TargetKind != "page" ||
		up.BytesReceived != 0 || up.NextSequence != 0 || up.Complete || up.MaxChunkBytes != MaxChunkBytes() || up.DataLimits != nil {
		t.Fatalf("start state %+v", up)
	}
	// A fresh large upload is told about the cheaper path before any base64.
	appends := (len(content) + MaxChunkBytes() - 1) / MaxChunkBytes()
	for _, want := range []string{fmt.Sprintf("This file is %d bytes", len(content)), "append_page_upload",
		fmt.Sprintf("%d appends for this file", appends), "create_upload_ticket"} {
		if !strings.Contains(up.NextStep, want) {
			t.Errorf("next_step lacks %q: %s", want, up.NextStep)
		}
	}
	wire := jsonv.Stringify(up)
	if !strings.HasPrefix(wire, `{"upload_id":"`+up.UploadID+`","target_kind":"page","slug":"mcp-large-upload","template":null,"content_sha256":"`) ||
		strings.Contains(wire, `"deduped"`) || strings.Contains(wire, `"ticket`) || strings.Contains(wire, "data_limits") {
		t.Fatalf("wire %s", wire)
	}

	_, err := Deploy(f.ctx, f.d, up.UploadID, a, keyA, deployPage(a, true), DeployOptions{})
	e := wantErr(t, err, 409, "page_upload_incomplete")
	if details(t, e).Value("total_bytes") != len(content) {
		t.Fatal("incomplete details")
	}

	maxChunk := MaxChunkBytes()
	first := content[:maxChunk]
	_, err = Append(f.ctx, f.d, AppendInput{up.UploadID, 1, b64(first)}, a)
	e = wantErr(t, err, 409, "upload_sequence_conflict")
	dt := details(t, e)
	if dt.Value("expected_sequence") != 0 || dt.Value("resumable") != true || dt.Value("bytes_received") != 0 ||
		dt.Value("total_bytes") != len(content) || jsonv.Stringify(dt) != fmt.Sprintf(`{"expected_sequence":0,"bytes_received":0,"total_bytes":%d,"resumable":true}`, len(content)) {
		t.Fatalf("details %s", jsonv.Stringify(dt))
	}
	if !strings.Contains(e.Message, "Do NOT cancel") || !strings.Contains(e.Message, "A FAILED append does not advance the sequence") {
		t.Fatal(e.Message)
	}
	resumed := must[*State](t)(Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64(first)}, a))
	if resumed.NextSequence != 1 || *resumed.Deduped {
		t.Fatalf("resumed %+v", resumed)
	}
	replayed := must[*State](t)(Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64(first)}, a))
	if !*replayed.Deduped || replayed.NextSequence != 1 {
		t.Fatalf("replay %+v", replayed)
	}
	_, err = Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64([]byte("different"))}, a)
	e = wantErr(t, err, 409, "upload_sequence_conflict")
	if details(t, e).Value("expected_sequence") != 1 || !strings.Contains(e.Message, "DIFFERENT bytes") {
		t.Fatal(e.Message)
	}
	_, err = Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64(first)}, other)
	wantErr(t, err, 404, "page_upload_not_found")

	var final *State
	seq := 1
	for off := maxChunk; off < len(content); off += maxChunk {
		chunk := content[off:min(off+maxChunk, len(content))]
		final = must[*State](t)(Append(f.ctx, f.d, AppendInput{up.UploadID, seq, b64(chunk)}, a))
		if seq == 1 {
			again := must[*State](t)(Append(f.ctx, f.d, AppendInput{up.UploadID, seq, b64(chunk)}, a))
			if !*again.Deduped || again.NextSequence != 2 {
				t.Fatal("ambiguous chunk retries are idempotent")
			}
		} else if !final.Complete && !strings.Contains(final.NextStep, fmt.Sprintf("Call append_page_upload with sequence %d", seq+1)) {
			t.Fatal(final.NextStep)
		}
		seq++
	}
	if !final.Complete || final.BytesReceived != len(content) || final.NextSequence != seq ||
		final.NextStep != "Upload verified. Call deploy_page_upload with this upload_id; do not resend the HTML." {
		t.Fatalf("final %+v", final)
	}

	calls := 0
	consumer := func(ctx context.Context, tx pgx.Tx, s Staged) (any, error) {
		calls++
		return deployPage(a, true)(ctx, tx, s)
	}
	receipt := must[any](t)(Deploy(f.ctx, f.d, up.UploadID, a, keyA, consumer, DeployOptions{}))
	obj := receipt.(*jsonv.Object)
	if obj.Value("upload_id") != up.UploadID || obj.Value("created") != true || obj.Keys()[0] != "upload_id" {
		t.Fatalf("receipt %s", jsonv.Stringify(receipt))
	}
	versionID := obj.Value("version").(*jsonv.Object).Value("id")
	var stored string
	if err := f.d.QueryRow(f.ctx, "SELECT html FROM page_versions WHERE id = $1::bigint", versionID).Scan(&stored); err != nil || stored != html {
		t.Fatalf("UTF-8 bytes must survive chunking exactly (%v)", err)
	}
	// A retry with the same options replays the immutable receipt.
	again := must[any](t)(Deploy(f.ctx, f.d, up.UploadID, a, keyA, consumer, DeployOptions{}))
	if calls != 1 || !jsonv.Equal(again, receipt) {
		t.Fatalf("replay ran the mutation again (%d) or changed the receipt", calls)
	}
	if n := f.count("SELECT count(*)::int FROM audit_log WHERE action = 'deploy' AND version_id = $1::bigint", versionID); n != 1 {
		t.Fatalf("deploy audits %d", n)
	}
	_, err = Deploy(f.ctx, f.d, up.UploadID, a, keyB, consumer, DeployOptions{})
	wantErr(t, err, 409, "page_upload_commit_conflict")
	if n := f.count("SELECT count(*)::int FROM page_content_upload_chunks WHERE upload_id = $1", up.UploadID); n != 0 {
		t.Fatal("committed chunk bytes are released")
	}
	_, err = Deploy(f.ctx, f.d, up.UploadID, other, keyA, consumer, DeployOptions{})
	wantErr(t, err, 404, "page_upload_not_found")
	_, err = Append(f.ctx, f.d, AppendInput{up.UploadID, seq, b64([]byte("x"))}, a)
	wantErr(t, err, 409, "page_upload_committed")
	_, err = Cancel(f.ctx, f.d, up.UploadID, a)
	wantErr(t, err, 409, "page_upload_committed")
	// Cross-token cancel of a committed upload is the non-revealing no-op.
	if r := must[*CancelResult](t)(Cancel(f.ctx, f.d, up.UploadID, other)); r.Cancelled {
		t.Fatal("cross-token cancel")
	}

	// Argument guards.
	if _, err := Deploy(f.ctx, f.d, up.UploadID, a, "nope", consumer, DeployOptions{}); err == nil || err.Error() != "commit key is required" {
		t.Fatal(err)
	}
	if _, err := Deploy(f.ctx, f.d, up.UploadID, a, keyA, nil, DeployOptions{}); err == nil || err.Error() != "deploy callback is required" {
		t.Fatal(err)
	}
	if _, err := Start(f.ctx, f.d, startIn("x", content), versions.Actor{}); err == nil || err.Error() != "authenticated MCP token id is missing" {
		t.Fatal(err)
	}
	_, err = Append(f.ctx, f.d, AppendInput{up.UploadID, -1, b64(first)}, a)
	wantErr(t, err, 400, "upload_sequence_invalid")
}

func TestReplayCommittedRefreshesGuidanceWithoutWriting(t *testing.T) {
	f := setup(t)
	a := f.agent("replay-agent")
	content := []byte("<!doctype html><html><head><title>Northwind</title></head><body>original</body></html>")
	up := must[*State](t)(Start(f.ctx, f.d, startIn("replay-page", content), a))
	must[*State](t)(Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64(content)}, a))
	replay := func(ctx context.Context, tx pgx.Tx, receipt any) (any, error) {
		o := receipt.(*jsonv.Object).Clone()
		o.Set("replayed", true)
		o.Set("next_step", "This retry did not republish.")
		return o, nil
	}
	opts := DeployOptions{ReplayCommitted: replay}
	first := must[any](t)(Deploy(f.ctx, f.d, up.UploadID, a, keyA, deployPage(a, true), opts))
	snapshot := func() string {
		var s string
		if err := f.d.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM page_versions)::text || '|' ||
			(SELECT count(*) FROM audit_log)::text || '|' || (SELECT row_to_json(u)::text FROM page_content_uploads u WHERE id = $1)`,
			up.UploadID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	got := must[any](t)(Deploy(f.ctx, f.d, up.UploadID, a, keyA, deployPage(a, true), opts)).(*jsonv.Object)
	if snapshot() != before {
		t.Fatal("replay cannot change a version, audit, upload receipt or expiry")
	}
	if got.Value("replayed") != true || !jsonv.Equal(got.Value("version"), first.(*jsonv.Object).Value("version")) {
		t.Fatalf("replay %s", jsonv.Stringify(got))
	}
	// A replay error surfaces and still writes nothing.
	_, err := Deploy(f.ctx, f.d, up.UploadID, a, keyA, deployPage(a, true), DeployOptions{
		ReplayCommitted: func(context.Context, pgx.Tx, any) (any, error) { return nil, errors.New("boom") },
	})
	if err == nil || snapshot() != before {
		t.Fatal(err)
	}
}

func TestFailedCommitIsAtomicAndRetryable(t *testing.T) {
	f := setup(t)
	a := f.agent("atomic-agent")
	content := []byte("<!doctype html><html><body>atomic staged content</body></html>")
	up := must[*State](t)(Start(f.ctx, f.d, startIn("mcp-upload-atomic", content), a))
	must[*State](t)(Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64(content)}, a))
	_, err := Deploy(f.ctx, f.d, up.UploadID, a, keyA, func(ctx context.Context, tx pgx.Tx, s Staged) (any, error) {
		if _, err := deployPage(a, true)(ctx, tx, s); err != nil {
			return nil, err
		}
		return nil, errors.New("simulated failure after version creation")
	}, DeployOptions{})
	if err == nil || !strings.Contains(err.Error(), "simulated failure") {
		t.Fatal(err)
	}
	if f.count("SELECT count(*)::int FROM pages WHERE slug = 'mcp-upload-atomic'") != 0 {
		t.Fatal("failed commit cannot strand a page/version")
	}
	if f.count("SELECT count(*)::int FROM page_content_uploads WHERE id = $1 AND commit_result IS NULL AND bytes_received = $2", up.UploadID, len(content)) != 1 {
		t.Fatal("verified upload remains retryable")
	}
	res := must[any](t)(Deploy(f.ctx, f.d, up.UploadID, a, keyB, deployPage(a, true), DeployOptions{})).(*jsonv.Object)
	if res.Value("created") != true {
		t.Fatal("retry")
	}
}

func TestHashMismatchRollsBackAndCancelIsIdempotent(t *testing.T) {
	f := setup(t)
	a := f.agent("hash-agent")
	intended := []byte("good")
	up := must[*State](t)(Start(f.ctx, f.d, startIn("mcp-upload-bad-hash", intended), a))
	if !strings.HasPrefix(up.NextStep, "Call append_page_upload with sequence 0 for the next 4 raw bytes at offset 0 (as few appends as max_chunk_bytes 49152 allows: 1 left)") {
		t.Fatal(up.NextStep)
	}
	_, err := Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64([]byte("evil"))}, a)
	e := wantErr(t, err, 409, "upload_hash_mismatch")
	if dt := details(t, e); dt.Value("expected_sequence") != 0 || dt.Value("resumable") != true {
		t.Fatal("hash details")
	}
	if !strings.Contains(e.Message, "create_upload_ticket") {
		t.Fatal(e.Message)
	}
	if f.count("SELECT bytes_received + next_sequence FROM page_content_uploads WHERE id = $1", up.UploadID) != 0 ||
		f.count("SELECT count(*)::int FROM page_content_upload_chunks WHERE upload_id = $1", up.UploadID) != 0 {
		t.Fatal("bad final hash rolls back its chunk")
	}
	_, err = Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64([]byte("goodbye"))}, a)
	e = wantErr(t, err, 400, "upload_size_mismatch")
	if jsonv.Stringify(e.Details) != `{"declared_total_bytes":4,"bytes_received":0,"overflow_bytes":3}` {
		t.Fatal(jsonv.Stringify(e.Details))
	}
	c := must[*CancelResult](t)(Cancel(f.ctx, f.d, up.UploadID, a))
	if !c.Cancelled || !strings.Contains(c.NextStep, "re-emits the whole document") {
		t.Fatalf("%+v", c)
	}
	c = must[*CancelResult](t)(Cancel(f.ctx, f.d, up.UploadID, a))
	if c.Cancelled || c.NextStep != "No active upload was found; no page or staged content changed." || c.UploadID != up.UploadID {
		t.Fatalf("%+v", c)
	}
	if jsonv.Stringify(c) != `{"upload_id":"`+up.UploadID+`","cancelled":false,"next_step":"No active upload was found; no page or staged content changed."}` {
		t.Fatal(jsonv.Stringify(c))
	}
}

func TestAttemptLoopIsCalledOut(t *testing.T) {
	f := setup(t)
	a := f.agent("churn-agent")
	content := []byte(strings.Repeat("<p>Northwind</p>", 60))
	in := startIn("mcp-upload-churn", content)
	s1 := must[*State](t)(Start(f.ctx, f.d, in, a))
	if strings.Contains(s1.NextStep, "does not converge") {
		t.Fatal("the first attempt is not scolded")
	}
	must[*CancelResult](t)(Cancel(f.ctx, f.d, s1.UploadID, a))
	s2 := must[*State](t)(Start(f.ctx, f.d, in, a))
	c2 := must[*CancelResult](t)(Cancel(f.ctx, f.d, s2.UploadID, a))
	if !strings.Contains(c2.NextStep, "started 2 chunked uploads for this target and cancelled 2") {
		t.Fatal(c2.NextStep)
	}
	s3 := must[*State](t)(Start(f.ctx, f.d, in, a))
	if !strings.HasPrefix(s3.NextStep, "You have started 3 chunked uploads for this target and cancelled 2. That loop does not converge") ||
		!strings.Contains(s3.NextStep, "create_upload_ticket") || !strings.HasSuffix(s3.NextStep, "do not generate it through model output.") {
		t.Fatal(s3.NextStep)
	}
	must[*CancelResult](t)(Cancel(f.ctx, f.d, s3.UploadID, a))
	// Taking the advice is not itself an attempt.
	tk := must[*State](t)(CreateTicket(f.ctx, f.d, in, a))
	if strings.Contains(tk.NextStep, "does not converge") || f.count("SELECT starts FROM page_upload_attempts") != 3 {
		t.Fatal(tk.NextStep)
	}
	must[*CancelResult](t)(Cancel(f.ctx, f.d, tk.UploadID, a))
	// Another target, and another kind on the same slug, count separately.
	other := must[*State](t)(Start(f.ctx, f.d, startIn("mcp-upload-other", content), a))
	if strings.Contains(other.NextStep, "does not converge") {
		t.Fatal("attempts are per target")
	}
	// Attempts age out of the window.
	f.exec("UPDATE page_upload_attempts SET updated_at = now() - interval '7 hours'")
	fresh := must[*State](t)(Start(f.ctx, f.d, in, a))
	if strings.Contains(fresh.NextStep, "does not converge") {
		t.Fatal("old attempts are forgotten")
	}
}

func TestActiveUploadCapStaleReaperAndConcurrency(t *testing.T) {
	f := setup(t)
	a := f.agent("cap-agent")
	content := []byte("<p>Contoso</p>")
	var ids []string
	for i := range MaxActiveUploads {
		ids = append(ids, must[*State](t)(Start(f.ctx, f.d, startIn(fmt.Sprintf("cap-%d", i), content), a)).UploadID)
	}
	_, err := Start(f.ctx, f.d, startIn("cap-x", content), a)
	e := wantErr(t, err, 409, "page_upload_limit")
	if !strings.HasPrefix(e.Message, "this token already has 5 uploads in flight and none are stale.") {
		t.Fatal(e.Message)
	}
	_, err = CreateTicket(f.ctx, f.d, startIn("cap-x", content), a)
	wantErr(t, err, 409, "page_upload_limit")
	// Another token has its own cap.
	other := f.agent("cap-other")
	must[*State](t)(Start(f.ctx, f.d, startIn("cap-x", content), other))
	// A silent upload is reaped only when the cap is in the way.
	f.exec("UPDATE page_content_uploads SET updated_at = now() - interval '61 minutes' WHERE id = $1", ids[0])
	must[*State](t)(Start(f.ctx, f.d, startIn("cap-x", content), a))
	if f.count("SELECT count(*)::int FROM page_content_uploads WHERE id = $1", ids[0]) != 0 {
		t.Fatal("stale upload not reaped")
	}
	// A paused-but-recent upload is never reaped.
	f.exec("UPDATE page_content_uploads SET updated_at = now() - interval '59 minutes' WHERE id = $1", ids[1])
	_, err = Start(f.ctx, f.d, startIn("cap-y", content), a)
	wantErr(t, err, 409, "page_upload_limit")
	// Committed uploads do not hold a slot.
	must[*State](t)(Append(f.ctx, f.d, AppendInput{ids[1], 0, b64(content)}, a))
	must[any](t)(Deploy(f.ctx, f.d, ids[1], a, keyA, func(context.Context, pgx.Tx, Staged) (any, error) {
		return jsonv.ObjectOf("ok", true), nil
	}, DeployOptions{}))
	must[*State](t)(Start(f.ctx, f.d, startIn("cap-y", content), a))

	// Concurrent starts cannot exceed the cap (advisory lock per token).
	racer := f.agent("cap-racer")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, limited := 0, 0
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Start(f.ctx, f.d, startIn(fmt.Sprintf("race-%d", i), content), racer)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case codeOf(err) == "page_upload_limit":
				limited++
			default:
				t.Errorf("race: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != MaxActiveUploads || limited != 12-MaxActiveUploads {
		t.Fatalf("ok %d limited %d", ok, limited)
	}
}

func TestConcurrentAppendsOfOneSequenceLandOnce(t *testing.T) {
	f := setup(t)
	a := f.agent("concurrent-agent")
	content := bytes.Repeat([]byte("Fabrikam "), 1000)
	up := must[*State](t)(Start(f.ctx, f.d, startIn("concurrent", content), a))
	var wg sync.WaitGroup
	var mu sync.Mutex
	deduped, fresh := 0, 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64(content)}, a)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if *s.Deduped {
				deduped++
			} else {
				fresh++
			}
		}()
	}
	wg.Wait()
	if fresh != 1 || deduped != 7 {
		t.Fatalf("fresh %d deduped %d", fresh, deduped)
	}
	// Concurrent commits with the same key run the consumer exactly once.
	var calls int
	var cmu sync.Mutex
	consumer := func(context.Context, pgx.Tx, Staged) (any, error) {
		cmu.Lock()
		calls++
		cmu.Unlock()
		return jsonv.ObjectOf("ok", true), nil
	}
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Deploy(f.ctx, f.d, up.UploadID, a, keyA, consumer, DeployOptions{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("consumer ran %d times", calls)
	}
}

func TestExpiredUploadsAreCleanedUp(t *testing.T) {
	f := setup(t)
	a := f.agent("ttl-agent")
	content := []byte("<p>Northwind</p>")
	up := must[*State](t)(Start(f.ctx, f.d, startIn("ttl", content), a))
	var hours float64
	if err := f.d.QueryRow(f.ctx, "SELECT extract(epoch FROM expires_at - now()) / 3600 FROM page_content_uploads WHERE id = $1", up.UploadID).Scan(&hours); err != nil || hours < 23.9 || hours > 24.1 {
		t.Fatalf("ttl %v %v", hours, err)
	}
	if up.ExpiresAt.IsZero() || !strings.HasSuffix(jsonv.Stringify(up.ExpiresAt), `Z"`) {
		t.Fatal("expires_at")
	}
	f.exec("UPDATE page_content_uploads SET expires_at = now() - interval '1 second' WHERE id = $1", up.UploadID)
	_, err := Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64(content)}, a)
	wantErr(t, err, 404, "page_upload_not_found")
	// The failed call rolled its cleanup back with it; the next successful
	// transaction deletes the expired row.
	must[*State](t)(Start(f.ctx, f.d, startIn("ttl-2", content), a))
	if f.count("SELECT count(*)::int FROM page_content_uploads WHERE id = $1", up.UploadID) != 0 {
		t.Fatal("expired rows are deleted")
	}
	c := must[*CancelResult](t)(Cancel(f.ctx, f.d, up.UploadID, a))
	if c.Cancelled {
		t.Fatal("expired cancel is a no-op")
	}
}

func TestTargetsKindsAndPeek(t *testing.T) {
	f := setup(t)
	a := f.agent("target-agent")
	content := []byte("<!doctype html><html><body>Contoso template</body></html>")
	sha := SHA256(content)
	bad := []struct {
		in   StartInput
		code string
	}{
		{StartInput{TotalBytes: 1, ContentSHA256: sha}, "upload_target_required"},
		{StartInput{Slug: ptr("a"), Template: ptr("b"), TotalBytes: 1, ContentSHA256: sha}, "upload_target_required"},
		{StartInput{Template: ptr("b"), Kind: ptr("data"), TotalBytes: 1, ContentSHA256: sha}, "upload_target_kind_invalid"},
		{StartInput{Slug: ptr("a"), Kind: ptr("template"), TotalBytes: 1, ContentSHA256: sha}, "upload_target_kind_invalid"},
		{StartInput{Template: ptr("bad/name"), TotalBytes: 1, ContentSHA256: sha}, "bad_template_name"},
		{StartInput{Slug: ptr("Bad Slug"), TotalBytes: 1, ContentSHA256: sha}, "bad_slug"},
		{StartInput{Slug: ptr("a"), TotalBytes: 0, ContentSHA256: sha}, "upload_size_invalid"},
		{StartInput{Slug: ptr("a"), TotalBytes: MaxUploadBytes + 1, ContentSHA256: sha}, "upload_size_invalid"},
		{StartInput{Slug: ptr("a"), TotalBytes: 1, ContentSHA256: strings.ToUpper(sha)}, "upload_hash_invalid"},
		{StartInput{Slug: ptr("a"), TotalBytes: 1, ContentSHA256: sha, DataSize: jsonv.ObjectOf("payload_bytes", 1.0, "escaped_payload_bytes", 1.0)}, "data_size_invalid"},
		{StartInput{Slug: ptr("a"), Kind: ptr("data"), TotalBytes: 1, ContentSHA256: sha, DataSize: versions.Null}, "data_size_invalid"},
	}
	for _, b := range bad {
		_, err := Start(f.ctx, f.d, b.in, a)
		if codeOf(err) != b.code {
			t.Errorf("%+v: %v", b.in, err)
		}
		_, err = CreateTicket(f.ctx, f.d, b.in, a)
		if codeOf(err) != b.code {
			t.Errorf("ticket %+v: %v", b.in, err)
		}
	}
	if f.count("SELECT count(*)::int FROM page_content_uploads") != 0 {
		t.Fatal("refused starts reserve nothing")
	}

	tpl := must[*State](t)(Start(f.ctx, f.d, StartInput{Template: ptr(" Contoso-Campaign "), Kind: ptr("template"), TotalBytes: len(content), ContentSHA256: sha}, a))
	if tpl.Slug != nil || *tpl.Template != "contoso-campaign" || tpl.TargetKind != "template" {
		t.Fatalf("%+v", tpl)
	}
	done := must[*State](t)(Append(f.ctx, f.d, AppendInput{tpl.UploadID, 0, b64(content)}, a))
	if done.NextStep != "Upload verified. Call register_template_upload with this upload_id; do not resend the HTML." {
		t.Fatal(done.NextStep)
	}
	_, err := Deploy(f.ctx, f.d, tpl.UploadID, a, keyA, deployPage(a, true), DeployOptions{})
	e := wantErr(t, err, 409, "page_upload_target_mismatch")
	if e.Message != "this upload was staged for a template, not a page; use register_template_upload instead" ||
		jsonv.Stringify(e.Details) != `{"target_kind":"template"}` {
		t.Fatal(e.Message)
	}
	_, err = Peek(f.ctx, f.d, tpl.UploadID, a, PeekOptions{})
	wantErr(t, err, 409, "page_upload_target_mismatch")
	peeked := must[*Peeked](t)(Peek(f.ctx, f.d, tpl.UploadID, a, PeekOptions{ExpectKind: KindTemplate}))
	if peeked.HTML != string(content) || *peeked.Template != "contoso-campaign" || peeked.Slug != nil ||
		!strings.HasPrefix(jsonv.Stringify(peeked), `{"upload_id":"`+tpl.UploadID+`","target_kind":"template","slug":null,"template":"contoso-campaign","total_bytes":`) {
		t.Fatalf("%+v", peeked)
	}
	// Peeking consumed nothing; the same upload registers afterwards.
	var gotTemplate string
	must[any](t)(Deploy(f.ctx, f.d, tpl.UploadID, a, keyA, func(_ context.Context, _ pgx.Tx, s Staged) (any, error) {
		gotTemplate = s.Template + "|" + s.Slug + "|" + s.TargetKind
		return jsonv.ObjectOf("template", s.Template), nil
	}, DeployOptions{ExpectKind: KindTemplate}))
	if gotTemplate != "contoso-campaign||template" {
		t.Fatal(gotTemplate)
	}
	_, err = Peek(f.ctx, f.d, tpl.UploadID, a, PeekOptions{ExpectKind: KindTemplate})
	wantErr(t, err, 409, "page_upload_already_committed")

	// A data upload reports its limits and its own consumer.
	payload := []byte(`{"count":1}`)
	data := must[*State](t)(Start(f.ctx, f.d, StartInput{Slug: ptr("northwind"), Kind: ptr("data"), TotalBytes: len(payload), ContentSHA256: SHA256(payload),
		DataSize: jsonv.ObjectOf("payload_bytes", 11.0, "escaped_payload_bytes", 11.0)}, a))
	if data.DataLimits == nil || data.DataLimits.MaxStagedBytes != MaxUploadBytes || !strings.Contains(jsonv.Stringify(data), `"max_chunk_bytes":49152,"data_limits":{"max_payload_bytes":1048576,`) {
		t.Fatal(jsonv.Stringify(data))
	}
	done = must[*State](t)(Append(f.ctx, f.d, AppendInput{data.UploadID, 0, b64(payload)}, a))
	if done.NextStep != "Upload verified. Call update_page_data_upload with this upload_id; do not resend the payload." || done.DataLimits == nil {
		t.Fatal(done.NextStep)
	}
	_, err = Deploy(f.ctx, f.d, data.UploadID, a, keyA, deployPage(a, true), DeployOptions{})
	if e := wantErr(t, err, 409, "page_upload_target_mismatch"); !strings.Contains(e.Message, "use update_page_data_upload instead") {
		t.Fatal(e.Message)
	}
}

func TestPeekVerifiesEncodingAndStoredBytes(t *testing.T) {
	f := setup(t)
	a := f.agent("utf8-agent")
	invalid := []byte{'<', 'p', '>', 0xff, 0xfe}
	up := must[*State](t)(Start(f.ctx, f.d, startIn("utf8", invalid), a))
	must[*State](t)(Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64(invalid)}, a))
	_, err := Peek(f.ctx, f.d, up.UploadID, a, PeekOptions{})
	wantErr(t, err, 400, "upload_encoding_invalid")
	_, err = Deploy(f.ctx, f.d, up.UploadID, a, keyA, deployPage(a, true), DeployOptions{})
	wantErr(t, err, 400, "upload_encoding_invalid")

	// TextDecoder drops a leading byte-order mark.
	bom := append([]byte{0xef, 0xbb, 0xbf}, "<p>Northwind</p>"...)
	up = must[*State](t)(Start(f.ctx, f.d, startIn("bom", bom), a))
	must[*State](t)(Append(f.ctx, f.d, AppendInput{up.UploadID, 0, b64(bom)}, a))
	if p := must[*Peeked](t)(Peek(f.ctx, f.d, up.UploadID, a, PeekOptions{})); p.HTML != "<p>Northwind</p>" {
		t.Fatalf("%q", p.HTML)
	}
	// Tampered storage fails verification rather than deploying.
	f.exec("UPDATE page_content_upload_chunks SET bytes = 'x'::bytea, content_sha256 = $2 WHERE upload_id = $1", up.UploadID, SHA256([]byte("x")))
	_, err = Peek(f.ctx, f.d, up.UploadID, a, PeekOptions{})
	wantErr(t, err, 409, "upload_hash_mismatch")
	f.exec("DELETE FROM page_content_upload_chunks WHERE upload_id = $1", up.UploadID)
	_, err = Peek(f.ctx, f.d, up.UploadID, a, PeekOptions{})
	if e := wantErr(t, err, 409, "page_upload_incomplete"); e.Message != "upload chunks are incomplete" {
		t.Fatal(e.Message)
	}
}

// ── 1 MiB chunk ceiling (test/upload-chunk-ceiling.integration.js) ─────────

func TestRaisedChunkCeilingIsExact(t *testing.T) {
	f := setup(t)
	const ceiling = 1024 * 1024
	t.Setenv("PAGE_UPLOAD_MAX_CHUNK_BYTES", "1048576")
	a := f.agent("chunk-ceiling-fixture")
	chunkRows := func(id string) string {
		rows, err := f.d.Query(f.ctx, "SELECT sequence, octet_length(bytes)::int FROM page_content_upload_chunks WHERE upload_id=$1 ORDER BY sequence", id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s, n int
			_ = rows.Scan(&s, &n)
			out = append(out, fmt.Sprintf("%d:%d", s, n))
		}
		return strings.Join(out, ",")
	}
	// ~990 KB kind='data': one append at this ceiling (21 at the default).
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for i := 0; b.Len() < 989_000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"date":"2026-07-%02d","line_item":"Contoso prospecting %d / Fabrikam audience %d","impressions":%d,"clicks":%d}`,
			1+i%31, i%97, i%13, 1000+(i*7919)%90000, (i*31)%900)
	}
	b.WriteString("]}")
	file := []byte(b.String())
	started := must[*State](t)(Start(f.ctx, f.d, StartInput{Slug: ptr("northwind-chunk-ceiling"), Kind: ptr("data"), TotalBytes: len(file), ContentSHA256: SHA256(file)}, a))
	if started.MaxChunkBytes != ceiling || !strings.Contains(started.NextStep, "as few appends as max_chunk_bytes (1048576) allows: 1 append for this file") {
		t.Fatal(started.NextStep)
	}
	appended := must[*State](t)(Append(f.ctx, f.d, AppendInput{started.UploadID, 0, b64(file)}, a))
	if !appended.Complete || appended.BytesReceived != len(file) || appended.NextSequence != 1 ||
		chunkRows(started.UploadID) != fmt.Sprintf("0:%d", len(file)) {
		t.Fatalf("%+v", appended)
	}

	page := bytes.Repeat([]byte("a"), ceiling+10)
	exact := must[*State](t)(Start(f.ctx, f.d, startIn("northwind-chunk-ceiling", page), a))
	if !strings.Contains(exact.NextStep, "2 appends for this file") {
		t.Fatal(exact.NextStep)
	}
	first := must[*State](t)(Append(f.ctx, f.d, AppendInput{exact.UploadID, 0, b64(page[:ceiling])}, a))
	if first.BytesReceived != ceiling || !strings.Contains(first.NextStep, "for the next 10 raw bytes at offset 1048576") {
		t.Fatal(first.NextStep)
	}
	last := must[*State](t)(Append(f.ctx, f.d, AppendInput{exact.UploadID, 1, b64(page[ceiling:])}, a))
	if !last.Complete || chunkRows(exact.UploadID) != "0:1048576,1:10" {
		t.Fatal(chunkRows(exact.UploadID))
	}

	over := bytes.Repeat([]byte("b"), ceiling+1)
	refused := must[*State](t)(Start(f.ctx, f.d, startIn("northwind-chunk-ceiling", over), a))
	_, err := Append(f.ctx, f.d, AppendInput{refused.UploadID, 0, b64(over)}, a)
	wantErr(t, err, 400, "upload_chunk_invalid")
	if chunkRows(refused.UploadID) != "" {
		t.Fatal("nothing is stored")
	}
	// The DB constraint is the backstop, and it sits at the same number.
	var def string
	if err := f.d.QueryRow(f.ctx, "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='page_content_upload_chunks_bytes_check'").Scan(&def); err != nil ||
		!strings.Contains(def, "octet_length(bytes) <= 1048576") {
		t.Fatal(def, err)
	}
	// A ticket PUT is split at the ceiling in force.
	tk := must[*State](t)(CreateTicket(f.ctx, f.d, startIn("northwind-chunk-ceiling", page), a))
	must[*State](t)(PutTicketContent(f.ctx, f.d, tk.Ticket, tk.UploadID, page))
	if chunkRows(tk.UploadID) != "0:1048576,1:10" {
		t.Fatal(chunkRows(tk.UploadID))
	}
}

// ── Managed-data staging limits (test/data-limits.integration.js) ──────────

func TestStagedDataSizeIsRefusedBeforeReserving(t *testing.T) {
	f := setup(t)
	t.Setenv("PAGES_DATA_MAX_BYTES", "2048")
	t.Setenv("PAGES_MCP_MAX_INLINE_DATA_BYTES", "1024")
	t.Setenv("MAX_HTML_BYTES", "4kb")
	a := f.agent("data-limit-fixture")
	limits := `{"max_payload_bytes":2048,"max_envelope_bytes":2048,"max_inline_bytes":1024,"max_staged_bytes":2097152,"max_request_bytes":4096,"min_envelope_overhead_bytes":`
	text := []byte(`{"value":"Northwind"}`)
	in := StartInput{Slug: ptr("data-size-contract"), Kind: ptr("data"), TotalBytes: len(text), ContentSHA256: SHA256(text)}
	for _, c := range []struct {
		payload, escaped float64
		boundary         string
	}{{2049, 2049, "payload"}, {100, 2048, "envelope"}} {
		in.DataSize = jsonv.ObjectOf("payload_bytes", c.payload, "escaped_payload_bytes", c.escaped)
		for _, open := range []func(context.Context, *db.DB, StartInput, versions.Actor) (*State, error){Start, CreateTicket} {
			_, err := open(f.ctx, f.d, in, a)
			e := wantErr(t, err, 400, "data_validation_failed")
			if dt := jsonv.Stringify(e.Details); !strings.HasPrefix(dt, `{"size_limit":"`+c.boundary+`"`) || !strings.Contains(dt, `"changing_transport_can_help":false`) {
				t.Fatal(dt)
			}
		}
	}
	if f.count("SELECT count(*)::int FROM page_content_uploads") != 0 || f.count("SELECT count(*)::int FROM page_upload_attempts") != 0 {
		t.Fatal("known-impossible data must not reserve uploads or attempts")
	}
	// Direct PUT has the staged-file limit, independently of the smaller JSON
	// request limit; the chunk ceiling falls to its floor at a 4 KB request.
	ticketText := []byte(strings.Repeat(" ", 5000) + "{\n  \"value\": \"Northwind direct file upload\"\n}\n")
	in = StartInput{Slug: ptr("data-size-contract"), Kind: ptr("data"), TotalBytes: len(ticketText), ContentSHA256: SHA256(ticketText),
		DataSize: jsonv.ObjectOf("payload_bytes", 40.0, "escaped_payload_bytes", 40.0)}
	tk := must[*State](t)(CreateTicket(f.ctx, f.d, in, a))
	if !strings.Contains(jsonv.Stringify(tk.DataLimits), limits) || tk.MaxChunkBytes != MinChunkCeilingBytes {
		t.Fatal(jsonv.Stringify(tk))
	}
	srv := httptest.NewServer(http.StripPrefix("/upload", NewHandler(HandlerOptions{DB: f.d})))
	defer srv.Close()
	res := put(t, srv.URL+"/upload/"+tk.UploadID, tk.Ticket, ticketText, nil)
	if res.status != 200 || !strings.Contains(res.body, `"complete":true`) || !strings.Contains(res.body, `"data_limits":`+limits) ||
		!strings.Contains(res.body, fmt.Sprintf(`"bytes_received":%d`, len(ticketText))) {
		t.Fatalf("%d %s", res.status, res.body)
	}
	if f.count("SELECT count(*)::int FROM page_content_upload_chunks WHERE upload_id = $1", tk.UploadID) != 2 {
		t.Fatal("split at the 4096-byte floor")
	}
}

// ── The ticket endpoint (lib/uploadticket.js) ──────────────────────────────

type response struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, method, url, token string, body []byte, hdr map[string]string) response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return response{res.StatusCode, res.Header, string(b)}
}

func put(t *testing.T, url, token string, body []byte, hdr map[string]string) response {
	t.Helper()
	return do(t, http.MethodPut, url, token, body, hdr)
}

func TestTicketEndpoint(t *testing.T) {
	f := setup(t)
	a := f.agent("ticket-agent")
	var limited int
	limit := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limited++
			next.ServeHTTP(w, r)
		})
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	srv := httptest.NewServer(http.StripPrefix("/upload", NewHandler(HandlerOptions{DB: f.d, Limit: limit, Next: next})))
	defer srv.Close()

	var hb strings.Builder
	hb.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>Ticketed</title></head><body><main>`)
	for i := range 2400 {
		fmt.Fprintf(&hb, `<p data-r="%d">row %d ☀</p>`, i, i)
	}
	hb.WriteString("</main></body></html>")
	html := []byte(hb.String())
	if len(html) <= 64*1024 {
		t.Fatal("fixture must exceed one chunk")
	}
	t.Setenv("DASHBOARD_ORIGIN", srv.URL)
	tk := must[*State](t)(CreateTicket(f.ctx, f.d, startIn("mcp-ticket-upload", html), a))
	if !strings.HasPrefix(tk.Ticket, "pgu_") || len(tk.Ticket) != 36 || tk.Complete || tk.BytesReceived != 0 || tk.TicketExpiresAt == nil {
		t.Fatalf("%+v", tk)
	}
	wire := jsonv.Stringify(tk)
	if !strings.Contains(wire, `"next_step":`) || !strings.Contains(wire, `,"ticket":"pgu_`) || !strings.HasSuffix(wire, `Z"}`) {
		t.Fatal(wire)
	}
	var minutes float64
	if err := f.d.QueryRow(f.ctx, "SELECT extract(epoch FROM ticket_expires_at - now())/60 FROM page_content_uploads WHERE id=$1", tk.UploadID).Scan(&minutes); err != nil || minutes < 14.9 || minutes > 15.1 {
		t.Fatal(minutes, err)
	}
	if f.count("SELECT count(*)::int FROM page_content_uploads WHERE ticket_hash = $1", tokens.HashToken(tk.Ticket)) != 1 {
		t.Fatal("only the hash is stored")
	}
	url := UploadURL(tk.UploadID)

	r := put(t, url, "", html, nil)
	if r.status != 401 || r.body != `{"error":"an upload ticket is required","code":"upload_ticket_missing"}` ||
		r.header.Get("WWW-Authenticate") != `Bearer realm="pages-upload"` || r.header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("%+v", r)
	}
	agentToken := must[*tokens.Minted](t)(tokens.Mint(f.ctx, f.d, tokens.MintParams{Label: "not-a-ticket"}))
	if r = put(t, url, agentToken.Token, html, nil); r.status != 401 || !strings.Contains(r.body, "upload_ticket_invalid") {
		t.Fatal("a deploy token is not an upload ticket", r)
	}
	wrong := append(append([]byte{}, html[:len(html)-1]...), 'X')
	if r = put(t, url, tk.Ticket, wrong, nil); r.status != 409 || !strings.Contains(r.body, `"code":"upload_hash_mismatch"`) || !strings.Contains(r.body, "the ticket is still valid") {
		t.Fatal(r)
	}
	if r = put(t, url, tk.Ticket, html[:100], nil); r.status != 400 || r.body != fmt.Sprintf(`{"error":"expected exactly %d bytes, received 100","code":"upload_size_mismatch"}`, len(html)) {
		t.Fatal(r)
	}
	if r = put(t, url, tk.Ticket, []byte{}, nil); r.status != 400 || !strings.Contains(r.body, "upload_body_empty") {
		t.Fatal(r)
	}
	otherTk := must[*State](t)(CreateTicket(f.ctx, f.d, startIn("mcp-ticket-other", html), a))
	if r = put(t, url, otherTk.Ticket, html, nil); r.status != 401 || r.body != `{"error":"unknown or expired upload ticket","code":"upload_ticket_invalid"}` {
		t.Fatal("a ticket for another upload must not work here", r)
	}
	// Too large is refused before it is read.
	if r = put(t, url, tk.Ticket, make([]byte, MaxUploadBytes+1), nil); r.status != 413 ||
		r.body != `{"error":"page content must be at most 2097152 bytes","code":"upload_size_invalid"}` {
		t.Fatal(r)
	}
	if r = put(t, url, tk.Ticket, html, map[string]string{"Content-Encoding": "br"}); r.status != 415 {
		t.Fatal(r)
	}

	r = put(t, url, tk.Ticket, html, map[string]string{"Content-Type": "text/html"})
	if r.status != 200 || !strings.Contains(r.body, `"complete":true`) || !strings.Contains(r.body, fmt.Sprintf(`"bytes_received":%d`, len(html))) ||
		!strings.HasSuffix(r.body, `"deduped":false}`) || !strings.Contains(r.body, "deploy_page_upload") || r.header.Get("ETag") == "" {
		t.Fatal(r)
	}
	if r = put(t, url, tk.Ticket, html, nil); r.status != 200 || !strings.HasSuffix(r.body, `"deduped":true}`) {
		t.Fatal("a retried curl of the identical file is a safe no-op", r)
	}
	if r = put(t, url, tk.Ticket, []byte("<!doctype html><p>substituted</p>"), nil); r.status != 409 || !strings.Contains(r.body, "upload_ticket_used") {
		t.Fatal(r)
	}
	if r = do(t, http.MethodGet, url, tk.Ticket, nil, nil); r.status != 405 || r.header.Get("Allow") != "PUT" ||
		r.body != `{"error":"use PUT to send the page bytes","code":"method_not_allowed"}` {
		t.Fatal(r)
	}
	if r = do(t, http.MethodPost, url, "", nil, nil); r.status != 405 {
		t.Fatal(r)
	}
	// The staged bytes deploy through the ordinary consumer.
	res := must[any](t)(Deploy(f.ctx, f.d, tk.UploadID, a, keyA, deployPage(a, true), DeployOptions{})).(*jsonv.Object)
	var stored string
	if err := f.d.QueryRow(f.ctx, "SELECT html FROM page_versions WHERE id = $1::bigint", res.Value("version").(*jsonv.Object).Value("id")).Scan(&stored); err != nil || stored != string(html) {
		t.Fatal("out-of-band bytes survive byte-for-byte", err)
	}
	if r = put(t, url, tk.Ticket, html, nil); r.status != 409 || !strings.Contains(r.body, "page_upload_committed") {
		t.Fatal(r)
	}

	// Expired tickets, and gzip transfer.
	gz := must[*State](t)(CreateTicket(f.ctx, f.d, startIn("mcp-ticket-gzip", html), a))
	f.exec("UPDATE page_content_uploads SET ticket_expires_at = now() - interval '1 second' WHERE id = $1", gz.UploadID)
	if r = put(t, UploadURL(gz.UploadID), gz.Ticket, html, nil); r.status != 401 || !strings.Contains(r.body, "upload_ticket_expired") {
		t.Fatal(r)
	}
	f.exec("UPDATE page_content_uploads SET ticket_expires_at = now() + interval '5 minutes' WHERE id = $1", gz.UploadID)
	if r = put(t, UploadURL(gz.UploadID), gz.Ticket, gzipBytes(t, html), map[string]string{"Content-Encoding": "gzip"}); r.status != 200 {
		t.Fatal(r)
	}
	if r = put(t, UploadURL(gz.UploadID), gz.Ticket, []byte("not gzip"), map[string]string{"Content-Encoding": "gzip"}); r.status != 400 {
		t.Fatal(r)
	}

	// Paths that are not /<upload_id> fall through, still behind the limiter.
	before := limited
	if r = do(t, http.MethodPut, srv.URL+"/upload/a/b", "", nil, nil); r.status != http.StatusTeapot || limited != before+1 {
		t.Fatal(r)
	}
	if r = do(t, http.MethodPut, srv.URL+"/upload/", "", nil, nil); r.status != http.StatusTeapot {
		t.Fatal(r)
	}
	if r = do(t, http.MethodPut, srv.URL+"/upload/%FF", "x", []byte("x"), nil); r.status != 500 || r.body != `{"error":"internal error"}` {
		t.Fatal(r)
	}
	if r = put(t, srv.URL+"/upload/not-a-uuid", "pgu_nope", []byte("x"), nil); r.status != 401 {
		t.Fatal(r)
	}
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

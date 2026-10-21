// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/pages/internal/env"
)

// maxDuration is COMPOSE_TIMEOUT_MS; 0 (the default) means no time limit —
// cutlass runs as long as it needs.
func maxDuration() time.Duration {
	ms := env.Int("COMPOSE_TIMEOUT_MS", 0)
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func pollInterval() time.Duration {
	return time.Duration(env.NonNegativeInt("COMPOSE_POLL_MS", 3000)) * time.Millisecond
}

// seconds is Math.round(d / 1000).
func seconds(d time.Duration) int64 { return int64(math.Round(d.Seconds())) }

// ── driver: spawn the local cutlass CLI ──────────────────────────────────────

func startSpawn(j *job, task string) {
	bin, dir := env.String("CUTLASS_BIN", ""), env.String("CUTLASS_DIR", "")
	childEnv := ChildEnv(processEnv())
	environ := make([]string, 0, len(childEnv))
	for k, v := range childEnv {
		environ = append(environ, k+"="+v)
	}
	sort.Strings(environ)

	// An argument vector and no shell: a prompt cannot become a command. The
	// run outlives the request that started it, so it is not tied to that
	// request's context.
	cmd := exec.CommandContext(context.Background(), bin, "-task", task)
	cmd.Dir = dir
	cmd.Env = environ
	cmd.Stdout = logWriter{j}
	cmd.Stderr = logWriter{j}
	// A killed cutlass may leave a grandchild holding its output open; stop
	// waiting on it shortly after the child itself is gone.
	cmd.WaitDelay = 5 * time.Second
	j.pushLog(fmt.Sprintf("▸ cutlass %s (cwd %s)", bin, dir))
	if err := cmd.Start(); err != nil {
		j.setStatus("error")
		j.pushLog("spawn error: " + err.Error())
		return
	}

	// No time limit by default. Only arm a hard kill if one is set.
	var killer *time.Timer
	if limit := maxDuration(); limit > 0 {
		killer = time.AfterFunc(limit, func() {
			j.pushLog(fmt.Sprintf("✗ timed out after %ds — killing", seconds(limit)))
			j.setStatus("error")
			_ = cmd.Process.Kill()
		})
	}

	go func() {
		err := cmd.Wait()
		if killer != nil {
			killer.Stop()
		}
		// A child killed by a signal has no exit code (JS: code null).
		var code *int
		if cmd.ProcessState != nil {
			if c := cmd.ProcessState.ExitCode(); c >= 0 {
				code = &c
			}
		} else if err != nil {
			j.pushLog("spawn error: " + err.Error())
		}
		j.mu.Lock()
		j.code = code
		if j.status == "running" {
			if code != nil && *code == 0 {
				j.status = "done"
			} else {
				j.status = "error"
			}
		}
		j.mu.Unlock()
		shown := "null"
		if code != nil {
			shown = strconv.Itoa(*code)
		}
		j.pushLog("▸ cutlass exited (code " + shown + ")")
	}()
}

// logWriter streams a child's output into the job log as it arrives.
type logWriter struct{ j *job }

func (w logWriter) Write(p []byte) (int, error) {
	w.j.pushLog(string(p))
	return len(p), nil
}

// ── driver: dispatch via the MOC task API, then poll ─────────────────────────

var mocClient = &http.Client{Timeout: 2 * time.Minute}

// mocFetch calls MOC and decodes its JSON answer. A body that is not JSON
// decodes as {"raw": text}; a non-2xx status is an error naming the path,
// status and MOC's own message.
func mocFetch(ctx context.Context, method, path string, payload any) (map[string]any, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, mocURL()+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", env.String("MOC_API_KEY", ""))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := mocClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	out := map[string]any{}
	if text != "" {
		dec := json.NewDecoder(strings.NewReader(text))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			out = map[string]any{"raw": text}
		} else if m, ok := v.(map[string]any); ok {
			out = m
		}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		msg := truthy(out["error"])
		if msg == "" {
			msg = truthy(out["message"])
		}
		if msg == "" {
			msg = firstUTF16(text, 200)
		}
		return nil, fmt.Errorf("MOC %s → %d %s", path, res.StatusCode, msg)
	}
	return out, nil
}

// jsString is String(v) for a decoded JSON value.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return x.String()
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// truthy is jsString(v) when v is JS-truthy, else "".
func truthy(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if !x {
			return ""
		}
	case json.Number:
		if f, err := x.Float64(); err == nil && f == 0 {
			return ""
		}
	}
	return jsString(v)
}

func orZero(v any) string {
	if s := truthy(v); s != "" {
		return s
	}
	return "0"
}

func renderLogSession(ls map[string]any) []string {
	var lines []string
	if t := truthy(ls["title"]); t != "" {
		lines = append(lines, "title: "+t)
	}
	if msgs, ok := ls["messages"].([]any); ok {
		for _, raw := range msgs {
			m, _ := raw.(map[string]any)
			c := strings.TrimSpace(clean(truthy(m["content"])))
			if c == "" {
				continue
			}
			role := truthy(m["role"])
			if role == "" {
				role = "?"
			}
			lines = append(lines, "["+role+"] "+c)
		}
	}
	if cost, ok := ls["cost"]; ok && cost != nil {
		lines = append(lines, fmt.Sprintf("cost: $%s (%s in / %s out)", jsString(cost), orZero(ls["prompt_tokens"]), orZero(ls["completion_tokens"])))
	}
	return lines
}

func startMoc(j *job, task string) {
	go func() {
		ctx := context.Background()
		payload := map[string]any{"prompt": task}
		if m := env.String("MOC_MODEL", ""); m != "" {
			payload["model"] = m
		}
		if m := env.String("MOC_FALLBACK_MODEL", ""); m != "" {
			payload["fallback_model"] = m
		}
		if m := env.String("MOC_TARGET_NODE_NAME", ""); m != "" {
			payload["target_node_name"] = m
		}
		created, err := mocFetch(ctx, http.MethodPost, "/tasks", payload)
		if err != nil {
			j.setStatus("error")
			j.pushLog("MOC create failed: " + err.Error())
			return
		}
		taskID := jsString(created["id"])
		if _, ok := created["id"]; !ok {
			taskID = "undefined"
		}
		status := truthy(created["status"])
		if status == "" {
			status = "pending"
		}
		j.pushLog(fmt.Sprintf("▸ MOC task %s created (status %s)", taskID, status))

		limit := maxDuration()
		var deadline time.Time
		if limit > 0 {
			deadline = time.Now().Add(limit)
		}
		for {
			if !deadline.IsZero() && time.Now().After(deadline) {
				j.setStatus("error")
				j.pushLog(fmt.Sprintf("✗ timed out after %ds waiting on MOC", seconds(limit)))
				return
			}
			time.Sleep(pollInterval())
			t, err := mocFetch(ctx, http.MethodGet, "/tasks/"+taskID, nil)
			if err != nil {
				j.pushLog("poll error (will retry): " + err.Error())
				continue
			}
			// Refresh the log from the session (best-effort: the log may not
			// exist yet).
			if ls, err := mocFetch(ctx, http.MethodGet, "/logs/"+taskID, nil); err == nil {
				if lines := renderLogSession(ls); len(lines) > 0 {
					j.setLog(append([]string{"▸ MOC task " + taskID}, lines...))
				}
			}

			st, _ := t["status"].(string)
			switch strings.ToLower(st) {
			case "success":
				j.setStatus("done")
				if r := truthy(t["result"]); r != "" {
					j.pushLog("result: " + clean(r))
				}
				j.pushLog("▸ MOC task " + taskID + " succeeded")
				return
			case "error", "cancelled":
				j.setStatus("error")
				if e := truthy(t["error_message"]); e != "" {
					j.pushLog("error: " + clean(e))
				}
				j.pushLog("▸ MOC task " + taskID + " " + strings.ToLower(st))
				return
			}
			// Still pending/assigned/leased/running/analyzing: keep polling.
		}
	}()
}

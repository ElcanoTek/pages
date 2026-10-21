// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	Register(&Command{
		Name:    "mcp-smoke",
		Summary: "drive /mcp the way agents do (initialize → initialized → tools/list → deploy_page → get_page) against a running server",
		Hidden:  true,
		Run:     runMCPSmoke,
	})
}

const mcpProtocolVersion = "2025-11-25"

// runMCPSmoke eyeballs the JSON-RPC wire before registering Pages in an
// agent. PAGES_MCP_URL (default http://127.0.0.1:3099/mcp), TOKEN (default
// .devdata/agent-token) and SLUG (smoke-demo) override.
func runMCPSmoke(args []string) int {
	if wantsHelp(args) {
		fmt.Println("Usage: pages mcp-smoke   (PAGES_MCP_URL, TOKEN, SLUG override; boot the server first with: pages dev)")
		return 0
	}
	url := os.Getenv("PAGES_MCP_URL")
	if url == "" {
		url = "http://127.0.0.1:3099/mcp"
	}
	token := os.Getenv("TOKEN")
	if token == "" {
		wd, _ := os.Getwd()
		if root := checkoutRoot(wd); root != "" {
			b, _ := os.ReadFile(filepath.Join(root, ".devdata", "agent-token"))
			token = strings.TrimSpace(string(b))
		}
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "no token: set TOKEN=… or run 'pages dev' first (writes .devdata/agent-token)")
		return 1
	}
	slug := os.Getenv("SLUG")
	if slug == "" {
		slug = "smoke-demo"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	post := func(body any, negotiated bool) (int, []byte, error) {
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
		if err != nil {
			return 0, nil, err
		}
		// Streamable HTTP clients accept both media types even though Pages
		// answers with stateless JSON; the protocol header starts after init.
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		if negotiated {
			req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(resp.Body)
		return resp.StatusCode, out, err
	}
	pretty := func(b []byte) string {
		var buf bytes.Buffer
		if json.Indent(&buf, b, "", "  ") != nil {
			return string(b)
		}
		return buf.String()
	}
	type rpc = map[string]any
	fmt.Printf("▸ POST %s\n\n── initialize ──\n", url)
	_, b, err := post(rpc{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": rpc{"protocolVersion": mcpProtocolVersion,
		"capabilities": rpc{}, "clientInfo": rpc{"name": "pages-mcp-smoke", "version": "1.0.0"}}}, false)
	if err != nil {
		return Fail("mcp-smoke", "%v", err)
	}
	fmt.Println(pretty(b))
	fmt.Println("\n── notifications/initialized ──")
	code, _, err := post(rpc{"jsonrpc": "2.0", "method": "notifications/initialized"}, true)
	if err != nil {
		return Fail("mcp-smoke", "%v", err)
	}
	fmt.Printf("  HTTP %d\n\n── tools/list (names) ──\n", code)
	_, b, err = post(rpc{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, true)
	if err != nil {
		return Fail("mcp-smoke", "%v", err)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return Fail("mcp-smoke", "tools/list: %s", pretty(b))
	}
	for _, t := range list.Result.Tools {
		fmt.Println("  - " + t.Name)
	}
	type result struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	call := func(id int, name string, arguments rpc) (*result, error) {
		_, b, err := post(rpc{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": rpc{"name": name, "arguments": arguments}}, true)
		if err != nil {
			return nil, err
		}
		var r result
		if err := json.Unmarshal(b, &r); err != nil || len(r.Result.Content) == 0 {
			return nil, fmt.Errorf("%s: %s", name, pretty(b))
		}
		return &r, nil
	}
	fmt.Printf("\n── tools/call deploy_page (slug=%s, publish) ──\n", slug)
	html := "<!doctype html><html><head><title>Smoke</title></head><body><h1>Hello from MCP smoke</h1><script>chart()</script></body></html>"
	r, err := call(3, "deploy_page", rpc{"slug": slug, "title": "Smoke", "html": html, "publish": true})
	if err != nil {
		return Fail("mcp-smoke", "%v", err)
	}
	fmt.Println("isError:", r.Result.IsError)
	fmt.Println(pretty([]byte(r.Result.Content[0].Text)))
	fmt.Println("\n── tools/call get_page (confirm it is live) ──")
	r, err = call(4, "get_page", rpc{"slug": slug})
	if err != nil {
		return Fail("mcp-smoke", "%v", err)
	}
	var page struct {
		Page struct {
			PublishedVersionID any `json:"published_version_id"`
		} `json:"page"`
		URLs json.RawMessage `json:"urls"`
	}
	_ = json.Unmarshal([]byte(r.Result.Content[0].Text), &page)
	fmt.Println("  published_version_id:", page.Page.PublishedVersionID)
	fmt.Println("  live urls:", string(page.URLs))
	fmt.Println("\n✓ mcp smoke complete")
	return 0
}

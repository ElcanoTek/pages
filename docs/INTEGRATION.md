# Registering Pages' MCP server in chat & cutlass

Pages exposes an MCP-over-HTTP server at `https://pages.elcanotek.com/mcp`
(see [API.md](API.md)). To let an agent use it, the agent must **register** the
server: a small, config-only edit in the agent's repo (no agent-logic change).
This doc has the exact edits for **cutlass** and **chat**, plus how to test the
whole thing **locally first** — before committing anything to either repo.

> These edits land in the cutlass / chat repos, applied by their owners. Nothing
> here is committed to those repos from pages. The patches below are anchored to
> each repo's existing `fast_io` HTTP MCP server so they apply unambiguously.
>
> Branch note (verified 2026-06-16): **cutlass** has no `main` — its canonical
> branch is `dev`. **chat**'s canonical branch is `main`. Anchor line numbers
> below are from those branches.

---

## Test locally first (no commits to any agent)

You can fully exercise the agent→pages loop on `127.0.0.1` before touching
cutlass/chat. Three tiers, cheapest first.

### Tier 0 — the wire, no agent (seconds)

Boot pages locally and run the exact JSON-RPC handshake an agent performs:

```bash
sudo bash scripts/dev.sh            # pages on :3099; token saved to .devdata/agent-token
bash scripts/mcp-smoke.sh           # initialize → tools/list → deploy_page → get_page
```

This proves the server speaks the cutlass/chat contract (protocol `2024-11-05`,
JSON-RPC 2.0, bearer auth, JSON responses). It needs no agent and no LLM key.

### Tier 1 — a real MCP client, no LLM (minutes)

Point the standard MCP Inspector at the local server and click through the tools:

```bash
npx @modelcontextprotocol/inspector        # opens a UI
#   Transport: Streamable HTTP
#   URL:       http://127.0.0.1:3099/mcp
#   Header:    Authorization: Bearer <paste .devdata/agent-token>
```

Same protocol cutlass/chat use, so a green run here = they will connect too.

### Tier 2 — the real cutlass agent, end to end (needs an OpenRouter key)

Run cutlass from an **isolated git worktree** so its checkout is never touched
and nothing is committed/pushed. Apply the patch (below) in the worktree only:

```bash
# 1. pages running locally (Tier 0 left it on :3099)
TOKEN=$(cat /root/pages/.devdata/agent-token)

# 2. isolated cutlass worktree (canonical branch is 'dev')
git -C /root/cutlass worktree add /tmp/cutlass-pages dev
cd /tmp/cutlass-pages
#    …apply the cutlass edits from this doc here, then:
make build

# 3. point it at LOCAL pages and run a task
export OPENROUTER_API_KEY=sk-or-...           # your key
export PAGES_MCP_TOKEN="$TOKEN"
export PAGES_MCP_URL="http://127.0.0.1:3099/mcp"
./bin/cutlass --task "Create and publish a page 'hello' on pages with an <h1>Hello from cutlass</h1>."

#    watch the log for: Connecting to MCP server: pages
#    then the agent calls deploy_page; confirm with scripts/mcp-smoke.sh or get_page.

# 4. throw the worktree away — cutlass's real checkout was never modified
git -C /root/cutlass worktree remove /tmp/cutlass-pages --force
```

(chat's equivalent: `scripts/start.sh` with `PAGES_API_TOKEN` + `PAGES_MCP_URL`
set; `CHAT_MOCK_MODE=1` skips the LLM if you only want to verify connection.)

The `PAGES_MCP_URL` override in the patches exists precisely for this — it
defaults to the prod URL and only changes for local runs.

---

## cutlass — the edits (branch `dev`)

All four are in `internal/config/config.go`, mirroring `fast_io`.

**1. Allowlist the env vars** — after `"FAST_IO_MCP_TOKEN": true,` (~line 108):
```go
		"FAST_IO_MCP_TOKEN": true,
		"PAGES_MCP_TOKEN":   true,
		"PAGES_MCP_URL":     true, // local/staging override for the /mcp URL
```

**2. Config struct field** — after `FastIOMCPToken string` (~line 281):
```go
	FastIOMCPToken string
	PagesMCPToken  string
```

**3. Read it in `Load()`** — after the `FastIOMCPToken` read (~line 431):
```go
	cfg.FastIOMCPToken = stripQuotes(os.Getenv("FAST_IO_MCP_TOKEN"))
	cfg.PagesMCPToken = stripQuotes(os.Getenv("PAGES_MCP_TOKEN"))
```

**4. Server definition** — in `getMCPServerDefinitions()`, immediately after the
closing `}` of the `fast_io` entry (~line 772):
```go
		{
			name:       "pages",
			serverType: mcpServerTypeHTTP,
			// PAGES_MCP_URL lets you point at a local pages (e.g.
			// http://127.0.0.1:3099/mcp) for testing; defaults to prod.
			URL:       getEnvOrDefault("PAGES_MCP_URL", "https://pages.elcanotek.com/mcp"),
			isEnabled: func(c *Config) bool { return c.PagesMCPToken != "" },
			headerBuilder: func(c *Config) map[string]string {
				return map[string]string{"Authorization": "Bearer " + c.PagesMCPToken}
			},
			// All pages agent tools (no admin actions are exposed by the server).
			toolAllowlist: []string{
				"list_pages", "get_page", "deploy_page", "update_page",
				"publish_page", "rollback_page", "list_versions", "page_urls",
			},
		},
```

Enable by setting `PAGES_MCP_TOKEN` (a token minted by pages, below).

---

## chat — the edits (branch `main`)

Three in `server/internal/config/config.go`, one in
`server/cmd/chat-server/main.go`.

**1. Allowlist the env vars** — after `"FAST_IO_MCP_TOKEN": true,` (~line 86):
```go
	"FAST_IO_MCP_TOKEN": true,
	"PAGES_API_TOKEN":   true,
	"PAGES_MCP_URL":     true, // local/staging override
```

**2. Config struct field** — after `FastIOMCPToken string` (~line 270):
```go
	FastIOMCPToken string
	PagesAPIToken  string
```

**3. Read it in `Load()`** — after the `FastIOMCPToken` read (~line 470):
```go
		FastIOMCPToken:              os.Getenv("FAST_IO_MCP_TOKEN"),
		PagesAPIToken:               os.Getenv("PAGES_API_TOKEN"),
```

**4. Server spec** — in `buildMCPSpecs()` (`server/cmd/chat-server/main.go`),
immediately after the `fast_io` `if/else` block (~line 251):
```go
	// pages: internal HTTP MCP for versioned client dashboards/pages.
	if cfg.PagesAPIToken != "" {
		pagesURL := "https://pages.elcanotek.com/mcp"
		if v := os.Getenv("PAGES_MCP_URL"); v != "" {
			pagesURL = v // local/staging override
		}
		specs["pages"] = agent.MCPServerSpec{
			Enabled: true,
			URL:     pagesURL,
			Headers: map[string]string{"Authorization": "Bearer " + cfg.PagesAPIToken},
			ToolAllowlist: []string{
				"list_pages", "get_page", "deploy_page", "update_page",
				"publish_page", "rollback_page", "list_versions", "page_urls",
			},
			Description: "Elcano Pages — versioned, Flag-themed client dashboards/pages",
		}
	} else {
		log.Println("pages MCP: disabled (PAGES_API_TOKEN missing)")
	}
```

(`os` is already imported in `main.go`. `MCPServerSpec.Description` exists on
current `main`.)

Also add `PAGES_API_TOKEN=` to chat's `.env.local.example`.

---

## The token

Each agent gets its own bearer token (so they revoke independently):

```bash
# on the pages host:
pages token add cutlass        # prints the raw token once
pages token add chat
# locally: node scripts/token.js add cutlass
```

Set it as `PAGES_MCP_TOKEN` (cutlass) / `PAGES_API_TOKEN` (chat). Revoke with
`pages token revoke <id>`.

> Auth note: pages currently issues its own bearer tokens (this section). If we
> later unify on Elcano-signed agent tokens, only the *token string* changes —
> the registration above stays the same. See the auth discussion in the project
> notes.

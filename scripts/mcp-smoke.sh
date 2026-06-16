#!/usr/bin/env bash
# scripts/mcp-smoke.sh — drive the /mcp endpoint exactly the way cutlass/chat do,
# over curl, against a RUNNING pages server. Lets you eyeball the JSON-RPC wire
# (initialize → tools/list → tools/call) before registering pages in any agent.
#
#   # boot pages first:  sudo bash scripts/dev.sh   (server on :3099)
#   bash scripts/mcp-smoke.sh
#
# Env overrides:
#   PAGES_MCP_URL   default http://127.0.0.1:3099/mcp
#   TOKEN           default $(cat .devdata/agent-token)
#   SLUG            default smoke-demo
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
URL="${PAGES_MCP_URL:-http://127.0.0.1:3099/mcp}"
TOKEN="${TOKEN:-$(cat "$ROOT/.devdata/agent-token" 2>/dev/null || true)}"
SLUG="${SLUG:-smoke-demo}"

if [ -z "$TOKEN" ]; then
  echo "no token: set TOKEN=… or run 'sudo bash scripts/dev.sh' first (writes .devdata/agent-token)" >&2
  exit 1
fi

# Same headers cutlass sends on every MCP request (client.go): JSON body, accepts
# JSON or SSE, bearer auth.
hdrs=(-H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" -H "Authorization: Bearer $TOKEN")
pp() { node -e "let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{try{console.log(JSON.stringify(JSON.parse(s),null,2))}catch{console.log(s)}})"; }

echo "▸ POST $URL"
echo
echo "── initialize ──"
curl -sS "${hdrs[@]}" "$URL" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"cutlass","version":"1.0.0"}}}' | pp

echo
echo "── tools/list (names) ──"
curl -sS "${hdrs[@]}" "$URL" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  | node -e "let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{const t=JSON.parse(s).result.tools;console.log(t.map(x=>'  - '+x.name).join('\n'))})"

echo
echo "── tools/call deploy_page (slug=$SLUG, publish) ──"
DEPLOY_HTML='<!doctype html><html><head><title>Smoke</title></head><body><h1>Hello from MCP smoke</h1><script>chart()</script></body></html>'
REQ=$(node -e "console.log(JSON.stringify({jsonrpc:'2.0',id:3,method:'tools/call',params:{name:'deploy_page',arguments:{slug:process.argv[1],title:'Smoke',html:process.argv[2],publish:true}}}))" "$SLUG" "$DEPLOY_HTML")
curl -sS "${hdrs[@]}" "$URL" -d "$REQ" \
  | node -e "let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{const r=JSON.parse(s).result;console.log('isError:',!!r.isError);console.log(JSON.stringify(JSON.parse(r.content[0].text),null,2))})"

echo
echo "── tools/call get_page (confirm it is live) ──"
REQ=$(node -e "console.log(JSON.stringify({jsonrpc:'2.0',id:4,method:'tools/call',params:{name:'get_page',arguments:{slug:process.argv[1]}}}))" "$SLUG")
curl -sS "${hdrs[@]}" "$URL" -d "$REQ" \
  | node -e "let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{const d=JSON.parse(JSON.parse(s).result.content[0].text);console.log('  published_version_id:',d.page.published_version_id);console.log('  live urls:',JSON.stringify(d.urls))})"

echo
echo "✓ mcp smoke complete"

#!/bin/bash
# End-to-end smoke for the wire-registry build: real binary, real HTTP,
# fake upstream. Run on the mac from ~/songguo-build.
set -u
cd "$(dirname "$0")"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "ok   - $1"; }
bad()  { FAIL=$((FAIL+1)); echo "FAIL - $1"; }
check(){ if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (got: $2, want: $3)"; fi }

cleanup() {
  [ -n "${GW_PID:-}" ] && kill "$GW_PID" 2>/dev/null
  [ -n "${MOCK_PID:-}" ] && kill "$MOCK_PID" 2>/dev/null
}
trap cleanup EXIT

rm -f /tmp/songguo-e2e.db /tmp/songguo-e2e.db-wal /tmp/songguo-e2e.db-shm

# 1. Fake upstream: chat-completions shape with DeepSeek cache fields.
python3 - <<'PY' &
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        n = int(self.headers.get("content-length", 0)); self.rfile.read(n)
        body = b'{"id":"x","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_cache_hit_tokens":90}}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
HTTPServer(("127.0.0.1", 18081), H).serve_forever()
PY
MOCK_PID=$!

# 2. Gateway.
SONGGUO_DB=/tmp/songguo-e2e.db SONGGUO_LISTEN=127.0.0.1:18080 ./songguo-e2e-bin >/tmp/songguo-e2e.log 2>&1 &
GW_PID=$!

for i in $(seq 1 50); do
  curl -fsS http://127.0.0.1:18080/healthz >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS http://127.0.0.1:18080/healthz >/dev/null || { bad "gateway never became healthy"; cat /tmp/songguo-e2e.log; exit 1; }
ok "gateway healthy"

# 3. Wire registry is served.
WIRES=$(curl -fsS http://127.0.0.1:18080/api/wires)
check "GET /api/wires lists openai/chat" "$(echo "$WIRES" | grep -c 'openai/chat')" "1"
check "GET /api/wires lists anthropic/messages" "$(echo "$WIRES" | grep -c 'anthropic/messages')" "1"

# 4. Create a deepseek-style service: only openai/chat wired, cache quirk, cached price.
SVC=$(curl -fsS -X POST http://127.0.0.1:18080/api/services -H 'Content-Type: application/json' -d '{
  "name":"ds","adapter":"openai-compatible","base_url":"http://127.0.0.1:18081/v1",
  "api_key":"sk-upstream-SECRET","wires":["openai/chat"],"quirks":{"cache_tokens":"deepseek"},
  "models":[{"model":"m1","input":0.14,"output":0.28,"cached_input":0.0028,"unit":"per_1m_tokens"}]}')
check "service created with wires" "$(echo "$SVC" | grep -c '"wires":\["openai/chat"\]')" "1"
check "service response masks the key" "$(echo "$SVC" | grep -c 'masked_key')" "1"
check "service response never leaks the raw key" "$(echo "$SVC" | grep -c 'sk-upstream-SECRET')" "0"
SVC_ID=$(echo "$SVC" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

# 4b. Replace the key via PATCH (one key per service; no pool endpoints).
PATCHED=$(curl -fsS -X PATCH "http://127.0.0.1:18080/api/services/$SVC_ID" -H 'Content-Type: application/json' -d '{"api_key":"sk-upstream-ROTATED"}')
check "PATCH api_key returns masked preview" "$(echo "$PATCHED" | grep -c 'sk-')" "1"
check "PATCH api_key never echoes raw key" "$(echo "$PATCHED" | grep -c 'sk-upstream-ROTATED')" "0"

# 4c. Dashboard SPA serves the /services/new route (embedded dist fallback).
CODE=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/services/new)
check "GET /services/new serves the SPA" "$CODE" "200"

# 5. Consumer user key.
TOKEN=$(curl -fsS -X POST http://127.0.0.1:18080/api/users -H 'Content-Type: application/json' -d '{"name":"t"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["key"])')
[ -n "$TOKEN" ] && ok "token created" || bad "token creation"

# 6. Wired chat call routes + meters with cache discount.
CODE=$(curl -s -o /tmp/chat.out -w '%{http_code}' -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"model":"m1","messages":[]}')
check "chat via openai/chat wire" "$CODE" "200"

# 7. Unwired path is denied 404 wire_unmatched.
CODE=$(curl -s -o /tmp/emb.out -w '%{http_code}' -X POST http://127.0.0.1:18080/v1/embeddings \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"model":"m1","input":"hi"}')
check "embeddings denied (no wire)" "$CODE" "404"
check "deny body says wire_unmatched" "$(grep -c wire_unmatched /tmp/emb.out)" "1"

# 8. Calls log: one measured chat row with cache-discounted cost + one unmatched row.
sleep 0.3
CALLS=$(curl -fsS 'http://127.0.0.1:18080/api/calls?limit=10')
python3 - "$CALLS" <<'PY'
import json, sys
page = json.loads(sys.argv[1])
rows = page["entries"]
chat = [r for r in rows if r["wire"] == "openai/chat"]
unmatched = [r for r in rows if "unmatched:" in r.get("err", "")]
assert len(chat) == 1, f"chat rows: {rows}"
r = chat[0]
assert r["confidence"] == "measured", r
# 10 miss @0.14 + 90 hit @0.0028 + 10 out @0.28 per 1M
want = (10*0.14 + 90*0.0028 + 10*0.28) / 1e6
assert abs(r["cost"] - want) < 1e-12, (r["cost"], want)
assert r["usage"]["prompt_cache_hit_tokens"] == 90, r["usage"]
assert len(unmatched) == 1 and unmatched[0]["status"] == 404 and unmatched[0]["confidence"] == "unknown", unmatched
print("ok   - calls log: measured chat row w/ cache-discounted cost + unmatched 404 row")
PY
[ $? -eq 0 ] || bad "calls log assertions"

echo "----"
echo "passed=$PASS failed=$FAIL"
[ $FAIL -eq 0 ]

#!/usr/bin/env bash
# Drive both endpoints end to end against a running dev stack (see `make smoke`),
# with curl standing in for the MCP client.
#
# MCP 2026-07-28 over stateless streamable HTTP carries per-request framing that
# a session would otherwise negotiate once: the protocol version and client
# capabilities travel in _meta, and the method and tool name are mirrored into
# headers. All of it is required; the server rejects a request missing any part.
# The legacy endpoint is the opposite shape, and is exercised with the ordinary
# initialize handshake it exists to serve.
set -euo pipefail

PORT="${MCP_PORT:-8090}"
BASE="http://localhost:${PORT}"
ENDPOINT="${BASE}/mcp"
LEGACY="${BASE}/mcp/legacy"
VERSION="2026-07-28"
CAPS='{"elicitation":{"form":{},"url":{}}}'
META="\"_meta\":{\"io.modelcontextprotocol/protocolVersion\":\"${VERSION}\",\"io.modelcontextprotocol/clientCapabilities\":${CAPS}}"

rpc() {
  local method="$1" body="$2"
  shift 2
  curl -sS --fail-with-body --max-time 30 -X POST "$ENDPOINT" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -H "Mcp-Protocol-Version: ${VERSION}" \
    -H "Mcp-Method: ${method}" \
    "$@" \
    -d "$body" | sed 's/^data: //' | grep -v -e '^event:' -e '^$'
}

# raw_call returns the whole JSON-RPC envelope, so a caller can inspect an
# error or an input-required result rather than only a successful one.
raw_call() {
  local name="$1" args="$2" extra="${3:-}"
  rpc tools/call \
    "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{${META},\"name\":\"${name}\",\"arguments\":${args}${extra}}}" \
    -H "Mcp-Name: ${name}"
}

call_tool() {
  raw_call "$@" | python3 -c '
import json, sys
payload = json.load(sys.stdin)
if "error" in payload:
    sys.exit("JSON-RPC error from %s: %s" % (sys.argv[1], payload["error"]))
result = payload["result"]
if result.get("isError"):
    sys.exit("tool error from %s: %s" % (sys.argv[1], result["content"][0].get("text")))
json.dump(result.get("structuredContent", {}), sys.stdout)
' "$1"
}

field() {
  python3 -c 'import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1], {}, {"d": d}))' "$1"
}

echo "==> Waiting for ${ENDPOINT}"
ready=""
for _ in $(seq 1 30); do
  if rpc tools/list "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\",\"params\":{${META}}}" \
      >/dev/null 2>&1; then
    ready=yes
    break
  fi
  sleep 1
done
[ -n "$ready" ] || { echo "server did not become ready" >&2; exit 1; }

echo "==> tools/list"
rpc tools/list "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\",\"params\":{${META}}}" | python3 -c '
import json, sys
tools = json.load(sys.stdin)["result"]["tools"]
expected = {"list_scenarios", "run_scenario", "custom_form", "custom_url",
            "session_info", "exchange_log", "reset_log"}
names = {t["name"] for t in tools}
missing = expected - names
if missing:
    sys.exit("missing tools: %s" % sorted(missing))
for name in sorted(names):
    print("    %s" % name)
'

echo "==> session_info"
call_tool session_info '{}' \
  | field '"    protocol=%s delivery=%s trusted=%s fields=%d" % (d["protocol_version"] or "(default)", d["default_delivery"], d["allowed_url"], d["max_form_fields"])'

echo "==> list_scenarios"
call_tool list_scenarios '{}' | python3 -c '
import collections, json, sys
d = json.load(sys.stdin)
kinds = collections.Counter(s["kind"] for s in d["scenarios"])
render = sum(1 for s in d["scenarios"] if s["conformant"])
print("    %d scenarios: %s" % (d["count"], ", ".join("%s=%d" % kv for kv in sorted(kinds.items()))))
print("    %d to render, %d to refuse" % (render, d["count"] - render))
'

echo "==> run_scenario form/minimal (round 1 asks for input)"
first="$(raw_call run_scenario '{"scenario":"form/minimal"}')"
state="$(printf '%s' "$first" | python3 -c '
import json, sys
result = json.load(sys.stdin)["result"]
if result.get("resultType") != "input_required":
    sys.exit("expected an input-required result, got %s" % result.get("resultType"))
requests = result["inputRequests"]
if len(requests) != 1:
    sys.exit("expected one input request, got %d" % len(requests))
[(rid, entry)] = requests.items()
if entry["method"] != "elicitation/create":
    sys.exit("expected an elicitation, got %s" % entry["method"])
print(result["requestState"])
print("    request %s: %s" % (rid, entry["params"]["message"]), file=sys.stderr)
')"
echo "    state carried: ${#state} characters"

echo "==> run_scenario form/minimal (round 2 answers it)"
call_tool run_scenario '{"scenario":"form/minimal"}' \
  ",\"requestState\":\"${state}\",\"inputResponses\":{\"q1\":{\"action\":\"accept\",\"content\":{\"token\":\"smoke-value\"}}}" \
  | field '"    status=%s round=%d/%d action=%s fields=%s" % (d["status"], d["round"], d["rounds"], d["responses"][0]["action"], [(f["name"], f["json_type"], f["digest"]) for f in d["responses"][0]["fields"]])'

echo "==> a replayed request state is refused"
raw_call run_scenario '{"scenario":"form/minimal"}' \
  ",\"requestState\":\"${state}xx\",\"inputResponses\":{\"q1\":{\"action\":\"accept\",\"content\":{\"token\":\"smoke-value\"}}}" \
  | python3 -c '
import json, sys
result = json.load(sys.stdin)["result"]
if not result.get("isError"):
    sys.exit("a forged request state was accepted")
print("    refused: %s" % result["content"][0]["text"])
'

echo "==> run_scenario error/url_required (a -32042 protocol error)"
raw_call run_scenario '{"scenario":"error/url_required"}' | python3 -c '
import json, sys
payload = json.load(sys.stdin)
error = payload.get("error")
if not error or error["code"] != -32042:
    sys.exit("expected a -32042 error, got %s" % (error or payload.get("result")))
items = error["data"]["elicitations"]
print("    %d elicitation(s): %s" % (len(items), items[0]["url"]))
'

echo "==> exchange_log"
call_tool exchange_log '{"limit":5}' | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("    %d entries" % d["count"])
for entry in d["entries"]:
    print("    %s %s round=%s sent=%d answered=%d %s" % (
        entry["tool"], entry.get("scenario", ""), entry.get("round", "-"),
        len(entry.get("requested") or []), len(entry.get("answered") or []), entry.get("note", "")))
'
call_tool exchange_log '{"limit":5,"include_values":true}' | python3 -c '
import json, sys
values = [v for e in json.load(sys.stdin)["entries"] for a in (e.get("answered") or []) for v in (a.get("values") or {}).values()]
if "smoke-value" not in values:
    sys.exit("include_values did not return the submitted value: %s" % values)
print("    include_values returned the submitted value")
'

echo "==> authorization stand-in page"
curl -sS --fail-with-body --max-time 10 "${BASE}/authorize?scenario=smoke" >/dev/null
echo "    ${BASE}/authorize answers"

echo "==> legacy endpoint (stateful, ordinary initialize handshake)"
headers="$(mktemp)"
trap 'rm -f "$headers"' EXIT
legacy_version="$(curl -sS --fail-with-body --max-time 30 -D "$headers" -X POST "$LEGACY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":${CAPS},\"clientInfo\":{\"name\":\"smoke\",\"version\":\"0\"}}}" \
  | sed 's/^data: //' | grep -v -e '^event:' -e '^$' \
  | field 'd["result"]["protocolVersion"]')"
session="$(grep -i '^mcp-session-id:' "$headers" | tr -d '\r' | cut -d' ' -f2)"
[ -n "$session" ] || { echo "legacy endpoint issued no session id" >&2; exit 1; }
case "$legacy_version" in
  2026-*) echo "legacy endpoint negotiated ${legacy_version}; it is supposed to cap below that" >&2; exit 1 ;;
esac
echo "    negotiated ${legacy_version}, session ${session:0:8}…"

curl -sS --fail-with-body --max-time 30 -X POST "$LEGACY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H "Mcp-Session-Id: ${session}" \
  -H "Mcp-Protocol-Version: ${legacy_version}" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null

curl -sS --fail-with-body --max-time 30 -X POST "$LEGACY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H "Mcp-Session-Id: ${session}" \
  -H "Mcp-Protocol-Version: ${legacy_version}" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
  | sed 's/^data: //' | grep -v -e '^event:' -e '^$' \
  | field '"    %d tools over the legacy endpoint" % len(d["result"]["tools"])'

echo "==> OK"

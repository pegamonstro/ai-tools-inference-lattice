#!/usr/bin/env bash
# Lattice per-event feeder → Bee terminal log screen (rpi4).
#
# Tails the control-plane, frontend, and gateway telemetry JSONL files and
# pushes every routing decision / completion / execution to /run/bee/logs.sock
# as a compact human line. Long-running (Type=simple) user service. No
# rate-limiting: the goal is *every* event, not a summary.
set -uo pipefail

# bee-feed-lib.sh lives under the feeder account's home (see the .service unit);
# override BEE_FEED_LIB to point elsewhere.
source "${BEE_FEED_LIB:-$HOME/work/bee-feeders/bin/bee-feed-lib.sh}"

CONTROL_LOG="/var/log/lattice/telemetry-control.jsonl"
FRONTEND_LOG="/var/log/lattice/telemetry-frontend.jsonl"
GATEWAY_LOG="/var/log/lattice/telemetry-gateway.jsonl"

# One tail over all three files; control events carry decision_time_s, frontend
# events carry total_time_s, gateway events carry elapsed_s, so we branch on
# which key is present rather than on the source file. The gateway file is
# written by the control plane (which pulls it from the Mac over HTTP) and may
# not exist yet — tail -F waits for it.
tail -n0 -F "$CONTROL_LOG" "$FRONTEND_LOG" "$GATEWAY_LOG" 2>/dev/null | while IFS= read -r line; do
  body=$(printf '%s\n' "$line" | python3 -c '
import sys, json
try:
    d = json.loads(sys.stdin.read())
except Exception:
    sys.exit(1)
rid = d.get("request_id", "?")
if "decision_time_s" in d:
    print("route %s: %s/%s -> %s (%s)" % (
        rid, d.get("privacy", ""), d.get("latency_class", ""),
        d.get("target", ""), d.get("model", "")))
elif "total_time_s" in d:
    print("%s done: %s %.1fs" % (
        rid, d.get("target", ""), d.get("total_time_s", 0)))
elif "elapsed_s" in d:
    model = d.get("model", "")
    err = d.get("error", "")
    if err:
        print("%s %s ERROR %s" % (rid, model, err))
    else:
        print("%s %s ctx=%s %s+%stok %.1fs" % (
            rid, model, d.get("context_window", 0),
            d.get("prompt_tokens", 0), d.get("completion_tokens", 0),
            d.get("elapsed_s", 0)))
else:
    sys.exit(1)
' 2>/dev/null) || continue
  bee_emit_raw lattice "$body" || true
done

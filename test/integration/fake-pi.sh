#!/usr/bin/env bash
# Minimal stand-in for `pi --mode rpc` used by controller tests.
# Behavior:
#   - On any get_state command, replies with a canned response immediately.
#   - On set_session_name, replies with success.
#   - On `__emit_event:<json>` command, echoes the JSON to stdout as an event.
#   - On `__exit:<code>`, exits with that code.
#   - On EOF, exits 0 after a brief delay (simulating shutdown handlers).
#
# Anything else: echoes a generic success response.

# Allow tests to slow the shutdown.
SHUTDOWN_DELAY="${FAKE_PI_SHUTDOWN_DELAY:-0}"

# Predefined session info
SESSION_ID="${FAKE_PI_SESSION_ID:-fake-sid-123}"
SESSION_FILE="${FAKE_PI_SESSION_FILE:-/tmp/fake/session.jsonl}"
SESSION_NAME="${FAKE_PI_SESSION_NAME:-}"
MODEL="${FAKE_PI_MODEL:-fake/model-1}"

while IFS= read -r line; do
  case "$line" in
    '{"type":"get_state"'*)
      id=$(printf '%s' "$line" | sed -E 's/.*"id":"([^"]*)".*/\1/' 2>/dev/null)
      [ "$id" = "$line" ] && id=""
      printf '{"type":"response","command":"get_state","id":"%s","success":true,"data":{"sessionId":"%s","sessionFile":"%s","sessionName":"%s","model":{"id":"%s","provider":"fake"},"isStreaming":false,"messageCount":0,"thinkingLevel":"medium"}}\n' "$id" "$SESSION_ID" "$SESSION_FILE" "$SESSION_NAME" "$MODEL"
      ;;
    '{"type":"set_session_name"'*)
      name=$(printf '%s' "$line" | sed -E 's/.*"name":"([^"]*)".*/\1/')
      SESSION_NAME="$name"
      printf '{"type":"response","command":"set_session_name","success":true}\n'
      ;;
    __emit_event:*)
      json="${line#__emit_event:}"
      printf '%s\n' "$json"
      ;;
    __exit:*)
      code="${line#__exit:}"
      exit "$code"
      ;;
    *)
      printf '{"type":"response","success":true}\n'
      ;;
  esac
done

# Stdin closed; simulate shutdown delay then exit cleanly.
if [ "$SHUTDOWN_DELAY" -gt 0 ]; then
  sleep "$SHUTDOWN_DELAY"
fi
exit 0

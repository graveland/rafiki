#!/usr/bin/env bash
# Minimal stand-in for `pi --mode rpc` used by controller tests.
# Behavior:
#   - On any get_state command, replies with a canned response immediately.
#   - On set_session_name, replies with success.
#   - On `__emit_event:<json>` command, echoes the JSON to stdout as an event.
#   - On `__exit:<code>`, exits with that code.
#   - On `{"type":"__ctrl_test_emit",...}`, emits a test event then acks.
#   - On EOF, exits 0 after a brief delay (simulating shutdown handlers).
#
# Anything else: echoes a generic success response.
#
# Session identity:
#   By default each invocation creates a fresh session using $$ (PID) for
#   uniqueness. If --session <path> was passed on the command line the script
#   reports that path as the session file (simulating a resumed session), with
#   a derived SESSION_ID so integration tests can distinguish resumed vs fresh.

# Allow tests to slow the shutdown.
SHUTDOWN_DELAY="${FAKE_PI_SHUTDOWN_DELAY:-0}"

# Parse --session <path> from argv so integration tests can verify whether
# RespawnChild correctly omits it for new_session (fresh) vs switch_session.
SESSION_FILE_ARG=""
args=("$@")
i=0
while [ "$i" -lt "${#args[@]}" ]; do
    if [ "${args[$i]}" = "--session" ]; then
        i=$((i+1))
        SESSION_FILE_ARG="${args[$i]}"
    fi
    i=$((i+1))
done

# Session info: unique per process unless --session was passed.
if [ -n "$SESSION_FILE_ARG" ]; then
    # Resuming an existing session: report the provided session file so tests
    # can assert that the session file is unchanged when --session is passed.
    SESSION_FILE="$SESSION_FILE_ARG"
    SESSION_ID="${FAKE_PI_SESSION_ID:-fake-sid-resume-$(basename "$SESSION_FILE_ARG" .jsonl)}"
else
    # Fresh session: use $$ (PID) for uniqueness so each invocation gets
    # distinct sessionId and sessionFile values.
    SESSION_FILE="${FAKE_PI_SESSION_FILE:-/tmp/fake/session-$$.jsonl}"
    SESSION_ID="${FAKE_PI_SESSION_ID:-fake-sid-$$}"
fi

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
    '{"type":"__ctrl_test_emit"'*)
      # Emit a test event then ack. Used by integration tests to populate the
      # ring buffer and trigger per-child subscriber delivery without needing a
      # real pi agent loop.
      # Format: {"type":"__ctrl_test_emit","eventType":"<event-type>"}
      evt=$(printf '%s' "$line" | sed -E 's/.*"eventType":"([^"]+)".*/\1/')
      if [ -n "$evt" ] && [ "$evt" != "$line" ]; then
        printf '{"type":"%s"}\n' "$evt"
      fi
      printf '{"type":"response","command":"__ctrl_test_emit","success":true}\n'
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

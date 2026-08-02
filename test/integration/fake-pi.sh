#!/usr/bin/env bash
# Minimal stand-in for `pi --mode rpc` used by controller tests.
# Behavior:
#   - On any get_state command, replies with a canned response immediately.
#   - On set_session_name, replies with success.
#   - On `__emit_event:<json>` command, echoes the JSON to stdout as an event.
#   - On `__exit:<code>`, exits with that code.
#   - On `{"type":"__ctrl_test_emit",...}`, emits a test event then acks.
#   - On `{"type":"__ctrl_test_burst"}`, emits $FAKE_PI_BURST_TURNS complete
#     turns in ONE write (then exits, if $FAKE_PI_BURST_THEN_EXIT is set).
#   - On EOF, exits 0 after a brief delay (simulating shutdown handlers).
#
# Anything else: echoes a generic success response.
#
# $FAKE_PI_IGNORE_GET_STATE leaves the readiness probe unanswered, which is how a
# test reaches the daemon's "stalled" spawn outcome.
#
# Session identity:
#   By default each invocation creates a fresh session using $$ (PID) for
#   uniqueness. If --session <path> was passed on the command line the script
#   reports that path as the session file (simulating a resumed session), with
#   a derived SESSION_ID so integration tests can distinguish resumed vs fresh.

# If RAFIKI_TEST_SPAWN_LOG is set, append one line recording this invocation
# before doing anything else. Used by tests that need to count how many real
# OS processes were actually forked (e.g. a concurrent-Resume race test),
# which per-process bookkeeping in the controller can't prove on its own.
# Opt-in and off by default, so it does not affect any existing test.
if [ -n "$RAFIKI_TEST_SPAWN_LOG" ]; then
    printf 'spawn pid=%s\n' "$$" >> "$RAFIKI_TEST_SPAWN_LOG"
fi

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
      if [ -n "$FAKE_PI_IGNORE_GET_STATE" ]; then
        # Deliberately silent. The daemon's spawn path waits for
        # response.get_state to release spawning→idle, so swallowing the probe
        # is how a test reaches the "stalled" spawn outcome.
        continue
      fi
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
    '{"type":"__ctrl_test_burst"'*)
      # Emit $FAKE_PI_BURST_TURNS complete turns (agent_start → agent_settled)
      # in a SINGLE write, with nothing between them. That is the point: the
      # whole burst lands in the daemon's stdout pipe at once, so the child's
      # state machine runs the full idle→streaming→idle round trip for every
      # turn far faster than the controller's monitor goroutine can consume the
      # bus. Used by the ctrl_child_status transition-loss test; no ack, so the
      # burst is the only thing on stdout.
      turns="${FAKE_PI_BURST_TURNS:-10}"
      burst=""
      i=0
      while [ "$i" -lt "$turns" ]; do
        burst="${burst}{\"type\":\"agent_start\"}
{\"type\":\"agent_settled\"}
"
        i=$((i+1))
      done
      printf '%s' "$burst"
      # Optionally exit in the same breath, so the process death races the
      # frames it just wrote. Used to prove the daemon still delivers a
      # transition recorded immediately before exit.
      if [ -n "$FAKE_PI_BURST_THEN_EXIT" ]; then
        exit 0
      fi
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

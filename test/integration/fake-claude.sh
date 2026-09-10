#!/bin/sh
# Minimal stand-in for the claude CLI used by test/integration's MCP
# child-token tests (mcp_child_test.go).
#
# It records the argv and environment it was ACTUALLY launched with — the
# observation those tests assert on: the per-child RAFIKI_MCP_TOKEN delivered
# by environment only, the per-boot proxy bearer beside it, the
# X-Rafiki-Session attribution header, and the flags that must survive the
# spawn path into real argv. It then stays alive reading stdin and exits on
# EOF, the way an un-prompted `claude -p --input-format stream-json` sits
# silent (pkg/child/provider_claude.go: readiness is process-up, so a claude
# child that emits nothing is live, not stalled).
#
# FAKE_CLAUDE_DIR names the dump directory. The test daemon is booted with it
# set, and every child inherits it through buildEnv, so each daemon's children
# dump into that test's own directory. One file per process, named by PID.

set -u
dir="${FAKE_CLAUDE_DIR:-/tmp/fake-claude-it}"
mkdir -p "$dir" || exit 1
{
  printf '%s\n' "$@"
  printf '%s\n' '---ENV---'
  env
} > "$dir/claude-$$.dump"

# Consume and ignore everything the daemon writes to stdin. No stdout: the
# daemon's claude translator treats silence as "no events yet", never an error.
while IFS= read -r line; do
  :
done
exit 0

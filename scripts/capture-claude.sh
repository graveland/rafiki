#!/bin/sh
# Record one real, scripted Claude session through the isolated recorder.
#
# Called by `make capture-claude`; also runnable directly. Everything it needs
# comes from the environment:
#
#   OUT       record directory (default: ~/rafiki-captures/claude-<timestamp>)
#   TURNS     prompts file, one per line (default: scripts/capture-turns.txt)
#   MODEL     model for the session (default: haiku)
#   SYSPROMPT one-line system prompt for the capture
#   RAFIKID   path to the rafikid binary (default: bin/rafikid)
#   RAFIKI    path to the rafiki binary   (default: bin/rafiki)
#
# The recorder runs the session itself (--exec), so no port is read by hand and
# the recordings are complete when this script returns. The inner command is
# generated into a temp file rather than quoted inline: a system prompt and a
# turn file both contain spaces, and nested quoting through make, sh and
# --exec is exactly the kind of thing that breaks silently.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
out=${OUT:-$HOME/rafiki-captures/claude-$(date +%Y%m%d-%H%M%S)}
turns=${TURNS:-scripts/capture-turns.txt}
model=${MODEL:-haiku}
sysprompt=${SYSPROMPT:-You are a minimal capture session. Answer in one short sentence, and use a tool when asked to.}
rafikid=${RAFIKID:-$root/bin/rafikid}
rafiki=${RAFIKI:-$root/bin/rafiki}

case $turns in /*) ;; *) turns=$root/$turns ;; esac

work=$(mktemp -d)
inner=$work/session.sh
cat > "$inner" <<EOF
#!/bin/sh
cd "$work" || exit 1
# No inherited API key and no bearer: the session uses whichever Claude Code
# login this machine already has. CLAUDE_CONFIG_DIR is left alone on purpose —
# on a machine with more than one config, that variable names the one holding
# the credential.
exec env -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN "$rafiki" claude \\
  --model "$model" \\
  --system-prompt "$sysprompt" \\
  --turn-file "$turns"
EOF
chmod 0700 "$inner"
trap 'rm -rf "$work"' EXIT INT TERM

echo "capture: model=$model"
echo "capture: turns=$turns"
echo "capture: recording into $out"

status=0
"$rafikid" proxy record \
  --listen 127.0.0.1:0 \
  --upstream https://api.anthropic.com \
  --record-dir "$out" \
  --exec "$inner" || status=$?

echo "capture: exit $status, $(ls "$out" 2>/dev/null | wc -l | tr -d ' ') file(s) in $out"
exit $status

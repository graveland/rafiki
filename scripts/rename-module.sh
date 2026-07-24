#!/usr/bin/env bash
#
# rename-module.sh — rewrite the Go module path from the upstream Timescale name
# to the graveland.dev fork name.
#
# This fork of github.com/timescale/rafiki stays structurally identical to
# upstream; the ONLY intentional divergence is the module path. Keeping the
# rename as a committed, idempotent script means an upstream merge can re-apply
# it deterministically (merge upstream, run this, commit), and upstreaming a
# patch just reverses it on that diff.
#
# Portable across macOS (BSD) and Linux (GNU): uses perl for in-place edits.
# Idempotent: re-running on an already-renamed tree is a no-op.

set -euo pipefail

OLD="github.com/timescale/rafiki"
NEW="git.graveland.dev/brent/rafiki"

cd "$(dirname "$0")/.."

# 1. Module directive.
go mod edit -module "$NEW"

# 2. Self-imports across all Go sources.
files="$(grep -rl --include='*.go' "$OLD" . || true)"
if [ -n "$files" ]; then
	printf '%s\n' "$files" | xargs perl -pi -e "s{\Q$OLD\E}{$NEW}g"
fi

echo "Renamed module $OLD -> $NEW"

#!/usr/bin/env bash
#
# rename-module.sh — rewrite the Go module path from the pi-controller ancestor
# name to the fundi fork name.
#
# fundi is a fork of git.graveland.dev/brent/pi-controller and keeps its history
# and supervision tests. Supervision fixes are cherry-picked from pi-controller;
# those commits carry the old import path, so re-run this after a cherry-pick to
# re-apply the rename deterministically. Idempotent: a no-op on an already-renamed
# tree. Only Go sources and go.mod are rewritten — historical docs keep their
# original prose.
#
# Portable across macOS (BSD) and Linux (GNU): uses perl for in-place edits.

set -euo pipefail

OLD="git.graveland.dev/brent/pi-controller"
NEW="git.graveland.dev/brent/fundi"

cd "$(dirname "$0")/.."

go mod edit -module "$NEW"

files="$(grep -rl --include='*.go' "$OLD" . || true)"
if [ -n "$files" ]; then
	printf '%s\n' "$files" | xargs perl -pi -e "s{\Q$OLD\E}{$NEW}g"
fi

echo "Renamed module $OLD -> $NEW"

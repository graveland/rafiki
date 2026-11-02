#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The one command behind `make check-sdk-rust-live`: reproduce the Rust SDK's
# live-daemon tests from a clean checkout, with no tooling beyond cargo, Go and
# a migrated database.
#
# What it starts, all of it scratch and all of it torn down on every exit path:
#
#   1. `rafikid` and `rafiki`, built from THIS tree (never a `$PATH` binary).
#   2. A daemon with an isolated HOME/XDG tree and an operator `[mcp_oauth]`
#      allowance for loopback + cleartext, so a login can reach the fixture.
#   3. test/livefixture: a fake authorization server and a real MCP SDK server
#      on loopback. No vendor account, no network traffic to a real provider.
#
# It then runs `cargo test --locked --all-features --test daemon -- --ignored`.
#
# The three MCP OAuth tests are the point of the run and must pass. The four
# pre-existing live tests that need a daemon HOLDING CHILD HISTORY (a spawn, a
# durable event, a child to replay) fail here by design and are reported as
# expected; they are not this feature's tests. Every other ignored test must
# pass. `RAFIKI_SDK_LIVE_STRICT=1` demands a fully green run instead, which only
# a daemon with child history could give.
#
# Requirements, each failing loudly rather than skipping:
#   - cargo on PATH (the Rust crate and its lockfile are the thing under test)
#   - Go (cmd/) to build the daemon, the CLI and the fixture
#   - a migrated database: RAFIKI_TEST_DSN, or RAFIKI_SDK_LIVE_DSN to override.
#     The repository's disposable one is the `pg-test` container; this script
#     migrates it (`rafikid migrate`) before anything starts.
#
# Rows it writes (one user, one MCP grant) land in that disposable database and
# are keyed by a per-run user name and a per-run fixture port, so a second run
# neither collides with nor observes the first.

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

die() {
	printf '\nFAILED: %s\n' "$*" >&2
	exit 1
}

say() { printf '%s\n' "$*"; }

# ─── prerequisites ──────────────────────────────────────────────────────────

go_bin=$(command -v go || true)
if [ -z "$go_bin" ] && [ -x /usr/local/go/bin/go ]; then
	go_bin=/usr/local/go/bin/go
fi
[ -n "$go_bin" ] || die "go is not on PATH and /usr/local/go/bin/go does not exist.
This script builds rafikid, rafiki and the fixture from this tree."

command -v cargo >/dev/null 2>&1 || die "cargo is not on PATH.
The live tests ARE the Rust SDK's own test binary: install the Rust toolchain
(https://rustup.rs) and re-run."

command -v base64 >/dev/null 2>&1 || die "base64 is not on PATH (it mints the run's RAFIKI_SECRETS_KEY)."

# Source the checkout's .env the way `make test` does: that is where the
# disposable DSN lives, and requiring the caller to remember it is how the run
# ends up pointed at nothing. Everything else it exports is neutralised below.
if [ -f "$root/.env" ]; then
	set -a
	# shellcheck disable=SC1091 # the checkout's own env file.
	. "$root/.env"
	set +a
fi

dsn=${RAFIKI_SDK_LIVE_DSN:-${RAFIKI_TEST_DSN:-}}
if [ -z "$dsn" ]; then
	die "no database to run against.
Set RAFIKI_TEST_DSN (or RAFIKI_SDK_LIVE_DSN to override it). The live daemon
needs a migrated database, and the repository's disposable one is the pg-test
container:

  docker start pg-test
  set -a; . .env; set +a
  make check-sdk-rust-live"
fi

# ─── isolated scratch tree ──────────────────────────────────────────────────

# Short paths matter: the control socket is a unix socket and sun_path has
# ~104 bytes to spend. /tmp is the shortest writable base on macOS, where
# $TMPDIR is /var/folders/… and the socket would not fit.
if [ -n "${RAFIKI_SDK_LIVE_TMPDIR:-}" ]; then
	scratch_base=$RAFIKI_SDK_LIVE_TMPDIR
elif [ -d /tmp ] && [ -w /tmp ]; then
	scratch_base=/tmp
else
	scratch_base=${TMPDIR:-/tmp}
fi
mkdir -p "$scratch_base"
tmp=$(mktemp -d "$scratch_base/rafiki-sdk-live.XXXXXX")

# Pin Go's caches BEFORE HOME moves. They are content-addressed and safe to
# share; letting them follow HOME would re-download the module graph into the
# scratch tree and leave a 0555 read-only module cache there that no `rm -rf`
# can remove.
go_modcache=$("$go_bin" env GOMODCACHE)
go_cache=$("$go_bin" env GOCACHE)
[ -n "$go_modcache" ] && [ -n "$go_cache" ] || die "go env GOMODCACHE/GOCACHE is empty"
export GOMODCACHE="$go_modcache" GOCACHE="$go_cache"

# Rust's caches follow HOME as well, and there the cost is worse than a slow
# build: rustup finds no toolchain under the scratch HOME and refuses to run
# cargo at all ("no default toolchain is configured"). Pin both when the
# developer has them; a system cargo needs neither.
original_home=$HOME
if [ -z "${RUSTUP_HOME:-}" ] && [ -d "$original_home/.rustup" ]; then
	export RUSTUP_HOME="$original_home/.rustup"
fi
if [ -z "${CARGO_HOME:-}" ] && [ -d "$original_home/.cargo" ]; then
	export CARGO_HOME="$original_home/.cargo"
fi

export HOME="$tmp/home"
export XDG_CONFIG_HOME="$tmp/config"
export XDG_STATE_HOME="$tmp/state"
export XDG_DATA_HOME="$tmp/data"
export XDG_RUNTIME_DIR="$tmp/run"
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_DATA_HOME" "$XDG_RUNTIME_DIR"

# A developer's shell must not redirect the run. The first six are hard errors
# on the client (profile.CheckRetiredEnv) and the profile pointer would name
# their own daemon; RAFIKI_DB would point the scratch daemon at real
# conversations, so it is set to the disposable DSN below and never inherited.
unset RAFIKI_URL RAFIKI_TOKEN RAFIKI_SOCKET RAFIKI_PROFILE \
	RAFIKI_DEFAULT_MODEL RAFIKI_DEFAULT_PRESET RAFIKI_DEFAULT_LABELS 2>/dev/null || true
# No real provider credentials either: a key wires the live provider directory
# (and a pricing fetch) into a daemon this run only asks about MCP OAuth. The
# integration harness blanks the same three.
unset ANTHROPIC_API_KEY OPENROUTER_API_KEY ANTHROPIC_BASE_URL 2>/dev/null || true
export RAFIKI_DB="$dsn"

sock="$XDG_RUNTIME_DIR/rafiki/controller.sock"
if [ "${#sock}" -gt 100 ]; then
	chmod -R u+w "$tmp" 2>/dev/null || true
	rm -rf "$tmp" 2>/dev/null || true
	die "the control socket path is too long for a unix socket (${#sock} bytes): $sock
Set RAFIKI_SDK_LIVE_TMPDIR to a shorter directory."
fi

daemon_id="sdk-live-$$"
user="sdk-live-$(date +%Y%m%d%H%M%S)-$$"
secrets_key=$(head -c 32 /dev/urandom | base64 | tr -d '\n')

daemon_pid=""
fixture_pid=""

stop_pid() {
	local pid=$1
	[ -n "$pid" ] || return 0
	kill -TERM "$pid" 2>/dev/null || true
	local i
	for i in $(seq 1 100); do
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.05
	done
	kill -KILL "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
}

cleanup() {
	local rc=$?
	trap - EXIT INT TERM
	# Cleanup must never decide the exit status, and the two children are
	# killed best-effort; -e would abort it halfway.
	set +e

	# A failing run is only diagnosable with the two logs, and they are about
	# to be deleted with the scratch tree.
	if [ "$rc" -ne 0 ]; then
		if [ -f "$tmp/cargo.log" ]; then
			say "--- cargo test output (tail) ---" >&2
			tail -n 40 "$tmp/cargo.log" >&2 || true
		fi
		if [ -f "$tmp/daemon.log" ]; then
			say "--- rafikid log (tail) ---" >&2
			tail -n 30 "$tmp/daemon.log" >&2 || true
		fi
		if [ -f "$tmp/fixture.log" ]; then
			say "--- fixture log (tail) ---" >&2
			tail -n 30 "$tmp/fixture.log" >&2 || true
		fi
	fi

	stop_pid "$daemon_pid"
	stop_pid "$fixture_pid"

	if [ -n "${RAFIKI_SDK_LIVE_KEEP:-}" ]; then
		say "kept the scratch tree: $tmp"
	else
		# A Go module cache inside the tree would be read-only; the chmod is
		# what makes the removal unconditional.
		chmod -R u+w "$tmp" 2>/dev/null || true
		rm -rf "$tmp" 2>/dev/null || true
	fi
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ─── build ─────────────────────────────────────────────────────────────────

say "scratch tree: $tmp"
say "database:     $dsn"
say "building rafikid, rafiki and the MCP OAuth fixture from $root"

mkdir -p "$tmp/bin"
(
	cd "$root"
	"$go_bin" build -o "$tmp/bin/rafikid" ./cmd/rafikid &&
		"$go_bin" build -o "$tmp/bin/rafiki" ./cmd/rafiki &&
		"$go_bin" build -o "$tmp/bin/livefixture" ./test/livefixture
) >"$tmp/build.log" 2>&1 || die "go build failed:
$(cat "$tmp/build.log")"

# The daemon does NOT migrate on startup: a fresh database comes up degraded
# (relation "conversations.child" does not exist) and every verb fails.
say "migrating the schema"
"$tmp/bin/rafikid" migrate --db "$dsn" >"$tmp/migrate.log" 2>&1 ||
	die "rafikid migrate failed against $dsn:
$(cat "$tmp/migrate.log")"

say "minting a user token"
created=$("$tmp/bin/rafikid" user create "$user" --token --db "$dsn" 2>"$tmp/user.log") ||
	die "rafikid user create failed:
$(cat "$tmp/user.log")"
token=$(printf '%s\n' "$created" | sed -n 's/^token: //p' | tail -n 1)
[ -n "$token" ] || die "rafikid user create printed no token: $created"

# ─── the fixture ───────────────────────────────────────────────────────────

say "starting the MCP OAuth fixture"
"$tmp/bin/livefixture" -dir "$tmp/fixture" >"$tmp/fixture.env" 2>"$tmp/fixture.log" &
fixture_pid=$!

# The fixture prints its exports only after it is listening and its config
# files are written, so that block IS the ready signal.
ready=""
for _ in $(seq 1 400); do
	if grep -q '^export RAFIKI_SDK_TEST_MCP_CONFIG=' "$tmp/fixture.env" 2>/dev/null; then
		ready=1
		break
	fi
	kill -0 "$fixture_pid" 2>/dev/null || break
	sleep 0.05
done
if [ -z "$ready" ]; then
	die "the MCP OAuth fixture never became ready:
$(cat "$tmp/fixture.log")"
fi
# shellcheck disable=SC1090 # the fixture wrote these lines; they are its output.
eval "$(cat "$tmp/fixture.env")"
for v in RAFIKI_SDK_TEST_MCP_CONFIG RAFIKI_SDK_TEST_MCP_SERVER \
	RAFIKI_SDK_TEST_MCP_RESOURCE RAFIKI_LIVEFIXTURE_RAFIKID_TOML; do
	[ -n "${!v:-}" ] || die "the fixture printed no $v:
$(cat "$tmp/fixture.env")"
done

# ─── the daemon ────────────────────────────────────────────────────────────

# The [mcp_oauth] snippet is the fixture's; everything else about the daemon is
# this script's. rafikid refuses an unknown key, so the file is exactly the
# snippet.
mkdir -p "$XDG_CONFIG_HOME/rafiki"
cp "$RAFIKI_LIVEFIXTURE_RAFIKID_TOML" "$XDG_CONFIG_HOME/rafiki/rafikid.toml"

# The client reaches the scratch daemon by PROFILE, never by an environment
# variable: RAFIKI_URL/RAFIKI_TOKEN/RAFIKI_SOCKET are retired and refused.
profile=sdk-live
printf '[profile.%s]\nsocket = "%s"\n' "$profile" "$sock" >"$XDG_CONFIG_HOME/rafiki/profiles.toml"
printf '%s\n' "$profile" >"$XDG_CONFIG_HOME/rafiki/current-profile"
mkdir -p "$XDG_CONFIG_HOME/rafiki/profiles/$profile"
(umask 077 && printf '%s\n' "$token" >"$XDG_CONFIG_HOME/rafiki/profiles/$profile/token")

say "starting rafikid ($daemon_id)"
RAFIKI_DB="$dsn" \
	RAFIKI_SECRETS_KEY="$secrets_key" \
	RAFIKI_EXECUTORS_ENABLED=0 \
	RAFIKI_PROXY_LISTEN=127.0.0.1:0 \
	RAFIKI_DAEMON_ID="$daemon_id" \
	"$tmp/bin/rafikid" >"$tmp/daemon.log" 2>&1 &
daemon_pid=$!

# Wait for the control plane, not just the socket file: `rafiki token list`
# is one authenticated Connect round trip, so it proves the listener is
# serving, the token resolves and the database is reachable — the three things
# every live test needs before it is worth running any of them.
say "waiting for $sock"
deadline=$(( $(date +%s) + 60 ))
while :; do
	if "$tmp/bin/rafiki" token list -J >/dev/null 2>"$tmp/readiness.log"; then
		break
	fi
	if ! kill -0 "$daemon_pid" 2>/dev/null; then
		wait "$daemon_pid" 2>/dev/null || true
		daemon_pid=""
		die "rafikid exited during startup:
$(tail -n 40 "$tmp/daemon.log")"
	fi
	if [ "$(date +%s)" -ge "$deadline" ]; then
		die "rafikid never answered on $sock within 60s.
last client error:
$(cat "$tmp/readiness.log")
daemon log:
$(tail -n 40 "$tmp/daemon.log")"
	fi
	sleep 0.25
done

# ─── the run ───────────────────────────────────────────────────────────────

# The three tests this driver exists for, and the four pre-existing live tests
# that need a daemon HOLDING CHILD HISTORY. The latter need a spawned child with
# durable events to replay and page; this daemon has never spawned anything, and
# no MCP OAuth test spawns, so they fail here by design. They are named so the
# script can tell "expected" from "regression" instead of hiding either.
mcp_tests="a_live_daemon_starts_and_completes_an_mcp_login
a_live_daemon_logs_out_an_mcp_grant
a_live_daemon_refuses_an_mcp_login_for_an_undeclared_server"

history_tests="a_live_stream_idles_and_resumes_from_its_cursor
a_live_follower_reopens_across_idle_windows_without_repeats
a_live_child_follower_never_replays_history
a_live_history_pages_backwards_without_gaps_or_repeats"

say "running the Rust SDK's live-daemon tests"
# The four history tests are EXPECTED to fail, so cargo's exit code is a signal
# to classify, not to abort: pipefail plus -e would kill the run before the
# classification below could tell an expected failure from a regression.
set +e
(
	cd "$root/sdk/rust"
	RAFIKI_SDK_TEST_URL="unix://$sock" \
		RAFIKI_SDK_TEST_TOKEN="$token" \
		RAFIKI_SDK_TEST_MCP_CONFIG="$RAFIKI_SDK_TEST_MCP_CONFIG" \
		RAFIKI_SDK_TEST_MCP_SERVER="$RAFIKI_SDK_TEST_MCP_SERVER" \
		RAFIKI_SDK_TEST_MCP_RESOURCE="$RAFIKI_SDK_TEST_MCP_RESOURCE" \
		cargo test --locked --all-features --test daemon -- --ignored
) 2>&1 | tee "$tmp/cargo.log"
cargo_rc=${PIPESTATUS[0]}
set -e

passed=$(sed -n 's/^test \([A-Za-z0-9_]*\) \.\.\. ok$/\1/p' "$tmp/cargo.log" | sort -u)
failed=$(sed -n 's/^test \([A-Za-z0-9_]*\) \.\.\. FAILED$/\1/p' "$tmp/cargo.log" | sort -u)
contains() { printf '%s\n' "$1" | grep -qx "$2"; }

missing=""
for t in $mcp_tests; do
	if ! contains "$passed" "$t"; then
		missing="$missing
  $t"
	fi
done
if [ -n "$missing" ]; then
	die "the MCP OAuth live tests did not pass:$missing
This run is the one that must reproduce them; see the cargo output above."
fi

unexpected=""
for t in $failed; do
	contains "$history_tests" "$t" || unexpected="$unexpected
  $t"
done
if [ -n "$unexpected" ]; then
	die "tests outside the documented set failed:$unexpected
Only the four child-history live tests may fail in this run."
fi

if [ -n "${RAFIKI_SDK_LIVE_STRICT:-}" ] && [ "$cargo_rc" -ne 0 ]; then
	die "RAFIKI_SDK_LIVE_STRICT is set and the run was not green (exit $cargo_rc)."
fi

# Leave the shared database as tidy as it can be: the grant this run logged in
# belongs to this run alone (its resource carries the fixture's port), and the
# logout verb tombstones it without needing a config file. A failure here is
# not a failure of the run — the database is disposable — so it is silent and
# best-effort.
if [ -n "$daemon_pid" ] && kill -0 "$daemon_pid" 2>/dev/null; then
	"$tmp/bin/rafiki" mcp logout --resource "$RAFIKI_SDK_TEST_MCP_RESOURCE" >/dev/null 2>&1 || true
fi

say ""
say "== MCP OAuth live tests (the point of this run) =="
for t in $mcp_tests; do say "PASS  $t"; done
say ""
say "== pre-existing live tests that need a daemon holding child history =="
if [ -n "$failed" ]; then
	for t in $failed; do say "FAIL  $t   (expected: this daemon has spawned no child)"; done
else
	say "(all four passed: this database supplied a child with durable events)"
fi
say ""
say "reproduced: daemon-brokered MCP OAuth login, logout and refusal, against a"
say "loopback fixture. No vendor account was involved."

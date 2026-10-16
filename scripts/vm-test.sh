#!/usr/bin/env bash
# vm-test.sh — install a released rafiki into a fresh OrbStack Debian machine
# and run the canned installation test. A developer tool: it assumes OrbStack
# (orb/orbctl on PATH), a git checkout, and this repo's .env.
#
#   scripts/vm-test.sh [--keep] [VERSION]
#
# VERSION defaults to the newest v* tag. The machine is rafiki-vmtest-<version>;
# an existing one is deleted first. It is deleted again on success unless
# --keep; on failure it is always kept for inspection (orb -m <name>).
#
# Read from .env (single lines, never sourced — .env holds other secrets):
#   ANTHROPIC_API_KEY, OPENROUTER_API_KEY  forwarded into the VM's service.env
#   VMTEST_MODEL                           model for `rafiki doctor --agent fundi:…`
#     (default: openrouter/~deepseek/deepseek-flash-latest with an OpenRouter
#     key, else anthropic/haiku-latest)
# With no key, doctor is skipped: the VM's Claude login is fake.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
keep=0
version=""
for arg in "$@"; do
	case "$arg" in
	--keep) keep=1 ;;
	-h | --help)
		sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) version=$arg ;;
	esac
done
[ -n "$version" ] || version=$(git -C "$repo" tag --list 'v*' --sort=-v:refname | head -1)
[ -n "$version" ] || { echo "no v* tag found; pass a version" >&2; exit 1; }

vm="rafiki-vmtest-${version//./-}"
logdir="${TMPDIR:-/tmp}/rafiki-vmtest/$version"
mkdir -p "$logdir"

envval() { # envval NAME — the last NAME= line of .env, unquoted; empty if absent
	[ -f "$repo/.env" ] || return 0
	sed -nE "s/^(export[[:space:]]+)?$1=(.*)$/\2/p" "$repo/.env" | tail -1 | sed -E "s/^[\"'](.*)[\"']$/\1/"
}

# run CMD in the VM as the default user with ~/.local/bin on PATH; a fresh
# login each time, which is what picks up a new docker group membership.
vmrun() { orb -m "$vm" bash -lc "export PATH=\$HOME/.local/bin:\$PATH; $1"; }

stage() { printf '\n==> %s\n' "$*"; }
pass() { printf 'PASS  %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*"
	echo "      logs: $logdir   machine kept: orb -m $vm"
	exit 1
}

stage "release $version"
curl -fsS -o /dev/null "https://api.github.com/repos/graveland/rafiki/releases/tags/$version" ||
	fail "release $version is not published on GitHub yet"
pass "release $version exists"

stage "machine $vm"
if orbctl list -q 2>/dev/null | grep -qx "$vm"; then
	orbctl delete -f "$vm" >/dev/null
fi
orbctl create debian:bookworm "$vm" >"$logdir/create.log" 2>&1 || fail "orbctl create (see create.log)"
vmrun 'sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates >/dev/null' \
	>"$logdir/apt.log" 2>&1 || fail "apt prep (see apt.log)"
pass "fresh debian:bookworm machine"

stage "credentials"
# A fake Claude login: setup's prerequisite check only reads this file.
vmrun 'mkdir -p ~/.claude && umask 077 && cat > ~/.claude/.credentials.json' <<'EOF'
{"claudeAiOauth":{"accessToken":"sk-ant-oat01-vmtest-fake-access-token","refreshToken":"sk-ant-ort01-vmtest-fake-refresh-token","expiresAt":9999999999999,"scopes":["user:inference","user:profile"],"subscriptionType":"max"}}
EOF
anthropic_key=$(envval ANTHROPIC_API_KEY)
openrouter_key=$(envval OPENROUTER_API_KEY)
keys=""
[ -n "$anthropic_key" ] && keys+="ANTHROPIC_API_KEY=\"$anthropic_key\""$'\n'
[ -n "$openrouter_key" ] && keys+="OPENROUTER_API_KEY=\"$openrouter_key\""$'\n'
if [ -n "$keys" ]; then
	# Over stdin, never argv: the documented "pre-write service.env" path.
	printf '%s' "$keys" | vmrun 'mkdir -p ~/.config/rafiki && umask 077 && cat >> ~/.config/rafiki/service.env'
	pass "fake Claude login; forwarded $(printf '%s' "$keys" | cut -d= -f1 | paste -sd, -)"
else
	pass "fake Claude login; no provider keys in .env"
fi

stage "install.sh ($version)"
git -C "$repo" show "$version:scripts/install.sh" |
	vmrun "RAFIKI_VERSION=$version RAFIKI_NO_SETUP=1 sh" >"$logdir/install.log" 2>&1 ||
	fail "install.sh (see install.log)"
got=$(vmrun 'rafiki --version' 2>&1) || fail "rafiki --version: $got"
case "$got" in *"${version#v}"*) pass "installed: $got" ;; *) fail "installed version: $got" ;; esac

stage "rafiki setup --non-interactive"
# The first pass installs docker and adds the user to the docker group, then
# stops for a re-login; each vmrun is a fresh login.
ok=0
for attempt in 1 2 3; do
	if vmrun 'rafiki setup --non-interactive' >"$logdir/setup-$attempt.log" 2>&1; then
		ok=1
		break
	fi
	grep -q 'log out and back in' "$logdir/setup-$attempt.log" ||
		fail "setup attempt $attempt (see setup-$attempt.log)"
	echo "      attempt $attempt stopped for the docker group re-login; retrying"
done
[ "$ok" = 1 ] || fail "setup never completed (see setup-*.log)"
pass "setup completed (attempt $attempt)"

stage "re-run is a no-op"
# shellcheck disable=SC2016 # expands inside the VM
snapshot='systemctl --user show rafiki -p MainPID --value; for f in ~/.config/rafiki/providers.toml ~/.config/rafiki/service.env; do [ -f "$f" ] && sha256sum "$f" || echo "absent $f"; done'
before=$(vmrun "$snapshot")
vmrun 'rafiki setup --non-interactive' >"$logdir/setup-rerun.log" 2>&1 || fail "re-run (see setup-rerun.log)"
after=$(vmrun "$snapshot")
[ "$before" = "$after" ] || fail "re-run changed state:"$'\n'"before: $before"$'\n'"after:  $after"
pass "re-run wrote nothing and did not restart rafikid"

stage "rafiki doctor"
if [ -z "$keys" ]; then
	echo "skip  no ANTHROPIC_API_KEY/OPENROUTER_API_KEY in .env"
elif ! vmrun 'rafiki doctor --help' 2>/dev/null | grep -q -- '--agent'; then
	echo "skip  $version's doctor has no --agent"
else
	model=$(envval VMTEST_MODEL)
	if [ -z "$model" ]; then
		if [ -n "$openrouter_key" ]; then model=openrouter/~deepseek/deepseek-flash-latest; else model=anthropic/haiku-latest; fi
	fi
	vmrun "rafiki doctor --agent fundi:$model" 2>&1 | tee "$logdir/doctor.log" || fail "doctor (see doctor.log)"
	pass "doctor --agent fundi:$model"
fi

stage "done"
if [ "$keep" = 1 ]; then
	echo "machine kept: orb -m $vm"
else
	orbctl delete -f "$vm" >/dev/null
	echo "machine deleted"
fi
echo "PASS  $version   logs: $logdir"

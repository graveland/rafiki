#!/bin/sh
# Install the rafiki CLI and daemon from GitHub releases.
#
#   curl -fsSL https://github.com/graveland/rafiki/releases/latest/download/install.sh | sh
#
# Env overrides:
#   RAFIKI_INSTALL_DIR     install prefix            (default: $HOME/.local/bin)
#   RAFIKI_VERSION         release tag to install    (default: latest)
#   RAFIKI_DOWNLOAD_BASE   release download base URL (default: GitHub; requires
#                          RAFIKI_VERSION — lets a test serve a local snapshot)
#   RAFIKI_NO_SETUP=1      do not run `rafiki setup` at the end
set -eu

REPO=graveland/rafiki
PREFIX="${RAFIKI_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${RAFIKI_VERSION:-latest}"

if [ -n "${RAFIKI_DOWNLOAD_BASE:-}" ] && [ "$VERSION" = "latest" ]; then
	echo "error: RAFIKI_DOWNLOAD_BASE requires RAFIKI_VERSION to be set" >&2
	exit 1
fi

BASE="${RAFIKI_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download}"

case "$(uname -s)" in
	Darwin) OS=darwin ;;
	Linux) OS=linux ;;
	*)
		echo "error: unsupported OS: $(uname -s)" >&2
		exit 1
		;;
esac

case "$(uname -m)" in
	x86_64 | amd64) ARCH=amd64 ;;
	arm64 | aarch64) ARCH=arm64 ;;
	*)
		echo "error: unsupported architecture: $(uname -m)" >&2
		exit 1
		;;
esac

TAG="$VERSION"
if [ "$TAG" = "latest" ]; then
	# Resolve the latest tag from the redirect, never the rate-limited API.
	TAG="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")"
	TAG="${TAG##*/}"
fi
# Archive name_template is `rafiki_{{ .Version }}_{{ .Os }}_{{ .Arch }}` and
# .Version carries no leading `v`.
VER="${TAG#v}"
NAME="rafiki_${VER}_${OS}_${ARCH}.tar.gz"

echo "Installing rafiki $TAG ($OS/$ARCH) into $PREFIX"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "$tmp/$NAME" "$BASE/$TAG/$NAME"
curl -fsSL -o "$tmp/checksums.txt" "$BASE/$TAG/checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
	(cd "$tmp" && sha256sum -c --ignore-missing "$tmp/checksums.txt")
elif command -v shasum >/dev/null 2>&1; then
	(cd "$tmp" && shasum -a 256 -c --ignore-missing "$tmp/checksums.txt")
else
	echo "error: no sha256 checksum tool found (need sha256sum or shasum)" >&2
	exit 1
fi

tar -xzf "$tmp/$NAME" -C "$tmp"

mkdir -p "$PREFIX"
install -m 0755 "$tmp/rafiki" "$PREFIX/rafiki"
install -m 0755 "$tmp/rafikid" "$PREFIX/rafikid"

resolved="$(command -v rafiki 2>/dev/null || true)"
if [ -n "$resolved" ] && [ "$resolved" != "$PREFIX/rafiki" ]; then
	echo "warning: 'rafiki' on PATH resolves to $resolved, not $PREFIX/rafiki" >&2
fi

case ":$PATH:" in
	*":$PREFIX:"*) ;;
	*) echo "Add $PREFIX to your PATH: export PATH=\"$PREFIX:\$PATH\"" ;;
esac

if [ "$OS" = "linux" ] && ! command -v rg >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
	echo "Installing ripgrep: sudo apt-get install -y ripgrep"
	sudo apt-get install -y ripgrep
fi

# The EXIT trap does not fire across `exec`, so drop the temp dir now.
rm -rf "$tmp"

if [ "${RAFIKI_NO_SETUP:-}" != "1" ]; then
	if [ -t 0 ]; then
		exec "$PREFIX/rafiki" setup
	elif (: </dev/tty) 2>/dev/null; then
		# curl | sh consumed stdin, so reattach the controlling terminal.
		exec "$PREFIX/rafiki" setup </dev/tty
	fi
fi

echo "run: rafiki setup"

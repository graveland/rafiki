#!/bin/bash
# Builds postgres-darwin-universal.txz: EnterpriseDB's universal macOS PostgreSQL,
# plus the extensions in versions.env and the client tools, repacked into the
# layout pkg/embedpg extracts (bin/, lib/, share/postgresql).
#
# usage: build-darwin.sh <output-dir> [work-dir]
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build-darwin.sh <output-dir> [work-dir]}
work=${2:-$(mktemp -d)}
mkdir -p "$out" "$work"
out=$(cd "$out" && pwd)
work=$(cd "$work" && pwd)

# shellcheck source=/dev/null
. "$here/versions.env"

sha256() { shasum -a 256 "$1" | awk '{print $1}'; }

zip="$work/edb.zip"
if [ ! -f "$zip" ]; then
  curl -fsSL -o "$zip" "$EDB_OSX_ZIP_URL"
fi
got=$(sha256 "$zip")
if [ "$got" != "$EDB_OSX_ZIP_SHA256" ]; then
  echo "sha256 mismatch for $EDB_OSX_ZIP_URL: want $EDB_OSX_ZIP_SHA256, got $got" >&2
  exit 1
fi

tree="$work/edb"
rm -rf "$tree"
mkdir -p "$tree"
unzip -q -d "$tree" "$zip"
pg="$tree/pgsql"
pg_config="$pg/bin/pg_config"

# The pgxs Makefile.global names the SDK of EDB's CI runner, which is not ours.
sdk=$(xcrun --show-sdk-path)

# build_ext <name> <tag> <commit> [extra make args...]
build_ext() {
  local name=$1 tag=$2 commit=$3
  shift 3
  local src="$work/src-$name"
  rm -rf "$src"
  git -c advice.detachedHead=false clone -q --branch "$tag" "https://github.com/$EXT_REPO_PREFIX/$name.git" "$src"
  local head
  head=$(git -C "$src" rev-parse HEAD)
  if [ "$head" != "$commit" ]; then
    echo "$name $tag is $head, pinned $commit" >&2
    exit 1
  fi
  make -C "$src" PG_CONFIG="$pg_config" PG_SYSROOT="$sdk" "$@" >"$work/$name-build.log" 2>&1 ||
    { tail -30 "$work/$name-build.log" >&2; exit 1; }
  make -C "$src" install PG_CONFIG="$pg_config" PG_SYSROOT="$sdk" "$@" >"$work/$name-install.log" 2>&1 ||
    { tail -30 "$work/$name-install.log" >&2; exit 1; }
}

# OPTFLAGS="" because pgvector's default is -march=native: the build host's CPU.
EXT_REPO_PREFIX=pgvector build_ext pgvector "$PGVECTOR_TAG" "$PGVECTOR_COMMIT" OPTFLAGS=
EXT_REPO_PREFIX=timescale build_ext pg_textsearch "$PG_TEXTSEARCH_TAG" "$PG_TEXTSEARCH_COMMIT"

# The modules are signed by the linker, but a module we installed is what the
# server dlopens, so sign it explicitly and fail the build if it does not verify.
for mod in "$pg"/lib/postgresql/vector.dylib "$pg"/lib/postgresql/pg_textsearch.dylib; do
  codesign --force -s - "$mod"
  codesign --verify "$mod"
done

cd "$pg"

# libncurses is only present in some EDB builds, so every optional library is
# listed through a guard rather than named.
libs=()
for pat in 'lib/libz*.dylib' 'lib/libpq*.dylib' 'lib/libuuid*.dylib' 'lib/libxml2*.dylib' \
  'lib/libssl*.dylib' 'lib/libcrypto*.dylib' 'lib/libedit*.dylib' 'lib/libicu*.dylib' \
  'lib/liblz*.dylib' 'lib/libzstd*.dylib' 'lib/libiconv*.dylib' 'lib/libgssapi_krb5.*.dylib' \
  'lib/libkrb5.*.dylib' 'lib/libkrb5support.*.dylib' 'lib/libk5crypto.*.dylib' \
  'lib/libcom_err.*.dylib' 'lib/libintl.*.dylib' 'lib/libncurses*.dylib'; do
  for f in $pat; do [ -e "$f" ] && libs+=("$f"); done
done

# COPYFILE_DISABLE stops bsdtar writing AppleDouble "._name" entries for files that
# carry extended attributes. They are not harmless: PostgreSQL treats every
# *.control file in share/postgresql/extension as an extension, so a
# "._pgcrypto.control" makes pg_available_extensions fail to parse.
COPYFILE_DISABLE=1 tar -cJf "$out/postgres-darwin-universal.txz" \
  share/postgresql \
  "${libs[@]}" \
  lib/postgresql/*.* \
  bin/initdb bin/pg_ctl bin/postgres bin/psql bin/pg_dump bin/pg_restore

# bsdtar hides "._" entries when listing, so tar -t cannot be the check; the
# stdlib reader sees what ExtractTar will extract.
python3 -I - "$out/postgres-darwin-universal.txz" <<'PY'
import sys
import tarfile

bad = [m.name for m in tarfile.open(sys.argv[1], "r:xz") if m.name.startswith("._") or "/._" in m.name]
if bad:
    sys.exit("%s contains %d AppleDouble (._*) entries, e.g. %s" % (sys.argv[1], len(bad), bad[:3]))
PY

echo "wrote $out/postgres-darwin-universal.txz ($(du -h "$out/postgres-darwin-universal.txz" | awk '{print $1}'))"

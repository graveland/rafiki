#!/bin/bash
# Runs inside Dockerfile.linux: turns an install prefix into the artifact. Keeps
# only what pkg/embedpg runs (six binaries, the server's lib/ and share/), copies in
# every shared library those binaries need except glibc's own, points every rpath
# inside the tree, and refuses to produce a tarball that still reaches outside it.
#
# usage: bundle-linux.sh <prefix> <out-dir> <artifact-name> <glibc-max>
set -euo pipefail

prefix=${1:?usage: bundle-linux.sh <prefix> <out-dir> <artifact-name> <glibc-max>}
out=${2:?}
name=${3:?}
glibc_max=${4:?}

# The glibc family is the one thing the host must provide.
host_libs='^(linux-vdso\.so|ld-linux[^ ]*|libc\.so\.6|libm\.so\.6|libdl\.so\.2|libpthread\.so\.0|librt\.so\.1|libutil\.so\.1|libresolv\.so\.2)'

cd "$prefix"

# Only what the server runs: the six binaries, and no headers, static libraries,
# pgxs, docs or pkgconfig.
keep=" initdb pg_ctl postgres psql pg_dump pg_restore "
for b in bin/*; do
  case "$keep" in *" $(basename "$b") "*) ;; *) rm -f "$b" ;; esac
done
rm -rf ./include ./lib/pkgconfig ./lib/postgresql/pgxs ./share/doc ./share/man
find lib -name '*.a' -delete

elfs() {
  find bin lib -type f | while read -r f; do
    if [ "$(head -c4 "$f" | tail -c3)" = "ELF" ]; then echo "$f"; fi
  done
}

# shellcheck disable=SC2046
strip --strip-unneeded $(elfs) 2>/dev/null || true

# Pull in dependencies until the closure stops growing (a bundled library has
# dependencies of its own).
for _ in 1 2 3 4 5 6; do
  added=0
  for f in $(elfs); do
    while read -r lib path; do
      [ -n "$path" ] || continue
      case "$path" in "$prefix"/*) continue ;; esac
      if echo "$lib" | grep -Eq "$host_libs"; then continue; fi
      if [ ! -e "lib/$lib" ]; then
        cp -L "$path" "lib/$lib"
        added=1
      fi
    done < <(LD_LIBRARY_PATH="$prefix/lib" ldd "$f" | awk '/=> \//{print $1, $3}')
  done
  [ "$added" = 1 ] || break
done

# patchelf takes a literal $ORIGIN, which the dynamic loader expands, not the shell.
# shellcheck disable=SC2016
for f in bin/*; do patchelf --set-rpath '$ORIGIN/../lib' "$f"; done
# shellcheck disable=SC2016
for f in lib/*.so*; do [ -L "$f" ] || patchelf --set-rpath '$ORIGIN' "$f"; done
# shellcheck disable=SC2016
for f in lib/postgresql/*.so; do patchelf --set-rpath '$ORIGIN/..' "$f"; done

# Closure check: nothing resolves outside the tree except the glibc family.
bad=0
for f in $(elfs); do
  while read -r lib path; do
    case "$path" in "$prefix"/*) continue ;; esac
    if echo "$lib" | grep -Eq "$host_libs"; then continue; fi
    echo "bundle: $f needs $lib from ${path:-nowhere}" >&2
    bad=1
  done < <(env -i ldd "$f" | awk '{ if ($0 ~ /not found/) print $1, ""; else if ($0 ~ /=> \//) print $1, $3 }')
done
[ "$bad" = 0 ] || exit 1

# Symbol-version check: no GLIBC_ requirement above the declared floor.
max=$(for f in $(elfs); do objdump -T "$f" 2>/dev/null; done | grep -o 'GLIBC_[0-9.]*' | sed 's/GLIBC_//' | sort -V | tail -1)
echo "highest GLIBC_ symbol version: $max (floor $glibc_max)"
if [ "$(printf '%s\n%s\n' "$max" "$glibc_max" | sort -V | tail -1)" != "$glibc_max" ]; then
  echo "bundle: needs glibc $max, floor is $glibc_max" >&2
  exit 1
fi

mkdir -p "$out"
tar -C "$prefix" --sort=name --owner=0 --group=0 --numeric-owner --mtime='2026-01-01 00:00:00 UTC' \
  --hard-dereference -cJf "$out/$name" bin lib share
ls -l "$out/$name"

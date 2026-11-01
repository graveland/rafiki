# Embedded PostgreSQL artifacts

What `pkg/embedpg` downloads: one PostgreSQL 18 server tree per platform, with
`vector`, `pg_textsearch`, all of contrib, and `psql`, `pg_dump` and `pg_restore`.
Built here, released from `embedpg-v<pg>-<rev>` tags by
`.github/workflows/embedpg.yml`, and pinned by sha256 in `pkg/embedpg/fetch.go`.

| File | |
|---|---|
| `versions.env` | every pinned input: PostgreSQL, extensions (tag **and** commit), the EDB zip, the glibc floor |
| `build-darwin.sh` | repacks EnterpriseDB's universal macOS tree, builds the extensions against it |
| `Dockerfile.linux`, `bundle-linux.sh`, `build-linux.sh` | builds PostgreSQL from source in `ubuntu:24.04` (pinned by digest), bundles its libraries |
| `smoke.sh` | what every artifact must do, run on an extracted tree |
| `extract/` | unpacks an artifact with `embedpg.ExtractTar`, the extractor `rafikid` itself uses |

Assets: `postgres-linux-amd64.txz`, `postgres-linux-arm64.txz`, and one
`postgres-darwin-universal.txz` for both macOS architectures.

## The contract with `pkg/embedpg`

- `bin/`, `lib/` and `share/postgresql` stay siblings: Postgres finds its share and
  lib directories relative to the binary.
- Only regular files, directories and relative non-climbing symlinks. **No hardlinks.**
- **No `._*` files.** PostgreSQL parses every `*.control` in
  `share/postgresql/extension`, so a macOS AppleDouble sidecar breaks
  `pg_available_extensions`, and migration `0036` with it. `bsdtar` hides these
  entries when listing, so the build checks with Python's `tarfile`.
- Every shared library resolves from inside the tree (`$ORIGIN` / `@rpath`); only
  glibc (Linux) and the system frameworks (macOS) come from the host. The Linux
  build fails if a library resolves outside the tree or a symbol needs a newer
  glibc than `GLIBC_MAX`.
- `libedit`, not readline: readline is GPL and `psql` would carry it.

## Building and checking one locally

```sh
# darwin (on a Mac): ~3 minutes; downloads EDB's ~425 MB zip once
build/embedpg/build-darwin.sh out [work-dir]

# linux: builds for the architecture of the docker engine you point it at
DOCKER_CONTEXT=orbstack build/embedpg/build-linux.sh out        # arm64
DOCKER_HOST=ssh://root@<amd64-host> build/embedpg/build-linux.sh out

# check it the way rafikid will use it
go run ./build/embedpg/extract out/postgres-<...>.txz tree
build/embedpg/smoke.sh tree          # not as root; on Linux, run it in a clean container
```

On Linux, run `smoke.sh` in a bare `ubuntu:24.04` and `debian:trixie-slim` as an
unprivileged user (`--user 65534:65534`; `trixie-slim` has no uid 1000), with the
tree bind-mounted read-only and its root readable by that user. That is what the
workflow does.

The x86_64 slice of the darwin tarball only runs on an Intel Mac; the workflow's
`darwin-x86_64` job covers it.

## Releasing a new build

1. Edit `versions.env`: `PG_VERSION` / `BUILD_REV`, and whichever pins moved. Bump
   `BUILD_REV` when only an extension or a bundled library changed: `embedpg.Version`
   keys the extracted tree, so an unchanged version string is never re-fetched.
   `PG_SOURCE_SHA256` comes from the PostgreSQL project's `.sha256` file; the EDB
   zip's digest is yours to compute and pin, and the build refuses a zip that does
   not match.
2. Merge, then tag `embedpg-v<PG_VERSION>-<BUILD_REV>` (the workflow refuses a tag
   that disagrees with `versions.env`). The tag builds every platform, tests each
   one, and publishes a release with `checksums.txt` and `--latest=false`:
   `rafiki upgrade` installs whatever `releases/latest` names, and this is not a
   rafiki release. The upload is not atomic: if the release job dies mid-upload
   the release exists with some assets and a re-run fails at `gh release create`.
   Delete the release (`gh release delete <tag>`) and re-run the job; the notes
   carry the full `checksums.txt`, so a missing asset is visible.
3. Copy the digests from the release notes into `pkg/embedpg/fetch.go`, set
   `Version` (and `MajorVersion` on a major bump), and run `make check`.

## Testing `pkg/embedpg` before a release exists

The gated real-server tests (`RAFIKI_TEST_EMBEDPG=1`) download the pinned asset. To
run them against a tree you built, copy it to
`<UserCacheDir>/rafiki-test-embedpg/rafiki/embedpg/<Version>-<os>-<arch>` (note the
`rafiki` segment: `paths.CacheDir` appends it), run the tests, and **delete the
directory afterwards**: a tree that never passed the digest check would otherwise
make every later run skip the real download.

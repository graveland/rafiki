#!/bin/bash
# Builds postgres-linux-<arch>.txz for the architecture of the docker engine this
# talks to (DOCKER_HOST / --context decide: OrbStack for arm64, a remote amd64 host).
#
# usage: build-linux.sh <output-dir> [docker buildx build args...]
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build-linux.sh <output-dir> [docker buildx build args...]}
shift
mkdir -p "$out"

# shellcheck source=/dev/null
. "$here/versions.env"

exec docker buildx build \
  --file "$here/Dockerfile.linux" --target artifact --output "type=local,dest=$out" \
  --build-arg PG_VERSION="$PG_VERSION" \
  --build-arg PG_SOURCE_SHA256="$PG_SOURCE_SHA256" \
  --build-arg PGVECTOR_TAG="$PGVECTOR_TAG" --build-arg PGVECTOR_COMMIT="$PGVECTOR_COMMIT" \
  --build-arg PG_TEXTSEARCH_TAG="$PG_TEXTSEARCH_TAG" --build-arg PG_TEXTSEARCH_COMMIT="$PG_TEXTSEARCH_COMMIT" \
  --build-arg GLIBC_MAX="$GLIBC_MAX" \
  "$@" "$here"

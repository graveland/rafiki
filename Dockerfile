FROM golang:1.26 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w -X go.graveland.dev/rafiki/pkg/version.Version=${VERSION}" -o /out/rafikid ./cmd/rafikid

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w -X go.graveland.dev/rafiki/pkg/version.Version=${VERSION}" -o /out/rafiki ./cmd/rafiki


FROM debian:trixie-slim AS rtk

# Pinned deliberately: rtk is pre-1.0 and moves fast, and an unpinned
# "latest" would make the image non-reproducible and could change bash
# output formatting under us. Bump this consciously.
ARG RTK_VERSION=v0.45.0
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$arch" in \
      amd64) target="x86_64-unknown-linux-musl" ;; \
      arm64) target="aarch64-unknown-linux-gnu" ;; \
      *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
    esac; \
    base="https://github.com/rtk-ai/rtk/releases/download/${RTK_VERSION}"; \
    cd /tmp; \
    curl -fsSL -O "${base}/rtk-${target}.tar.gz"; \
    curl -fsSL -O "${base}/checksums.txt"; \
    grep "rtk-${target}.tar.gz\$" checksums.txt | sha256sum -c -; \
    tar -xzf "rtk-${target}.tar.gz" -C /usr/local/bin rtk; \
    chmod 0755 /usr/local/bin/rtk; \
    /usr/local/bin/rtk --version


# The workspace image: what a container workspace runs. Build it with
#
#   docker build --target workspace -t rafiki-workspace:dev .
#
# and pass it as `rafiki executor serve --isolation container --image rafiki-workspace:dev`.
#
# The tool server is `rafiki executor serve-stdio`, and the rafiki binary is
# BAKED IN rather than copied in at Provision time. An earlier design had the
# daemon `docker cp` a statically linked linux binary into every container, which
# needs a cross-compile target, an artifact-location flag, and a refuse-to-start
# check — all to preserve the ability to bring an arbitrary image. That ability
# was already spent: ripgrep is mandatory (glob and grep
# DECLINE without it rather than erroring, silently removing two tools), so the
# image is validated at Provision either way. Baking makes the binary a shared
# read-only layer instead of ~30MB in every container's writable layer, and
# building it here means it is compiled natively for the target arch — the
# macOS-host cross-compile problem never arises.
#
# The cost baking introduces is version skew: the inner binary is no longer
# guaranteed to match the outer executor. Provision checks it.
#
# No USER. The daemon passes `--user <host uid>:<host gid>` (container.go), so
# writes through the read-write /work bind mount are owned by the invoking user
# rather than by whoever the image happens to declare; a baked USER would be
# overridden on every real run and is a false comfort. Running this image by
# hand without --user therefore gets root — inside a container with --cap-drop
# ALL, --network none and --pids-limit, whose mounts are the whole grant.
FROM debian:trixie-slim AS workspace

# ripgrep: required, glob and grep shell out to it.
# git: the agent's own workflows need it, and /repo is mounted read-only for it.
# ca-certificates: any HTTPS the workload does itself.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git ripgrep \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/rafiki /usr/local/bin/rafiki
COPY --from=rtk /usr/local/bin/rtk /usr/local/bin/rtk

# /work is the read-write mount the daemon derives from the child's worktree.
# It is created by the bind mount at run time; this only sets the default.
WORKDIR /work

# No ENTRYPOINT: the daemon starts the container with `sleep infinity` and then
# `docker exec`s `rafiki executor serve-stdio` into it (container.go).


FROM debian:trixie-slim AS release

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates ripgrep \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd -r -g 1000 rafiki && useradd -r -m -u 1000 -g rafiki -s /usr/sbin/nologin rafiki

COPY --from=build /out/rafikid /usr/local/bin/rafikid
COPY --from=build /out/rafiki /usr/local/bin/rafiki
COPY --from=rtk /usr/local/bin/rtk /usr/local/bin/rtk

EXPOSE 8035 8036

USER rafiki

ENTRYPOINT ["rafikid"]

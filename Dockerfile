FROM golang:1.27 AS build

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
ARG RTK_VERSION=v0.50.0
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




FROM debian:trixie-slim AS sandbox

# Image for sandboxed executors and the sandbox relay foothold. Both launch it
# with an explicit `rafiki` entrypoint (pkg/sandbox/createbody.go,
# pkg/foothold/foothold.go), so it carries no rafikid.
#
# It is the machine an agent works on, and the rafiki user has no root, so
# anything missing here stays missing (under `network: none` nothing can be
# fetched either). The Go and Rust toolchains resolve to the latest stable
# release at build time; the weekly rebuild in .github/workflows/images.yml
# re-resolves them and picks up Debian security fixes.
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
      bash ca-certificates git ripgrep \
      procps psmisc lsof strace iproute2 iputils-ping dnsutils netcat-openbsd \
      findutils diffutils patch less file tree jq fd-find gawk \
      tar gzip xz-utils bzip2 zip unzip \
      curl wget openssh-client rsync \
      build-essential pkg-config cmake \
      vim-tiny nano sqlite3 tzdata \
      python3 python3-venv python3-pip nodejs npm \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /usr/local/bin/

RUN set -eux; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    entry="$(curl -fsSL 'https://go.dev/dl/?mode=json' | jq -r --arg a "$arch" \
      '[.[] | select(.stable)][0].files[] | select(.os == "linux" and .arch == $a and .kind == "archive") | "\(.filename) \(.sha256)"')"; \
    file="${entry% *}"; sum="${entry#* }"; \
    curl -fsSL -o "/tmp/${file}" "https://go.dev/dl/${file}"; \
    echo "${sum}  /tmp/${file}" | sha256sum -c -; \
    tar -C /usr/local -xzf "/tmp/${file}"; \
    rm "/tmp/${file}"; \
    /usr/local/go/bin/go version

RUN groupadd -r -g 1000 rafiki && useradd -r -m -u 1000 -g rafiki -s /bin/bash rafiki

COPY --from=build /out/rafiki /usr/local/bin/rafiki
COPY --from=rtk /usr/local/bin/rtk /usr/local/bin/rtk

USER rafiki

# `rafiki executor serve` roots its state (daraja-sockets, workspaces) at the
# working directory when --root is absent, and a sandbox create with no workdir
# inherits this one; the image default of / is unwritable by the rafiki user.
WORKDIR /home/rafiki

# Rust lives under $HOME so `cargo install` and rustup updates work without
# root. rustup-init is checksum-verified per architecture.
RUN set -eux; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$arch" in \
      amd64) triple="x86_64-unknown-linux-gnu" ;; \
      arm64) triple="aarch64-unknown-linux-gnu" ;; \
      *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
    esac; \
    base="https://static.rust-lang.org/rustup/dist/${triple}"; \
    cd /tmp; \
    curl -fsSL -O "${base}/rustup-init"; \
    curl -fsSL -O "${base}/rustup-init.sha256"; \
    sed 's#\*\?rustup-init$#rustup-init#' rustup-init.sha256 | sha256sum -c -; \
    chmod +x rustup-init; \
    ./rustup-init -y --no-modify-path --profile minimal -c clippy -c rustfmt --default-toolchain stable; \
    rm rustup-init rustup-init.sha256

ENV LANG=C.UTF-8 \
    GOPATH=/home/rafiki/go \
    NPM_CONFIG_PREFIX=/home/rafiki/.npm-global \
    PATH=/home/rafiki/.cargo/bin:/home/rafiki/go/bin:/home/rafiki/.local/bin:/home/rafiki/.npm-global/bin:/usr/local/go/bin:${PATH}

ENTRYPOINT ["rafiki"]


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

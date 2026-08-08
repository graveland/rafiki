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

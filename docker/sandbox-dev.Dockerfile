# Development sandbox for this repo: the published sandbox image plus the tools
# `make proto` and `make check` need, and a fixed home for build caches.
#
# Nothing from the repo is copied in. The build context is empty (the Makefile
# feeds this file on stdin), so the image cannot go stale against go.mod or any
# other project file, and the same image serves every checkout.
#
#   make sandbox-dev-image
#
# Use it per sandbox (spawn block `image`, or `rafiki sandbox create --image`),
# not as RAFIKI_SANDBOX_IMAGE: it exists only in the docker engine that built
# it, and the daemon's default applies to every launcher.
ARG BASE=ghcr.io/graveland/rafiki-sandbox:latest
FROM ${BASE}

# protoc ships no checksum file, so each release asset is pinned here. Bump
# PROTOC_VERSION and both hashes together. The zip also carries
# include/google/protobuf/*.proto, which control.proto imports.
ARG TARGETARCH
ARG PROTOC_VERSION=35.1
ARG PROTOC_SHA256_ARM64=01bf9d08808c7f96678b63f4bd8efa559bb4f83d5a7a270d5edaf507f9d5d9cf
ARG PROTOC_SHA256_AMD64=6930ebf62bd4ea607b98fff052596c6ee564b9835b4ce172c75a3f53ae9d91b7
ARG GOLANGCI_LINT_VERSION=v2.13.2

USER root
RUN set -eux; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$arch" in \
      amd64) asset="linux-x86_64"; sha="${PROTOC_SHA256_AMD64}" ;; \
      arm64) asset="linux-aarch_64"; sha="${PROTOC_SHA256_ARM64}" ;; \
      *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
    esac; \
    cd /tmp; \
    curl -fsSL -o protoc.zip "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-${asset}.zip"; \
    echo "${sha}  protoc.zip" | sha256sum -c -; \
    unzip -q protoc.zip -d /usr/local; \
    rm protoc.zip; \
    protoc --version

# One place for build output, so it is easy to find, mount, or throw away with
# the container. CARGO_HOME is left alone: it holds the rustup proxies in bin/.
RUN install -d -o rafiki -g rafiki /cache /cache/go-build /cache/go-mod /cache/cargo-target

USER rafiki

# Built with the image's own Go, so the linter always matches the toolchain it
# lints (a prebuilt binary links an older Go and refuses newer language
# versions).
RUN go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}" \
    && golangci-lint --version \
    && go clean -cache -modcache

ENV GOCACHE=/cache/go-build \
    GOMODCACHE=/cache/go-mod \
    CARGO_TARGET_DIR=/cache/cargo-target

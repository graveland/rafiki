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


FROM debian:trixie-slim AS release

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd -r -g 1000 rafiki && useradd -r -m -u 1000 -g rafiki -s /usr/sbin/nologin rafiki

COPY --from=build /out/rafikid /usr/local/bin/rafikid
COPY --from=build /out/rafiki /usr/local/bin/rafiki

EXPOSE 8035 8036

USER rafiki

ENTRYPOINT ["rafikid"]

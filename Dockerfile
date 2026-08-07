FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/rafikid ./cmd/rafikid
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/rafiki ./cmd/rafiki

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/rafikid /usr/local/bin/rafikid
COPY --from=build /out/rafiki /usr/local/bin/rafiki
EXPOSE 8035 8036
ENTRYPOINT ["rafikid"]

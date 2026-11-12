# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine3.24 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/otransd ./cmd/otransd

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 otrans \
    && adduser -S -D -H -u 10001 -G otrans otrans \
    && mkdir -p /data \
    && chown 10001:10001 /data
COPY --from=build /out/otransd /usr/local/bin/otransd

USER 10001:10001
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080
ENV OTRANS_LISTEN_ADDR=:8080 \
    OTRANS_DATA_DIR=/data \
    OTRANS_MAX_UPLOAD_BYTES=4294967296 \
    OTRANS_MAX_STORAGE_BYTES=107374182400 \
    OTRANS_RETENTION_DAYS=30
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz | grep -q '"status":"ok"'
ENTRYPOINT ["/usr/local/bin/otransd"]

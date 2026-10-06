# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# Pure-Go SQLite driver: no cgo, so the result is one static binary.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/jannyq ./cmd/jannyq

# ---- runtime ----
FROM alpine:3.21
# Tools needed by jannyq at runtime. Later phases add more (poppler, ...).
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 jannyq \
 && mkdir -p /data /knowledge /skills \
 && chown jannyq:jannyq /data
COPY --from=build /out/jannyq /usr/local/bin/jannyq

USER jannyq
WORKDIR /data
ENV JANNYQ_DATA_DIR=/data \
    JANNYQ_LISTEN=:8080
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/jannyq"]

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

# ---- sandbox: executes the model's shell commands ----
# Build with:  docker build --target sandbox -t jannyq-sandbox .
# Tools available to commands. Add your own with --build-arg SANDBOX_PACKAGES="...".
FROM alpine:3.21 AS sandbox
ARG SANDBOX_PACKAGES="bash python3 curl jq bc coreutils findutils grep sed gawk tar gzip zip unzip file tree openssl sqlite"
# The bot has the sandbox read the PDFs that users send (text extraction, page pictures, OCR of scans),
# so that a hostile file is parsed here, away from its secrets. Add OCR languages with
# --build-arg OCR_PACKAGES="tesseract-ocr tesseract-ocr-data-eng tesseract-ocr-data-tha tesseract-ocr-data-deu"
ARG PDF_PACKAGES="poppler-utils"
ARG OCR_PACKAGES="tesseract-ocr tesseract-ocr-data-eng tesseract-ocr-data-tha"
# tini is PID 1 so that processes left behind by commands are reaped (no zombies).
RUN apk add --no-cache tini ca-certificates tzdata ${SANDBOX_PACKAGES} ${PDF_PACKAGES} ${OCR_PACKAGES} \
 && mkdir -p /work /skills \
 && chmod 711 /work
COPY --from=build /out/jannyq /usr/local/bin/jannyq
# The executor runs as root on purpose: it needs CAP_SETUID/SETGID/CHOWN to give every chat its own
# unprivileged user. docker-compose.yml drops all other capabilities, makes the root filesystem
# read-only and keeps the container off the internet. Commands themselves never run as root.
ENV JANNYQ_SANDBOX_LISTEN=:9090 \
    JANNYQ_SANDBOX_WORKDIR=/work
VOLUME ["/work"]
EXPOSE 9090
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:9090/healthz >/dev/null || exit 1
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/jannyq", "sandbox"]

# ---- runtime: the bot (default target) ----
FROM alpine:3.21 AS runtime
# Tools needed by the bot itself. Commands run in the separate sandbox image, not here.
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

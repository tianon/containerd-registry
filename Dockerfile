FROM --platform=$BUILDPLATFORM golang:1.23 AS build

ENV CGO_ENABLED 0

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TARGETOS TARGETARCH TARGETVARIANT
ENV GOOS=$TARGETOS GOARCH=$TARGETARCH VARIANT=$TARGETVARIANT

RUN set -eux; \
	case "$GOARCH" in \
		arm) export GOARM="${VARIANT#v}" ;; \
		amd64) export GOAMD64="$VARIANT" ;; \
		arm64) [ "${VARIANT:-v8}" = 'v8' ] ;; \
		*) [ -z "$VARIANT" ] ;; \
	esac; \
	go env | grep -E 'OS=|ARCH=|ARM=|AMD64='; \
	go build -v -trimpath -ldflags '-d -w' -o /containerd-registry

FROM --platform=$TARGETPLATFORM alpine:3.21

COPY --from=build --link /containerd-registry /usr/local/bin/

# Server configuration
# Listen address (default: ":5000")
ENV LISTEN_ADDRESS=":5000"

# Logging configuration
# Log format: "text" for human-readable, "json" for structured logging (default: "text")
ENV LOG_FORMAT="text"

# HTTP timeout configuration (use Go duration format: "5m", "30s", etc.)
# Read timeout for incoming requests (default: "5m")
ENV READ_TIMEOUT="5m"
# Write timeout for responses (default: "5m")
ENV WRITE_TIMEOUT="5m"
# Idle timeout for keep-alive connections (default: "120s")
ENV IDLE_TIMEOUT="120s"
# Graceful shutdown timeout (default: "30s")
ENV SHUTDOWN_TIMEOUT="30s"

# Registry limits configuration
# Blob lease expiration time (default: "15m")
ENV BLOB_LEASE_EXPIRATION="15m"
# Maximum manifest size in bytes (default: "4194304" = 4 MiB)
ENV MAX_MANIFEST_SIZE="4194304"

CMD ["containerd-registry"]

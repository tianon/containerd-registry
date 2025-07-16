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

CMD ["containerd-registry"]

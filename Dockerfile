FROM --platform=$BUILDPLATFORM golang:1.20 AS build

ENV CGO_ENABLED 0

WORKDIR /app
COPY go.mod go.sum ./
RUN set -eux; go mod download; go mod verify
COPY . .

ARG TARGETOS TARGETARCH TARGETVARIANT
ENV GOOS=$TARGETOS GOARCH=$TARGETARCH VARIANT=$TARGETVARIANT

RUN set -eux; \
	case "$GOARCH" in \
		arm) export GOARM="${VARIANT#v}" ;; \
		amd64) export GOAMD64="$VARIANT" ;; \
		*) [ -z "$VARIANT" ] ;; \
	esac; \
	go env | grep -E 'OS=|ARCH=|ARM=|AMD64='; \
	go build -v -trimpath -ldflags '-d -w' -o /containerd-registry

FROM --platform=$TARGETPLATFORM alpine:3.18

COPY --from=build --link /containerd-registry /usr/local/bin/

CMD ["containerd-registry"]

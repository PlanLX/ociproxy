# docker build -t ghcr.io/planlx/ociproxy:0.1.2 -f deploy/Dockerfile .

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS TARGETARCH
ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
ENV GOPROXY=https://goproxy.cn,https://goproxy.io,direct
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w -X main.version=${VERSION}" -o /ociproxy ./cmd/ociproxy

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

RUN apk --no-cache add ca-certificates

COPY --from=builder /ociproxy /usr/local/bin/ociproxy
COPY examples/default_config.yaml /etc/ociproxy/config.yaml

# Create cache directory with proper ownership for non-root user
RUN mkdir -p /data/cache && chown 65534:65534 /data/cache

USER 65534:65534

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/ociproxy"]
CMD ["--config", "/etc/ociproxy/config.yaml"]

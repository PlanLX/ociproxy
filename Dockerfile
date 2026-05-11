FROM golang:1.23-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o /ociproxy ./cmd/ociproxy

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

RUN apk --no-cache add ca-certificates

COPY --from=builder /ociproxy /usr/local/bin/ociproxy
COPY --from=builder /src/examples/mirror_config.yaml /etc/ociproxy/config.yaml

USER 65534:65534

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/ociproxy"]
CMD ["--config", "/etc/ociproxy/config.yaml"]

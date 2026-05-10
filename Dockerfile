FROM golang:1.23-alpine AS builder

WORKDIR /src

COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /ociproxy ./cmd/ociproxy

FROM alpine:3.20

RUN apk --no-cache add ca-certificates

COPY --from=builder /ociproxy /usr/local/bin/ociproxy
COPY --from=builder /src/examples/config.yaml /etc/ociproxy/config.yaml

USER 65534:65534

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/ociproxy"]
CMD ["--config", "/etc/ociproxy/config.yaml"]

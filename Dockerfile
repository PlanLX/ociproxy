FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

ARG TARGETARCH

RUN apk --no-cache add ca-certificates

COPY bin/ociproxy-linux-${TARGETARCH} /usr/local/bin/ociproxy
COPY examples/default_config.yaml /etc/ociproxy/config.yaml

USER 65534:65534

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/ociproxy"]
CMD ["--config", "/etc/ociproxy/config.yaml"]

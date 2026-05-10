# ociproxy

A stateless, zero-storage OCI image registry proxy. Forward requests to multiple configurable upstream mirrors with alias and path-prefix support — no caching, no disk I/O.

## Features

- **Stateless** — no storage, no cache. Every request is forwarded to an upstream mirror.
- **Registry groups** — one namespace (e.g., `docker.io`) with multiple mirror endpoints.
- **Path prefix** — upstream proxies that require a base path (e.g., `m.daocloud.io/docker.io`) are supported via `base_path`.
- **Default registry** — when no registry prefix is in the path, the default namespace is used.
- **Docker Registry token auth** — automatic Bearer token exchange for private registries.
- **Configurable** — YAML config file or CLI flags.

## Quick Start

### Run with CLI flags

```bash
go run ./cmd/ociproxy \
  --registry "docker.io=https://registry-1.docker.io" \
  --mirrors "https://dockerproxy.net,https://docker.1ms.ru,https://m.daocloud.io" \
  --addr :8080
```

### Run with config file

```bash
go run ./cmd/ociproxy --config examples/config.yaml
```

### Docker

```bash
docker build -t ociproxy .
docker run -p 8080:8080 -v $(pwd)/examples/config.yaml:/etc/ociproxy/config.yaml ociproxy
```

## Usage

### Pull through the proxy

```bash
# Via the docker.io namespace (default mirror: registry-1.docker.io)
docker pull localhost:8080/v2/docker.io/library/busybox:latest

# Via the default registry (no prefix needed)
docker pull localhost:8080/v2/library/nginx:latest
```

### Configuration

```yaml
default_registry: docker.io

registries:
  - name: docker.io
    mirrors:
      - url: https://registry-1.docker.io
        default: true
      - url: https://dockerproxy.net
      - url: https://docker.gh-proxy.org
        base_path: /docker.io
      - url: https://m.daocloud.io
        base_path: /docker.io
```

| Field | Description |
|-------|-------------|
| `default_registry` | Namespace used when the URL has no registry prefix (e.g., `/v2/library/busybox`) |
| `registries[].name` | Registry namespace — this IS the URL path prefix (e.g., `docker.io`) |
| `registries[].mirrors[].url` | Upstream mirror base URL |
| `registries[].mirrors[].base_path` | Optional path prefix to prepend (e.g., `/docker.io`). Defaults to the registry name if empty. |
| `registries[].mirrors[].default` | Exactly one mirror per registry must be marked as default |
| `registries[].mirrors[].credentials` | Optional username/password for private registries |

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--addr` | `:8080` | Listen address |
| `--config` | | Config file path |
| `--default-registry` | `docker.io` | Default registry namespace |
| `--registry` | `docker.io=https://registry-1.docker.io` | Primary registry: `name=url` |
| `--mirrors` | | Additional mirrors, comma-separated URLs |
| `--log-level` | `info` | Log level |

## Architecture

```
Client → ociproxy → /v2/docker.io/library/busybox/manifests/latest
                          ↓
                    registry = "docker.io"
                    mirror   = default (registry-1.docker.io)
                    path     = /docker.io/library/busybox/manifests/latest
                          ↓
              https://registry-1.docker.io/docker.io/library/busybox/manifests/latest
```

**Path resolution:**

1. Client requests `/v2/docker.io/library/busybox/manifests/latest`
2. `docker.io` matches the configured registry namespace
3. The default mirror for `docker.io` is selected
4. The path is rewritten with the mirror's `base_path` (defaults to the registry name)
5. Response is streamed back to the client

No blobs or manifests are stored locally.

## Build

```bash
make build   # build binary
make test    # run tests
make lint    # run linter
make run     # run with example config
```

# 架构时序图

## 整体架构

```
┌─────────────┐     ┌─────────────────────────────────────────────────────┐
│  Docker     │     │                    ociproxy                         │
│  Client     │     │                                                     │
│             │     │  ┌──────────┐    ┌─────────────┐   ┌──────────────┐│
│  push/pull  │     │  │ Handler  │───▶│ Distribution│   │ Proxy        ││
│             │     │  │ (wrapper)│    │ (pull-      │◀──│ (upstream    ││
│             │     │  │          │    │  through)   │   │  routing)    ││
└──────┬──────┘     │  └──────────┘    └──────┬──────┘   └──────┬───────┘│
       │            │                          │                 │        │
       │            │  ┌──────────┐            │          ┌──────┴───────┐│
       │            │  │ 404Cache │            │          │ Mirror Cache ││
       │            │  │ (60s TTL)│            │          │ (per mirror) ││
       │            │  └──────────┘            │          └──────────────┘│
       │            └──────────────────────────┼──────────────────────────┘
       │                                       │
       │            ┌──────────────────────────┼──────────────────────────┐
       │            │         MinIO / S3        │                         │
       │            │                           │                         │
       │            │  /ociproxy/docker/registry/v2/                      │
       │            │    ├── repositories/                                │
       │            │    └── blobs/                                       │
       │            └─────────────────────────────────────────────────────┘
       │
       │            ┌──────────────────────┐
       │            │  Upstream Registries │
       │            │  docker.io, quay.io  │
       │            │  ghcr.io, etc.       │
       │            └──────────────────────┘
       │
```

## 场景 1: 客户端拉取镜像 — S3 缓存命中

```
Docker Client          ociproxy Handler        Distribution           S3
     │                       │                      │                  │
     │── pull library/nginx ─▶│                      │                  │
     │                       │                      │                  │
     │                       │── 查 404 Cache ──▶    │                  │
     │                       │   (未命中)            │                  │
     │                       │                      │                  │
     │                       │── 重写路径 ──▶        │                  │
     │                       │  /v2/docker.io/       │                  │
     │                       │  library/nginx/...    │                  │
     │                       │                      │                  │
     │                       │──────────────────────▶│                  │
     │                       │                      │── 检查 S3 ──────▶│
     │                       │                      │◀─ manifest 存在 ─│
     │                       │                      │                  │
     │                       │◀─────────────────────│                  │
     │◀─ 200 OK (manifest) ─│                      │                  │
     │                       │                      │                  │
     │── pull blob sha256:xx─▶│                      │                  │
     │                       │──────────────────────▶│                  │
     │                       │                      │── 检查 S3 ──────▶│
     │                       │                      │◀─ blob 存在 ─────│
     │                       │                      │                  │
     │◀─ 200 OK (blob) ─────│                      │                  │
```

## 场景 2: 客户端拉取镜像 — S3 缓存未命中

```
Docker Client          ociproxy Handler        Distribution           S3              Proxy            Upstream
     │                       │                      │                  │                 │                 │
     │── pull busybox:latest─▶│                      │                  │                 │                 │
     │                       │── 查 404 Cache ──▶    │                  │                 │                 │
     │                       │   (未命中)            │                  │                 │                 │
     │                       │── 重写: /v2/          │                  │                 │                 │
     │                       │  docker.io/...        │                  │                 │                 │
     │                       │                      │                  │                 │                 │
     │                       │──────────────────────▶│                  │                 │                 │
     │                       │                      │── 检查 S3 ──────▶│                 │                 │
     │                       │                      │◀─ 404 未找到 ────│                 │                 │
     │                       │                      │                  │                 │                 │
     │                       │                      │── 转发到 Proxy ──────────────────────▶│                 │
     │                       │                      │                                      │── HEAD /v2/... ─▶│
     │                       │                      │                                      │◀─ 200 manifest ─│
     │                       │                      │◀─────────────────────────────────────│                 │
     │                       │                      │                  │                 │                 │
     │                       │                      │── 写入 S3 ──────▶│                 │                 │
     │                       │                      │   (manifest)      │                 │                 │
     │                       │                      │                  │                 │                 │
     │                       │                      │── 拉 blob ──────▶│                 │                 │
     │                       │                      │── 写入 S3 ──────▶│                 │                 │
     │                       │                      │   (blob)          │                 │                 │
     │                       │                      │                  │                 │                 │
     │                       │◀─────────────────────│                  │                 │                 │
     │◀─ 200 OK ────────────│                      │                  │                 │                 │
```

## 场景 3: 404 负响应缓存 — 防止上游打爆

```
Docker Client          ociproxy Handler        Distribution           S3              Proxy            Upstream
     │                       │                      │                  │                 │                 │
     │── HEAD lakekeeper/   ─▶│                      │                  │                 │                 │
     │   /manifests/latest   │                      │                  │                 │                 │
     │   ?ns=quay.io         │                      │                  │                 │                 │
     │                       │── 查 404 Cache ──▶    │                  │                 │                 │
     │                       │   (未命中)            │                  │                 │                 │
     │                       │── 重写: /v2/quay.io/  │                  │                 │                 │
     │                       │  lakekeeper/catalog   │                  │                 │                 │
     │                       │                      │                  │                 │                 │
     │                       │──────────────────────▶│                  │                 │                 │
     │                       │                      │── 检查 S3 ──────▶│                 │                 │
     │                       │                      │◀─ 404 未找到 ────│                 │                 │
     │                       │                      │── 转发 Proxy ──────────────────────▶│                 │
     │                       │                      │                                      │── HEAD ────────▶│
     │                       │                      │◀─────────────────────────────────────│◀─ 404 ─────────│
     │                       │                      │                  │                 │                 │
     │                       │                      │── 写入 S3 ──────▶│                 │                 │
     │                       │                      │   (404 manifest)  │                 │                 │
     │                       │                      │                  │                 │                 │
     │◀─ 404 Not Found ─────│                      │                  │                 │                 │
     │                       │── 存入 404 Cache ─▶   │                  │                 │                 │
     │   (key="HEAD:/v2/     │   (TTL=60s)           │                  │                 │                 │
     │    lakekeeper/...")   │                      │                  │                 │                 │
     │                       │                      │                  │                 │                 │
     │                       │                      │                  │                 │                 │
     │── 15s 后重试 ────────▶│                      │                  │                 │                 │
     │                       │── 查 404 Cache ──▶    │                  │                 │                 │
     │                       │   HIT! ────┐          │                  │                 │                 │
     │                       │            │          │                  │                 │                 │
     │                       │◀───────────┘          │                  │                 │                 │
     │◀─ 404 (本地返回) ─────│                       │                  │                 │                 │
     │                       │  ❌ 不经过 Distribution                  │                 │                 │
     │                       │  ❌ 不查 S3                              │                 │                 │
     │                       │  ❌ 不打上游                             │                 │                 │
     │                       │                      │                  │                 │                 │
     │── 60s 后重试 ────────▶│                      │                  │                 │                 │
     │                       │── 查 404 Cache ──▶    │                  │                 │                 │
     │                       │   EXPIRED             │                  │                 │                 │
     │                       │── 重新走完整流程 ──▶   │                  │                 │                 │
```

## 场景 4: ns= 参数路径重写

```
Docker Client          ociproxy Handler        Distribution
     │                       │                      │
     │── /v2/library/nginx/ ─▶│                      │
     │   manifests/latest     │                      │
     │   ?ns=docker.io        │                      │
     │                       │                      │
     │                       │── 检测到 ns=docker.io │
     │                       │── 重写路径:            │
     │                       │  /v2/docker.io/       │
     │                       │  library/nginx/...    │
     │                       │── 去掉 ?ns             │
     │                       │                      │
     │                       │──────────────────────▶│
     │                       │                      │
     │                       │  S3 路径:             │
     │                       │  /ociproxy/docker/    │
     │                       │  registry/v2/         │
     │                       │  repositories/        │
     │                       │  docker.io/library/   │
     │                       │  nginx/               │
```

## S3 存储结构

```
/ociproxy/
└── docker/
    └── registry/
        └── v2/
            ├── repositories/
            │   ├── docker.io/              ← 默认 registry namespace
            │   │   └── library/
            │   │       ├── busybox/
            │   │       │   └── _manifests/
            │   │       │       ├── tags/
            │   │       │       │   └── latest/
            │   │       │       │       ├── current/link
            │   │       │       │       └── index/
            │   │       │       │           └── sha256:<digest>/link
            │   │       │       └── revisions/
            │   │       │           └── sha256:<digest>/link
            │   │       └── nginx/
            │   │           └── _manifests/...
            │   ├── quay.io/                ← 带 ns 参数的 registry
            │   │   └── lakekeeper/
            │   │       └── catalog/
            │   │           └── _manifests/...
            │   └── registry.k8s.io/        ← 带路径前缀的 registry
            │       └── git-sync/
            │           └── git-sync/
            │               └── _manifests/...
            └── blobs/
                └── sha256/
                    ├── 14/
                    │   └── <digest>/data   ← blob 内容
                    └── e1/
                        └── <digest>/data
```

## 请求路由规则

| 客户端请求 | 内部路径 | S3 存储路径 |
|-----------|---------|------------|
| `/v2/library/nginx/...` | `/v2/docker.io/library/nginx/...` | `repositories/docker.io/library/nginx/` |
| `/v2/library/busybox/...?ns=docker.io` | `/v2/docker.io/library/busybox/...` | `repositories/docker.io/library/busybox/` |
| `/v2/lakekeeper/catalog/...?ns=quay.io` | `/v2/quay.io/lakekeeper/catalog/...` | `repositories/quay.io/lakekeeper/catalog/` |
| `/v2/registry.k8s.io/git-sync/...` | `/v2/registry.k8s.io/git-sync/...` | `repositories/registry.k8s.io/git-sync/git-sync/` |
| `/v2/ghcr.io/planlx/ociproxy/...` | `/v2/ghcr.io/planlx/ociproxy/...` | `repositories/ghcr.io/planlx/ociproxy/` |

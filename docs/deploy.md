# 全局聚合镜像仓库部署指南

## 架构

```
Docker Client ──→ registry.piston.ink (APISIX HTTPS)
                    │
                    ├──→ 192.168.1.105:15000 (ociproxy)
                    │       │
                    │       ├──→ m.daocloud.io (DaoCloud Mirror)
                    │       ├──→ docker.gh-proxy.org (GH Proxy)
                    │       ├──→ dockerproxy.net (Fallback)
                    │       └──→ registry-1.docker.io (Origin)
                    │
                    └──→ S3 Cache (pull-through 缓存)
```

**核心价值**：一个域名 `registry.piston.ink` 自动代理所有公网 registry，通过国内镜像加速下载并缓存到 S3。

## 服务端部署

### 1. 部署 ociproxy

```bash
# 配置环境变量
cp .env.example .env
# 编辑 .env 填入 S3 配置

# 启动服务
docker compose -f deploy/docker-compose-dev.yml up -d --build

# 验证
curl http://localhost:15000/v2/
# 应返回 200 + Docker-Distribution-Api-Version: registry/2.0
```

### 2. 配置 APISIX

```bash
# 基本部署（HTTP）
bash deploy/apisix.sh http://127.0.0.1:9180 192.168.1.105

# 带 TLS 证书部署
bash deploy/apisix.sh http://127.0.0.1:9180 192.168.1.105 /etc/cert/tls.crt /etc/cert/tls.key
```

**脚本自动完成**：
- 安装 TLS 证书（覆盖 17 个 registry 域名）
- 创建统一路由，超时设置：connect=10s, read=300s
- 移除不必要的 proxy-rewrite 插件
- 验证路由配置

### 3. 验证服务端

```bash
# 从 APISIX 机器测试后端连通性
curl http://192.168.1.105:15000/v2/

# 从 APISIX 机器测试路由
curl http://127.0.0.1:15000/v2/

# 从客户端测试 HTTPS
curl https://registry.piston.ink/v2/

# 测试 manifest 拉取（不应返回 502）
curl https://registry.piston.ink/v2/gcr.io/kaniko-project/executor/manifests/debug
```

## 客户端配置

### 方式 A：registry-mirrors（仅 docker.io）

编辑 `/etc/docker/daemon.json`：

```json
{
  "registry-mirrors": ["https://registry.piston.ink"]
}
```

```bash
sudo systemctl restart docker
```

**适用场景**：只需加速 `docker pull nginx` 等默认 docker.io 镜像。

**限制**：`docker pull gcr.io/...` 等带明确注册表前缀的请求**不会**走 mirror。

### 方式 B：DNS/hosts 全拦截（所有 registry）

在每台 Docker 客户端机器上添加 `/etc/hosts`：

```
# <APISIX_IP> 替换为 APISIX 实际 IP
<APISIX_IP>  registry.piston.ink docker.io gcr.io ghcr.io quay.io registry.k8s.io nvcr.io docker.elastic.co mcr.microsoft.com registry.ollama.ai lscr.io public.ecr.aws cgr.dev us.gcr.io eu.gcr.io asia.gcr.io k8s.gcr.io
```

配置 insecure registry（如果使用自签证书）：

```json
{
  "insecure-registries": ["registry.piston.ink", "gcr.io", "ghcr.io", "quay.io"]
}
```

```bash
sudo systemctl restart docker
```

**适用场景**：需要 `docker pull gcr.io/...` 等所有 registry 都走代理。

## Pull 命令示例

```bash
# docker.io（通过 registry-mirrors 或 DNS 拦截）
docker pull library/nginx:latest

# gcr.io
docker pull gcr.io/kaniko-project/executor:debug
docker pull gcr.io/google-containers/pause:3.9

# ghcr.io
docker pull ghcr.io/owner/repo:tag

# quay.io
docker pull quay.io/prometheus/prometheus:latest

# registry.k8s.io
docker pull registry.k8s.io/pause:3.9

# 显式路径方式（无需 DNS 配置）
docker pull registry.piston.ink/gcr.io/kaniko-project/executor:debug
docker pull registry.piston.ink/ghcr.io/owner/repo:tag
```

## 覆盖的 Registry 域名

| 域名 | 说明 |
|------|------|
| docker.io | Docker Hub |
| gcr.io | Google Container Registry |
| ghcr.io | GitHub Container Registry |
| quay.io | Red Hat Quay |
| registry.k8s.io | Kubernetes 官方镜像 |
| nvcr.io | NVIDIA Container Registry |
| docker.elastic.co | Elastic 官方镜像 |
| mcr.microsoft.com | Microsoft Container Registry |
| registry.ollama.ai | Ollama 模型 |
| lscr.io | LinuxServer.io |
| public.ecr.aws | Amazon ECR Public |
| cgr.dev | Chainguard Images |
| us.gcr.io / eu.gcr.io / asia.gcr.io | GCR 区域端点 |
| k8s.gcr.io | K8s 旧版 Registry（重定向到 registry.k8s.io） |

## 故障排查

### 502 Bad Gateway

**原因**：APISIX 无法连接 ociproxy 或超时。

```bash
# 1. 检查 APISIX 到后端的连通性
curl http://<OCPROXY_IP>:15000/v2/

# 2. 检查 APISIX 错误日志
tail -50 /usr/local/apisix/logs/error.log

# 3. 确认路由超时设置
curl http://127.0.0.1:9180/apisix/admin/routes/1 \
  -H "X-API-KEY: edd1c9f034335f136f87ad84b625c8f1"
```

### TLS 证书错误

```bash
# 检查证书覆盖的域名
echo | openssl s_client -connect registry.piston.ink:443 2>/dev/null | openssl x509 -noout -text | grep "Subject Alternative Name"

# 临时使用 insecure registry
# /etc/docker/daemon.json:
{
  "insecure-registries": ["registry.piston.ink"]
}
```

### 请求未到达 ociproxy

```bash
# 检查 ociproxy 日志
docker logs ociproxy --tail 50

# 检查 APISIX 路由是否匹配
curl http://127.0.0.1:9180/apisix/admin/routes/1 \
  -H "X-API-KEY: edd1c9f034335f136f87ad84b625c8f1" | python3 -m json.tool
```

### DNS 解析验证

```bash
# 确认域名解析到 APISIX IP
nslookup gcr.io
nslookup registry.piston.ink

# 或在 /etc/hosts 配置的机器上
getent hosts gcr.io
```

## 性能调优

### APISIX 超时设置

已在 `deploy/apisix.sh` 中配置：
- `connect: 10s` — 快速失败
- `read: 300s` — 支持大镜像 blob 下载（通常 100MB+）
- `send: 300s` — 同上

### ociproxy 超时设置

- 上游请求超时：120s（`internal/proxy/proxy.go`）
- HTTP 服务器 WriteTimeout：5 分钟（`cmd/ociproxy/main.go`）

APISIX 的 300s read 超时 > ociproxy 的 120s 上游超时，确保不会被提前掐断。

### 缓存策略

- 404 响应缓存 60 秒，防止重复请求上游
- S3 pull-through 缓存已拉取的镜像层，后续请求直接命中本地

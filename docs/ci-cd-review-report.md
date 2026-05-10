# CI/CD Code Review Report

Date: 2026-05-11
Reviewers: 6 parallel agents (gitea-reviewer, docker-reviewer, go-reviewer, architect-reviewer, reliability-reviewer, copyright-auditor)

## Summary

| Severity | Count |
|----------|-------|
| CRITICAL | 2 |
| HIGH | 5 |
| MEDIUM | 13 |
| LOW | 12 |

Files reviewed:
- `.github/workflows/build_docker_image.yaml`
- `.github/workflows/release_binary.yaml`
- `Dockerfile`
- `Makefile`
- `go.mod`
- `cmd/ociproxy/main.go`
- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/proxy/proxy.go`
- `internal/proxy/proxy_test.go`
- `internal/proxy/auth.go`
- `internal/proxy/auth_test.go`
- `internal/router/router.go`
- `test/e2e/e2e_test.go`
- `deploy/docker-compose.yml`
- `deploy/test-config.yaml`
- `examples/config.yaml`
- `config.prod.yaml`
- `scripts/e2e-test.sh`
- `README.md`
- `.gitignore`

---

## CRITICAL

### 1. GHA Cache Not Supported on Gitea

- **File**: `build_docker_image.yaml:51-52`
- **Reviewer**: gitea-reviewer

```yaml
cache-from: type=gha
cache-to: type=gha,mode=max
```

`type=gha` uses GitHub's proprietary Cache Service API, which is not available in Gitea Act Runner. This will cause the build step to fail or silently skip caching, resulting in significantly slower builds.

**Recommendation**: Remove the cache lines or switch to registry-based cache:

```yaml
cache-from: type=registry,ref=${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}:buildcache
cache-to: type=registry,ref=${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}:buildcache,mode=max
```

### 2. `softprops/action-gh-release` with `generate_release_notes` Not Supported on Gitea

- **File**: `release_binary.yaml:67-72`
- **Reviewer**: gitea-reviewer

`generate_release_notes: true` uses GitHub's auto-generated release notes API, which Gitea does not support. File upload semantics may also differ between GitHub and Gitea release asset APIs.

**Recommendation**: Remove `generate_release_notes: true`, or replace with direct Gitea API calls, or test thoroughly against the target Gitea instance.

---

## HIGH

### 3. No CI Workflow for Tests/Lint

- **Reviewer**: architect-reviewer

No `ci.yaml` or equivalent workflow runs tests, linting, or build verification on pull requests or pushes to `main`. The Makefile defines `test`, `lint`, and `cover` targets with existing test files, but none are wired into CI. Code can be merged to `main` without automated quality gates.

**Recommendation**: Add a `ci.yaml` workflow triggered on `push` to `main` and on `pull_request`, running at minimum:

```bash
go test -race ./...
golangci-lint run ./...
go build ./cmd/ociproxy
```

### 4. Docker Images Built from Untested Code on `main` Push

- **Reviewer**: architect-reviewer

`build_docker_image.yaml` triggers on `push` to `main` and builds a Docker image tagged `main` and `latest` with no prior test or lint step. Every push to `main` can produce a broken Docker image tagged `latest`.

**Recommendation**: Either add a test/lint job as a prerequisite within the Docker workflow for `main` branch pushes, restrict the Docker workflow to only trigger on tags, or add a separate CI workflow that must pass first.

### 5. No Concurrency Control

- **File**: both workflows
- **Reviewer**: reliability-reviewer

Neither workflow defines `concurrency` groups. If a tag is pushed, deleted, and re-pushed, multiple workflow runs execute simultaneously, causing race conditions on releases and Docker registry tag overwrites.

**Recommendation**: Add concurrency groups:

```yaml
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true
```

### 6. Tag Re-push Not Idempotent

- **File**: `release_binary.yaml`
- **Reviewer**: reliability-reviewer

`softprops/action-gh-release@v2` fails if a release for the tag already exists. A re-pushed tag triggers a new run that crashes at release creation, after all 5 matrix builds have consumed resources.

**Recommendation**: Handle existing releases or document that tags must never be re-pushed.

### 7. `upload/download-artifact@v4` with `merge-multiple` May Not Work on Gitea

- **File**: `release_binary.yaml:47,57-60`
- **Reviewer**: gitea-reviewer

v4 of these actions uses a new artifact backend. `merge-multiple: true` is a v4-only feature. If Gitea's artifact backend doesn't support this, the `release` job gets an empty `dist/` directory.

**Recommendation**: Test on the target Gitea instance. If v4 doesn't work, downgrade to v3 and download each artifact individually.

---

## MEDIUM

### 8. Dockerfile Lacks Dependency Layer Caching

- **File**: `Dockerfile:5-6`
- **Reviewer**: docker-reviewer

`COPY . .` before `go mod tidy` copies the entire source tree before dependency resolution, breaking Docker layer caching. Any source file change invalidates the dependency download cache.

**Recommendation**:

```dockerfile
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build ...
```

### 9. No Container Vulnerability Scanning

- **Reviewer**: docker-reviewer

No container vulnerability scanning step (Trivy, Grype, Snyk) exists in the workflow. Known CVEs in the base image and Go dependencies are not caught before pushing.

**Recommendation**: Add a Trivy scan step between build and push.

### 10. Base Images Not Pinned by Digest

- **File**: `Dockerfile:1,8`
- **Reviewer**: docker-reviewer

`golang:1.23-alpine` and `alpine:3.20` use mutable tags. Tags can be overwritten with different content, risking supply chain attacks.

**Recommendation**: Pin by digest for the runtime image at minimum.

### 11. GitHub Actions Not Pinned by Commit SHA

- **File**: `build_docker_image.yaml`
- **Reviewer**: docker-reviewer

All Actions are pinned by major version tag, not by commit SHA. A compromised tag could inject malicious code.

**Recommendation**: Pin actions by commit SHA, especially `docker/login-action` which handles credentials.

### 12. Go Version Mismatch

- **Files**: `go.mod`, `release_binary.yaml`, `Dockerfile`
- **Reviewers**: go-reviewer, architect-reviewer

`go.mod` declares `go 1.22`, but the workflow uses `go-version: "1.23"` and Dockerfile uses `golang:1.23-alpine`. While functionally safe due to backward compatibility, the inconsistency signals maintenance drift.

**Recommendation**: Bump `go.mod` to `go 1.23` to align with the build toolchain.

### 13. No Version Info Injected via ldflags

- **File**: `release_binary.yaml`
- **Reviewer**: go-reviewer

Current ldflags are `-s -w` only. No version information is injected. The binary cannot report its version.

**Recommendation**: Add version injection:

```
-ldflags="-s -w -X main.version=${GITHUB_REF_NAME}"
```

### 14. Binary Release Builds Without Running Tests

- **File**: `release_binary.yaml`
- **Reviewers**: go-reviewer, architect-reviewer

The workflow builds binaries directly without running `go test` or `go vet`. A tagged release could ship broken binaries.

**Recommendation**: Add a test job before the build matrix.

### 15. Checksums Without Signing

- **File**: `release_binary.yaml`
- **Reviewer**: go-reviewer

`sha256sum` generates checksums but there is no GPG/cosign signature. Users cannot verify release integrity.

**Recommendation**: Consider cosign signing for the checksums file.

### 16. Dual Workflow Race Condition on Tag Push

- **Reviewers**: reliability-reviewer, architect-reviewer

Both workflows trigger independently on `v*` tags with no coordination. If Docker build fails but binary release succeeds, a partial release exists.

**Recommendation**: Consider a unified release workflow or a coordinator workflow.

### 17. No Timeout Settings

- **File**: both workflows
- **Reviewer**: reliability-reviewer

Neither workflow sets `timeout-minutes`. Default is 6 hours. Multi-arch Docker builds via QEMU can hang.

**Recommendation**: Set explicit timeouts (30 min for Docker, 15 min for Go build, 10 min for release).

### 18. Matrix `fail-fast` Default Cancels All Platforms

- **File**: `release_binary.yaml`
- **Reviewer**: reliability-reviewer

Default `fail-fast: true` means a single platform failure cancels all other builds and skips the release job entirely.

**Recommendation**: Consider `fail-fast: false` if partial releases are acceptable.

### 19. Project Missing LICENSE File

- **Reviewer**: copyright-auditor

No LICENSE file exists. The project is technically "all rights reserved" by default.

**Recommendation**: Add a LICENSE file (MIT, Apache 2.0, etc.) if the project is intended to be shared.

### 20. `permissions` Block Silently Ignored on Gitea

- **File**: `release_binary.yaml:8-9`
- **Reviewer**: gitea-reviewer

Gitea Actions ignores the `permissions` block entirely. Token permissions are determined by token/secret configuration, not the workflow file.

**Recommendation**: Ensure the Gitea runner token has write access. Add a comment noting this Gitea behavior.

---

## LOW

### 21. No Docker Image Signing

- **Reviewer**: docker-reviewer

No cosign/notation signing step. Acceptable for self-hosted registry but worth noting.

### 22. `go mod tidy` in Docker Build

- **File**: `Dockerfile:6`
- **Reviewer**: docker-reviewer

`go mod tidy` modifies `go.mod`/`go.sum` at build time, reducing reproducibility. Should use `go mod download` instead.

### 23. Example Config Bundled in Image

- **File**: `Dockerfile:13`
- **Reviewer**: docker-reviewer

`examples/config.yaml` is copied as the default config. Could ship insecure defaults.

### 24. Docker Workflow Trigger Scope Too Broad

- **File**: `build_docker_image.yaml:4-6`
- **Reviewer**: docker-reviewer

Triggers on every `main` push including doc-only changes. Consider path filters.

### 25. No Failure Notifications

- **Reviewer**: reliability-reviewer

No Slack/email/webhook on workflow failure. Acceptable for small teams.

### 26. GHA Cache Scope May Miss on Tag Builds

- **File**: `build_docker_image.yaml:51-52`
- **Reviewer**: reliability-reviewer

Cache is scoped to branches; tag-triggered builds may not benefit from `main` branch cache.

### 27. Artifact Retention Uses 90-Day Default

- **File**: `release_binary.yaml:47-50`
- **Reviewer**: reliability-reviewer

Default 90-day retention is wasteful since artifacts are immediately consumed by the release job.

**Recommendation**: Set `retention-days: 1`.

### 28. Multi-Arch QEMU Build Performance

- **Reviewer**: reliability-reviewer

Building `linux/arm64` via QEMU emulation is slow. Acceptable for now.

### 29. No Branch Protection or Required Checks

- **Reviewer**: architect-reviewer

Without CI producing status checks, branch protection has nothing to enforce.

### 30. `softprops/action-gh-release` Gitea Compatibility (duplicate)

- **Reviewers**: architect-reviewer, go-reviewer

Duplicate of CRITICAL #2 and HIGH #7. Confirmed by multiple reviewers.

### 31. `.DS_Store` Committed to Repository

- **File**: `.github/.DS_Store`
- **Reviewer**: architect-reviewer

macOS artifact should be excluded via `.gitignore`.

### 32. `{{is_default_branch}}` Needs Verification on Gitea

- **File**: `build_docker_image.yaml:41`
- **Reviewer**: gitea-reviewer

Template evaluated by `docker/metadata-action` checks event payload's `repository.default_branch`. Should work on Gitea but version-dependent.

---

## Copyright & License Audit

**Overall Risk: LOW -- No infringement detected.**

| Check | Result |
|-------|--------|
| License headers from other projects | None found |
| Comments referencing other projects | None found |
| Name remnants from other OCI projects | None found |
| Module path consistency | Consistent |
| README originality | Original |
| External repository URLs | None found |
| Code patterns copied | None detected |
| Dependencies properly declared | Yes |
| Error messages from other projects | None found |
| Test file references | None found |
| Config format copied | Not detected |

OCI protocol constants (`Docker-Distribution-API-Version`, `application/vnd.docker.distribution.manifest.v2+json`) are standard protocol references, not code from other projects.

---

## Recommended Fix Priority

1. **Immediate**: CRITICAL #1, #2 (Gitea compatibility blockers)
2. **Before first release**: HIGH #3-#7 (CI, concurrency, idempotency)
3. **Short term**: MEDIUM #8-#20 (build optimization, security hardening, version alignment)
4. **Optional**: LOW #21-#32 (polish and optimization)

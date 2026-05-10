#!/usr/bin/env bash
# e2e-test.sh — bring up the test environment, run E2E tests, tear down.
# Usage: ./scripts/e2e-test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DEPLOY_DIR="$ROOT_DIR/deploy"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

cleanup() {
    echo -e "\n${YELLOW}Tearing down test environment...${NC}"
    cd "$DEPLOY_DIR" && docker compose down -v 2>/dev/null || true
    cd "$ROOT_DIR"
}

trap cleanup EXIT

echo -e "${GREEN}=== ociproxy E2E Test ===${NC}"

# 1. Build and start the environment
echo -e "${YELLOW}[1/3] Starting Docker Compose environment...${NC}"
cd "$DEPLOY_DIR"
docker compose up -d --build

# Wait for services to be healthy
echo -e "${YELLOW}[2/3] Waiting for services to be ready...${NC}"
for i in $(seq 1 30); do
    if curl -sf http://localhost:8081/v2/ >/dev/null 2>&1; then
        echo -e "${GREEN}ociproxy is ready.${NC}"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo -e "${RED}ociproxy failed to start within 30 seconds.${NC}"
        docker compose logs ociproxy
        exit 1
    fi
    sleep 1
done

# Wait for upstream
for i in $(seq 1 30); do
    if curl -sf http://localhost:5001/v2/ >/dev/null 2>&1; then
        echo -e "${GREEN}upstream registry is ready.${NC}"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo -e "${RED}upstream failed to start within 30 seconds.${NC}"
        docker compose logs upstream
        exit 1
    fi
    sleep 1
done

# 2. Run the tests
echo -e "${YELLOW}[3/3] Running E2E tests...${NC}"
cd "$ROOT_DIR/test/e2e"
export OCIPROXY_E2E_URL="http://localhost:8081"
export OCIPROXY_E2E_UPSTREAM="http://localhost:5001"
export OCIPROXY_E2E_MIRROR="http://localhost:5002"

go test -v -count=1 ./...
TEST_EXIT=$?

if [ $TEST_EXIT -eq 0 ]; then
    echo -e "\n${GREEN}=== All E2E tests passed ===${NC}"
else
    echo -e "\n${RED}=== E2E tests failed (exit code $TEST_EXIT) ===${NC}"
    echo -e "${YELLOW}ociproxy logs:${NC}"
    docker compose -f "$DEPLOY_DIR/docker-compose.yml" logs ociproxy
    echo -e "${YELLOW}upstream logs:${NC}"
    docker compose -f "$DEPLOY_DIR/docker-compose.yml" logs upstream
fi

exit $TEST_EXIT

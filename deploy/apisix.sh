#!/bin/bash
# Unified APISIX deployment for global OCI registry aggregation.
# Routes all major registry domains through a single APISIX gateway
# to the ociproxy backend with proper timeouts and TLS.
#
# Usage: bash deploy/apisix.sh <APISIX_ADMIN_URL> <OCPROXY_IP> [CERT_PATH] [KEY_PATH]
#
# Examples:
#   bash deploy/apisix.sh http://127.0.0.1:9180 192.168.1.105
#   bash deploy/apisix.sh http://127.0.0.1:9180 192.168.1.105 /etc/cert/tls.crt /etc/cert/tls.key

set -euo pipefail

ADMIN="${1:-http://127.0.0.1:9180}"
UPSTREAM="${2:?ociproxy IP is required (e.g., 192.168.1.105)}"
CERT="${3:-}"
KEY="${4:-}"
API_KEY="edd1c9f034335f136f87ad84b625c8f1"

# All registry domains to intercept
read -r -d '' HOSTS_JSON << 'EOF' || true
["registry.piston.ink","docker.io","gcr.io","ghcr.io","quay.io","registry.k8s.io","nvcr.io","docker.elastic.co","mcr.microsoft.com","registry.ollama.ai","lscr.io","public.ecr.aws","cgr.dev","us.gcr.io","eu.gcr.io","asia.gcr.io","k8s.gcr.io"]
EOF

echo "=========================================="
echo " APISIX Global Registry Gateway Setup"
echo "=========================================="
echo ""
echo "  Admin URL:  ${ADMIN}"
echo "  Upstream:   ${UPSTREAM}:15000"
echo "  Domains:    17 registries"
echo ""

# ──────────────────────────────────────────────
# Step 1: Install TLS certificate (if provided)
# ──────────────────────────────────────────────
if [ -n "$CERT" ] && [ -n "$KEY" ]; then
    echo ">>> Installing TLS certificate..."

    PEM_KEY=$(awk '{printf "%s\\n", $0}' "$KEY")
    PEM_CERT=$(awk '{printf "%s\\n", $0}' "$CERT")

    RESP=$(curl -s -w "\n%{http_code}" "${ADMIN}/apisix/admin/ssl/1" -X PUT \
      -H "X-API-KEY: ${API_KEY}" \
      -H "Content-Type: application/json" \
      -d "{
        \"hosts\": ${HOSTS_JSON},
        \"key\": \"${PEM_KEY}\",
        \"cert\": \"${PEM_CERT}\"
      }")

    HTTP_CODE=$(echo "$RESP" | tail -1)
    BODY=$(echo "$RESP" | sed '$d')

    if [ "$HTTP_CODE" = "200" ] || [ "$HTTP_CODE" = "201" ]; then
        echo "  [OK] TLS certificate installed"
    else
        echo "  [WARN] TLS install returned HTTP ${HTTP_CODE}"
        echo "  ${BODY}" | python3 -m json.tool 2>/dev/null || echo "  ${BODY}"
    fi
else
    echo ">>> Skipping TLS certificate (no cert/key paths provided)"
    echo "    To add TLS later: bash deploy/apisix.sh ${ADMIN} ${UPSTREAM} /path/to/tls.crt /path/to/tls.key"
fi

echo ""

# ──────────────────────────────────────────────
# Step 2: Create unified route with proper timeouts
# ──────────────────────────────────────────────
echo ">>> Creating route: all registries -> ${UPSTREAM}:15000..."

RESP=$(curl -s -w "\n%{http_code}" "${ADMIN}/apisix/admin/routes/1" -X PUT \
  -H "X-API-KEY: ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"all-oci-registries\",
    \"hosts\": ${HOSTS_JSON},
    \"methods\": [\"GET\", \"HEAD\", \"POST\", \"PUT\", \"DELETE\", \"PATCH\", \"OPTIONS\"],
    \"uris\": [\"/*\"],
    \"upstream\": {
      \"type\": \"roundrobin\",
      \"nodes\": {
        \"${UPSTREAM}:15000\": 1
      },
      \"timeout\": {
        \"connect\": 10,
        \"send\": 300,
        \"read\": 300
      }
    },
    \"status\": 1
  }")

HTTP_CODE=$(echo "$RESP" | tail -1)
BODY=$(echo "$RESP" | sed '$d')

if [ "$HTTP_CODE" = "200" ] || [ "$HTTP_CODE" = "201" ]; then
    echo "  [OK] Route created successfully"
    echo ""
    echo "  Route details:"
    echo "    Name:        all-oci-registries"
    echo "    ID:          1"
    echo "    Hosts:       17 registry domains"
    echo "    Upstream:    ${UPSTREAM}:15000"
    echo "    Timeout:     connect=10s, send=300s, read=300s"
    echo "    Proxy-rewrite: (none removed)"
else
    echo "  [ERROR] Route creation failed with HTTP ${HTTP_CODE}"
    echo "  ${BODY}" | python3 -m json.tool 2>/dev/null || echo "  ${BODY}"
    exit 1
fi

echo ""

# ──────────────────────────────────────────────
# Step 3: Verify
# ──────────────────────────────────────────────
echo ">>> Verifying route configuration..."
curl -s "${ADMIN}/apisix/admin/routes/1" \
  -H "X-API-KEY: ${API_KEY}" | python3 -m json.tool 2>/dev/null || true

echo ""
echo "=========================================="
echo " Deployment Complete"
echo "=========================================="
echo ""
echo "  Next steps:"
echo ""
echo "  1. Verify APISIX can reach ociproxy:"
echo "     curl http://${UPSTREAM}:15000/v2/"
echo ""
echo "  2. Test from a client machine:"
echo "     curl https://registry.piston.ink/v2/"
echo "     docker pull registry.piston.ink/library/nginx:latest"
echo ""
echo "  3. Client DNS setup - add to /etc/hosts on each Docker client:"
echo "     <APISIX_IP>  registry.piston.ink docker.io gcr.io ghcr.io"
echo ""

#!/usr/bin/env bash
set -euo pipefail

GO_VERSION="${GO_VERSION:-1.23.12}"
INSTALL_DIR="${GO_INSTALL_DIR:-/usr/local}"

echo "Installing Go ${GO_VERSION} to ${INSTALL_DIR}..."

case "$(uname -s)" in
  Linux)   GOOS=linux;;
  Darwin)  GOOS=darwin;;
  *)       echo "Unsupported OS: $(uname -s)" >&2; exit 1;;
esac

case "$(uname -m)" in
  x86_64)  GOARCH=amd64;;
  arm64)   GOARCH=arm64;;
  *)       echo "Unsupported arch: $(uname -m)" >&2; exit 1;;
esac

TARBALL="go${GO_VERSION}.${GOOS}-${GOARCH}.tar.gz"
URL="https://go.dev/dl/${TARBALL}"

if [ -f "${INSTALL_DIR}/go/bin/go" ]; then
  CURRENT=$("${INSTALL_DIR}/go/bin/go" version | awk '{print $3}')
  if [ "$CURRENT" = "go${GO_VERSION}" ]; then
    echo "Go ${GO_VERSION} already installed at ${INSTALL_DIR}/go/bin/go"
    exit 0
  fi
  echo "Replacing ${CURRENT} with go${GO_VERSION}..."
  rm -rf "${INSTALL_DIR}/go"
fi

curl -sSfL "$URL" | tar -C "$INSTALL_DIR" -xzf -

"${INSTALL_DIR}/go/bin/go" version
echo "Go installed: ${INSTALL_DIR}/go/bin/go"

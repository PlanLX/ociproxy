#!/usr/bin/env bash
set -euo pipefail

LINT_VERSION="${GOLANGCI_LINT_VERSION:-v1.64.8}"
INSTALL_DIR="${GO_INSTALL_DIR:-$HOME/go/bin}"

echo "Installing golangci-lint ${LINT_VERSION} to ${INSTALL_DIR}..."

if command -v golangci-lint &>/dev/null; then
  CURRENT=$(golangci-lint version --format short 2>/dev/null || true)
  if [ "$CURRENT" = "${LINT_VERSION#v}" ]; then
    echo "golangci-lint ${LINT_VERSION} already installed"
    exit 0
  fi
  echo "Replacing ${CURRENT} with ${LINT_VERSION}..."
fi

curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
  | sh -s -- -b "$INSTALL_DIR" "$LINT_VERSION"

golangci-lint version
echo "golangci-lint installed: ${INSTALL_DIR}/golangci-lint"

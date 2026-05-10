.PHONY: build test test-e2e cover lint clean run e2e-up e2e-down

BINARY    := ociproxy
CMD       := ./cmd/ociproxy
GO        := go
GOFLAGS   :=
VERSION   ?= dev
LDFLAGS   := -s -w -X main.version=$(VERSION)

build:
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

# Run unit and integration tests (mocked, no Docker required)
test:
	$(GO) test -race -v ./...

cover:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

lint:
	golangci-lint run ./...

clean:
	$(GO) clean
	rm -f $(BINARY) coverage.out

run:
	$(GO) run $(CMD) --config examples/config.yaml

# -- E2E Tests (requires Docker Compose) --

e2e-up:
	cd deploy && docker compose up -d --build

e2e-down:
	cd deploy && docker compose down -v

e2e-wait:
	@for i in $$(seq 1 30); do \
		if curl -sf http://localhost:8081/v2/ >/dev/null 2>&1; then \
			echo "ociproxy ready"; exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "ociproxy failed to start"; exit 1

test-e2e: e2e-up e2e-wait
	OCIPROXY_E2E_URL=http://localhost:8081 \
	OCIPROXY_E2E_UPSTREAM=http://localhost:5001 \
	OCIPROXY_E2E_MIRROR=http://localhost:5002 \
	cd test/e2e && $(GO) test -v -count=1 ./...
	$(MAKE) e2e-down

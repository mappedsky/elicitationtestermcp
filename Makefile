# Local development harness; see README.
MCP_PORT    ?= 8090
ALLOWED_URL ?= http://localhost:$(MCP_PORT)/authorize

COMPOSE = MCP_PORT="$(MCP_PORT)" ALLOWED_URL="$(ALLOWED_URL)" docker compose

.PHONY: test lint up down logs smoke

test:
	go vet ./...
	go test -race ./...

# Checked separately from `test` because a missing gofmt would otherwise pass
# vacuously: `gofmt -l` prints nothing when it is not installed either.
lint:
	@command -v gofmt >/dev/null || { echo "gofmt is not installed" >&2; exit 1; }
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed for:" >&2; echo "$$unformatted" >&2; exit 1; fi

up:
	$(COMPOSE) up -d --build mcp
	@echo "==> stateless MCP on http://localhost:$(MCP_PORT)/mcp"
	@echo "==> legacy  MCP on http://localhost:$(MCP_PORT)/mcp/legacy"
	@echo "==> trusted URL    $(ALLOWED_URL)"

down:
	$(COMPOSE) down --remove-orphans

logs:
	$(COMPOSE) logs -f mcp

# Start the server and drive both endpoints end to end, curl standing in for
# the client.
smoke: up
	MCP_PORT=$(MCP_PORT) ./scripts/smoke.sh

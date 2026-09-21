# agentloop — developer and operator tasks.
#
# The service is a single Go binary: cmd/agentloop. `make` with no target
# lists what is available. See docs/USAGE.md for the full walkthrough.

BINARY  := agentloop
PKG     := ./cmd/agentloop

# 8080 belongs to onegw on this machine (it binds 127.0.0.1:8080, and xdev's
# models.yml points there). agentloop defaults to 8080 too, so it would fail
# to bind — 8081 by default here, override with `make run PORT=9090`.
PORT    ?= 8081

GO      ?= go

.PHONY: help build run test lint vet fmt fmt-check tidy prd check smoke clean

help: ## list the available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'

build: ## build the server binary (./agentloop, gitignored)
	$(GO) build -o $(BINARY) $(PKG)

run: build ## run the server on $(PORT)
	AGENTLOOP_PORT=$(PORT) ./$(BINARY)

test: ## run the test suite
	$(GO) test ./...

check: fmt-check vet test lint prd ## everything CI runs — run this before opening a PR

fmt-check: ## fail if any file is not gofmt-clean (what CI's gofmt step does)
	@unformatted="$$(gofmt -l ./cmd ./internal)"; \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

lint: ## golangci-lint (config pinned in .golangci.yml; same scope as CI)
	golangci-lint run ./cmd/... ./internal/...

vet: ## go vet
	$(GO) vet ./...

fmt: ## gofmt the tree
	$(GO) fmt ./...

tidy: ## go mod tidy
	$(GO) mod tidy

prd: ## the PRD's own consistency check (12 properties, self-tested)
	python3 docs/check-prd.py

smoke: ## submit one run and print the response
	@curl -sS -X POST localhost:$(PORT)/v1/runs \
		-H 'Content-Type: application/json' \
		-d '{"goal":"smoke test","context":"make smoke"}'
	@echo

clean: ## remove the built binary
	rm -f $(BINARY)

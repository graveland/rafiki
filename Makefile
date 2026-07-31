SHELL := bash

.DEFAULT_GOAL := help

# Internal Go module proxy: serves private timescale modules (savannah-common)
# to environments with no GitHub credentials, e.g. CI runners. Same recipe as
# savannah-deployer/hot-forge; overridable from the environment.
GOPROXY   := $(or $(strip $(GOPROXY)),   REDACTED,direct)
GONOPROXY := $(or $(strip $(GONOPROXY)), none)
GOPRIVATE := $(strip $(GOPRIVATE))

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. (Standard kubebuilder recipe, as used by
# other kubebuilder-based projects.)

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# Run the standalone server locally in --dev mode (auto-migrate, client token
# "dev", :8035) against the local rafiki-test-db container. Sources a
# gitignored .env from the repo root when present (keys + optional config;
# see .env.example). RAFIKI_DB precedence: .env wins over the inherited
# environment; the DSN below is only the fallback when neither sets it.
.PHONY: run
run: ## Run the server locally: serve --dev on :8035, sourcing .env.
	@set -a; [ -f .env ] && . ./.env; set +a; \
	export RAFIKI_DB="$${RAFIKI_DB:-postgres://postgres:postgres@localhost:5433/rafiki_live?sslmode=disable}"; \
	go run ./cmd/rafiki serve --dev

.PHONY: build
build: ## Build the standalone binary to bin/rafiki.
	go build -o bin/rafiki ./cmd/rafiki

# The docs (README, docs/agent-cli.md) spell every command as a bare
# `rafiki agent ...`, which only works once the binary is on PATH. Honours
# GOBIN, else $(go env GOPATH)/bin — make sure that directory is on your PATH.
.PHONY: install
install: ## Install the rafiki binary to GOBIN (or GOPATH/bin).
	go install ./cmd/rafiki
	@bin="$$(go env GOBIN)"; [ -n "$$bin" ] || bin="$$(go env GOPATH)/bin"; \
	echo "installed $$bin/rafiki"; \
	command -v rafiki >/dev/null || echo "warning: $$bin is not on your PATH"

# Interactive by default; pass extra flags via ARGS, e.g.:
#   make claude ARGS='-p "what changed today"'
# RAFIKI_URL / RAFIKI_TOKEN override the target server (environment wins over
# .env, which wins over the make-run defaults). Only those two client-side
# values are read from .env — the server's upstream keys
# (ANTHROPIC/OPENROUTER_API_KEY) must NEVER reach the client: Claude Code
# would present the real Anthropic key to rafiki's static auth (401) and
# warn about conflicting auth sources. They are explicitly unset besides.
# A per-invocation X-Rafiki-Session id (RAFIKI_SESSION overrides) correlates
# every turn of the launched session onto ONE captured conversation — without
# it the proxy falls back to one conversation per request.
.PHONY: claude
claude: ## Launch Claude Code against the local rafiki server (ARGS= for flags).
	@if [ -f .env ]; then \
		RAFIKI_URL="$${RAFIKI_URL:-$$(sed -n 's/^RAFIKI_URL=//p' .env)}"; \
		RAFIKI_TOKEN="$${RAFIKI_TOKEN:-$$(sed -n 's/^RAFIKI_TOKEN=//p' .env)}"; \
	fi; \
	unset ANTHROPIC_API_KEY OPENROUTER_API_KEY; \
	url="$${RAFIKI_URL:-http://localhost:8035}"; \
	curl -sf --max-time 2 "$$url/healthz" >/dev/null || { \
		echo "error: rafiki is not answering at $$url — start it with 'make run' first"; exit 1; }; \
	session="$${RAFIKI_SESSION:-make-claude-$$(uuidgen | tr '[:upper:]' '[:lower:]')}"; \
	ANTHROPIC_BASE_URL="$$url" ANTHROPIC_AUTH_TOKEN="$${RAFIKI_TOKEN:-dev}" \
	ANTHROPIC_CUSTOM_HEADERS="X-Rafiki-Session: $$session" exec claude $(ARGS)

.PHONY: configure-proxy
configure-proxy: ## Point the Go toolchain at the internal module proxy (CI).
	go env -w GOPROXY="$(GOPROXY)"
	go env -w GONOPROXY="$(GONOPROXY)"
	go env -w GOPRIVATE="$(GOPRIVATE)"

##@ Quality

.PHONY: check
check: vet lint test ## Run vet + lint + tests (the full local gate).

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run ./...

# Sources .env the same way `run` does, so the DB-backed tests actually run
# outside a direnv-hooked shell. Without this they skip on a missing
# RAFIKI_TEST_DSN and the run still reports success — ~94 tests silently not
# run, which is indistinguishable from a clean pass. The guard below makes that
# state loud instead: an unset DSN is announced, not inferred from a test count
# nobody checks. Use test-nodb when you deliberately want the short run.
.PHONY: test
test: ## Run tests with -race, sourcing .env so DB-backed tests run.
	@set -a; [ -f .env ] && . ./.env; set +a; \
	if [ -z "$${RAFIKI_TEST_DSN}" ]; then \
		echo "WARNING: RAFIKI_TEST_DSN unset (no .env?) — every DB-backed test will SKIP."; \
		echo "         A green result here does NOT mean the store/insights code was exercised."; \
	fi; \
	go test -race ./...

.PHONY: test-nodb
test-nodb: ## Run only the DSN-free tests (explicitly skips DB-backed ones).
	RAFIKI_TEST_DSN= go test -race ./...

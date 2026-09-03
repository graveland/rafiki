SHELL := bash

.DEFAULT_GOAL := help

GO      ?= go
BIN_DIR := bin

# Where `make install` puts the binaries. ~/.local/bin is the XDG counterpart to
# the paths package's locations, and sidesteps a ~/bin that may already hold a
# pi-controller install.
DESTDIR ?= $(HOME)/.local/bin

DAEMON_BIN := rafikid
CLI_BIN    := rafiki

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. (Standard kubebuilder recipe, as used by
# other kubebuilder-based projects.)

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# Run rafikid in the foreground. rafikid serves the proxy face itself on :8035 —
# the port the old standalone `rafiki serve` used to hold — so anything already
# pointed at a local rafiki keeps working, and pi/claude children get capture,
# failover and model resolution without a second process to start.
#
# RAFIKI_SERVE_TOKEN is retired: the face authenticates against the `users`
# table now, not a shared secret in the environment. Without RAFIKI_DB, only
# the daemon's per-boot child token authenticates — which `make claude`, a
# separate human-invoked process, cannot know. So a fresh dev daemon needs
# RAFIKI_DB set (see below) and, once, a real user:
#
#   go run ./cmd/rafiki user create dev
#
# That mints a token and writes it to ~/.config/rafiki/token (0600), which
# `make claude` picks up automatically (RAFIKI_TOKEN unset falls back to that
# file) — no export needed. Re-run it any time the token file is missing or
# stale; `rafiki user list` shows what already exists.
#
# RAFIKI_DB is what makes turns land in the conversations schema, for
# proxied children and in-process agent children alike — the server's own DSN
# and the agent runtime's are the same variable, sourced straight from .env.
#
# Note this does NOT migrate, where the old standalone `rafiki serve --dev` did:
# run `go run ./cmd/rafikid migrate` once against a fresh database.
.PHONY: run
run: ## Run rafikid in the foreground, serving the proxy face on :8035.
	@set -a; [ -f .env ] && . ./.env; set +a; \
	go run ./cmd/rafikid

# NOTE (merge): fundi's `build` also depended on build-attach. It no longer
# does — the TUI is in-process Go (pkg/tui), built into `rafiki` itself, so
# there is no separate attach artifact to build. The two Go binaries build from
# a bare `git clone` with nothing but a Go toolchain.
.PHONY: build
build: build-daemon build-cli ## Build both Go binaries into bin/.

.PHONY: build-daemon
build-daemon: ## Build the rafiki daemon (bin/rafikid).
	mkdir -p $(BIN_DIR)
	$(eval GO_VERSION := $(shell git describe --always --dirty))
	$(GO) build -ldflags "-s -w -X go.graveland.dev/rafiki/pkg/version.Version=$(GO_VERSION)" -o $(BIN_DIR)/$(DAEMON_BIN) ./cmd/rafikid

.PHONY: build-cli
build-cli: ## Build the rafiki CLI client (bin/rafiki).
	mkdir -p $(BIN_DIR)
	$(eval GO_VERSION := $(shell git describe --always --dirty))
	$(GO) build -ldflags "-s -w -X go.graveland.dev/rafiki/pkg/version.Version=$(GO_VERSION)" -o $(BIN_DIR)/$(CLI_BIN) ./cmd/rafiki

# There is no CI on this repo, so nothing else exercises GOOS=linux — the
# daemon and CLI silently bitrotted on Linux for an entire phase until this
# target's absence was flagged in review.
.PHONY: build-linux
build-linux: ## Cross-compile for linux/amd64 (catches Linux-only breaks; no CI runs this).
	mkdir -p $(BIN_DIR)/linux
	GOOS=linux GOARCH=amd64 $(GO) vet ./...
	$(eval GO_VERSION := $(shell git describe --always --dirty))
	GOOS=linux GOARCH=amd64 $(GO) build -ldflags "-s -w -X go.graveland.dev/rafiki/pkg/version.Version=$(GO_VERSION)" -o $(BIN_DIR)/linux/$(DAEMON_BIN) ./cmd/rafikid
	GOOS=linux GOARCH=amd64 $(GO) build -ldflags "-s -w -X go.graveland.dev/rafiki/pkg/version.Version=$(GO_VERSION)" -o $(BIN_DIR)/linux/$(CLI_BIN) ./cmd/rafiki

# The paths package (via rafikid -h) is the one authority for resolved config
# paths — this used to hand-roll its own shell guesses and got all four of
# these wrong: it hardcoded ~/.config/rafiki ignoring $XDG_CONFIG_HOME, printed
# the literal unexpanded "~" instead of an actual path, read the invoking
# shell's $RAFIKI_* values rather than what the daemon itself resolves, and
# never mentioned presets.json at all. Shelling out to the built binary's own
# -h output means this can never drift from the code again. Depends on
# build-daemon (not build-cli): `rafikid`, not `rafiki`, is the process that
# actually reads these paths at runtime.
.PHONY: print-config
print-config: build-daemon ## Show the resolved rafiki config paths (shells out to rafikid -h).
	@$(BIN_DIR)/$(DAEMON_BIN) -h | awk '/^  socket /,/^  mcp /'
	@printf "  %-12s %s\n" "agent db" "$${RAFIKI_DB:-<unset — rafikid will not start>}"

# Interactive by default; pass extra flags via ARGS, e.g.:
#   make claude ARGS='-p "what changed today"'
# RAFIKI_URL / RAFIKI_TOKEN override the target server (environment wins over
# .env, which wins over the make-run defaults).
#
# The launch itself lives in `rafiki claude`, not here. It was inline shell
# until it needed three things a Makefile recipe should not be doing: stripping
# inherited ANTHROPIC_* vars so a nested session does not adopt its parent's
# conversation, registering the model as a custom /model option so Claude Code
# does not reject non-Anthropic ids against its client-side allowlist before a
# request ever leaves, and pinning the auto-compact threshold to the model's
# real context window from the OpenRouter catalog. See cmd/rafiki/claude.go —
# and note that sourcing .env here is now safe precisely because the command
# strips the provider keys itself.
.PHONY: claude
claude: ## Launch Claude Code against the local rafiki server (ARGS= for flags).
	@set -a; [ -f .env ] && . ./.env; set +a; \
	go run ./cmd/rafiki claude $(ARGS)

##@ Install

# Copies the built binaries to $(DESTDIR) (default ~/.local/bin), then checks
# whether that is actually the copy $PATH will find.
#
# The shadowing check is not paranoia: rafiki began as a fork of pi-controller,
# whose own install may already have put a `pi-controller` (and historically a
# `pic`) on $PATH ahead of ~/.local/bin. Installing successfully and then
# running a different binary than the one you just built is a genuinely
# confusing failure, so say so at install time.
.PHONY: install
install: build ## Install rafikid + rafiki to $(DESTDIR).
	@mkdir -p "$(DESTDIR)"
	@for b in $(DAEMON_BIN) $(CLI_BIN); do \
	    rm -f "$(DESTDIR)/$$b"; \
	    cp "$(BIN_DIR)/$$b" "$(DESTDIR)/$$b" || exit 1; \
	    echo "installed $(DESTDIR)/$$b"; \
	done
	@for b in $(DAEMON_BIN) $(CLI_BIN); do \
	    found=$$(command -v "$$b" 2>/dev/null || true); \
	    if [ -z "$$found" ]; then \
	        echo "warning: $$b is not on \$$PATH at all — add $(DESTDIR) to it"; \
	    elif [ "$$found" != "$(DESTDIR)/$$b" ]; then \
	        echo "warning: \$$PATH finds a different $$b first:"; \
	        echo "           $$found"; \
	        echo "         shadowing the one just installed at $(DESTDIR)/$$b"; \
	    fi; \
	done

# The edit loop for daemon code: build, install, and bounce the running daemon
# so it is actually executing what you just built. Without the restart, `make
# install` leaves the old rafikid resident and the change appears not to have
# landed — the daemon answers RPCs (ctrl_list_models among them) from whatever
# binary launchd started, not from $(DESTDIR).
#
# `service restart`, deliberately NOT `service install`. Install rewrites the
# plist from a template carrying only HOME and PATH (cmd/rafiki/service_darwin.go),
# so it silently drops any hand-added environment. RAFIKI_DB is the one
# that matters: lose it and agent conversations go back to in-memory with no
# per-turn cost recorded anywhere, and the only symptom is a "mem-" session id
# where a UUIDv7 should be.
#
# The restart runs the just-installed binary by explicit path rather than
# whatever `rafiki` resolves to, so a shadowing install elsewhere on $PATH
# cannot bounce the service with a different client's idea of where things
# live — the same failure the warnings above exist to catch.
#
# build-attach ran before install (prerequisite order, not just listed for
# documentation). With the TUI in-process there is nothing extra to rebuild, so
# redeploy is just install + restart.
.PHONY: redeploy
redeploy: install ## Rebuild + install, then restart the rafiki daemon.
	@"$(DESTDIR)/$(CLI_BIN)" service restart
	@"$(DESTDIR)/$(CLI_BIN)" service status

##@ Quality

PROTOC ?= protoc

bin/protoc-gen-go:
	go build -o bin/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go

bin/protoc-gen-connect-go:
	go build -o bin/protoc-gen-connect-go connectrpc.com/connect/cmd/protoc-gen-connect-go

.PHONY: proto
proto: bin/protoc-gen-go bin/protoc-gen-connect-go ## Regenerate Go code from proto/ definitions (executorpb + pkg/gen).
	$(PROTOC) \
		--proto_path=proto/rafiki/executor/v1 \
		--go_out=pkg/executorpb \
		--go_opt=paths=source_relative \
		--connect-go_out=pkg/executorpb \
		--connect-go_opt=paths=source_relative \
		proto/rafiki/executor/v1/executor.proto
	gofmt -w pkg/executorpb
	mkdir -p pkg/darajapb
	$(PROTOC) \
		--plugin=protoc-gen-go=bin/protoc-gen-go \
		--plugin=protoc-gen-connect-go=bin/protoc-gen-connect-go \
		--proto_path=proto \
		--go_out=pkg/darajapb --go_opt=module=go.graveland.dev/rafiki/pkg/darajapb \
		--connect-go_out=pkg/darajapb --connect-go_opt=module=go.graveland.dev/rafiki/pkg/darajapb \
		proto/rafiki/daraja/v1/daraja.proto
	gofmt -w pkg/darajapb
	rm -rf pkg/gen
	mkdir -p pkg/gen
	$(PROTOC) \
		--plugin=protoc-gen-go=bin/protoc-gen-go \
		--plugin=protoc-gen-connect-go=bin/protoc-gen-connect-go \
		--proto_path=proto \
		--go_out=pkg/gen --go_opt=module=go.graveland.dev/rafiki/pkg/gen \
		--connect-go_out=pkg/gen --connect-go_opt=module=go.graveland.dev/rafiki/pkg/gen \
		proto/rafiki/v1/*.proto
	$(MAKE) fmt

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
# -count=1 is not optional: test/integration builds the daemon binary in a
# subprocess inside TestMain, so its import graph is only pkg/protocol and a
# cached PASS survives any change to the daemon.
.PHONY: test
test: ## Run tests with -race, sourcing .env so DB-backed tests run.
	@set -a; [ -f .env ] && . ./.env; set +a; \
	if [ -z "$${RAFIKI_TEST_DSN}" ]; then \
		echo "WARNING: RAFIKI_TEST_DSN unset (no .env?) — every DB-backed test will SKIP."; \
		echo "         A green result here does NOT mean the store/insights code was exercised."; \
	fi; \
	go test -race -count=1 ./...

.PHONY: test-nodb
test-nodb: ## Run only the DSN-free tests (explicitly skips DB-backed ones).
	RAFIKI_TEST_DSN= go test -race -count=1 ./...

.PHONY: fmt
fmt: ## gofmt all Go sources.
	$(GO) fmt ./...

.PHONY: clean
clean: ## Remove built binaries.
	rm -rf $(BIN_DIR)

SHELL := bash

.DEFAULT_GOAL := help

GO      ?= go
BIN_DIR := bin

# Where `make install` puts the binaries. ~/.local/bin is the XDG counterpart to
# the paths package's locations, and sidesteps a ~/bin that may already hold a
# pi-controller install.
DESTDIR ?= $(HOME)/.local/bin

RAFIKI_BIN := rafiki
DAEMON_BIN := fundid
CLI_BIN    := fundi
ATTACH_BIN := fundi-attach

# These must be defined ABOVE build-attach, not down in the pi section with the
# rules that use them: make expands a prerequisite list when it parses the rule,
# so `build-attach: $(PI_MODULES) $(PI_DIST)` against not-yet-defined variables
# silently becomes a rule with no prerequisites — pi is never built first, and
# attach bundles whatever stale dist happens to be on disk.
PI_DIR  := pi
PI_PKG  := $(PI_DIR)/packages/coding-agent
PI_DIST := $(PI_PKG)/dist/cli.js
PI_MODULES := $(PI_DIR)/node_modules

# Every package's TypeScript source. $(PI_DIST) must depend on these: keying the
# rebuild on package.json alone meant editing a .ts file never recompiled dist,
# so `make`/`pi-install` shipped a stale bundle from previously-compiled JS.
PI_SRC := $(shell find $(PI_DIR)/packages/*/src -type f \( -name '*.ts' -o -name '*.tsx' \) ! -name '*.test.ts' 2>/dev/null)

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. (Standard kubebuilder recipe, as used by
# other kubebuilder-based projects.)

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# Run the standalone server locally in --dev mode (auto-migrate, client token
# "dev", :8035) against the local rafiki-test-db container. Sources a
# gitignored .env from the repo root when present (keys + optional config;
# see .env.example). RAFIKI_DB precedence: .env wins over the inherited
# environment; the DSN below is only the fallback when neither sets it.
.PHONY: run
run: ## Run the rafiki server locally: serve --dev on :8035, sourcing .env.
	@set -a; [ -f .env ] && . ./.env; set +a; \
	export RAFIKI_DB="$${RAFIKI_DB:-postgres://postgres:postgres@localhost:5433/rafiki_live?sslmode=disable}"; \
	go run ./cmd/rafiki serve --dev

# NOTE (merge): fundi's `build` also depended on build-attach. It no longer
# does. build-attach needs bun AND the pi submodule initialised, so folding it
# into the default build meant `make build` hard-failed on a fresh clone that
# had neither — for a TUI that most work on this repo never touches. The three
# Go binaries build from a bare `git clone` with nothing but a Go toolchain;
# attach stays one explicit target away.
.PHONY: build
build: build-rafiki build-daemon build-cli ## Build all three Go binaries into bin/.

.PHONY: build-rafiki
build-rafiki: ## Build the rafiki proxy/server binary (bin/rafiki).
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/$(RAFIKI_BIN) ./cmd/rafiki

.PHONY: build-daemon
build-daemon: ## Build the fundi daemon (bin/fundid).
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/$(DAEMON_BIN) ./cmd/fundid

.PHONY: build-cli
build-cli: ## Build the fundi CLI client (bin/fundi).
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/$(CLI_BIN) ./cmd/fundi

# There is no CI on this repo, so nothing else exercises GOOS=linux — the
# daemon and CLI silently bitrotted on Linux for an entire phase until this
# target's absence was flagged in review. fundi-attach is excluded: it ships a
# native binary via bun, which does not cross-compile from here.
.PHONY: build-linux
build-linux: ## Cross-compile for linux/amd64 (catches Linux-only breaks; no CI runs this).
	mkdir -p $(BIN_DIR)/linux
	GOOS=linux GOARCH=amd64 $(GO) vet ./...
	GOOS=linux GOARCH=amd64 $(GO) build -o $(BIN_DIR)/linux/$(RAFIKI_BIN) ./cmd/rafiki
	GOOS=linux GOARCH=amd64 $(GO) build -o $(BIN_DIR)/linux/$(DAEMON_BIN) ./cmd/fundid
	GOOS=linux GOARCH=amd64 $(GO) build -o $(BIN_DIR)/linux/$(CLI_BIN) ./cmd/fundi

# fundi-attach bundles pi (via attach/package.json -> file:../$(PI_PKG)), so pi
# must be built first. Fail loudly if the submodule isn't initialised.
.PHONY: build-attach
build-attach: $(PI_MODULES) $(PI_DIST) ## Bundle the fundi-attach TUI binary (recompiles pi dist first).
	@if command -v bun >/dev/null 2>&1; then \
	    cd attach && bun install --silent && bun run build; \
	else \
	    echo "skipping fundi-attach build: bun not installed (install via 'brew install oven-sh/bun/bun')"; \
	fi

# The paths package (via fundid -h) is the one authority for resolved config
# paths — this used to hand-roll its own shell guesses and got all four of
# these wrong: it hardcoded ~/.config/fundi ignoring $XDG_CONFIG_HOME, printed
# the literal unexpanded "~" instead of an actual path, read the invoking
# shell's $FUNDI_* values rather than what the daemon itself resolves, and
# never mentioned presets.json at all. Shelling out to the built binary's own
# -h output means this can never drift from the code again. Depends on
# build-daemon (not build-cli): `fundid`, not `fundi`, is the process that
# actually reads these paths at runtime.
.PHONY: print-config
print-config: build-daemon ## Show the resolved fundi config paths (shells out to fundid -h).
	@$(BIN_DIR)/$(DAEMON_BIN) -h | awk '/^  socket /,/^  mcp /'
	@printf "  %-12s %s\n" "agent db" "$${FUNDI_AGENT_DB:-<unset — NO COST DATA>}"

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

##@ Install

# Copies the built binaries to $(DESTDIR) (default ~/.local/bin), then checks
# whether that is actually the copy $PATH will find.
#
# The shadowing check is not paranoia: fundi began as a fork of pi-controller,
# whose own install may already have put a `pi-controller` (and historically a
# `pic`) on $PATH ahead of ~/.local/bin. Installing successfully and then
# running a different binary than the one you just built is a genuinely
# confusing failure, so say so at install time.
#
# NOTE (merge): rafiki previously installed via `go install ./cmd/rafiki` to
# GOBIN. It now goes through the same build-to-bin/ + copy-to-DESTDIR path as
# the fundi binaries, so one command installs all three consistently and the
# shadowing check covers rafiki too. Set DESTDIR=$(go env GOPATH)/bin for the
# old destination.
#
# fundi-attach is copied only when it has been built — build-attach needs bun
# and the pi submodule, and skips itself when bun is absent.
.PHONY: install
install: build ## Install rafiki + fundid + fundi (+ fundi-attach if built) to $(DESTDIR).
	@mkdir -p "$(DESTDIR)"
	@for b in $(RAFIKI_BIN) $(DAEMON_BIN) $(CLI_BIN); do \
	    cp "$(BIN_DIR)/$$b" "$(DESTDIR)/$$b" || exit 1; \
	    echo "installed $(DESTDIR)/$$b"; \
	done
	@if [ -x "$(BIN_DIR)/$(ATTACH_BIN)" ]; then \
	    cp "$(BIN_DIR)/$(ATTACH_BIN)" "$(DESTDIR)/$(ATTACH_BIN)" || exit 1; \
	    echo "installed $(DESTDIR)/$(ATTACH_BIN)"; \
	else \
	    echo "note: $(BIN_DIR)/$(ATTACH_BIN) not built — run 'make build-attach' (needs bun + the pi submodule)"; \
	fi
	@for b in $(RAFIKI_BIN) $(DAEMON_BIN) $(CLI_BIN); do \
	    found=$$(command -v "$$b" 2>/dev/null || true); \
	    if [ -z "$$found" ]; then \
	        echo "warning: $$b is not on \$$PATH at all — add $(DESTDIR) to it"; \
	    elif [ "$$found" != "$(DESTDIR)/$$b" ]; then \
	        echo "warning: \$$PATH finds a different $$b first:"; \
	        echo "           $$found"; \
	        echo "         shadowing the one just installed at $(DESTDIR)/$$b"; \
	    fi; \
	done

##@ pi submodule lifecycle

# fundi-attach links against the bundled pi tree at $(PI_DIR), and the daemon
# spawns the matching pi binary off PATH. `pi-install` keeps the global install
# in lock-step with the submodule pin.
#
# NOTE (merge): the submodule points at a private host, so a public
# `clone --recursive` breaks here. Replacing it with the published
# @earendil-works/pi-* packages at 0.80.6 is a tracked follow-up; until it
# lands this repo should not be pushed to its public remote.

# Compile only — deliberately NOT `npm run build` at the pi root: packages/ai's
# build script first re-fetches the model catalogs from live provider APIs,
# which makes every build non-reproducible and leaves the submodule dirty with
# synced drift. The generated catalogs are committed source (upstream's model
# too); resync them deliberately with `make pi-refresh-catalogs` and commit the
# result in the submodule.
$(PI_DIST): $(PI_PKG)/package.json $(PI_SRC)
	cd $(PI_DIR) && npm install
	cd $(PI_DIR)/packages/tui && npm run build
	cd $(PI_DIR)/packages/ai && npx tsgo -p tsconfig.build.json
	cd $(PI_DIR)/packages/agent && npm run build
	cd $(PI_DIR)/packages/coding-agent && npm run build
	cd $(PI_DIR)/packages/orchestrator && npm run build

# pi's deps (yaml, chalk, typebox, ...) hoist to $(PI_MODULES) and are imported
# by the bundled dist when attach is compiled, so node_modules must exist even
# when dist/ is already built. This is kept separate from $(PI_DIST): the bundle
# only needs node_modules + a present dist, whereas rebuilding dist runs the pi
# toolchain (npm run build) which requires a newer Node than some hosts have.
# Keyed on the lockfile so it reinstalls when deps change or node_modules is gone.
$(PI_MODULES): $(PI_DIR)/package-lock.json
	cd $(PI_DIR) && npm install
	@touch $@

$(PI_PKG)/package.json:
	@$(MAKE) --no-print-directory pi-not-initialised

# $(PI_MODULES) depends on the lockfile, which has no rule of its own — so an
# uninitialised submodule died with make's raw "No rule to make target
# 'pi/package-lock.json'" before ever reaching the friendly message above. Both
# entry points now route to the same explanation.
$(PI_DIR)/package-lock.json:
	@$(MAKE) --no-print-directory pi-not-initialised

.PHONY: pi-not-initialised
pi-not-initialised:
	@echo "The pi submodule at ./$(PI_DIR) is not initialised." >&2
	@echo "Run 'make bootstrap' (fresh clone), or:" >&2
	@echo "    git submodule update --init --recursive" >&2
	@echo >&2
	@echo "Only the fundi-attach TUI needs it — 'make build' works without it." >&2
	@exit 1

.PHONY: pi-build
pi-build: $(PI_DIST) ## Recompile the pi submodule's TypeScript to dist.

.PHONY: pi-install
pi-install: $(PI_DIST) ## Install the pi binary globally (the daemon-spawned backend).
	npm install -g ./$(PI_PKG)

.PHONY: pi-update
pi-update: ## Bump the pi submodule to its remote tip, then rebuild + install.
	git submodule update --remote $(PI_DIR)
	$(MAKE) pi-build pi-install

.PHONY: pi-refresh-catalogs
pi-refresh-catalogs: $(PI_MODULES) ## Regenerate pi's model catalogs from live provider APIs.
	cd $(PI_DIR)/packages/ai && npm run generate-models && npm run generate-image-models

.PHONY: bootstrap
bootstrap: ## Fresh-clone setup — init submodules, build and install everything.
	git submodule update --init --recursive
	$(MAKE) pi-build pi-install build

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

.PHONY: fmt
fmt: ## gofmt all Go sources.
	$(GO) fmt ./...

.PHONY: clean
clean: ## Remove built binaries.
	rm -rf $(BIN_DIR)

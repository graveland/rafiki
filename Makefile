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
ATTACH_BIN := rafiki-attach

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

# Run rafikid in the foreground. rafikid serves the proxy face itself on :8035 —
# the port the old standalone `rafiki serve` used to hold — so anything already
# pointed at a local rafiki keeps working, and pi/claude children get capture,
# failover and model resolution without a second process to start.
#
# RAFIKI_SERVE_TOKEN=dev makes the face accept the same token `make claude`
# sends as RAFIKI_TOKEN. The daemon also mints a per-boot token for its own
# children; this is the extra one for humans and tools, which cannot know a
# per-boot secret.
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
	export RAFIKI_SERVE_TOKEN="$${RAFIKI_SERVE_TOKEN:-dev}"; \
	go run ./cmd/rafikid

# NOTE (merge): fundi's `build` also depended on build-attach. It no longer
# does. build-attach needs bun AND the pi submodule initialised, so folding it
# into the default build meant `make build` hard-failed on a fresh clone that
# had neither — for a TUI that most work on this repo never touches. The three
# Go binaries build from a bare `git clone` with nothing but a Go toolchain;
# attach stays one explicit target away.
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
# target's absence was flagged in review. rafiki-attach is excluded: it ships a
# native binary via bun, which does not cross-compile from here.
.PHONY: build-linux
build-linux: ## Cross-compile for linux/amd64 (catches Linux-only breaks; no CI runs this).
	mkdir -p $(BIN_DIR)/linux
	GOOS=linux GOARCH=amd64 $(GO) vet ./...
	$(eval GO_VERSION := $(shell git describe --always --dirty))
	GOOS=linux GOARCH=amd64 $(GO) build -ldflags "-s -w -X go.graveland.dev/rafiki/pkg/version.Version=$(GO_VERSION)" -o $(BIN_DIR)/linux/$(DAEMON_BIN) ./cmd/rafikid
	GOOS=linux GOARCH=amd64 $(GO) build -ldflags "-s -w -X go.graveland.dev/rafiki/pkg/version.Version=$(GO_VERSION)" -o $(BIN_DIR)/linux/$(CLI_BIN) ./cmd/rafiki

# rafiki-attach bundles pi (via attach/package.json -> file:../$(PI_PKG)), so pi
# must be built first. Fail loudly if the submodule isn't initialised.
.PHONY: build-attach
build-attach: $(PI_MODULES) $(PI_DIST) ## Bundle the rafiki-attach TUI binary (recompiles pi dist first).
	@if command -v bun >/dev/null 2>&1; then \
	    cd attach && bun install --silent && bun run build; \
	else \
	    echo "skipping rafiki-attach build: bun not installed (install via 'brew install oven-sh/bun/bun')"; \
	fi

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
	@printf "  %-12s %s\n" "agent db" "$${RAFIKI_DB:-<unset — NO COST DATA>}"

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
#
# rafiki-attach is copied only when it has been built — build-attach needs bun
# and the pi submodule, and skips itself when bun is absent.
#
# It does not travel alone. bun compiles it to a static binary, and pi's
# config.js then resolves package assets relative to dirname(process.execPath)
# — so package.json, theme/*.json and assets/ must sit NEXT TO the binary
# wherever it lands, not merely in bin/. Copying only the executable produces a
# rafiki-attach that runs fine from bin/ and dies from $(DESTDIR) with
# "ENOENT: .../theme/dark.json" followed by "Theme not initialized", which
# reads like a TUI bug rather than a missing file. attach/scripts/copy-pi-assets.ts
# is the authority for this list and documents the same contract.
.PHONY: install
install: build ## Install rafikid + rafiki (+ rafiki-attach if built) to $(DESTDIR).
	@mkdir -p "$(DESTDIR)"
	@for b in $(DAEMON_BIN) $(CLI_BIN); do \
	    rm -f "$(DESTDIR)/$$b"; \
	    cp "$(BIN_DIR)/$$b" "$(DESTDIR)/$$b" || exit 1; \
	    echo "installed $(DESTDIR)/$$b"; \
	done
	@if [ -x "$(BIN_DIR)/$(ATTACH_BIN)" ]; then \
	    rm -f "$(DESTDIR)/$(ATTACH_BIN)"; \
	    cp "$(BIN_DIR)/$(ATTACH_BIN)" "$(DESTDIR)/$(ATTACH_BIN)" || exit 1; \
	    echo "installed $(DESTDIR)/$(ATTACH_BIN)"; \
	    for a in package.json theme assets; do \
	        if [ -e "$(BIN_DIR)/$$a" ]; then \
	            rm -rf "$(DESTDIR)/$$a"; \
	            cp -R "$(BIN_DIR)/$$a" "$(DESTDIR)/$$a" || exit 1; \
	            echo "installed $(DESTDIR)/$$a"; \
	        else \
	            echo "warning: $(BIN_DIR)/$$a missing — rafiki-attach will fail at startup;" >&2; \
	            echo "         re-run 'make build-attach'" >&2; \
	        fi; \
	    done; \
	else \
	    echo "note: $(BIN_DIR)/$(ATTACH_BIN) not built — run 'make build-attach' (needs bun + the pi submodule)"; \
	fi
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
# build-attach runs before install (prerequisite order, not just listed for
# documentation) so its output is on disk in time for install's `[ -x
# $(BIN_DIR)/$(ATTACH_BIN) ]` check — otherwise redeploy silently ships
# whatever rafiki-attach happened to be built last, or skips it entirely on a
# machine that has never run build-attach. It degrades the same way
# build-attach always has: no bun on PATH just skips with a warning, it does
# not fail the daemon redeploy.
.PHONY: redeploy
redeploy: build-attach install ## Rebuild (incl. rafiki-attach) + install, then restart the rafiki daemon.
	@"$(DESTDIR)/$(CLI_BIN)" service restart
	@"$(DESTDIR)/$(CLI_BIN)" service status

##@ pi submodule lifecycle

# rafiki-attach links against the bundled pi tree at $(PI_DIR), and the daemon
# spawns the matching pi binary off PATH. `pi-install` keeps the global install
# in lock-step with the submodule pin.
#
# NOTE (merge): the submodule points at a private host, so a public
# `clone --recursive` breaks here. Replacing it with the published
# @earendil-works/pi-* packages at 0.83.0 is a tracked follow-up; until it
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
	cd $(PI_DIR)/packages/telemetry && npm run build
	cd $(PI_DIR)/packages/ai && npx tsgo -p tsconfig.build.json
	cd $(PI_DIR)/packages/agent && npm run build
	cd $(PI_DIR)/packages/protocol && npm run build
	cd $(PI_DIR)/packages/client && npm run build
	cd $(PI_DIR)/packages/server && npm run build
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
	@echo "Only the rafiki-attach TUI needs it — 'make build' works without it." >&2
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

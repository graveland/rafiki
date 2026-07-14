.PHONY: help build update build-controller build-pic build-attach \
        bootstrap pi-build pi-install pi-update pi-refresh-catalogs \
        test test-race vet fmt clean

GO      ?= go
BIN_DIR := bin

BOLD   := \033[1m
NORMAL := \033[0m
GREEN  := \033[1;32m

.DEFAULT_GOAL := update
HELP_TARGET_DEPTH ?= \#
help: # Show available targets
	@printf "make targets (e.g. $(BOLD)make build$(NORMAL)):\n\n"
	@awk -F':+ |$(HELP_TARGET_DEPTH)' '/^[0-9a-zA-Z._%-]+:+.+$(HELP_TARGET_DEPTH).+$$/ { printf "$(GREEN)%-18s$(NORMAL) %s\n", $$1, $$3 }' $(MAKEFILE_LIST)
	@echo

PI_DIR  := pi
PI_PKG  := $(PI_DIR)/packages/coding-agent
PI_DIST := $(PI_PKG)/dist/cli.js
PI_MODULES := $(PI_DIR)/node_modules

# Every package's TypeScript source. $(PI_DIST) must depend on these: keying the
# rebuild on package.json alone meant editing a .ts file never recompiled dist,
# so `make`/`pi-install` shipped a stale bundle from previously-compiled JS.
PI_SRC := $(shell find $(PI_DIR)/packages/*/src -type f \( -name '*.ts' -o -name '*.tsx' \) ! -name '*.test.ts' 2>/dev/null)

# Evaluated fresh on each invocation; empty when no .go files exist yet.
PKGS := $(shell $(GO) list ./... 2>/dev/null)

build: build-controller build-pic build-attach # Build pi-controller, pic, and pic-attach

update: build pi-install # Build everything AND install the global pi backend

build-controller: # Build the daemon binary (bin/pi-controller)
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/pi-controller ./cmd/pi-controller

build-pic: # Build the pic CLI (bin/pic)
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/pic ./cmd/pic

# pic-attach bundles pi (via attach/package.json -> file:../$(PI_PKG)), so pi
# must be built first. Fail loudly if the submodule isn't initialised.
build-attach: $(PI_MODULES) $(PI_DIST) # Bundle the pic-attach TUI binary (recompiles pi dist first)
	@if command -v bun >/dev/null 2>&1; then \
	    cd attach && bun install --silent && bun run build; \
	else \
	    echo "skipping pic-attach build: bun not installed (install via 'brew install oven-sh/bun/bun')"; \
	fi

# ─── pi submodule lifecycle ───────────────────────────────────────────────────
# pic-attach links against the bundled pi tree at $(PI_DIR), and the daemon
# spawns the matching pi binary off PATH. `pi-install` keeps the global install
# in lock-step with the submodule pin.

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

pi-refresh-catalogs: $(PI_MODULES) # Regenerate pi's model catalogs from live provider APIs (commit the result in the submodule)
	cd $(PI_DIR)/packages/ai && npm run generate-models && npm run generate-image-models

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
	@echo "vendor/pi is not initialised. Run 'make bootstrap' (fresh clone)" >&2
	@echo "or 'git submodule update --init --recursive'." >&2
	@exit 1

pi-build: $(PI_DIST) # Recompile the pi submodule's TypeScript to dist

pi-install: $(PI_DIST) # Install the pi binary globally (the daemon-spawned backend)
	npm install -g ./$(PI_PKG)

pi-update: # Bump the pi submodule to its remote tip, then rebuild + install
	git submodule update --remote $(PI_DIR)
	$(MAKE) pi-build pi-install

bootstrap: # Fresh-clone setup — init submodules, build and install everything
	git submodule update --init --recursive
	$(MAKE) pi-build pi-install build

# ─── tests / housekeeping ─────────────────────────────────────────────────────

test: # Run the Go test suite
	$(if $(PKGS),$(GO) test $(PKGS),@echo "(no Go packages yet)")

test-race: # Run the Go test suite with the race detector
	$(if $(PKGS),$(GO) test -race $(PKGS),@echo "(no Go packages yet)")

vet: # Run go vet over all packages
	$(if $(PKGS),$(GO) vet $(PKGS),@echo "(no Go packages yet)")

fmt: # gofmt all Go sources
	$(GO) fmt ./...

clean: # Remove built binaries
	rm -rf $(BIN_DIR)

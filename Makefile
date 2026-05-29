.PHONY: build build-controller build-pic build-attach \
        bootstrap pi-build pi-install pi-update \
        test test-race vet fmt clean

GO      ?= go
BIN_DIR := bin

PI_DIR  := pi
PI_PKG  := $(PI_DIR)/packages/coding-agent
PI_DIST := $(PI_PKG)/dist/cli.js

# Evaluated fresh on each invocation; empty when no .go files exist yet.
PKGS := $(shell $(GO) list ./... 2>/dev/null)

build: build-controller build-pic build-attach

build-controller:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/pi-controller ./cmd/pi-controller

build-pic:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/pic ./cmd/pic

# pic-attach bundles pi (via attach/package.json -> file:../$(PI_PKG)), so pi
# must be built first. Fail loudly if the submodule isn't initialised.
build-attach: $(PI_DIST)
	@if command -v bun >/dev/null 2>&1; then \
	    cd attach && bun install --silent && bun run build; \
	else \
	    echo "skipping pic-attach build: bun not installed (install via 'brew install oven-sh/bun/bun')"; \
	fi

# ─── pi submodule lifecycle ───────────────────────────────────────────────────
# pic-attach links against the bundled pi tree at $(PI_DIR), and the daemon
# spawns the matching pi binary off PATH. `pi-install` keeps the global install
# in lock-step with the submodule pin.

$(PI_DIST): $(PI_PKG)/package.json
	cd $(PI_DIR) && npm install && npm run build

$(PI_PKG)/package.json:
	@echo "vendor/pi is not initialised. Run 'make bootstrap' (fresh clone)" >&2
	@echo "or 'git submodule update --init --recursive'." >&2
	@exit 1

pi-build: $(PI_DIST)

pi-install: $(PI_DIST)
	npm install -g ./$(PI_PKG)

pi-update:
	git submodule update --remote $(PI_DIR)
	$(MAKE) pi-build pi-install

bootstrap:
	git submodule update --init --recursive
	$(MAKE) pi-build pi-install build

# ─── tests / housekeeping ─────────────────────────────────────────────────────

test:
	$(if $(PKGS),$(GO) test $(PKGS),@echo "(no Go packages yet)")

test-race:
	$(if $(PKGS),$(GO) test -race $(PKGS),@echo "(no Go packages yet)")

vet:
	$(if $(PKGS),$(GO) vet $(PKGS),@echo "(no Go packages yet)")

fmt:
	$(GO) fmt ./...

clean:
	rm -rf $(BIN_DIR)

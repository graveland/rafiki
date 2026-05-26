.PHONY: build build-controller build-pic test test-race vet fmt clean

GO      ?= go
BIN_DIR := bin

# Evaluated fresh on each invocation; empty when no .go files exist yet.
PKGS := $(shell $(GO) list ./... 2>/dev/null)

build: build-controller build-pic

build-controller:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/pi-controller ./cmd/pi-controller

build-pic:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/pic ./cmd/pic

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

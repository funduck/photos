# Build, test and install the ingest service.
#
# Unlike everything else in this repo, ingest is not containerized: it needs
# hevc_videotoolbox, so it runs as a host binary under launchd.
#
# This file only builds and tests. Setting the service up lives elsewhere, where
# it can ask questions and report errors properly:
#
#   config   ->  `ingest setup`, which writes <source>/.ingest/config.yaml
#   launchd  ->  ./launchd.sh, which owns the plist and every launchctl verb
#
#   make check          # fmt + vet + test
#   make config         # interview, write a config
#   make install        # build and install the binary
#   make install-plist  # write the LaunchAgent and load it

SHELL := /bin/bash

PKG       := ./src
BUILD_DIR := bin
BINARY    := $(BUILD_DIR)/ingest
GO        ?= go
GOFLAGS   ?= -trimpath
LAUNCHD   := ./launchd.sh

# Extra arguments for `ingest setup`:  make config ARGS='-source ~/SyncPhones'
ARGS ?=
# Source trees for the agent, one -config each. Empty reuses whatever the
# installed plist already names.
SOURCES ?=

.PHONY: help check fmt vet test test-race build config install install-plist \
        load unload restart status logs clean uninstall

help:
	@echo "Build and test"
	@echo "  make build          compile $(BINARY)"
	@echo "  make test           go test $(PKG)"
	@echo "  make test-race      go test -race"
	@echo "  make check          fmt + vet + test"
	@echo
	@echo "Set up"
	@echo "  make config         write <source>/.ingest/config.yaml  (ARGS='-source ...')"
	@echo "  make install        build, then install the binary"
	@echo "  make install-plist  write the LaunchAgent and load it   (SOURCES='dir dir')"
	@echo
	@echo "Run under launchd"
	@echo "  make load / unload / restart / status / logs"
	@echo
	@echo "Clean up"
	@echo "  make clean          remove $(BUILD_DIR)/"
	@echo "  make uninstall      unload the agent, remove the plist and binary"
	@echo
	@echo "Paths: $(LAUNCHD) bin-path | agent-path | log-path"

# --- build and test --------------------------------------------------------

check: fmt vet test

fmt:
	$(GO) fmt $(PKG)

vet:
	$(GO) vet $(PKG)

test:
	$(GO) test $(PKG)

test-race:
	$(GO) test -race $(PKG)

build:
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -o $(BINARY) $(PKG)
	@echo "built $(BINARY)"

# --- set up ----------------------------------------------------------------

config: build
	@$(BINARY) setup $(ARGS)

install: build
	@$(LAUNCHD) install-bin $(BINARY)

install-plist:
	@$(LAUNCHD) install $(SOURCES)

# --- launchd control -------------------------------------------------------

load:
	@$(LAUNCHD) load

unload:
	@$(LAUNCHD) unload

restart:
	@$(LAUNCHD) restart

status:
	@$(LAUNCHD) status

logs:
	@$(LAUNCHD) logs

# --- cleanup ---------------------------------------------------------------

clean:
	rm -rf $(BUILD_DIR)

uninstall:
	@$(LAUNCHD) uninstall

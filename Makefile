BINARY     := gowarm
PKG        := ./...
CMD_PKG    := ./cmd
GO         ?= go
LDFLAGS    := -s -w
BUILD_DIR  := bin

.PHONY: all build run test test-race lint vet fmt tidy clean dry-run install help

all: build

## build: compile the gowarm binary into ./bin/gowarm
build:
	@mkdir -p $(BUILD_DIR)
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BINARY) $(CMD_PKG)
	@echo "built $(BUILD_DIR)/$(BINARY)"

## run: build and run with the default config
run: build
	./$(BUILD_DIR)/$(BINARY) -config config.yaml

## dry-run: list every (URL, region) pair without sending requests
dry-run: build
	./$(BUILD_DIR)/$(BINARY) -config config.yaml -dry-run | head -40

## test: run unit tests
test:
	$(GO) test -count=1 $(PKG)

## test-race: run unit tests with the race detector
test-race:
	$(GO) test -race -count=1 $(PKG)

## vet: go vet
vet:
	$(GO) vet $(PKG)

## fmt: gofmt sources in place
fmt:
	$(GO) fmt $(PKG)

## tidy: tidy go.mod / go.sum
tidy:
	$(GO) mod tidy

## install: install the binary into $GOBIN
install:
	$(GO) install $(CMD_PKG)

## clean: remove build artefacts
clean:
	rm -rf $(BUILD_DIR)

## help: list documented targets
help:
	@grep -E '^## [a-zA-Z_-]+:' Makefile | sed 's/## //'

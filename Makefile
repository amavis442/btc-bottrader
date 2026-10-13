# Bouwt de tradingbot voor Linux en Windows. De bot is pure Go (geen CGO), dus beide binaries
# kunnen vanaf elk platform gebouwd worden.
#
#   make            Linux- en Windows-binary bouwen (in bin/)
#   make linux      alleen Linux (amd64)
#   make windows    alleen Windows (amd64)
#   make linux-arm64  Linux op ARM, bijv. een Raspberry Pi
#   make test       alle tests, met race-detector
#   make check      gofmt + go vet + tests
#   make clean      bin/ verwijderen
#   make version    toon de versie die in de binary komt

BINARY  := tradingbot
PKG     := ./cmd/tradingbot
BIN_DIR := bin

# Versie uit git (tag of commit), anders "dev".
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

GO       ?= go
GOFLAGS  := -trimpath -ldflags "$(LDFLAGS)"
export CGO_ENABLED := 0

.PHONY: all build linux windows linux-arm64 test check fmt vet clean version

all: build

build: linux windows

linux:
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/$(BINARY)-linux-amd64 $(PKG)

windows:
	GOOS=windows GOARCH=amd64 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/$(BINARY)-windows-amd64.exe $(PKG)

linux-arm64:
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/$(BINARY)-linux-arm64 $(PKG)

test:
	CGO_ENABLED=1 $(GO) test -race ./...

fmt:
	@test -z "$$(gofmt -l .)" || (echo "Niet geformatteerd (draai gofmt -w .):"; gofmt -l .; exit 1)

vet:
	$(GO) vet ./...

check: fmt vet test

clean:
	$(GO) clean
	rm -rf $(BIN_DIR)

version:
	@echo $(VERSION)

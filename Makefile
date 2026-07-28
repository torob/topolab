GO ?= go

.PHONY: all build test test-race vet fmt check integration integration-routed

all: check build

build:
	mkdir -p bin
	$(GO) build -o bin/topolab ./cmd/topolab

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

integration: build
	TOPOLAB_BINARY=$(CURDIR)/bin/topolab ./hack/integration-network.sh

integration-routed: build
	TOPOLAB_BINARY=$(CURDIR)/bin/topolab ./hack/acceptance-routed.sh

fmt:
	gofmt -w $$(find cmd internal -name '*.go' -type f)

check: test test-race vet

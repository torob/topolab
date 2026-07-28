GO ?= go

.PHONY: all build test test-race vet fmt check

all: check build

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o bin/topolab ./cmd/topolab

test:
	$(GO) test ./...

test-race:
	mkdir -p bin
	CGO_ENABLED=1 $(GO) test -race ./...
	CGO_ENABLED=1 $(GO) build -race -o bin/topolab-race ./cmd/topolab

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $$(find cmd internal -name '*.go' -type f)

check: test test-race vet

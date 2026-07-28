GO ?= go

.PHONY: all build test test-race vet fmt check

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

fmt:
	gofmt -w $$(find cmd internal -name '*.go' -type f)

check: test test-race vet

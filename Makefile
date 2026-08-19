BINARY  := baton
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= /usr/local

.PHONY: build test lint check install uninstall clean

build:
	go build -ldflags "-X baton/internal/cli.Version=$(VERSION)" -o bin/$(BINARY) ./cmd/baton

test:
	go test ./...

lint:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

check: lint test

install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/$(BINARY) $(PREFIX)/bin/$(BINARY)
	@echo "installed $(PREFIX)/bin/$(BINARY) ($(VERSION))"

uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)

clean:
	rm -rf bin

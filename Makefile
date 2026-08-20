BINARY  := baton
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= /usr/local

APP     := Baton.app
APP_DIR := menubar/build/$(APP)

.PHONY: build test lint check install uninstall clean menubar menubar-install menubar-run menubar-check snapshots

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

# The menu bar app is assembled by hand rather than through Xcode: SwiftPM
# produces a bare executable, and MenuBarExtra needs a real bundle with
# LSUIElement set or macOS gives the app a Dock icon and an app menu.
menubar:
	cd menubar && swift build -c release
	rm -rf $(APP_DIR)
	mkdir -p $(APP_DIR)/Contents/MacOS
	cp menubar/Info.plist $(APP_DIR)/Contents/Info.plist
	cp menubar/.build/release/BatonMenuBar $(APP_DIR)/Contents/MacOS/BatonMenuBar
	codesign --force --sign - $(APP_DIR) 2>/dev/null || true
	@echo "built $(APP_DIR)"

menubar-run: menubar
	open $(APP_DIR)

menubar-check:
	cd menubar && swift build && ./.build/debug/baton-probe --selftest

# Renders every popover state to PNGs in both appearances. A menu bar popover
# cannot be screenshotted without screen recording permission, so this is how
# the layout gets reviewed — and how a state that renders blank gets noticed.
snapshots:
	cd menubar && swift build && ./.build/debug/BatonMenuBar --snapshot $(CURDIR)/menubar/snapshots
	@echo "open $(CURDIR)/menubar/snapshots"

menubar-install: menubar
	rm -rf /Applications/$(APP)
	cp -R $(APP_DIR) /Applications/$(APP)
	@echo "installed /Applications/$(APP) — open it, then add it to Login Items to keep it around"

clean:
	rm -rf bin menubar/build menubar/.build

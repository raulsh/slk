VERSION ?= 0.1.0
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)

PKG := slk
BIN := bin/$(PKG)

# Debian packaging. The maintainer is a GitHub identity on purpose: this is a
# published artifact, so it carries no personal address.
ARCH       := $(shell dpkg --print-architecture 2>/dev/null || echo amd64)
MAINTAINER ?= raulsh <raulsh@users.noreply.github.com>
STAGE      := build/deb
DEB        := dist/$(PKG)_$(VERSION)_$(ARCH).deb

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# CGO_ENABLED=0 keeps the binary fully static so it can be copied into a
# container an agent runs in. The SQLite cache is modernc's pure-Go build, so
# nothing here needs a C toolchain.
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

PLATFORMS := linux/amd64 linux/arm64 darwin/arm64 darwin/amd64

.PHONY: build install completions deb install-local uninstall-local test cover vet lint fmt check golden dist clean help

## build: compile the static binary
build:
	@mkdir -p bin
	$(GOBUILD) -o $(BIN) ./cmd/$(PKG)
	@echo "built $(BIN) ($(VERSION))"

## install: build and copy the binary into ~/.local/bin
install: build
	@mkdir -p $(HOME)/.local/bin
	install -m 0755 $(BIN) $(HOME)/.local/bin/$(PKG)
	@echo "installed $(HOME)/.local/bin/$(PKG)"

## completions: generate shell completion scripts into build/completions
#
# GOOS and GOARCH are cleared deliberately: the packaged binary may be
# cross-compiled for another architecture, and running that to emit
# completions would fail on the build host.
completions:
	@mkdir -p build/completions
	GOOS= GOARCH= go run ./cmd/$(PKG) completion bash > build/completions/$(PKG).bash
	GOOS= GOARCH= go run ./cmd/$(PKG) completion zsh  > build/completions/_$(PKG)
	GOOS= GOARCH= go run ./cmd/$(PKG) completion fish > build/completions/$(PKG).fish
	@echo "generated build/completions"

## deb: build the installable Debian package
deb: build completions
	@rm -rf $(STAGE)
	@mkdir -p $(STAGE)/DEBIAN dist
	install -D -m 0755 $(BIN) $(STAGE)/usr/bin/$(PKG)
	install -D -m 0644 build/completions/$(PKG).bash $(STAGE)/usr/share/bash-completion/completions/$(PKG)
	install -D -m 0644 build/completions/_$(PKG) $(STAGE)/usr/share/zsh/vendor-completions/_$(PKG)
	install -D -m 0644 build/completions/$(PKG).fish $(STAGE)/usr/share/fish/vendor_completions.d/$(PKG).fish
	install -D -m 0644 README.md $(STAGE)/usr/share/doc/$(PKG)/README.md
	install -D -m 0644 packaging/debian/copyright $(STAGE)/usr/share/doc/$(PKG)/copyright
	@gzip -9n $(STAGE)/usr/share/doc/$(PKG)/README.md
	@size=$$(du -sk $(STAGE) | cut -f1); \
	sed -e 's|@VERSION@|$(VERSION)|' \
	    -e 's|@ARCH@|$(ARCH)|' \
	    -e 's|@MAINTAINER@|$(MAINTAINER)|' \
	    -e "s|@SIZE@|$$size|" \
	    packaging/debian/control.in > $(STAGE)/DEBIAN/control
	dpkg-deb --build --root-owner-group $(STAGE) $(DEB)
	@echo
	@dpkg-deb --info $(DEB) | head -10
	@echo "package: $(DEB)"

## install-local: build and install the .deb on this machine
install-local: deb
	sudo dpkg -i $(DEB)
	@echo
	@$(PKG) version

## uninstall-local: remove the package, keeping your credentials and cache
uninstall-local:
	sudo dpkg -r $(PKG)

## test: run the Go test suite
test:
	go test ./...

## cover: run tests with coverage and print the per-package summary
cover:
	go test -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -20

## vet: run go vet
vet:
	go vet ./...

## lint: run golangci-lint
lint:
	golangci-lint run

## fmt: format all Go sources
fmt:
	gofmt -l -w ./cmd ./internal

## check: fmt verification, vet, lint and tests - what CI should run
check:
	@unformatted=$$(gofmt -l ./cmd ./internal); \
	if [ -n "$$unformatted" ]; then echo "unformatted files:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed, skipped"
	go test ./...

## golden: regenerate the output renderer golden files
golden:
	go test ./internal/output -run Golden -update

## dist: cross-compile release archives into dist/
dist:
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch $(GOBUILD) -o dist/$(PKG) ./cmd/$(PKG) || exit 1; \
		tar -czf dist/$(PKG)_$(VERSION)_$${os}_$${arch}.tar.gz -C dist $(PKG); \
		rm -f dist/$(PKG); \
	done
	@cd dist && sha256sum *.tar.gz > SHA256SUMS
	@ls -1 dist/

## clean: remove build artifacts
clean:
	rm -rf bin build dist coverage.out

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'

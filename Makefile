BINARY  := agentsitter
PKG     := ./cmd/agentsitter
DIST    := dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# Platforms built by `make dist`. Linux amd64 and arm64 cover most servers you
# would drop this on next to a swarm of agents.
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

.PHONY: help
help:
	@echo "agentsitter $(VERSION)"
	@echo
	@echo "  make build        build ./$(DIST)/$(BINARY) for this machine"
	@echo "  make install      install into GOBIN"
	@echo "  make test         unit tests"
	@echo "  make integration  integration tests (needs tmux)"
	@echo "  make check        fmt, vet, and both test suites"
	@echo "  make dist         cross-compiled binaries with checksums"
	@echo "  make clean        remove build output"

.PHONY: build
build:
	@mkdir -p $(DIST)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY) $(PKG)

.PHONY: install
install:
	go install -trimpath -ldflags '$(LDFLAGS)' $(PKG)

.PHONY: test
test:
	go test ./...

.PHONY: integration
integration:
	go test -tags integration -count=1 -timeout 300s ./...

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: vet
vet:
	go vet ./...
	go vet -tags integration ./...

.PHONY: fmtcheck
fmtcheck:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "not gofmt clean:"; echo "$$out"; exit 1; fi

.PHONY: check
check: fmtcheck vet test integration

.PHONY: dist
dist:
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=$(DIST)/$(BINARY)-$$os-$$arch; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags '$(LDFLAGS)' -o $$out $(PKG) || exit 1; \
	done
	@cd $(DIST) && shasum -a 256 $(BINARY)-* > SHA256SUMS 2>/dev/null \
		|| sha256sum $(BINARY)-* > SHA256SUMS
	@echo "wrote $(DIST)/SHA256SUMS"

.PHONY: clean
clean:
	rm -rf $(DIST)

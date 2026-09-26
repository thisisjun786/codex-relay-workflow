GO ?= go
BINARY := dist/crw
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
STATICCHECK := $(GO) run honnef.co/go/tools/cmd/staticcheck

.PHONY: build test contract lint dist crw-dev

build:
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: build skipped"; else $(GO) build -o $(BINARY) -trimpath -ldflags="$(LDFLAGS)" ./cmd/crw; fi

test:
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: test skipped"; else $(GO) test ./... && $(GO) test -tags dev ./cmd/crw-dev/... ./internal/dev/...; fi

contract:
	@if [ ! -d ./internal/contracttest ] || ! $(GO) list ./internal/contracttest/... 2>/dev/null | grep -q .; then echo "no Go packages yet: contract skipped"; else $(GO) test ./internal/contracttest/...; fi

# The development tooling (cmd/crw-dev, internal/dev) builds only with -tags dev, so lint and
# test cover it in a second pass; dist and goreleaser never pass the tag.
lint:
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: lint skipped"; else $(GO) vet ./... && $(GO) vet -tags dev ./cmd/crw-dev/... ./internal/dev/... && $(STATICCHECK) ./... && $(STATICCHECK) -tags dev ./cmd/crw-dev/... ./internal/dev/... && test -z "$$(find . -name '*.go' -not -path './.git/*' -exec gofmt -l {} +)"; fi

dist:
ifneq ($(CGO_ENABLED),0)
	$(error dist requires CGO_ENABLED=0)
endif
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: dist skipped"; else CGO_ENABLED=0 $(GO) build -o $(BINARY) -trimpath -ldflags="$(LDFLAGS)" ./cmd/crw; fi

# The development binary: CI checks as `crw-dev ci <check>`. Never part of a release.
crw-dev:
	$(GO) build -tags dev -o dist/crw-dev ./cmd/crw-dev

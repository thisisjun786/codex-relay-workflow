GO ?= go
BINARY := dist/crw
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
STATICCHECK := $(GO) run honnef.co/go/tools/cmd/staticcheck

.PHONY: build test test-part contract parity lint dist crw-dev

build:
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: build skipped"; else $(GO) build -o $(BINARY) -trimpath -ldflags="$(LDFLAGS)" ./cmd/crw; fi

test:
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: test skipped"; else $(GO) test ./... && $(GO) test -tags dev ./cmd/crw-dev/... ./internal/dev/...; fi

# CI runs `make test` as parallel parts, one runner each (.github/workflows/ci.yml).
# Parts 1-4 name the slowest packages; `rest` is every other package plus the dev-tagged
# tests, so the parts are disjoint, together equal `make test`, and a new package always
# lands in `rest`. A renamed package makes its part fail in `go list`, never skip.
TEST_PART_1 := ./internal/relay/faults ./internal/relay/sync ./internal/relay/linkage ./internal/relay/evidence
TEST_PART_2 := ./internal/contracttest ./internal/relay/hook ./internal/relay/store ./internal/relay/mergeturn
TEST_PART_3 := ./internal/relay/delivery ./internal/relay/cli ./internal/relay/registry
TEST_PART_4 := ./internal/relay/supervisor ./internal/relay/service ./internal/relay/managed
TEST_PARTS := $(TEST_PART_1) $(TEST_PART_2) $(TEST_PART_3) $(TEST_PART_4)

test-part:
ifeq ($(TEST_PART),rest)
	@set -e; named="$$($(GO) list $(TEST_PARTS))"; \
	rest="$$($(GO) list ./... | grep -vxF "$$named")"; \
	$(GO) test $$rest; \
	$(GO) test -tags dev ./cmd/crw-dev/... ./internal/dev/...
else ifneq ($(filter $(TEST_PART),1 2 3 4),)
	$(GO) test $(TEST_PART_$(TEST_PART))
else
	$(error TEST_PART must be 1, 2, 3, 4 or rest)
endif

contract:
	@if [ ! -d ./internal/contracttest ] || ! $(GO) list ./internal/contracttest/... 2>/dev/null | grep -q .; then echo "no Go packages yet: contract skipped"; else $(GO) test ./internal/contracttest/...; fi

# Exhaustive live-Python CLI matrices. The default suite keeps mutation-backed
# representatives so ordinary CI remains bounded on four-core runners.
parity:
	$(GO) test -tags parity -count=1 ./internal/relay/cli/... ./internal/relay/adapter/... ./internal/relay/hook/...

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

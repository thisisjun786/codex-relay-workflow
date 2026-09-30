GO ?= go
BINARY := dist/crw
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# The git tree the binary is built from: the fault sweepers read it from the host record as the
# installed revision, and Go build information carries only the commit (internal/runtime/record).
# Stamped only from a clean working tree (`git status --porcelain` empty, the test Go's own
# vcs.modified applies): a build with modified or untracked files is not HEAD's tree, so it
# stamps nothing and the sweepers report the revision as incomplete (decision 34).
SOURCE_TREE := $(shell test -z "$$(git status --porcelain 2>/dev/null)" && git rev-parse 'HEAD^{tree}' 2>/dev/null)
LDFLAGS := -s -w -X main.version=$(VERSION) -X github.com/thisisjun786/codex-relay-workflow/internal/runtime/record.sourceTree=$(SOURCE_TREE)
STATICCHECK := $(GO) run honnef.co/go/tools/cmd/staticcheck
# Per-package test binary budget. internal/relay/delivery drove its Python oracle serially and
# took about 450-500 s on eight CPUs, most of go test's 10m default; since todo 44 it replays
# recorded answers, and the budget stays until the suite is measured again (CRW-241). In CI each
# leg's job timeout still bounds a hang.
TEST_TIMEOUT := -timeout 20m
# The one crw every package's tests run (internal/testsupport CRW): built once per `make test`
# or part, release-shaped (-trimpath), instead of once or more in each package that runs it.
TEST_BINARY := $(CURDIR)/dist/test/crw
TEST_ENV := CRW_TEST_BINARY=$(TEST_BINARY)

.PHONY: build test test-binary test-part contract lint dist crw-dev

build:
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: build skipped"; else $(GO) build -o $(BINARY) -trimpath -ldflags="$(LDFLAGS)" ./cmd/crw; fi

test: test-binary
	$(TEST_ENV) $(GO) test $(TEST_TIMEOUT) ./... && $(TEST_ENV) $(GO) test $(TEST_TIMEOUT) -tags dev ./cmd/crw-dev/... ./internal/dev/...

test-binary:
	$(GO) build -trimpath -o $(TEST_BINARY) ./cmd/crw

# CI runs `make test` as parallel parts, one runner each (.github/workflows/ci.yml).
# Parts 1-5 name the slowest packages; `rest` is every other package plus the dev-tagged
# tests, so the parts are disjoint, together equal `make test`, and a new package always
# lands in `rest`. A renamed package makes its part fail in `go list`, never skip.
# The Stop-hook package has wall-clock budgets, so it shares its runner only with light
# packages (part 5).
TEST_PART_1 := ./internal/relay/faults ./internal/relay/sync
TEST_PART_2 := ./internal/contracttest ./internal/relay/store ./internal/relay/mergeturn
TEST_PART_3 := ./internal/relay/delivery ./internal/relay/cli ./internal/relay/registry
TEST_PART_4 := ./internal/relay/supervisor ./internal/relay/service ./internal/relay/managed
TEST_PART_5 := ./internal/relay/hook ./internal/relay/linkage ./internal/relay/evidence
TEST_PARTS := $(TEST_PART_1) $(TEST_PART_2) $(TEST_PART_3) $(TEST_PART_4) $(TEST_PART_5)

test-part: test-binary
ifeq ($(TEST_PART),rest)
	@set -e; named="$$($(GO) list $(TEST_PARTS))"; \
	rest="$$($(GO) list ./... | grep -vxF "$$named")"; \
	$(TEST_ENV) $(GO) test $(TEST_TIMEOUT) $$rest; \
	$(TEST_ENV) $(GO) test $(TEST_TIMEOUT) -tags dev ./cmd/crw-dev/... ./internal/dev/...
else ifneq ($(filter $(TEST_PART),1 2 3 4 5),)
	$(TEST_ENV) $(GO) test $(TEST_TIMEOUT) $(TEST_PART_$(TEST_PART))
else
	$(error TEST_PART must be 1, 2, 3, 4, 5 or rest)
endif

contract:
	@if [ ! -d ./internal/contracttest ] || ! $(GO) list ./internal/contracttest/... 2>/dev/null | grep -q .; then echo "no Go packages yet: contract skipped"; else $(GO) test ./internal/contracttest/...; fi

# The development tooling (cmd/crw-dev, internal/dev) builds only with -tags dev, so lint and
# test cover it in a second pass; dist and goreleaser never pass the tag. The isolated-home
# integration test builds only with -tags integration (CI runs it in the dist leg), so lint vets
# it too and it cannot rot outside `make test`.
lint:
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: lint skipped"; else $(GO) vet ./... && $(GO) vet -tags dev ./cmd/crw-dev/... ./internal/dev/... && $(GO) vet -tags integration ./internal/runtime/integration/... && $(STATICCHECK) ./... && $(STATICCHECK) -tags dev ./cmd/crw-dev/... ./internal/dev/... && test -z "$$(find . -name '*.go' -not -path './.git/*' -exec gofmt -l {} +)"; fi

dist:
ifneq ($(CGO_ENABLED),0)
	$(error dist requires CGO_ENABLED=0)
endif
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: dist skipped"; else CGO_ENABLED=0 $(GO) build -o $(BINARY) -trimpath -ldflags="$(LDFLAGS)" ./cmd/crw; fi

# The development binary: CI checks as `crw-dev ci <check>`. Never part of a release.
crw-dev:
	$(GO) build -tags dev -o dist/crw-dev ./cmd/crw-dev

GO ?= go
BINARY := dist/crw
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
STATICCHECK := $(GO) run honnef.co/go/tools/cmd/staticcheck

.PHONY: build test contract lint dist

build:
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: build skipped"; else $(GO) build -o $(BINARY) -trimpath -ldflags="$(LDFLAGS)" ./...; fi

test:
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: test skipped"; else $(GO) test ./...; fi

contract:
	@if [ ! -d ./internal/contracttest ] || ! $(GO) list ./internal/contracttest/... 2>/dev/null | grep -q .; then echo "no Go packages yet: contract skipped"; else $(GO) test ./internal/contracttest/...; fi

lint:
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: lint skipped"; else $(GO) vet ./... && $(STATICCHECK) ./... && test -z "$$(find . -name '*.go' -not -path './.git/*' -exec gofmt -l {} +)"; fi

dist:
ifneq ($(CGO_ENABLED),0)
	$(error dist requires CGO_ENABLED=0)
endif
	@if ! $(GO) list ./... 2>/dev/null | grep -q .; then echo "no Go packages yet: dist skipped"; else CGO_ENABLED=0 $(GO) build -o $(BINARY) -trimpath -ldflags="$(LDFLAGS)" ./...; fi

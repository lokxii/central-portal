GO ?= go
BIN ?= bin
LDFLAGS ?= -s -w

.PHONY: all build server cli test fmt vet typecheck clean

all: build

build: server cli

# CGO is disabled so both binaries are fully static and run on any Linux host
# with the same architecture. -trimpath keeps build paths out of the binaries.
server:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/central-portal ./cmd/server

cli:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/cpctl ./cmd/cpctl

test:
	$(GO) test ./...

fmt:
	gofmt -l -w cmd internal web

vet:
	$(GO) vet ./...

typecheck:
	./node_modules/.bin/tsc -p jsconfig.json

integration: build
	./test/integration.sh

clean:
	rm -rf $(BIN)

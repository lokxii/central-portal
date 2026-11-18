GO ?= go
BIN := bin

.PHONY: all build server cli test fmt vet typecheck clean

all: build

build: server cli

server:
	@mkdir -p $(BIN)
	$(GO) build -trimpath -o $(BIN)/central-portal ./cmd/server

cli:
	@mkdir -p $(BIN)
	$(GO) build -trimpath -o $(BIN)/cpctl ./cmd/cpctl

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

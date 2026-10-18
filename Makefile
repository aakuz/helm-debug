BIN      := bin
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/aakuz/helm-debug/internal/cli.Version=$(VERSION)
EXE      := $(shell go env GOEXE)

.PHONY: build test update-golden lint e2e clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/helm-debug$(EXE) ./cmd/helm-debug
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/kubectl-helm_debug$(EXE) ./cmd/helm-debug

test:
	go test ./...

update-golden:
	go test ./internal/cli -update

lint:
	golangci-lint run

e2e: build
	./test/e2e.sh

clean:
	rm -rf $(BIN) dist

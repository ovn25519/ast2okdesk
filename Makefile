BINARY  := okdesk
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# CGO_ENABLED=0 — статическая сборка (modernc.org/sqlite — чистый Go).
GOFLAGS := -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)"

.PHONY: build test test-race vet fmt shellcheck clean

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -o $(BINARY) ./cmd/okdesk

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

shellcheck:
	shellcheck install.sh

clean:
	rm -f $(BINARY)
	rm -rf dist

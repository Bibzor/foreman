VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/Bibzor/foreman/internal/version.Version=$(VERSION)

.PHONY: all build test lint vuln clean

all: lint test vuln build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/foreman ./cmd/foreman

test:
	go test -race ./...

lint:
	golangci-lint run

vuln:
	go tool govulncheck ./...

clean:
	rm -rf bin

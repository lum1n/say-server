STATIC := internal/api/static
SQLC := github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build web go generate test clean

build: web go

web:
	pnpm install --frozen-lockfile
	pnpm -r build
	find $(STATIC) -mindepth 1 ! -name README.md -exec rm -rf {} +
	cp -r web/dist/client/. $(STATIC)/
	cp widget/dist/widget.js $(STATIC)/

go:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/say ./cmd/say

generate:
	cd internal/store && go run $(SQLC) generate
	pnpm --filter @say/sdk generate

test:
	go vet ./...
	go test ./...
	pnpm -r typecheck

clean:
	rm -rf bin web/dist widget/dist
	find $(STATIC) -mindepth 1 ! -name README.md -exec rm -rf {} +

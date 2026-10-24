STATIC := internal/api/static
SQLC := github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build web go generate test image cluster-test clean

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

image:
	docker build --build-arg VERSION=$(VERSION) -t say:$(VERSION) -t say:latest .

# cluster-test runs three nodes and Postgres in docker compose and tests
# delivery and replay across them. The cluster is removed afterwards.
CLUSTER := docker compose -f test/cluster/compose.yaml -p say-cluster-test
cluster-test:
	docker build -t say:cluster-test .
	$(CLUSTER) up -d
	SAY_CLUSTER_NODES=http://localhost:8081,http://localhost:8082,http://localhost:8083 \
	SAY_SETUP_CODE=cluster-test go test -count=1 -v ./test/cluster \
	|| { $(CLUSTER) logs; $(CLUSTER) down -v; exit 1; }
	$(CLUSTER) down -v

clean:
	rm -rf bin web/dist widget/dist
	find $(STATIC) -mindepth 1 ! -name README.md -exec rm -rf {} +

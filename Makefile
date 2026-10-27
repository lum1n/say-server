STATIC := internal/api/static
SQLC := github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
# The Python SDK's types: TypedDicts, so the package needs nothing at runtime.
PYGEN := uvx --from datamodel-code-generator==0.86.0 datamodel-codegen \
	--input protocol/openapi.yaml --input-file-type openapi \
	--output sdk/py/say/schema_gen.py --output-model-type typing.TypedDict \
	--target-python-version 3.11 --formatters builtin --disable-timestamp \
	--use-schema-description --use-field-description --enum-field-as-literal all \
	--use-standard-collections --use-union-operator --no-use-closed-typed-dict
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build web go generate generate-py test image node-image node-test cluster-test clean libsay android ios

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
	$(MAKE) generate-py

generate-py:
	$(PYGEN)

test:
	go vet ./...
	go test ./...
	pnpm -r typecheck
	$(MAKE) go
	SAY_BIN=$(CURDIR)/bin/say python3 -m unittest discover -s sdk/py -t sdk/py

# Embedding say in other programs (docs/adr/0013-embedding.md).
SOEXT := $(if $(filter Darwin,$(shell uname -s)),.dylib,.so)
libsay:
	mkdir -p build
	go build -trimpath -ldflags "-s -w" -buildmode=c-shared -o build/libsay$(SOEXT) ./cmd/libsay

# gomobile needs gobind on PATH; both come from the versions in go.mod.
# android needs the Android SDK and NDK (ANDROID_HOME, ANDROID_NDK_HOME);
# ios needs macOS with Xcode.
ANDROID_TARGETS ?= android/arm64,android/amd64
MOBILE := GOBIN=$(CURDIR)/build/bin go install golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind && PATH=$(CURDIR)/build/bin:$$PATH build/bin/gomobile bind
android:
	$(MOBILE) -target=$(ANDROID_TARGETS) -androidapi 24 -javapkg dev.say -ldflags "-s -w" -o build/say.aar ./mobile
ios:
	$(MOBILE) -target=ios,iossimulator -ldflags "-s -w" -o build/Say.xcframework ./mobile

image:
	docker build --build-arg VERSION=$(VERSION) -t say:$(VERSION) -t say:latest .

# The say-node image for your own node (ADR 0017): systemd, say on port 80.
node-image:
	docker build --target node --build-arg VERSION=$(VERSION) -t say-node:$(VERSION) -t say-node:latest .

# node-test boots the say-node image under Docker and claims it.
node-test: node-image
	image/node/test.sh say-node:$(VERSION)

# cluster-test runs three nodes and Postgres in docker compose and tests
# delivery and replay across them. The cluster is removed afterwards.
CLUSTER := docker compose -f test/cluster/compose.yaml -p say-cluster-test
cluster-test:
	docker build -t say:cluster-test .
	$(CLUSTER) up -d
	SAY_CLUSTER_NODES=http://localhost:7291,http://localhost:7292,http://localhost:7293 \
	SAY_SETUP_CODE=cluster-test go test -count=1 -v ./test/cluster \
	|| { $(CLUSTER) logs; $(CLUSTER) down -v; exit 1; }
	$(CLUSTER) down -v

clean:
	rm -rf bin build web/dist widget/dist
	find $(STATIC) -mindepth 1 ! -name README.md -exec rm -rf {} +

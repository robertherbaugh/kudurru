MODULE      := github.com/robertherbaugh/kudurru
BIN         := bin/kudurru
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.Commit=$(COMMIT) -X $(MODULE)/internal/version.Date=$(DATE)
GOFLAGS     := -trimpath
CGO_ENABLED := 0
export CGO_ENABLED

CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0
GOLANGCI_LINT  ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

.PHONY: all build run test lint vet generate manifests fmt tidy clean docker helm-lint check-generated dex test-auth-e2e

all: build

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/kudurru

run: build
	$(BIN) serve --config hack/dev/kudurru.yaml

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint: vet
	$(GOLANGCI_LINT) run ./...

fmt:
	gofmt -s -w ./cmd ./api ./internal

tidy:
	go mod tidy

## generate: deepcopy functions, CRD manifests and the CRD chart copies
generate: manifests
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

manifests:
	$(CONTROLLER_GEN) crd:crdVersions=v1 paths=./api/... output:crd:artifacts:config=config/crd/bases
	mkdir -p charts/kudurru-crds/templates
	cp config/crd/bases/*.yaml charts/kudurru-crds/templates/

## check-generated: fail if `make generate` would change tracked files
check-generated: generate
	git diff --exit-code -- api config/crd charts/kudurru-crds

## dex: build the pinned dex release (Apache-2.0; a separate test program,
## never linked) from its source tarball for the OIDC end-to-end test. dex
## publishes no binaries and `go install ...@latest` does not work (its
## go.mod has a replace directive), hence the tarball build.
DEX_VERSION ?= v2.46.0
DEX_SHA256  ?= 3c09c0f2be88719d5bc2a061d3df9310052c8220b651a3b72d774eecd9b50b65
DEX_BIN     ?= bin/dex-$(DEX_VERSION)

dex: $(DEX_BIN)

$(DEX_BIN):
	rm -rf bin/.dex-src && mkdir -p bin/.dex-src
	curl -fsSL -o bin/.dex-src/dex.tar.gz https://github.com/dexidp/dex/archive/refs/tags/$(DEX_VERSION).tar.gz
	echo "$(DEX_SHA256)  bin/.dex-src/dex.tar.gz" | sha256sum -c -
	tar -xzf bin/.dex-src/dex.tar.gz -C bin/.dex-src --strip-components=1
	cd bin/.dex-src && go build -trimpath -ldflags '-X main.version=$(DEX_VERSION)' -o $(abspath $(DEX_BIN)) ./cmd/dex
	rm -rf bin/.dex-src

## test-auth-e2e: OIDC BFF login end to end against a real dex (and the mock
## OP). The race detector needs cgo, so this test run re-enables it; the
## binaries kudurru ships stay CGO_ENABLED=0.
test-auth-e2e: $(DEX_BIN)
	CGO_ENABLED=1 KUDURRU_DEX_BIN=$(abspath $(DEX_BIN)) go test -race -count=1 -v -run 'TestDexEndToEnd|TestMockOPEndToEnd' ./internal/auth/oidc/

helm-lint:
	helm lint charts/kudurru charts/kudurru-crds

docker:
	docker build -t ghcr.io/robertherbaugh/kudurru:$(VERSION) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

clean:
	rm -rf bin dist

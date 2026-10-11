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

.PHONY: all build run test lint vet generate manifests fmt tidy clean docker helm-lint check-generated bench bench-baseline

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

## bench: blocking benchmarks gated against bench/baseline.txt (+20 %) and the
## absolute limits in bench/limits.txt (docs/architecture.md §7.3). The
## baseline is machine-specific: refresh it with `make bench-baseline` on the
## runner class that enforces it (docs/spikes/blocking.md, "Refreshing").
BENCH_PKGS      ?= ./internal/blocking/...
BENCH_COUNT     ?= 5
BENCH_BASELINE  ?= bench/baseline.txt
BENCH_LIMITS    ?= bench/limits.txt
BENCH_THRESHOLD ?= 20
BENCH_OUT       ?= bin/bench-current.txt
# Each count runs in a fresh process so that the medians also cover the
# run-to-run variance of memory layout, which -count inside one process hides.
BENCH_RUN        = for i in $$(seq $(BENCH_COUNT)); do \
	  go test -run '^$$' -bench . -benchmem -count 1 -timeout 30m $(BENCH_PKGS) 2>&1 || echo "FAIL: go test exited non-zero"; \
	done

bench:
	@mkdir -p $(dir $(BENCH_OUT))
	{ $(BENCH_RUN); } | tee $(BENCH_OUT)
	go run ./internal/blocking/benchgate -baseline $(BENCH_BASELINE) -limits $(BENCH_LIMITS) -threshold $(BENCH_THRESHOLD) $(BENCH_OUT)

## bench-baseline: re-record bench/baseline.txt on this machine and check it against the limits
bench-baseline:
	@mkdir -p $(dir $(BENCH_OUT))
	{ $(BENCH_RUN); } | tee $(BENCH_OUT)
	go run ./internal/blocking/benchgate -limits $(BENCH_LIMITS) $(BENCH_OUT)
	cp $(BENCH_OUT) $(BENCH_BASELINE)

helm-lint:
	helm lint charts/kudurru charts/kudurru-crds

docker:
	docker build -t ghcr.io/robertherbaugh/kudurru:$(VERSION) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

clean:
	rm -rf bin dist

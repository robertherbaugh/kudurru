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
SETUP_ENVTEST  ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
ENVTEST_K8S_VERSION ?= 1.37.x

.PHONY: all build size run test test-short lint vet generate manifests fmt tidy clean docker docker-debug helm-lint helm-check deploy-tools deploy-assets release-manifests check-generated validator-crds dex test-auth-e2e bench bench-baseline envtest

all: build

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/kudurru

## size: build the stripped CGO_ENABLED=0 release binary and fail when it is
## larger than SIZE_BUDGET bytes (D75). The budget is 10 % above the
## 46,575,776 bytes measured at the end of milestone M1 (linux/amd64,
## Go 1.27.2); raise it deliberately, in the change that explains the growth.
SIZE_BUDGET ?= 51234000
size: build
	@size=$$(wc -c < $(BIN) | tr -d ' '); \
	echo "$(BIN): $$size bytes, budget $(SIZE_BUDGET) bytes ($$((size * 100 / $(SIZE_BUDGET)))% used, $$(( ($(SIZE_BUDGET) - size) / 1024 )) KiB headroom)"; \
	if [ "$$size" -gt "$(SIZE_BUDGET)" ]; then \
		echo "error: $(BIN) exceeds the binary-size budget by $$((size - $(SIZE_BUDGET))) bytes (see D75; raise SIZE_BUDGET in the Makefile with a justification)"; \
		exit 1; \
	fi

run: build
	$(BIN) serve --config hack/dev/kudurru.yaml

## test: the race detector needs cgo, so the test run re-enables it; shipped
## binaries stay CGO_ENABLED=0 (`make build`). The kube store driver tests run
## when `make envtest` has installed the control-plane binaries and skip
## otherwise.
test:
	CGO_ENABLED=1 go test -race -count=1 ./...

## test-short: no race detector, no cgo; what `go test` gives on a plain checkout
test-short:
	go test -count=1 ./...

## envtest: download kube-apiserver and etcd into bin/k8s/<version>-<os>-<arch>;
## the kube store driver tests find them there (or via KUBEBUILDER_ASSETS)
## and skip when they are missing
envtest:
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin -p path

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
	@# The chart copies carry helm.sh/resource-policy: keep so that
	@# `helm uninstall kudurru-crds` never deletes the CRDs and every object (§5.1).
	for f in config/crd/bases/*.yaml; do \
		awk '{ print } /^    controller-gen.kubebuilder.io\/version:/ { print "    helm.sh/resource-policy: keep" }' $$f > charts/kudurru-crds/templates/$$(basename $$f); \
	done
	$(MAKE) --no-print-directory validator-crds

## validator-crds: copy the CRDs into the store validator's go:embed directory (D34)
validator-crds:
	mkdir -p internal/store/validator/crds
	rm -f internal/store/validator/crds/*.yaml
	cp config/crd/bases/*.yaml internal/store/validator/crds/

## check-generated: fail if `make generate` would change tracked files
check-generated: generate
	git diff --exit-code -- api config/crd charts/kudurru-crds internal/store/validator/crds

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

## bench: blocking benchmarks and the zone lookup benchmark gated against
## bench/baseline.txt (+20 %) and the absolute limits in bench/limits.txt
## (docs/architecture.md §7.3, docs/m2/xfr.md). The baseline is
## machine-specific: refresh it with `make bench-baseline` on the runner
## class that enforces it (docs/spikes/blocking.md, "Refreshing").
BENCH_PKGS      ?= ./internal/blocking/...
# Only the 100k-record lookup benchmark of internal/zone is gated; its
# build and export benchmarks are reported in docs/m1/zone.md.
BENCH_ZONE_PKGS ?= ./internal/zone
BENCH_ZONE      ?= ^BenchmarkLookup100k$$
BENCH_COUNT     ?= 5
BENCH_BASELINE  ?= bench/baseline.txt
BENCH_LIMITS    ?= bench/limits.txt
BENCH_THRESHOLD ?= 20
BENCH_OUT       ?= bin/bench-current.txt
# Each count runs in a fresh process so that the medians also cover the
# run-to-run variance of memory layout, which -count inside one process hides.
BENCH_RUN        = for i in $$(seq $(BENCH_COUNT)); do \
	  go test -run '^$$' -bench . -benchmem -count 1 -timeout 30m $(BENCH_PKGS) 2>&1 || echo "FAIL: go test exited non-zero"; \
	  go test -run '^$$' -bench '$(BENCH_ZONE)' -benchmem -count 1 -timeout 30m $(BENCH_ZONE_PKGS) 2>&1 || echo "FAIL: go test exited non-zero"; \
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

## Deployment assets (docs/m3/deploy.md). The tools are Apache-2.0 and are
## installed into bin/ at pinned versions; helm comes from the system.
KUBECONFORM_VERSION ?= v0.8.0
KUSTOMIZE_VERSION   ?= v5.8.3
DEPLOY_BIN          := $(CURDIR)/bin/deploy-tools

deploy-tools: $(DEPLOY_BIN)/kubeconform $(DEPLOY_BIN)/kustomize

$(DEPLOY_BIN)/kubeconform:
	GOBIN=$(DEPLOY_BIN) go install github.com/yannh/kubeconform/cmd/kubeconform@$(KUBECONFORM_VERSION)

$(DEPLOY_BIN)/kustomize:
	GOBIN=$(DEPLOY_BIN) go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

helm-lint:
	helm lint --strict charts/kudurru charts/kudurru-crds

## helm-check: helm lint, the chart render matrix, the kustomize overlays and
## the compose/dashboard/rules checks, with kubeconform against the
## Kubernetes 1.30 schemas and the CRDs (downloads schemas; cached in bin/).
helm-check: helm-lint deploy-tools
	PATH=$(DEPLOY_BIN):$$PATH KUDURRU_KUBECONFORM=1 KUBECONFORM_CACHE=$(CURDIR)/bin/kubeconform-cache \
		go test -count=1 -v ./deploy/

## deploy-assets: regenerate the Grafana dashboard, the chart's copies of
## deploy/ files and the kustomize bases rendered from the chart.
deploy-assets:
	go test -count=1 ./deploy/ -update

## release-manifests: crds.yaml and install.yaml (kustomize build
## config/default) for the GitHub release, written to bin/release/.
release-manifests: $(DEPLOY_BIN)/kustomize
	mkdir -p bin/release
	$(DEPLOY_BIN)/kustomize build config/crd > bin/release/crds.yaml
	$(DEPLOY_BIN)/kustomize build config/default > bin/release/install.yaml

docker:
	docker build -t ghcr.io/robertherbaugh/kudurru:$(VERSION) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

docker-debug:
	docker build --target debug -t ghcr.io/robertherbaugh/kudurru:$(VERSION)-debug --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

clean:
	rm -rf bin dist

MODULE      := github.com/robertherbaugh/kudurru
BIN         := bin/kudurru
# RELEASE_VERSION is what a build without a reachable tag reports (a
# tarball, a shallow clone, a fork without tags): the VERSION file, which
# also names the next release candidate and release tags
# (docs/release-checklist.md, "Release pipeline").
RELEASE_VERSION := $(shell tr -d '[:space:]' < VERSION 2>/dev/null)
VERSION     ?= $(shell git describe --tags --dirty 2>/dev/null || echo $(RELEASE_VERSION))
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

.PHONY: all build size run test test-short lint vet generate manifests fmt openapi client-go rbac-manifests tidy clean docker helm-lint check-generated validator-crds dex test-auth-e2e bench bench-baseline envtest
.PHONY: all build size run test test-short lint vet generate manifests fmt tidy clean docker helm-lint check-generated validator-crds dex test-auth-e2e bench bench-baseline envtest web-install web-build web-test web-lint web-dev web-e2e
.PHONY: all build size run test test-short lint vet generate manifests fmt tidy clean docker helm-lint check-generated validator-crds dex test-auth-e2e bench bench-baseline envtest edge-check edge-images kind-e2e
.PHONY: all build size run test test-short lint vet generate manifests fmt tidy clean docker docker-debug docker-hostnetwork hostnetwork-check volume-check helm-lint helm-check deploy-tools deploy-assets release-manifests check-generated validator-crds dex test-auth-e2e bench bench-baseline envtest

all: build

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/kudurru

## size: build the stripped CGO_ENABLED=0 release binary and fail when it is
## larger than SIZE_BUDGET bytes (D75, D138). History: 46,575,776 at the end
## of M1, 53,862,560 at the end of M4, 54,874,272 for v0.1.0-rc.1 (budget
## 60,362,000), 58,523,808 after the v0.1 UX round, 59,826,336 after the
## catalogue round and 57,991,328 with the dashboard embedded precompressed
## (web/scripts/compress.ts: 2.5 MiB of bundle as 0.75 MiB of gzip)
## (linux/amd64, Go 1.27.2, built dashboard). The project owner set the ceiling to 100,000,000 bytes
## (D138); growth still needs a reason in the change that causes it, and
## cmd/kudurru TestNoTemplateExecution guards the one known 12 MB trap.
SIZE_BUDGET ?= 100000000
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
	gofmt -s -w ./cmd ./api ./internal ./web/*.go

## rbac-manifests: generate the chart's aggregated ClusterRoles
## (charts/kudurru/templates/rbac-aggregated.yaml) from
## internal/rbac/permissions.yaml (D31); TestAggregatedRolesDrift fails when
## the committed file is stale. Run `make deploy-assets` afterwards so the
## kustomize bases rendered from the chart follow.
rbac-manifests:
	go run ./internal/rbac/rbacgen -out charts/kudurru/templates/rbac-aggregated.yaml

## openapi: write api/openapi.json (OpenAPI 3.1, what /api/v1/openapi.json
## serves) and api/openapi-3.0.json from the huma route registry;
## TestOpenAPIDrift fails when the committed files are stale
openapi:
	go run ./internal/api/openapigen -dir api

## client-go: generate the Go API client internal/apiclient/openapi from
## api/openapi-3.0.json with oapi-codegen (Apache-2.0)
OAPI_CODEGEN ?= go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0
client-go: openapi
	$(OAPI_CODEGEN) -config internal/apiclient/openapi/oapi-codegen.yaml api/openapi-3.0.json
	gofmt -s -w internal/apiclient/openapi

tidy:
	go mod tidy

## generate: deepcopy functions, CRD manifests and the CRD chart copies.
## Deepcopy covers api/v1alpha1 and the internal kinds of api/internalkinds/v1alpha1
## (LocalUser, APIToken, ServiceAccount, RoleBinding, IdentityProvider); the
## CRD run reads api/v1alpha1 only, because the internal kinds are never CRDs
## (Secrets in Kubernetes, JSON rows in state.db; D26).
generate: manifests
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/v1alpha1/... paths=./api/internalkinds/v1alpha1/...

manifests:
	$(CONTROLLER_GEN) crd:crdVersions=v1 paths=./api/v1alpha1/... output:crd:artifacts:config=config/crd/bases
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

## check-generated: fail if `make generate` or `make docs-gen` would change
## tracked files (the docs site's references follow the CRDs, the settings
## table, the CLI and the OpenAPI document)
check-generated: generate
	$(MAKE) --no-print-directory docs-gen
	git diff --exit-code -- api config/crd charts/kudurru-crds internal/store/validator/crds site/content/reference

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

## e2e: end-to-end tests against a freshly built `kudurru serve`
## (docs/m5/e2e.md): two nodes on loopback sharing the encryption key,
## SIGTERM lameduck, restarts over existing databases, configuration and
## port failures (test/e2e), and the BIND 9 interop (test/interop, skipped
## when named is not installed). Recorded bugs are skipped; set
## KUDURRU_E2E_KNOWN_BUGS=1 to reproduce them. The dex OIDC end to end
## test is `make test-auth-e2e`; CI's e2e job runs both.
.PHONY: e2e interop fuzz
E2E_FLAGS ?= -race -count=1 -timeout 20m
e2e:
	CGO_ENABLED=1 KUDURRU_E2E=1 go test $(E2E_FLAGS) -v ./test/e2e/ ./test/interop/

## interop: only the BIND 9 interop (dnf install bind / apt-get install bind9)
interop:
	CGO_ENABLED=1 KUDURRU_E2E=1 go test $(E2E_FLAGS) -v ./test/interop/

## fuzz: every fuzz target for FUZZTIME each (hack/fuzz.sh --list); FUZZ_JOBS
## targets at a time. Without -fuzz, `make test` replays the seed corpora.
FUZZTIME  ?= 60s
FUZZ_JOBS ?= 1
fuzz:
	FUZZTIME=$(FUZZTIME) FUZZ_JOBS=$(FUZZ_JOBS) hack/fuzz.sh

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

## web-*: the dashboard SPA in web/ (Bun + Vite, docs/architecture.md §10,
## D69). `make build` never needs Bun: it embeds whatever web/dist holds,
## which on a plain checkout is the committed placeholder index.html. Run
## `make web-build` first for a binary with the real dashboard (CI and
## release images do). web-build overwrites the tracked placeholder
## web/dist/index.html; do not commit the built one (TestIndexAssetsPresent
## in ./web fails if you do).
BUN ?= bun

web-install:
	cd web && $(BUN) install --frozen-lockfile

web-build: web-install
	cd web && $(BUN) run build

web-test: web-install
	cd web && $(BUN) run test

web-lint: web-install
	cd web && $(BUN) run lint && $(BUN) run typecheck

## web-dev: the dashboard against the MSW mock API (no server needed)
web-dev: web-install
	cd web && $(BUN) run dev:mock

## web-e2e: the Playwright suite (web/e2e) against a real `kudurru serve`
## with the embedded dashboard: builds the bundle and the binary, starts
## the server on free loopback ports with its own data directory, sends
## DNS traffic and drives every page in Chrome. Locally it uses
## /usr/bin/google-chrome (KUDURRU_E2E_CHROME names another; without one,
## run `cd web && bunx playwright install chromium` once); CI uses
## Playwright's chromium. The HTML report lands in web/playwright-report.
web-e2e: web-build build
	cd web && KUDURRU_BIN=../$(BIN) $(BUN) run e2e

## Docs site (site/, Hugo; site/README.md). The Hugo extended release
## binary (Apache-2.0) is downloaded into bin/ at HUGO_VERSION and checked
## against the pinned SHA-256 of the linux tarball; on other platforms
## install Hugo extended $(HUGO_VERSION) and pass HUGO=/path/to/hugo.
##   docs-gen:   regenerate the references under site/content/reference
##               (settings, CRDs, CLI); TestReferenceDrift (hack/docsgen) and
##               TestCLIReferenceDrift (cmd/kudurru) fail when they are stale
##   docs-site:  build the site into site/public with --panicOnWarning, so
##               an unresolved link in any page (docs/ included) fails
##   docs-serve: live preview on http://localhost:1313/
HUGO_VERSION            ?= 0.167.0
HUGO_SHA256_linux_amd64 := 0163f5c3deddac1f494a1629ddc40c65d18de9d5794facd98f7f96ac2c7d8957
HUGO_SHA256_linux_arm64 := c73eaba13738754b50de4d07606670d5c0cd2eaaf2057af657cec9efd3b01876
HUGO_OS                 := $(shell uname -s | tr A-Z a-z)
HUGO_ARCH               := $(shell uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
HUGO_DIR                := $(CURDIR)/bin/hugo-$(HUGO_VERSION)
HUGO                    ?= $(HUGO_DIR)/hugo
HUGO_FLAGS              ?= --minify --gc --cleanDestinationDir --panicOnWarning
# The Scalar bundle (MIT, internal/api/scalar) renders the REST reference
# page; Hugo reads it decompressed from this ignored directory.
DOCS_GENERATED          := site/.generated

.PHONY: docs-gen docs-assets docs-site docs-serve docs-check-links hugo

hugo: $(HUGO)

$(HUGO_DIR)/hugo:
	@sha='$(HUGO_SHA256_$(HUGO_OS)_$(HUGO_ARCH))'; \
	if [ -z "$$sha" ]; then echo "error: no pinned Hugo checksum for $(HUGO_OS)/$(HUGO_ARCH); install Hugo extended $(HUGO_VERSION) and pass HUGO=/path/to/hugo"; exit 1; fi; \
	rm -rf $(HUGO_DIR).dl && mkdir -p $(HUGO_DIR).dl && \
	curl -fsSL -o $(HUGO_DIR).dl/hugo.tar.gz https://github.com/gohugoio/hugo/releases/download/v$(HUGO_VERSION)/hugo_extended_$(HUGO_VERSION)_$(HUGO_OS)-$(HUGO_ARCH).tar.gz && \
	echo "$$sha  $(HUGO_DIR).dl/hugo.tar.gz" | sha256sum -c - && \
	mkdir -p $(HUGO_DIR) && tar -xzf $(HUGO_DIR).dl/hugo.tar.gz -C $(HUGO_DIR) hugo && \
	rm -rf $(HUGO_DIR).dl

docs-gen:
	go run ./hack/docsgen -root .
	go test -count=1 ./cmd/kudurru -run '^TestCLIReferenceDrift$$' -update-cli-reference

## docs-assets: the decompressed Scalar bundle, and the list of repository
## files the link hook checks links into the repository against (Hugo cannot
## stat files outside site/)
docs-assets:
	mkdir -p $(DOCS_GENERATED)/scalar
	gunzip -c internal/api/scalar/standalone.js.gz > $(DOCS_GENERATED)/scalar/standalone.js
	{ git ls-files --cached --others --exclude-standard 2>/dev/null || find . -path ./.git -prune -o -type f -print | sed 's|^\./||'; } > $(DOCS_GENERATED)/repo-files.txt

docs-site: docs-assets $(HUGO)
	cd site && $(HUGO) $(HUGO_FLAGS)

docs-serve: docs-assets $(HUGO)
	cd site && $(HUGO) server --bind 127.0.0.1 --port $${PORT:-1313}

## docs-check-links: build the site for the root of its own domain
## (docs.kudurru.io) and check every internal link, asset and fragment of the
## HTML with htmltest (MIT, site/.htmltest.yml)
HTMLTEST ?= go run github.com/wjdp/htmltest@v0.17.0
docs-check-links: docs-assets $(HUGO)
	rm -rf bin/docs-links bin/docs-links-log
	cd site && $(HUGO) $(HUGO_FLAGS) --baseURL http://localhost/ --destination ../bin/docs-links
	$(HTMLTEST) -c site/.htmltest.yml

## docs-check-workers: build the site as Cloudflare Workers Builds does and
## run `wrangler deploy --dry-run` on site/wrangler.jsonc (no Cloudflare
## account needed; downloads wrangler). Workers Builds deploys docs.kudurru.io
## from GitHub; nothing here deploys. site/README.md, "Publishing".
DOCS_URL ?= https://docs.kudurru.io
WRANGLER ?= $(BUN) x wrangler@4.149.0
.PHONY: docs-check-workers
docs-check-workers: docs-assets $(HUGO)
	cd site && $(HUGO) $(HUGO_FLAGS) --baseURL $(DOCS_URL)/
	cd site && WRANGLER_SEND_METRICS=false $(WRANGLER) deploy --dry-run
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

## edge-check: validate the golden dnsdist and keepalived configurations
## (internal/edge/*/testdata) with the real programs inside the pinned images:
## `dnsdist --check-config` in the upstream dnsdist image (digest from
## images/dnsdist/Dockerfile) and `keepalived -t` in the kudurru-keepalived
## image built from source. Needs Docker and network; CI runs it in the
## optional edge workflow. The unit tests only check the files structurally.
EDGE_DNSDIST_IMAGE    ?= powerdns/dnsdist-21:2.1.2@sha256:efabf173811e27438bebee5f1f40e047f263ea39093a8f7d6097df494cf1ed67
EDGE_KEEPALIVED_IMAGE ?= kudurru-keepalived:edge-check
EDGE_SUPERVISOR_IMAGE ?= kudurru-dnsdist:edge-check
EDGE_KUDURRU_IMAGE    ?= ghcr.io/robertherbaugh/kudurru:$(VERSION)
EDGE_CHECK_DIR        ?= bin/edge-check

edge-images: docker
	docker build -t $(EDGE_KEEPALIVED_IMAGE) --build-arg KUDURRU_IMAGE=$(EDGE_KUDURRU_IMAGE) images/keepalived
	docker build -t $(EDGE_SUPERVISOR_IMAGE) --build-arg KUDURRU_IMAGE=$(EDGE_KUDURRU_IMAGE) images/dnsdist

edge-check: edge-images
	CGO_ENABLED=1 go test -count=1 -run 'Golden' ./internal/edge/...
	rm -rf $(EDGE_CHECK_DIR) && mkdir -p $(EDGE_CHECK_DIR)/certs
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=edge-check \
	  -keyout $(EDGE_CHECK_DIR)/certs/tls.key -out $(EDGE_CHECK_DIR)/certs/tls.crt 2>/dev/null
	chmod 0644 $(EDGE_CHECK_DIR)/certs/tls.key
	docker run --rm --entrypoint /bin/sh \
	  -v $(CURDIR)/internal/edge/dnsdist/testdata:/golden:ro -v $(CURDIR)/$(EDGE_CHECK_DIR)/certs:/certs:ro \
	  $(EDGE_DNSDIST_IMAGE) -c 'set -e; dnsdist --version | head -1; for d in /golden/*/; do \
	    case $$d in *-ebpf/) continue;; esac; \
	    echo "== $$d (yaml)"; dnsdist --check-config -C $$d/dnsdist.yml; \
	    echo "== $$d (lua)"; dnsdist --check-config -C $$d/dnsdist.conf; done'
	# eBPF DynBlocks: --check-config creates the BPF maps, which needs
	# CAP_BPF/CAP_SYS_ADMIN as an effective capability, so root with the
	# chart's capability set (charts/kudurru _pod.tpl, kudurru.edge.rootStart).
	docker run --rm --user 0:953 --cap-drop ALL --cap-add NET_BIND_SERVICE --cap-add SETUID --cap-add SETGID \
	  --cap-add KILL --cap-add BPF --cap-add SYS_ADMIN --security-opt no-new-privileges --entrypoint /bin/sh \
	  -v $(CURDIR)/internal/edge/dnsdist/testdata:/golden:ro -v $(CURDIR)/$(EDGE_CHECK_DIR)/certs:/certs:ro \
	  $(EDGE_DNSDIST_IMAGE) -c 'set -e; for d in /golden/*-ebpf/; do \
	    echo "== $$d (yaml)"; dnsdist --check-config -C $$d/dnsdist.yml; \
	    echo "== $$d (lua)"; dnsdist --check-config -C $$d/dnsdist.conf; done'
	docker run --rm --entrypoint /bin/sh -v $(CURDIR)/internal/edge/keepalived/testdata:/golden:ro \
	  $(EDGE_KEEPALIVED_IMAGE) -c 'set -e; keepalived --version 2>&1 | head -1; mkdir -p /run/secrets; umask 077; \
	    for v in 53 54; do printf %s 0123456789abcdef0123456789abcdef > /run/secrets/vrrp_hmac_$$v; done; \
	    for f in /golden/*.conf; do echo "== $$f"; keepalived -t -f $$f; done'
	# The chart's root start at run time: dnsdist creates the maps, binds :53
	# without the sysctl, switches to 953 keeping CAP_SYS_ADMIN, and stops.
	hack/edge-root-check.sh $(EDGE_SUPERVISOR_IMAGE)
	docker run --rm --entrypoint /usr/local/bin/kudurru $(EDGE_SUPERVISOR_IMAGE) version

## kind-e2e: kind + MetalLB + both charts, dig through the LoadBalancer over
## UDP and TCP (split and mixed Services), rolling restart without unanswered
## queries (hack/kind-e2e.sh; needs docker, kind, helm, kubectl, dig).
kind-e2e:
	hack/kind-e2e.sh

docker:
	docker build -t ghcr.io/robertherbaugh/kudurru:$(VERSION) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

DEBUG_BASE_IMAGE ?= $(shell sed -n 's/^FROM \(gcr.io\/distroless\/static:debug-nonroot@sha256:[0-9a-f]*\) AS debug$$/\1/p' Dockerfile)

docker-debug:
	docker build --target debug -t ghcr.io/robertherbaugh/kudurru:$(VERSION)-debug --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

## docker-hostnetwork: the -hostnetwork variant of the `make docker` image
## (images/hostnetwork/Dockerfile): the same binary with the file capability
## cap_net_bind_service=+ep, for host networking (publish.yml builds it from
## the ko image).
docker-hostnetwork: docker
	docker build -t ghcr.io/robertherbaugh/kudurru:$(VERSION)-hostnetwork --build-arg KUDURRU_IMAGE=ghcr.io/robertherbaugh/kudurru:$(VERSION) images/hostnetwork

## hostnetwork-check: uid 65532 in the -hostnetwork image binds :53 with
## only NET_BIND_SERVICE and no_new_privs where the ip_unprivileged_port_start
## sysctl does not apply, and neither the image without the capability nor
## the plain image does (hack/hostnetwork-check.sh; needs Docker).
hostnetwork-check: docker-hostnetwork
	hack/hostnetwork-check.sh ghcr.io/robertherbaugh/kudurru:$(VERSION)-hostnetwork ghcr.io/robertherbaugh/kudurru:$(VERSION)

## volume-check: the Dockerfile images (plain, -debug, -hostnetwork) and
## both release bases (images/base, the ko images' base_image) come up on
## fresh named volumes at /var/lib/kudurru and /run/kudurru that uid 65532
## can write (hack/volume-check.sh; needs Docker).
volume-check: docker-debug docker-hostnetwork
	docker build -t kudurru-base:volume-check images/base
	docker build -t kudurru-base:volume-check-debug --build-arg BASE_IMAGE=$(DEBUG_BASE_IMAGE) images/base
	hack/volume-check.sh ghcr.io/robertherbaugh/kudurru:$(VERSION) ghcr.io/robertherbaugh/kudurru:$(VERSION)-debug \
	  ghcr.io/robertherbaugh/kudurru:$(VERSION)-hostnetwork kudurru-base:volume-check kudurru-base:volume-check-debug

clean:
	rm -rf bin dist web/dist/assets

## www-*: the kudurru.io website in www/ (Bun + TypeScript, served by
## Cloudflare Workers static assets; www/README.md). The pages are rendered
## at build time from www/src; the brand artwork and the dashboard
## screenshots come from docs/brand and docs/guide/images. Cloudflare
## Workers Builds deploys it from GitHub; nothing here deploys.
##   www-check: typecheck, tests (pages, links, CSP, _headers), a build and
##              `wrangler deploy --dry-run` (no Cloudflare account needed)
##   www-dev:   rebuild on change and serve with `wrangler dev` on
##              http://localhost:8787/
.PHONY: www-install www-build www-check www-dev
www-install:
	cd www && $(BUN) install --frozen-lockfile

www-build: www-install
	cd www && $(BUN) run build

www-check: www-install
	cd www && $(BUN) run typecheck && $(BUN) test && $(BUN) run build && $(BUN) run check:deploy

www-dev: www-install
	cd www && $(BUN) run dev

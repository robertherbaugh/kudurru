MODULE      := github.com/robertherbaugh/kudurru
BIN         := bin/kudurru
# RELEASE_VERSION is what a build without a reachable tag reports (a
# tarball, a shallow clone, a fork without tags): the VERSION file, which
# also names the next release candidate and release tags
# (dev-docs/release-checklist.md, "Release pipeline").
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

.PHONY: test-postgres all build size run test test-short lint vet generate manifests fmt openapi client-go rbac-manifests tidy clean docker helm-lint check-generated validator-crds dex test-auth-e2e bench bench-baseline envtest
.PHONY: all build size run test test-short lint vet generate manifests fmt tidy clean docker helm-lint check-generated validator-crds dex test-auth-e2e bench bench-baseline envtest dashboard-install dashboard-build dashboard-test dashboard-lint dashboard-dev dashboard-e2e
.PHONY: all build size run test test-short lint vet generate manifests fmt tidy clean docker helm-lint check-generated validator-crds dex test-auth-e2e bench bench-baseline envtest edge-check edge-images kind-e2e
.PHONY: all build size run test test-short lint vet generate manifests fmt tidy clean docker docker-debug docker-hostnetwork docker-dhcp hostnetwork-check volume-check helm-lint helm-check deploy-tools deploy-assets release-manifests check-generated validator-crds dex test-auth-e2e bench bench-baseline envtest

all: build

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/kudurru

## size: build the stripped CGO_ENABLED=0 release binary and fail when it is
## larger than SIZE_BUDGET bytes (D75, D138). History: 46,575,776 at the end
## of M1, 53,862,560 at the end of M4, 54,874,272 for v0.1.0-rc.1 (budget
## 60,362,000), 58,523,808 after the v0.1 UX round, 59,826,336 after the
## catalog round and 57,991,328 with the dashboard embedded precompressed
## (dashboard/scripts/compress.ts: 2.5 MiB of bundle as 0.75 MiB of gzip)
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

## test-postgres: the central-database tests (internal/central, D158)
## against PostgreSQL in Docker; CI runs them in every test job.
POSTGRES_IMAGE ?= postgres:17-alpine
test-postgres:
	docker rm -f kudurru-test-postgres >/dev/null 2>&1 || true
	docker run -d --rm --name kudurru-test-postgres -e POSTGRES_PASSWORD=kudurru -p 55432:5432 $(POSTGRES_IMAGE) >/dev/null
	until docker exec kudurru-test-postgres pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	KUDURRU_TEST_POSTGRES_DSN='postgres://postgres:kudurru@127.0.0.1:55432/postgres?sslmode=disable' KUDURRU_REQUIRE_POSTGRES=1 \
		go test -count=1 $$(go list ./... | grep -E '/internal/(central|audit|querylog)|/cmd/kudurru'); \
		status=$$?; docker rm -f kudurru-test-postgres >/dev/null; exit $$status

vet:
	go vet ./...

lint: vet
	$(GOLANGCI_LINT) run ./...

fmt:
	gofmt -s -w ./cmd ./api ./internal ./ee ./dashboard/*.go

## rbac-manifests: generate the chart's aggregated ClusterRoles
## (deploy/helm/kudurru/templates/rbac-aggregated.yaml) from
## internal/rbac/permissions.yaml (D31); TestAggregatedRolesDrift fails when
## the committed file is stale. Run `make deploy-assets` afterwards so the
## kustomize bases rendered from the chart follow.
rbac-manifests:
	go run ./ee/rbac/rbacgen -out deploy/helm/kudurru/templates/rbac-aggregated.yaml

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
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/v1alpha1/... paths=./api/internalkinds/v1alpha1/... paths=./api/audit/v1alpha1/... paths=./api/dhcp/v1alpha1/...

## manifests: the kudurru.io CRDs into deploy/kustomize/crd/bases (the store's kinds:
## validator, CRD reference) and the audit.kudurru.io AuditLog CRD into
## deploy/kustomize/crd/audit (runtime data the store never serves, D157) and the
## dhcp.kudurru.io DHCPLease CRD into deploy/kustomize/crd/dhcp (runtime data, D180);
## all are copied into the kudurru-crds chart.
manifests:
	$(CONTROLLER_GEN) crd:crdVersions=v1 paths=./api/v1alpha1/... output:crd:artifacts:config=deploy/kustomize/crd/bases
	$(CONTROLLER_GEN) crd:crdVersions=v1 paths=./api/audit/v1alpha1/... output:crd:artifacts:config=deploy/kustomize/crd/audit
	$(CONTROLLER_GEN) crd:crdVersions=v1 paths=./api/dhcp/v1alpha1/... output:crd:artifacts:config=deploy/kustomize/crd/dhcp
	mkdir -p deploy/helm/kudurru-crds/templates
	@# The chart copies carry helm.sh/resource-policy: keep so that
	@# `helm uninstall kudurru-crds` never deletes the CRDs and every object (§5.1).
	for f in deploy/kustomize/crd/bases/*.yaml deploy/kustomize/crd/audit/*.yaml deploy/kustomize/crd/dhcp/*.yaml; do \
		awk '{ print } /^    controller-gen.kubebuilder.io\/version:/ { print "    helm.sh/resource-policy: keep" }' $$f > deploy/helm/kudurru-crds/templates/$$(basename $$f); \
	done
	$(MAKE) --no-print-directory validator-crds

## validator-crds: copy the CRDs into the store validator's go:embed directory (D34)
validator-crds:
	mkdir -p internal/store/validator/crds
	rm -f internal/store/validator/crds/*.yaml
	cp deploy/kustomize/crd/bases/*.yaml internal/store/validator/crds/

## check-generated: fail if `make generate` or `make docs-gen` would change
## tracked files (the docs site's references follow the CRDs, the settings
## table, the CLI and the OpenAPI document)
check-generated: generate
	$(MAKE) --no-print-directory docs-gen
	git diff --exit-code -- api deploy/kustomize/crd deploy/helm/kudurru-crds internal/store/validator/crds docs-site/content/reference

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
	CGO_ENABLED=1 KUDURRU_DEX_BIN=$(abspath $(DEX_BIN)) go test -race -count=1 -v -run 'TestDexEndToEnd|TestMockOPEndToEnd' ./ee/oidc/

## e2e: end-to-end tests against a freshly built `kudurru serve`
## (dev-docs/milestones/m5/e2e.md): two nodes on loopback sharing the encryption key,
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
## (dev-docs/design-record.md §7.3, dev-docs/milestones/m2/xfr.md). The baseline is
## machine-specific: refresh it with `make bench-baseline` on the runner
## class that enforces it (dev-docs/spikes/blocking.md, "Refreshing").
BENCH_PKGS      ?= ./internal/blocking/...
# Only the 100k-record lookup benchmark of internal/zone is gated; its
# build and export benchmarks are reported in dev-docs/milestones/m1/zone.md.
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

## dashboard-*: the dashboard SPA in dashboard/ (Bun + Vite, dev-docs/design-record.md §10,
## D69). `make build` never needs Bun: it embeds whatever dashboard/dist holds,
## which on a plain checkout is the committed placeholder index.html. Run
## `make dashboard-build` first for a binary with the real dashboard (CI and
## release images do). dashboard-build overwrites the tracked placeholder
## dashboard/dist/index.html; do not commit the built one (TestIndexAssetsPresent
## in ./web fails if you do).
BUN ?= bun

dashboard-install:
	cd dashboard && $(BUN) install --frozen-lockfile

dashboard-build: dashboard-install
	cd dashboard && $(BUN) run build

dashboard-test: dashboard-install
	cd dashboard && $(BUN) run test

dashboard-lint: dashboard-install
	cd dashboard && $(BUN) run lint && $(BUN) run typecheck

## dashboard-dev: the dashboard against the MSW mock API (no server needed)
dashboard-dev: dashboard-install
	cd dashboard && $(BUN) run dev:mock

## dashboard-e2e: the Playwright suite (dashboard/e2e) against a real `kudurru serve`
## with the embedded dashboard: builds the bundle and the binary (with
## -tags licensetest, so the suite's node can run with a test license key),
## starts
## the server on free loopback ports with its own data directory, sends
## DNS traffic and drives every page in Chrome. Locally it uses
## /usr/bin/google-chrome (KUDURRU_E2E_CHROME names another; without one,
## run `cd dashboard && bunx playwright install chromium` once); CI uses
## Playwright's chromium. The HTML report lands in dashboard/playwright-report.
dashboard-e2e: dashboard-build
	$(MAKE) build GOFLAGS="$(GOFLAGS) -tags=licensetest"
	cd dashboard && KUDURRU_BIN=../$(BIN) $(BUN) run e2e

## Docs site (docs-site/, Hugo; docs-site/README.md). The Hugo extended release
## binary (Apache-2.0) is downloaded into bin/ at HUGO_VERSION and checked
## against the pinned SHA-256 of the linux tarball; on other platforms
## install Hugo extended $(HUGO_VERSION) and pass HUGO=/path/to/hugo.
##   docs-gen:   regenerate the references under docs-site/content/reference
##               (settings, CRDs, CLI); TestReferenceDrift (hack/docsgen) and
##               TestCLIReferenceDrift (cmd/kudurru) fail when they are stale
##   docs-site:  build the site into docs-site/public with --panicOnWarning, so
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
DOCS_GENERATED          := docs-site/.generated

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

## docs-assets: the decompressed Scalar and Mermaid bundles
## (internal/api/scalar, docs-site/vendor/mermaid), and the list of repository
## files the link hook checks links into the repository against (Hugo cannot
## stat files outside docs-site/)
docs-assets:
	mkdir -p $(DOCS_GENERATED)/scalar $(DOCS_GENERATED)/mermaid
	gunzip -c internal/api/scalar/standalone.js.gz > $(DOCS_GENERATED)/scalar/standalone.js
	gunzip -c docs-site/vendor/mermaid/mermaid.min.js.gz > $(DOCS_GENERATED)/mermaid/mermaid.min.js
	{ git ls-files --cached --others --exclude-standard 2>/dev/null || find . -path ./.git -prune -o -type f -print | sed 's|^\./||'; } > $(DOCS_GENERATED)/repo-files.txt

docs-site: docs-assets $(HUGO)
	cd docs-site && $(HUGO) $(HUGO_FLAGS)

docs-serve: docs-assets $(HUGO)
	cd docs-site && $(HUGO) server --bind 127.0.0.1 --port $${PORT:-1313}

## docs-check-links: build the site for the root of its own domain
## (docs.kudurru.io) and check every internal link, asset and fragment of the
## HTML with htmltest (MIT, docs-site/.htmltest.yml)
HTMLTEST ?= go run github.com/wjdp/htmltest@v0.17.0
docs-check-links: docs-assets $(HUGO)
	rm -rf bin/docs-links bin/docs-links-log
	cd docs-site && $(HUGO) $(HUGO_FLAGS) --baseURL http://localhost/ --destination ../bin/docs-links
	$(HTMLTEST) -c docs-site/.htmltest.yml

## docs-check-workers: build the site as Cloudflare Workers Builds does and
## run `wrangler deploy --dry-run` on docs-site/wrangler.jsonc (no Cloudflare
## account needed; installs the locked Wrangler dependency). Workers Builds
## deploys docs.kudurru.io from GitHub; nothing here deploys.
## docs-site/README.md, "Publishing".
DOCS_URL ?= https://docs.kudurru.io
WRANGLER ?= $(BUN) x wrangler
.PHONY: docs-check-workers
docs-check-workers: docs-assets $(HUGO)
	cd docs-site && $(BUN) install --frozen-lockfile
	cd docs-site && $(HUGO) $(HUGO_FLAGS) --baseURL $(DOCS_URL)/
	cd docs-site && WRANGLER_SEND_METRICS=false $(WRANGLER) deploy --dry-run
## Deployment assets (dev-docs/milestones/m3/deploy.md). The tools are Apache-2.0 and are
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
	helm lint --strict deploy/helm/kudurru deploy/helm/kudurru-crds

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
## deploy/kustomize/default) for the GitHub release, written to bin/release/.
release-manifests: $(DEPLOY_BIN)/kustomize
	mkdir -p bin/release
	$(DEPLOY_BIN)/kustomize build deploy/kustomize/crd > bin/release/crds.yaml
	$(DEPLOY_BIN)/kustomize build deploy/kustomize/default > bin/release/install.yaml

## edge-check: validate the golden dnsdist and keepalived configurations
## (ee/edge/*/testdata) with the real programs inside the pinned images:
## `dnsdist --check-config` in the upstream dnsdist image (digest from
## images/dnsdist/Dockerfile) and `keepalived -t` in the kudurru-keepalived
## image built from source. Needs Docker and network; CI runs it in the
## optional edge workflow. The unit tests only check the files structurally.
EDGE_DNSDIST_IMAGE    ?= $(shell sed -n 's/^FROM \(powerdns\/dnsdist-[0-9]*:[^@ ]*@sha256:[0-9a-f]*\)$$/\1/p' images/dnsdist/Dockerfile)
EDGE_KEEPALIVED_IMAGE ?= kudurru-keepalived:edge-check
EDGE_SUPERVISOR_IMAGE ?= kudurru-dnsdist:edge-check
EDGE_KUDURRU_IMAGE    ?= kudurru:edge-check
EDGE_CHECK_DIR        ?= bin/edge-check

# The supervisors refuse to start without a license key that covers the edge,
# so the kudurru image of the check is built with -tags licensetest (it trusts
# the throwaway key of internal/license/testdata, which hack/edge-root-check.sh
# hands to the supervisor). It is never pushed.
edge-images:
	docker build -t $(EDGE_KUDURRU_IMAGE) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) --build-arg GOTAGS=licensetest .
	docker build -t $(EDGE_KEEPALIVED_IMAGE) --build-arg KUDURRU_IMAGE=$(EDGE_KUDURRU_IMAGE) images/keepalived
	docker build -t $(EDGE_SUPERVISOR_IMAGE) --build-arg KUDURRU_IMAGE=$(EDGE_KUDURRU_IMAGE) images/dnsdist

edge-check: edge-images
	CGO_ENABLED=1 go test -count=1 -run 'Golden' ./ee/edge/...
	rm -rf $(EDGE_CHECK_DIR) && mkdir -p $(EDGE_CHECK_DIR)/certs
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=edge-check \
	  -keyout $(EDGE_CHECK_DIR)/certs/tls.key -out $(EDGE_CHECK_DIR)/certs/tls.crt 2>/dev/null
	chmod 0644 $(EDGE_CHECK_DIR)/certs/tls.key
	docker run --rm --entrypoint /bin/sh \
	  -v $(CURDIR)/ee/edge/dnsdist/testdata:/golden:ro -v $(CURDIR)/$(EDGE_CHECK_DIR)/certs:/certs:ro \
	  $(EDGE_DNSDIST_IMAGE) -c 'set -e; dnsdist --version | head -1; for d in /golden/*/; do \
	    case $$d in *-ebpf/) continue;; esac; \
	    echo "== $$d (yaml)"; dnsdist --check-config -C $$d/dnsdist.yml; \
	    echo "== $$d (lua)"; dnsdist --check-config -C $$d/dnsdist.conf; done'
	# eBPF DynBlocks: --check-config creates the BPF maps, which needs
	# CAP_BPF/CAP_SYS_ADMIN as an effective capability, so root with the
	# chart's capability set (deploy/helm/kudurru _pod.tpl, kudurru.edge.rootStart).
	docker run --rm --user 0:953 --cap-drop ALL --cap-add NET_BIND_SERVICE --cap-add SETUID --cap-add SETGID \
	  --cap-add KILL --cap-add BPF --cap-add SYS_ADMIN --security-opt no-new-privileges --entrypoint /bin/sh \
	  -v $(CURDIR)/ee/edge/dnsdist/testdata:/golden:ro -v $(CURDIR)/$(EDGE_CHECK_DIR)/certs:/certs:ro \
	  $(EDGE_DNSDIST_IMAGE) -c 'set -e; for d in /golden/*-ebpf/; do \
	    echo "== $$d (yaml)"; dnsdist --check-config -C $$d/dnsdist.yml; \
	    echo "== $$d (lua)"; dnsdist --check-config -C $$d/dnsdist.conf; done'
	docker run --rm --entrypoint /bin/sh -v $(CURDIR)/ee/edge/keepalived/testdata:/golden:ro \
	  $(EDGE_KEEPALIVED_IMAGE) -c 'set -e; keepalived --version 2>&1 | head -1; mkdir -p /run/secrets; umask 077; \
	    for v in 53 54; do printf %s 0123456789abcdef0123456789abcdef > /run/secrets/vrrp_hmac_$$v; done; \
	    for f in /golden/*.conf; do echo "== $$f"; keepalived -t -f $$f; done'
	# The chart's root start at run time: dnsdist creates the maps, binds :53
	# without the sysctl, switches to 953 keeping CAP_SYS_ADMIN, and stops.
	hack/edge-root-check.sh $(EDGE_SUPERVISOR_IMAGE)
	docker run --rm --entrypoint /usr/local/bin/kudurru $(EDGE_SUPERVISOR_IMAGE) version
	# The supervisor images name both licenses (GPL program + kudurru).
	test "$$(docker inspect -f '{{index .Config.Labels "org.opencontainers.image.licenses"}}' $(EDGE_SUPERVISOR_IMAGE))" = "LicenseRef-Kudurru-Community AND GPL-2.0-only"
	test "$$(docker inspect -f '{{index .Config.Labels "org.opencontainers.image.licenses"}}' $(EDGE_KEEPALIVED_IMAGE))" = "LicenseRef-Kudurru-Community AND GPL-2.0-or-later"

## kind-e2e: three replicas on a four-node kind cluster with MetalLB and both
## charts: dig through the LoadBalancer over UDP and TCP (split and mixed
## Services), the API, query log and audit log of every replica, a rolling
## restart without unanswered queries, then the same with a PostgreSQL
## database that is stopped and started (hack/kind-e2e.sh; needs docker, kind,
## helm, kubectl, jq, dig).
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

## docker-dhcp: opt-in local DHCP development image with both file
## capabilities; requires NET_BIND_SERVICE and NET_RAW at runtime.
## This does not change the capabilities of the published DNS images.
docker-dhcp: docker
	docker build -t kudurru:dhcp-development --build-arg KUDURRU_IMAGE=ghcr.io/robertherbaugh/kudurru:$(VERSION) --build-arg DHCP_CAPABILITIES=true images/hostnetwork

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
	rm -rf bin dist dashboard/dist/assets

## website-*: the kudurru.io website in website/ (Bun + TypeScript, served by
## Cloudflare Workers static assets; website/README.md). The pages are rendered
## at build time from website/src; the brand artwork and the dashboard
## screenshots come from branding and docs/guide/images. Cloudflare
## Workers Builds deploys it from GitHub; nothing here deploys.
##   website-check: typecheck, tests (pages, links, CSP, _headers), a build and
##              `wrangler deploy --dry-run` (no Cloudflare account needed)
##   website-dev:   rebuild on change and serve with `wrangler dev` on
##              http://localhost:8787/
.PHONY: website-install website-build website-check website-dev
website-install:
	cd website && $(BUN) install --frozen-lockfile

website-build: website-install
	cd website && $(BUN) run build

website-check: website-install
	cd website && $(BUN) run typecheck && $(BUN) test && $(BUN) run build && $(BUN) run check:deploy

website-dev: website-install
	cd website && $(BUN) run dev

## license-server-*: the kudurru license portal in license-server/, the
## licensor's service that issues license keys (Bun, TypeScript, Postgres
## and an Astryx UI in one container; license-server/README.md). It is not
## part of kudurru or its release and no node contacts it (D163); nothing
## here deploys. The image builds from the repository root, because it
## carries the trusted public keys of internal/license/keys.
##   license-server-check: typecheck, lint, the server and UI tests and the
##                         build. The database tests need TEST_DATABASE_URL,
##                         for example the compose Postgres:
##                         postgres://postgres:postgres@127.0.0.1:55432/postgres
##                         (license-server-dev starts it); without it they
##                         skip, and REQUIRE_DB=1 (CI) makes that a failure
##   license-server-keys:  the development signing key and the Dex client
##                         secret in license-server/dev/secrets (gitignored),
##                         once; an existing key is kept (dev/README.md)
##   license-server-dev:   Postgres, Dex and Mailpit in compose, the Bun
##                         server on :8790 and Vite on http://localhost:5174/
##                         (settings: license-server/dev/server.env)
##   license-server-image: ghcr.io/robertherbaugh/kudurru-license-server:dev
##   license-server-up:    that image with Postgres, Dex and Mailpit on
##                         http://localhost:8791/ (dev users: dev/README.md);
##                         license-server-down stops it and keeps the database
##                         volume (add -v to the compose command to drop it)
##   license-server-e2e:   the Playwright suite (license-server/e2e) against a
##                         fresh compose stack, removed afterwards with its
##                         volumes; the logs are printed when it fails
## The image is built with `docker build`, not `compose --build`, so the
## stack runs exactly what license-server-image produces (and compose's
## build, which needs buildx 0.17 or later, is not involved).
LICENSE_SERVER_COMPOSE := docker compose -f license-server/compose.yaml
# The tag license-server/compose.yaml runs; change both together.
LICENSE_SERVER_IMAGE   := ghcr.io/robertherbaugh/kudurru-license-server:dev
.PHONY: license-server-install license-server-check license-server-keys license-server-dev license-server-image license-server-up license-server-down license-server-e2e
license-server-install:
	cd license-server && $(BUN) install --frozen-lockfile

license-server-check: license-server-install
	cd license-server && $(BUN) run typecheck && $(BUN) run lint && $(BUN) run test && $(BUN) run build

license-server-keys:
	@[ -f license-server/dev/secrets/signing.key ] || (cd license-server && $(BUN) scripts/dev-keygen.ts)

license-server-dev: license-server-install license-server-keys
	$(LICENSE_SERVER_COMPOSE) up -d --wait postgres dex mailpit
	cd license-server && $(BUN) --env-file=dev/server.env run dev

license-server-image:
	docker build -f license-server/Dockerfile --build-arg COMMIT=$(COMMIT) --build-arg BUILT_AT=$(DATE) -t $(LICENSE_SERVER_IMAGE) .

license-server-up: license-server-image license-server-keys
	$(LICENSE_SERVER_COMPOSE) --profile server up -d --wait

license-server-down:
	$(LICENSE_SERVER_COMPOSE) --profile server --profile e2e down

license-server-e2e: license-server-install license-server-image license-server-keys
	status=0; \
	$(LICENSE_SERVER_COMPOSE) --profile e2e up -d --wait && (cd license-server && $(BUN) run e2e) || status=$$?; \
	[ $$status -eq 0 ] || $(LICENSE_SERVER_COMPOSE) --profile e2e logs --no-color --tail 300; \
	$(LICENSE_SERVER_COMPOSE) --profile e2e down -v; \
	exit $$status

## Security scans (.github/workflows/security.yml runs them on every pull
## request, on main and daily). Each fails on a finding; the policy and the
## accepted findings, each with its reason, are in .semgrepignore and the
## `nosemgrep` comments, trivy.yaml and .trivyignore.yaml.
##   sast:                 Semgrep CE (LGPL-2.1) in its pinned image with the
##                         community rule packs (needs Docker; no account,
##                         no metrics)
##   vulncheck:            govulncheck: known vulnerabilities the Go code
##                         can reach
##   trivy-fs:             Trivy (Apache-2.0) on the checkout: the go.mod and
##                         bun.lock dependencies, secrets, and
##                         misconfigurations in the Dockerfiles, the chart
##                         and the manifests
##   trivy-images:         build the kudurru, -debug, kudurru-dnsdist and
##                         kudurru-keepalived images and scan them (needs
##                         Docker; -hostnetwork and kudurru-base add only
##                         distroless and the same binary)
##   trivy-license-server: the license portal image (license-server-image)
##   trivy-published:      the images publish.yml pushed for
##                         PUBLISHED_TAG=v<VERSION>[-rc.<N>], pulled from
##                         ghcr.io (needs `docker login ghcr.io` while the
##                         packages are private) rather than built: the ko
##                         kudurru and -debug, -hostnetwork, kudurru-dnsdist
##                         and kudurru-keepalived. Run it from a checkout of
##                         the tag, whose images/*/Dockerfile give the
##                         supervisor image tags.
## The Semgrep rules come from the registry at run time, so a new rule can
## fail a run without a code change, like a new CVE. Excluded rules: Go
## checks gosec already enforces in `make lint` (G404, G705, G204, G110,
## G124, G115), so a false positive needs one suppression, not two; and
## hostnetwork-pod, since the host-network overlay is an opt-in mode by
## design. Trivy is downloaded into bin/ at TRIVY_VERSION and checked
## against the pinned SHA-256 of the release tarball, rather than run
## through aquasecurity/trivy-action or setup-trivy, whose tags were moved
## to malicious commits in March 2026; on other platforms install Trivy
## $(TRIVY_VERSION) and pass TRIVY=/path/to/trivy.
SEMGREP_IMAGE ?= semgrep/semgrep:1.181.0@sha256:81474464c1f07483ba0682a44787fd93c91397419762a04abe2bafaae396304b
SEMGREP_CONFIGS := p/default p/golang p/typescript p/github-actions p/dockerfile
SEMGREP_EXCLUDE := \
	go.lang.security.audit.crypto.math_random.math-random-used \
	go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter \
	go.lang.security.audit.xss.no-fprintf-to-responsewriter.no-fprintf-to-responsewriter \
	go.lang.security.audit.xss.no-io-writestring-to-responsewriter.no-io-writestring-to-responsewriter \
	go.lang.security.audit.dangerous-exec-command.dangerous-exec-command \
	go.lang.security.decompression_bomb.potential-dos-via-decompression-bomb \
	go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure \
	trailofbits.go.string-to-int-signedness-cast.string-to-int-signedness-cast \
	yaml.kubernetes.security.hostnetwork-pod.hostnetwork-pod
GOVULNCHECK ?= go run golang.org/x/vuln/cmd/govulncheck@v1.9.0
TRIVY_VERSION              ?= 0.76.0
TRIVY_ASSET_linux_amd64    := Linux-64bit
TRIVY_ASSET_linux_arm64    := Linux-ARM64
TRIVY_ASSET_darwin_amd64   := macOS-64bit
TRIVY_ASSET_darwin_arm64   := macOS-ARM64
TRIVY_SHA256_linux_amd64   := e24b1c51293c1b15e4ba8d45bbc7891ba053f90278ec2698f3562c993e145fe7
TRIVY_SHA256_linux_arm64   := aab21e677a7b7071fc79f1711480840754c18a1b9d9cefa7ccd06af63a0e8f4c
TRIVY_SHA256_darwin_amd64  := 89563d4d495cf4ef06a0d1950f8710b124e9f7bd2615ec6c47b03673f4ad45a2
TRIVY_SHA256_darwin_arm64  := e7ec0d21fe842553f59af99aba2efbcc822369fbaed27019c575408b6d135fce
TRIVY_DIR                  := $(CURDIR)/bin/trivy-$(TRIVY_VERSION)
TRIVY                      ?= $(TRIVY_DIR)/trivy
SCAN_TAG                   ?= scan
PUBLISHED_REPO             ?= ghcr.io/robertherbaugh
PUBLISHED_TAG              ?=

.PHONY: sast vulncheck trivy trivy-fs trivy-images trivy-license-server trivy-published

sast:
	docker run --rm -u "$$(id -u):$$(id -g)" -e HOME=/tmp -v "$(CURDIR):/src" -w /src \
	  -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0=/src \
	  $(SEMGREP_IMAGE) semgrep scan --error --metrics off --disable-version-check \
	  $(addprefix --config ,$(SEMGREP_CONFIGS)) $(addprefix --exclude-rule ,$(SEMGREP_EXCLUDE))

vulncheck:
	$(GOVULNCHECK) ./...

trivy: $(TRIVY)

$(TRIVY_DIR)/trivy:
	@platform='$(HUGO_OS)_$(HUGO_ARCH)'; asset='$(TRIVY_ASSET_$(HUGO_OS)_$(HUGO_ARCH))'; sha='$(TRIVY_SHA256_$(HUGO_OS)_$(HUGO_ARCH))'; \
	if [ -z "$$sha" ]; then echo "error: no pinned Trivy checksum for $$platform; install Trivy $(TRIVY_VERSION) and pass TRIVY=/path/to/trivy"; exit 1; fi; \
	rm -rf $(TRIVY_DIR).dl && mkdir -p $(TRIVY_DIR).dl && \
	curl -fsSL --retry 3 --retry-all-errors -o $(TRIVY_DIR).dl/trivy.tar.gz https://github.com/aquasecurity/trivy/releases/download/v$(TRIVY_VERSION)/trivy_$(TRIVY_VERSION)_$$asset.tar.gz && \
	echo "$$sha  $(TRIVY_DIR).dl/trivy.tar.gz" | sha256sum -c - && \
	mkdir -p $(TRIVY_DIR) && tar -xzf $(TRIVY_DIR).dl/trivy.tar.gz -C $(TRIVY_DIR) trivy && \
	rm -rf $(TRIVY_DIR).dl

trivy-fs: $(TRIVY)
	$(TRIVY) fs --scanners vuln,secret,misconfig .

trivy-images: $(TRIVY)
	docker build -t kudurru:$(SCAN_TAG) .
	docker build --target debug -t kudurru:$(SCAN_TAG)-debug .
	docker build -t kudurru-dnsdist:$(SCAN_TAG) --build-arg KUDURRU_IMAGE=kudurru:$(SCAN_TAG) images/dnsdist
	docker build -t kudurru-keepalived:$(SCAN_TAG) --build-arg KUDURRU_IMAGE=kudurru:$(SCAN_TAG) images/keepalived
	@status=0; \
	for image in kudurru:$(SCAN_TAG) kudurru:$(SCAN_TAG)-debug kudurru-dnsdist:$(SCAN_TAG) kudurru-keepalived:$(SCAN_TAG); do \
	  echo "== $$image"; $(TRIVY) image $$image || status=1; \
	done; \
	exit $$status

trivy-license-server: $(TRIVY) license-server-image
	$(TRIVY) image $(LICENSE_SERVER_IMAGE)

# linux/amd64 only, as security.yml's trivy-images: the arm64 images carry
# the same package versions and Go modules. The supervisor image tags are
# derived the way publish.yml's supervisor-manifests derives them.
trivy-published: $(TRIVY)
	@if ! echo '$(PUBLISHED_TAG)' | grep -Eqx 'v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[1-9][0-9]*)?'; then \
	  echo "usage: make trivy-published PUBLISHED_TAG=v<VERSION>[-rc.<N>]"; exit 1; \
	fi; \
	version='$(PUBLISHED_TAG)'; version=$${version#v}; \
	dnsdist=$$(sed -n 's/^FROM powerdns\/dnsdist-[0-9]*:\([^@]*\)@.*$$/\1/p' images/dnsdist/Dockerfile); \
	keepalived=$$(sed -n 's/^ARG KEEPALIVED_VERSION=\(.*\)$$/\1/p' images/keepalived/Dockerfile); \
	if [ -z "$$dnsdist" ] || [ -z "$$keepalived" ]; then echo "error: no dnsdist or keepalived version in images/*/Dockerfile"; exit 1; fi; \
	status=0; \
	for image in kudurru:$$version kudurru:$$version-debug kudurru:$$version-hostnetwork \
	  kudurru-dnsdist:$$dnsdist-k$$version kudurru-keepalived:$$keepalived-k$$version; do \
	  echo "== $(PUBLISHED_REPO)/$$image"; \
	  $(TRIVY) image --image-src remote --platform linux/amd64 $(PUBLISHED_REPO)/$$image || status=1; \
	done; \
	exit $$status

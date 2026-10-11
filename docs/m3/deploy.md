# M3/M4 package: deployment assets (key `deploy`)

Docker, compose, Kubernetes (Helm and kustomize), Linux packages and the
release pipeline for kudurru (docs/architecture.md §5.2, §5.4, §5.7, §6.4,
§6.5, §11.6, §12; D19, D25, D77). Guides: [docs/deploy/](../deploy/README.md).

## What landed

| Area | Files |
|---|---|
| Helm chart | `charts/kudurru`: §5.2 values in full (`values.yaml`, every value commented), Deployment / DaemonSet (hostNetwork) / StatefulSet (standalone or per-pod persistence), mixed-protocol dns Service with `splitProtocols`, api and headless Services, two ConfigMaps (D25), generated-once Secrets (encryption key, admin, PROXYv2, VRRP, bootstrap token; `helm.sh/resource-policy: keep`, reused on upgrade via `lookup`), least-privilege RBAC (D77) and aggregated roles, PDB, topology spread, priorityClassName, `dnsPolicy: Default`, Restricted PSS, edge Deployment/DaemonSet/sidecar, keepalived container, kube-vip/MetalLB annotations, NetworkPolicy, ServiceMonitor/PodMonitor, PrometheusRule, Grafana dashboard ConfigMap, Ingress, seed objects, NOTES.txt with the first login |
| CRD chart | `charts/kudurru-crds`: copies of `config/crd/bases` with `helm.sh/resource-policy: keep` (`make manifests`) |
| kustomize | `config/rbac`, `config/manager` (rendered from the chart), `config/namespace`, `config/default`, overlays `metallb`, `kube-vip`, `cloud-lb-mixed` (patches) and `hostnetwork-daemonset`, `standalone`, `with-edge` (rendered from the chart) |
| Compose | `deploy/compose/single`, `single-edge`, `ha/node-{a,b,c}` (ha-static twins) |
| Monitoring | `deploy/grafana/kudurru-overview.json` (generated: 11 rows, 57 panels, 64 queries), `deploy/prometheus/rules.yaml` (12 alerts on existing metrics) |
| Packages | `deploy/systemd/` unit, default config, env file, pre/post-install scripts |
| Images and release | `Dockerfile` (CGO_ENABLED=0 distroless, `release` and `debug` targets), `.goreleaser.yaml`, `.github/workflows/release.yml`, the `helm` job in `ci.yml` |
| Make | `helm-lint` (strict), `helm-check`, `deploy-tools`, `deploy-assets`, `release-manifests`, `docker-debug` |
| Tests | `deploy/` (package `deploy`, tests only plus a doc file) |

## Serve wiring

None. The package changes no Go code outside `deploy/`; `cmd/kudurru/serve.go`
stays as it is. What the integrator should know:

- The chart starts `kudurru serve --config=/etc/kudurru/node/kudurru.yaml`
  and sets `KUDURRU_NODE__NAME` (pod name), `KUDURRU_HEADLESS_SERVICE`
  (`<release>-headless.<ns>.svc.<clusterDomain>`), `KUDURRU_ENCRYPTION_KEY_FILE`,
  `KUDURRU_ADMIN_PASSWORD_FILE`, optionally `KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE`
  and `KUDURRU_BOOTSTRAP_TOKEN_FILE`, and `GOMEMLIMIT` from the memory limit.
  The settings ConfigMap is mounted at `/etc/kudurru/settings/settings.yaml`;
  a settings package that reads and watches it should be told that path
  (followup row), after which `kudurru.nodeConfig` in `_helpers.tpl` drops
  the merge of `config.settings`.
- The `deploy` tests import `internal/config` only. Any new settings key is
  picked up automatically; renaming or removing a key the chart or compose
  files render fails `go test ./deploy`.
- `make test` runs the deploy tests when `helm` (and for the overlays
  `kustomize`) is on PATH and skips them otherwise; with `CI` set they are
  required. `make helm-check` adds kubeconform.
- The CI `helm` job runs `make helm-check`, verifies the generated assets
  (`make deploy-assets` plus a clean tree) and `goreleaser check`.

## Tests

| Test | What it checks |
|---|---|
| `TestChartRenderMatrix` | 33 values permutations: both modes, split Service, each edge mode with each store mode, each `vip.provider`, hostNetwork (both modes), persistence on/off, ingress, monitoring, NetworkPolicy (plain, edge, keepalived, sidecar), encrypted transports with and without the edge, external-dns webhook, existing secrets, bootstrap token, seed objects (both modes), settings, everything at once. Every render: object identity and namespaces, every rendered `kudurru.yaml` loaded through `config.Load` with the container's `KUDURRU_*__*` env plus a strict unknown-key check against `config.Schema`, every `KUDURRU_*` variable names a key or a reserved variable, Restricted PSS fields on every container except keepalived, Service selectors and named targetPorts resolve to container ports with the right protocol, volume mounts resolve, chart Secrets referenced are rendered, PDB/Ingress/ServiceMonitor targets exist; plus per-case assertions |
| `TestChartRejectsInvalidValues` | 15 invalid combinations fail with a message naming the value |
| `TestChartChecksumCoversNodeFileOnly` | D25: node keys roll pods, other values do not |
| `TestChartLint` | `helm lint --strict` on both charts |
| `TestCRDChartMatchesBases` | CRD chart = `config/crd/bases` + the keep annotation |
| `TestChartKubeconform` | (`KUDURRU_KUBECONFORM=1`) the matrix against Kubernetes 1.30 strict schemas, the kudurru CRDs (converted in the test) and the Prometheus Operator CRDs |
| `TestKustomizeBases` | the rendered kustomize files are current (`-update` regenerates) |
| `TestKustomizeOverlays` | `kustomize build` of `config/default` and the six overlays, the same generic checks, and kubeconform |
| `TestComposeFiles`, `TestComposeHATwins` | structural Compose Specification subset, references, ports, env names, each mounted `kudurru.yaml` through `config.Load`; HA twins identical except node identity |
| `TestGrafanaDashboard`, `TestDashboardMetricsExist`, `TestPrometheusRules` | the generated dashboard is current, parses, has the §11.6 rows, and every `kudurru_*` metric in a query or alert is registered by the code (grep of non-test sources, histogram suffixes resolved); the chart copies are identical |
| `TestGoreleaserConfig`, `TestSystemdUnit`, `TestDockerfile` | the release contract (CGO_ENABLED=0, platforms, distroless nonroot, `-debug`, deb/rpm/apk with the unit, cosign), the unit's ExecStart and hardening, the packaged config through `config.Load` |

## Measurements

| Measurement | Value |
|---|---|
| Render matrix | 33 permutations + 15 failure cases, about 2.6 s for `go test ./deploy` (helm 3.19) |
| kubeconform (Kubernetes 1.30.0, strict) | chart matrix: 612 resources in 33 files valid; overlays: 115 resources in 7 builds valid |
| goreleaser snapshot (`--skip=publish,sign,ko,sbom,docker`) | 2 min 46 s on the shared 8-CPU machine; `goreleaser check` clean |
| Archives | linux/amd64 16.8 MB, linux/arm64 15.0 MB, darwin/amd64 17.1 MB, darwin/arm64 15.6 MB; deb amd64 16.8 MB; compose bundle 4.8 KB |
| Dashboard | 11 rows, 57 panels, 64 queries, all on registered metrics |
| Alerts | 12 (9 of the 14 §11.6 alerts plus `KudurruProxyProtocolRejected`, `KudurruObjectDiagnoses`, `KudurruBlocklistNotRefreshed`) |

## Values invented beyond §5.2

`clusterDomain`, `tls.secretName`, `edge.backendPort`, `edge.proxyProtocolFrom`
(default RFC 1918 + ULA, narrow it), `edge.tlsSecretName`, `edge.existingSecret`,
`edge.dnsdistVersion`, `edge.securityContext` (uid 953 of the dnsdist image),
`edge.resources/nodeSelector/tolerations/affinity/podAnnotations/extraArgs/podDisruptionBudget`,
`vip.metallbPool`, `vip.keepalived.{image,keepalivedVersion,existingSecret,tolerations,resources}`,
`auth.encryptionKeyPreviousSecret`, `auth.bootstrapToken.{enabled,existingSecret}`,
`podMonitor`, `probes`, `lameduckSeconds`, `persistence.standalone.enabled`,
`persistence.*.accessModes`, `service.dns.loadBalancerSourceRanges`,
`service.dns.ipFamilies`, `service.api.annotations`, `grafanaDashboard.labels/annotations`,
`networkPolicy.apiFrom/metricsFrom`, `externalDNS.webhook.port`, `extraEnv`,
`extraVolumes`, `extraVolumeMounts`. Left out of §5.2 because the binary has
nothing to map them onto: `auth.oidc`, `auth.kubernetes.mode`, `webhook`.

## Surprises

- **Unknown `KUDURRU_<SECTION>__<KEY>` variables stop the binary** and list or
  map keys cannot come from the environment at all. The §6.5 compose example
  sets `KUDURRU_LISTENERS__DNS` (a list) and
  `KUDURRU_LISTENERS__INTERNAL__TOKENFILE`-style keys freely; the compose
  files now put listeners in `kudurru.yaml` and the tests check every
  variable. Unknown *file* keys are silently ignored by koanf, hence the
  strict key check in the tests.
- **The binary reads one config file.** D25's hot-reloaded settings
  ConfigMap has no reader yet; the chart mounts it and also merges the keys
  into the node file so they take effect (and roll pods) until it does.
- **`dnsPolicy: Default` and the leader lookup collide.** serve.go dials
  `<pod>.<headless>:8082`; with the node's resolver that name usually does
  not resolve. The chart sets `subdomain` and the FQDN anyway; the fix
  belongs in the binary (EndpointSlice lookup).
- **Non-root :53 in Kubernetes needs a sysctl, not a capability.** Capabilities
  added in the securityContext do not reach the effective set of a non-root
  process (Kubernetes sets no ambient capabilities), so uid 65532 with
  `NET_BIND_SERVICE` still cannot bind :53. The chart sets the safe
  namespaced sysctl `net.ipv4.ip_unprivileged_port_start=0` (allowed under
  Restricted); hostNetwork pods cannot, and ko cannot set file capabilities.
- **`helm uninstall kudurru-crds` would have deleted every kudurru object**:
  CRDs in `templates/` are ordinary release resources. `make manifests` now
  adds `helm.sh/resource-policy: keep` to the chart copies.
- **Per-pod persistence needs a StatefulSet**: querylog.db and local.db share
  `/var/lib/kudurru`, and a Deployment's replicas cannot each have a claim,
  so `persistence.querylog/local` switch mode=kubernetes to a StatefulSet.
- **No `kudurru lameduck`, `edge run`, `vip run` yet**: the chart uses a
  Kubernetes 1.30 `preStop: sleep` (SIGTERM already starts the binary's
  lameduck) and renders the supervisor commands with the flags the
  architecture describes, documented as M4.
- Five §11.6 metrics do not exist (`kudurru_zone_serial`,
  `kudurru_edge_backend_up`, `kudurru_vip_master`, `kudurru_cluster_fenced`,
  `kudurru_dnssec_key_expiry_timestamp_seconds`); their alerts are left out
  and the dashboard shows notes instead of panels.
- The Bash guard of this environment rejects heredocs and arguments that
  contain the substring "git" (for example `github.com` module paths); files
  were written with the editor tools.

## Architecture changes (for the integrator to record in §5.2/§16)

1. The node file is mounted at `/etc/kudurru/node/kudurru.yaml` (passed with
   `--config`), the settings file at `/etc/kudurru/settings/settings.yaml`;
   nested ConfigMap mounts under one read-only directory are avoided.
2. `preStop: sleep: {seconds: lameduckSeconds}` replaces `kudurru lameduck 5s`.
3. `net.ipv4.ip_unprivileged_port_start=0` replaces the file capability for
   non-root low ports in Kubernetes (kudurru and dnsdist pods).
4. mode=kubernetes with `persistence.querylog` or `persistence.local` renders
   a StatefulSet (per-pod claims); hostNetwork plus persistence is rejected.
5. `objects:` seeds are post-install hook resources (no Job, no
   `kudurru apply` dependency); in standalone mode they are inline objects.
6. With `vip.provider=keepalived`, keepalived is a second container in the
   hostNetwork edge DaemonSet (shared `/run/kudurru` emptyDir for the dnsdist
   API key), not a separate DaemonSet.
7. The CRD chart copies carry `helm.sh/resource-policy: keep`.
8. The kustomize bases and shape overlays are rendered from the Helm chart
   and drift-tested; the chart is the single source.
9. The `-debug` image is distroless `debug-nonroot` (busybox, `nslookup`);
   it has no `dig`.
10. The Grafana dashboard is generated from a Go panel list and checked
    against the registered metrics.

## Follow-ups

Added to docs/followups.md under "Deployment": settings-file reader with hot
reload; leader address via EndpointSlice; the M4 supervisors and their flags;
the missing CLI verbs; the missing §11.6 metrics; `externalDNS.webhook.listen`,
OIDC and `cluster.role` keys; consumers of the admin password and bootstrap
token files; aggregated ClusterRoles generated from permissions.yaml;
separate querylog/local paths; hostNetwork file capability; digest pinning
of the distroless bases. One row asks the integrator to record the
architecture changes above. No existing row is closed by this package.

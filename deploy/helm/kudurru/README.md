# kudurru Helm chart

kudurru (koo-DOO-roo) is a Kubernetes-native DNS server: authoritative zones
as CRDs, forwarding with a cache, blocklists, per-client policies, a query
log and a UI. Install the `kudurru-crds` chart first.

```sh
helm install kudurru-crds oci://ghcr.io/robertherbaugh/charts/kudurru-crds --version 0.1.0
helm install kudurru oci://ghcr.io/robertherbaugh/charts/kudurru --version 0.1.0 \
  -n kudurru-system --create-namespace --set replicaCount=1
```

These commands install v0.1.0 with one replica and no license key. For
multiple replicas, add a key as described in the
[license-key guide](../../../docs/deploy/helm.md#license-key).

The guide, the shapes (standalone, hostNetwork, dnsdist edge, MetalLB,
kube-vip, keepalived) and the monitoring values are in
[docs/deploy/helm.md](../../../docs/deploy/helm.md); every value is commented in
[values.yaml](values.yaml). `files/` holds generated copies of
`deploy/prometheus/rules.yaml` and `deploy/grafana/kudurru-overview.json`
(`make deploy-assets`).

Keepalived supports multiple addresses through `vip.keepalived.vips`
(entries with `name`, CIDR `address`, `vrid`, and optional `interface` and
`preemptDelay`). `vip.keepalived.vrrpInterface` separates VRRP traffic
from the VIP interfaces. The existing `vip.address`, `interface`, and
`vrid` values still configure one VIP when the list is empty. The Edge
panel reports live readiness, backend health, and VIP states from every
supervisor.

For Traefik, `ingress.enabled=true` and `ingress.kind=IngressRoute` create
an HTTPS route with `nativeLB: false` and the `kudurru_instance` sticky
cookie by default. Configure the host, `ingress.traefik.entryPoints`, and
TLS settings; override or disable affinity under `ingress.traefik.sticky`.
Standard Ingress resources with `ingress.className=traefik` get the same
cookie settings through Service annotations. Direct Service clients can
opt into `service.api.sessionAffinity=ClientIP`.

Cluster settings default to GitOps ownership. On **DNS > Upstreams**, explicit
adoption enables UI/API management of the entire upstreams section. Values and
ownership then live in `<fullname>-ui-settings`, created by the API and never
rendered by Helm. `config.uiSettingsResource` overrides this name; keep it
stable after adoption. Existing release-namespace ConfigMap RBAC is sufficient.
The other settings sections keep their current management behavior.

After adoption, set `config.settings.upstreams: null` to remove Helm's upstream
declarations and clear the ownership-conflict warning. For a return to GitOps,
generate current Helm values, commit and apply them, wait for the settings
volume to update on every replica, refresh the preview, then release ownership.
Release checks the serving replica's configuration before handoff.
See the [upstreams guide](../../../docs/guide/dashboard/upstreams.md) and
[settings architecture](../../../docs/architecture/configuration-and-store.md#settings).

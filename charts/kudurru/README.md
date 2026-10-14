# kudurru Helm chart

kudurru (koo-DOO-roo) is a Kubernetes-native DNS server: authoritative zones
as CRDs, forwarding with a cache, blocklists, per-client policies, a query
log and a UI. Install the `kudurru-crds` chart first.

```sh
helm install kudurru-crds oci://ghcr.io/robertherbaugh/charts/kudurru-crds
helm install kudurru oci://ghcr.io/robertherbaugh/charts/kudurru -n kudurru-system --create-namespace
```

The guide, the shapes (standalone, hostNetwork, dnsdist edge, MetalLB,
kube-vip, keepalived) and the monitoring values are in
[docs/deploy/helm.md](../../docs/deploy/helm.md); every value is commented in
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

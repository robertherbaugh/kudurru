`kudurru-overview.json` is the §11.6 dashboard (docs/architecture.md): rows
for the overview tiles, traffic, the latency heatmap, blocking, cache,
upstreams, zones, pipeline and query log, cluster and store, the dnsdist edge
and the VIP. It has a `datasource` and a multi-value `instance` (node)
variable.

The file is generated from the panel list in `deploy/dashboard_test.go`;
regenerate it (and the chart's copy in `charts/kudurru/files/`) with
`make deploy-assets`. The test fails when a query names a metric the binary
does not register, so the dashboard only uses metrics that exist today; the
serial-divergence panel and the VIP row are notes until
`kudurru_zone_serial` and `kudurru_vip_master` exist (docs/followups.md).

Import it in Grafana (Dashboards, New, Import), or enable
`grafanaDashboard.enabled` in the Helm chart for the Grafana sidecar.

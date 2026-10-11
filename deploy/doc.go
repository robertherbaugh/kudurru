// Package deploy holds no runtime code. Its tests check the deployment
// assets that live next to it and in charts/ and config/: the Helm chart
// render matrix (every rendered kudurru.yaml loads through config.Load, every
// KUDURRU_* variable names a real key), the compose files, the Grafana
// dashboard and the Prometheus rules (every metric they reference is
// registered by the binary), and optionally kubeconform validation of the
// rendered manifests and the kustomize overlays (docs/m3/deploy.md).
package deploy

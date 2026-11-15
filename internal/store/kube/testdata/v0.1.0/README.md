# AuditLog and DHCPLease CRDs of kudurru 0.1.0

These files are verbatim copies from the `v0.1.0` release tag (commit
`89c59f899d7bfc061e697a563c25732f5ea97985`), the only release that 0.2.0
supports upgrading from (decision D373):

- `audit.kudurru.io_auditlogs.yaml`: `deploy/kustomize/crd/audit/`
- `dhcp.kudurru.io_dhcpleases.yaml`: `deploy/kustomize/crd/dhcp/`

The seven `kudurru.io` CRDs and the samples of 0.1.0 are in
`internal/store/validator/testdata/v0.1.0/`. `TestEnvtestCRDUpgradeFromV01`
(`../../upgrade_v01_test.go`) installs all nine, writes what 0.1.0 stored and
replaces them with this checkout's CRDs. The CRD chart of 0.1.0 carried the
same files with the `helm.sh/resource-policy: keep` annotation. Do not edit
them. To check one:

```sh
git show v0.1.0:deploy/kustomize/crd/audit/audit.kudurru.io_auditlogs.yaml |
  diff - internal/store/kube/testdata/v0.1.0/audit.kudurru.io_auditlogs.yaml
```

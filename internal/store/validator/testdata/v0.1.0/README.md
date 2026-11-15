# CRDs and samples of kudurru 0.1.0

These files are verbatim copies from the `v0.1.0` release tag (commit
`89c59f899d7bfc061e697a563c25732f5ea97985`), the only release that 0.2.0
supports upgrading from (decision D373):

- `crds/`: `deploy/kustomize/crd/bases/kudurru.io_*.yaml`, the schemas the
  0.1.0 binary embedded and the 0.1.0 CRD chart installed.
- `samples/`: `deploy/kustomize/samples/kudurru.io_v1alpha1_*.yaml`.

`TestV01SamplesRemainValid` and `TestV01ObjectsThisReleaseRefuses`
(`../../v01_test.go`) build the 0.1.0 validator from them to tell what that
release stored from what this release accepts. Do not edit them, not even
their spelling. To check or restore one:

```sh
git show v0.1.0:deploy/kustomize/crd/bases/kudurru.io_dnszones.yaml |
  diff - internal/store/validator/testdata/v0.1.0/crds/kudurru.io_dnszones.yaml
```

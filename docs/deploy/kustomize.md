# kudurru on Kubernetes with kustomize

`deploy/kustomize/` is a kubebuilder-style kustomize tree (dev-docs/design-record.md §5.7).
Its bases are rendered from the Helm chart (`make deploy-assets`), so both
install paths deploy the same objects; the deploy tests build every overlay
and validate it with kubeconform.

| Path | Content |
|---|---|
| `deploy/kustomize/crd` | the CRDs (`make manifests`) |
| `deploy/kustomize/rbac` | ServiceAccount, least-privilege ClusterRole/Role, aggregated roles |
| `deploy/kustomize/manager` | Deployment, the node and settings ConfigMaps, dns/api/headless Services, PDB |
| `deploy/kustomize/namespace` | `kudurru-system` with the Restricted Pod Security labels |
| `deploy/kustomize/default` | all of the above: `kustomize build deploy/kustomize/default` is the release's `install.yaml` |
| `deploy/kustomize/samples` | example DNSZone, DNSRecordSet, BlockList, ClientGroup, Policy |
| `deploy/kustomize/overlays/metallb` | MetalLB address and pool annotations on the dns Service |
| `deploy/kustomize/overlays/kube-vip` | kube-vip address annotation and load balancer class |
| `deploy/kustomize/overlays/cloud-lb-mixed` | cloud LoadBalancer annotations (AWS NLB with IP targets) |
| `deploy/kustomize/overlays/hostnetwork-daemonset` | DaemonSet on the host network, privileged namespace |
| `deploy/kustomize/overlays/standalone` | sqlite store, StatefulSet(1) with a PVC, no CRDs or RBAC |
| `deploy/kustomize/overlays/with-edge` | dnsdist edge Deployment in front, kudurru on :5353 with PROXYv2 |

## Install

The manifests mount Secrets you create first (kustomize has no random
generator):

```sh
kubectl apply -f deploy/kustomize/namespace/namespace.yaml
kubectl -n kudurru-system create secret generic kudurru-encryption-key \
  --from-literal=key="$(openssl rand -base64 32)"
kubectl -n kudurru-system create secret generic kudurru-admin --type=kubernetes.io/basic-auth \
  --from-literal=username=admin --from-literal=password="$(openssl rand -base64 18)"
# with-edge only:
kubectl -n kudurru-system create secret generic kudurru-proxy-protocol \
  --from-literal=proxy-protocol-secret="$(openssl rand -hex 32)"

kubectl apply --server-side -k deploy/kustomize/default          # or deploy/kustomize/overlays/<name>
kubectl apply -k deploy/kustomize/samples                        # optional examples
```

The manifests read an optional Secret `kudurru-license` (key `license.key`) as
`KUDURRU_LICENSE_KEY`; without it kudurru runs with no key (one node,
one policy, no edge). To use a key: `kubectl -n kudurru-system create secret
generic kudurru-license --from-file=license.key=./license.key`. With the default two
replicas and no key, changes through the API and the dashboard answer
`402 license_required` ([LICENSING.md](../../LICENSING.md)).

Or straight from a release: `kubectl apply --server-side -f
https://github.com/robertherbaugh/kudurru/releases/download/v0.2.0/install.yaml`.

## Customizing

Write your own overlay on top of `deploy/kustomize/default` (or any overlay) and patch
what you need, for example the upstreams in the node ConfigMap or the
replica count:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - github.com/robertherbaugh/kudurru/deploy/kustomize/overlays/metallb?ref=v0.2.0
patches:
  - target: { kind: Deployment, name: kudurru }
    patch: |-
      - op: replace
        path: /spec/replicas
        value: 3
```

Releases up to v0.1.0 keep this tree under `config/`, so a remote overlay
pinned to one of them uses `config/overlays/<name>`; later releases use
`deploy/kustomize/overlays/<name>`.

Changing the node ConfigMap through kustomize does not update the
`checksum/node-config` annotation the Helm chart maintains; restart the
pods (`kubectl -n kudurru-system rollout restart deployment/kudurru`) after
changing it.

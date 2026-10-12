# kudurru on Kubernetes with kustomize

`config/` is a kubebuilder-style kustomize tree (docs/architecture.md §5.7).
Its bases are rendered from the Helm chart (`make deploy-assets`), so both
install paths deploy the same objects; the deploy tests build every overlay
and validate it with kubeconform.

| Path | Content |
|---|---|
| `config/crd` | the CRDs (`make manifests`) |
| `config/rbac` | ServiceAccount, least-privilege ClusterRole/Role, aggregated roles |
| `config/manager` | Deployment, the node and settings ConfigMaps, dns/api/headless Services, PDB |
| `config/namespace` | `kudurru-system` with the Restricted Pod Security labels |
| `config/default` | all of the above: `kustomize build config/default` is the release's `install.yaml` |
| `config/samples` | example DNSZone, DNSRecordSet, BlockList, ClientGroup, Policy |
| `config/overlays/metallb` | MetalLB address and pool annotations on the dns Service |
| `config/overlays/kube-vip` | kube-vip address annotation and load balancer class |
| `config/overlays/cloud-lb-mixed` | cloud LoadBalancer annotations (AWS NLB with IP targets) |
| `config/overlays/hostnetwork-daemonset` | DaemonSet on the host network, privileged namespace |
| `config/overlays/standalone` | sqlite store, StatefulSet(1) with a PVC, no CRDs or RBAC |
| `config/overlays/with-edge` | dnsdist edge Deployment in front, kudurru on :5353 with PROXYv2 |

## Install

The manifests mount Secrets you create first (kustomize has no random
generator):

```sh
kubectl apply -f config/namespace/namespace.yaml
kubectl -n kudurru-system create secret generic kudurru-encryption-key \
  --from-literal=key="$(openssl rand -base64 32)"
kubectl -n kudurru-system create secret generic kudurru-admin --type=kubernetes.io/basic-auth \
  --from-literal=username=admin --from-literal=password="$(openssl rand -base64 18)"
# with-edge only:
kubectl -n kudurru-system create secret generic kudurru-proxy-protocol \
  --from-literal=proxy-protocol-secret="$(openssl rand -hex 32)"

kubectl apply --server-side -k config/default          # or config/overlays/<name>
kubectl apply -k config/samples                        # optional examples
```

The manifests read an optional Secret `kudurru-license` (key `license.key`) as
`KUDURRU_LICENSE_KEY`; without it kudurru runs with the free license (one node,
no edge). To use a key: `kubectl -n kudurru-system create secret generic
kudurru-license --from-file=license.key=./license.key`. With the default two
replicas and no key, changes through the API and the dashboard answer
`402 license_required` ([LICENSING.md](../../LICENSING.md)).

Or straight from a release: `kubectl apply --server-side -f
https://github.com/robertherbaugh/kudurru/releases/download/v0.1.0/install.yaml`.

## Customising

Write your own overlay on top of `config/default` (or any overlay) and patch
what you need, for example the upstreams in the node ConfigMap or the
replica count:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - github.com/robertherbaugh/kudurru/config/overlays/metallb?ref=v0.1.0
patches:
  - target: { kind: Deployment, name: kudurru }
    patch: |-
      - op: replace
        path: /spec/replicas
        value: 3
```

Changing the node ConfigMap through kustomize does not update the
`checksum/node-config` annotation the Helm chart maintains; restart the
pods (`kubectl -n kudurru-system rollout restart deployment/kudurru`) after
changing it.

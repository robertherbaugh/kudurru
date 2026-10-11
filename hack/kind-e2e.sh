#!/usr/bin/env bash
# kind + MetalLB + Helm smoke test (week-1 spike (f), docs/release-checklist.md
# §3): build the kudurru image, create a kind cluster, install MetalLB in L2
# mode on a slice of the kind Docker network, install the kudurru-crds and
# kudurru charts with vip.provider=metallb, then
#   - dig an authoritative record and a forwarded name through the
#     LoadBalancer address over UDP and TCP,
#   - kubectl get dnszones,
#   - repeat with service.dns.splitProtocols=true (two Services sharing one
#     address through metallb.io/allow-shared-ip),
#   - query continuously over UDP and TCP during a rolling restart (preStop
#     lameduck drain) and fail on any unanswered query.
#
# Needs docker, kind, helm, kubectl and dig on PATH; runs on GitHub's
# ubuntu-latest (.github/workflows/kind.yml). KEEP_CLUSTER=1 keeps the
# cluster for debugging; FORWARD_CHECK=0 skips the forwarded query (no
# internet access to 1.1.1.1/9.9.9.9).
set -euo pipefail

CLUSTER=${CLUSTER:-kudurru-e2e}
NS=${NS:-kudurru}
IMAGE=${IMAGE:-ghcr.io/robertherbaugh/kudurru:kind-e2e}
METALLB_VERSION=${METALLB_VERSION:-v0.16.1}
FORWARD_CHECK=${FORWARD_CHECK:-1}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)

log() { printf '\n== %s\n' "$*"; }

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    log "failure: cluster state"
    kubectl get pods,svc,endpointslices -A -o wide || true
    kubectl -n "$NS" describe pods || true
    kubectl -n "$NS" logs -l app.kubernetes.io/name=kudurru --all-containers --tail=200 --prefix || true
    kubectl -n metallb-system logs -l app=metallb --all-containers --tail=50 --prefix || true
  fi
  if [ "${KEEP_CLUSTER:-0}" != 1 ]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT

log "build $IMAGE"
docker build -q -t "$IMAGE" --build-arg VERSION=kind-e2e --build-arg COMMIT="$(git -C "$ROOT" rev-parse --short HEAD)" "$ROOT"

log "kind cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --wait 120s
# kind's single node is a control-plane node, which carries the label that
# makes MetalLB's speaker skip it; remove it so the address is announced.
kubectl label nodes --all node.kubernetes.io/exclude-from-external-load-balancers-
kind load docker-image "$IMAGE" --name "$CLUSTER"

log "MetalLB $METALLB_VERSION (L2)"
kubectl apply -f "https://raw.githubusercontent.com/metallb/metallb/${METALLB_VERSION}/config/manifests/metallb-native.yaml"
kubectl -n metallb-system rollout status deployment/controller --timeout=180s
kubectl -n metallb-system rollout status daemonset/speaker --timeout=180s
# The pool is the top of the kind network's IPv4 subnet (x.y.255.200-250 of
# a /16), reachable from the Docker host.
subnet=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep -m1 '\.')
prefix=$(echo "$subnet" | cut -d. -f1-2)
LB_IP="${prefix}.255.200"
cat > "$WORK/metallb.yaml" <<EOF
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata: { name: kind, namespace: metallb-system }
spec:
  addresses: ["${prefix}.255.200-${prefix}.255.250"]
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata: { name: kind, namespace: metallb-system }
spec:
  ipAddressPools: [kind]
EOF
# The webhook can lag behind the rollout; retry until it accepts the pool.
for _ in $(seq 1 30); do
  kubectl apply -f "$WORK/metallb.yaml" && break
  sleep 2
done

log "helm install kudurru-crds and kudurru (vip.provider=metallb, address $LB_IP)"
kubectl create namespace "$NS"
helm install kudurru-crds "$ROOT/charts/kudurru-crds" --namespace "$NS" --wait
install_kudurru() {
  helm upgrade --install kudurru "$ROOT/charts/kudurru" --namespace "$NS" \
    --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" --set image.pullPolicy=Never \
    --set vip.provider=metallb --set vip.address="$LB_IP" --set vip.metallbPool=kind \
    --set service.dns.splitProtocols="$1" \
    --wait --timeout 5m
  kubectl -n "$NS" rollout status deployment/kudurru --timeout=180s
}
install_kudurru false

log "seed a zone"
cat > "$WORK/zone.yaml" <<EOF
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: kind-example }
spec:
  zoneName: kind.example.
  type: primary
  soa: { mname: ns1.kind.example., rname: hostmaster.kind.example. }
  nameservers: [ns1.kind.example.]
  recordSetSelector: { namespaces: [${NS}] }
---
apiVersion: kudurru.io/v1alpha1
kind: DNSRecordSet
metadata: { name: www, namespace: ${NS} }
spec:
  zoneRef: { name: kind-example }
  endpoints:
    - { dnsName: www.kind.example, recordType: A, targets: ["192.0.2.10"], recordTTL: 60 }
    - { dnsName: ns1.kind.example, recordType: A, targets: ["192.0.2.53"], recordTTL: 60 }
EOF
kubectl apply -f "$WORK/zone.yaml"
kubectl get dnszones -o wide

# expect_answer <proto flag> <name> <want>: retries for up to 60 s, since
# the LoadBalancer address, ARP and the zone cache settle asynchronously.
expect_answer() {
  local flag=$1 name=$2 want=$3 got=""
  for _ in $(seq 1 30); do
    got=$(dig @"$LB_IP" "$flag" +time=2 +tries=1 +short "$name" A 2>/dev/null || true)
    if [ -n "$want" ] && echo "$got" | grep -qx "$want"; then
      echo "ok $flag $name -> $want"; return 0
    fi
    if [ -z "$want" ] && echo "$got" | grep -Eq '^[0-9.]+$'; then
      echo "ok $flag $name -> $(echo "$got" | head -1)"; return 0
    fi
    sleep 2
  done
  echo "FAIL $flag $name: got '${got}', want '${want:-an address}'"
  return 1
}

check_dns() {
  kubectl -n "$NS" get svc -o wide
  for flag in +notcp +tcp; do
    expect_answer "$flag" www.kind.example. 192.0.2.10
    if [ "$FORWARD_CHECK" = 1 ]; then
      expect_answer "$flag" example.com. ""
    fi
  done
  dig @"$LB_IP" +norec SOA kind.example. | grep -E 'status:|flags:'
}

log "mixed-protocol LoadBalancer Service (splitProtocols=false)"
check_dns

log "split UDP/TCP Services sharing $LB_IP (splitProtocols=true)"
install_kudurru true
check_dns

log "rolling restart under load"
fail=0
total=0
kubectl -n "$NS" rollout restart deployment/kudurru
kubectl -n "$NS" rollout status deployment/kudurru --timeout=300s &
rollout=$!
# Query until the rollout is done, then 5 s more for the old pods' drain.
end=0
while [ "$end" -eq 0 ] || [ "$SECONDS" -lt "$end" ]; do
  for flag in +notcp +tcp; do
    total=$((total + 1))
    if ! dig @"$LB_IP" "$flag" +time=2 +tries=1 +short www.kind.example. A | grep -qx 192.0.2.10; then
      fail=$((fail + 1))
      echo "unanswered ${flag} query at $(date -u +%T)"
    fi
  done
  if [ "$end" -eq 0 ] && ! kill -0 "$rollout" 2>/dev/null; then
    wait "$rollout"
    end=$((SECONDS + 5))
  fi
  sleep 0.2
done
echo "rolling restart: $fail of $total queries unanswered"
[ "$fail" -eq 0 ]

log "kind e2e passed"

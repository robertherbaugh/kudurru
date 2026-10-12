#!/usr/bin/env bash
# kind + MetalLB + Helm end-to-end test (week-1 spike (f), docs/release-checklist.md
# §3, extended in docs/m5/e2e.md): build the kudurru image, create a
# multi-node kind cluster (hack/kind/cluster.yaml: a control-plane node and
# three workers), install MetalLB in L2 mode on a slice of the kind Docker
# network, install the kudurru-crds and kudurru charts with
# vip.provider=metallb and three replicas, then
#
#   1. Cluster without a database
#      - the replicas run on different nodes;
#      - every replica's API lists all replicas, ready, with their version,
#        and one leader (#8), and shows an object created through another;
#      - dig an authoritative record and a forwarded name through the
#        LoadBalancer address over UDP and TCP, with
#        service.dns.splitProtocols false and true;
#      - "All nodes" of the query log on every replica shows the queries
#        (#9), the audit log shows a change made through one replica on all
#        of them and keeps it after that replica was replaced (#10);
#      - no query goes unanswered during a rolling restart (preStop lameduck
#        drain).
#   2. Cluster with the central PostgreSQL database (docs/deploy/postgres.md)
#      - the query log and the audit log come from the database, with the
#        rows of replicas that no longer exist;
#      - with the database gone DNS keeps answering, the pods stay ready,
#        changes still work and the Diagnosis page reports it; when it is
#        back nothing recorded meanwhile is missing.
#
# Needs docker, kind, helm, kubectl, jq, curl and dig on PATH; runs on
# GitHub's runners (.github/workflows/kind.yml). KEEP_CLUSTER=1 keeps the
# cluster for debugging; FORWARD_CHECK=0 skips the forwarded query (no
# internet access to 1.1.1.1/9.9.9.9); DATABASE=0 skips part 2.
# The jq filters are single-quoted on purpose: $name is a jq variable.
# shellcheck disable=SC2016
set -euo pipefail

CLUSTER=${CLUSTER:-kudurru-e2e}
NS=${NS:-kudurru}
IMAGE=${IMAGE:-ghcr.io/robertherbaugh/kudurru:kind-e2e}
METALLB_VERSION=${METALLB_VERSION:-v0.16.1}
REPLICAS=${REPLICAS:-3}
FORWARD_CHECK=${FORWARD_CHECK:-1}
DATABASE=${DATABASE:-1}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
# The pods of the Deployment (chart label helpers kudurru.selectorLabels).
SELECTOR=app.kubernetes.io/name=kudurru,app.kubernetes.io/instance=kudurru,app.kubernetes.io/component=dns
# DNS names asked through the LoadBalancer to put rows in the query log.
PROBE=probe.kind.example

log() { printf '\n== %s\n' "$*"; }

cleanup() {
  status=$?
  # Port-forwards of a request that was interrupted.
  jobs -p | xargs -r kill 2>/dev/null || true
  if [ "$status" -ne 0 ]; then
    log "failure: cluster state"
    kubectl get nodes,pods,svc,endpointslices -A -o wide || true
    kubectl -n "$NS" get dnszones,dnsrecordsets,auditlogs || true
    kubectl -n "$NS" describe pods || true
    kubectl -n "$NS" logs -l "$SELECTOR" --all-containers --tail=200 --prefix || true
    kubectl -n "$NS" logs statefulset/postgres --tail=50 || true
    kubectl -n metallb-system logs -l app=metallb --all-containers --tail=50 --prefix || true
    kubectl get events -A --sort-by=.lastTimestamp | tail -40 || true
  fi
  if [ "${KEEP_CLUSTER:-0}" != 1 ]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT

# eventually <seconds> <description> <command...>: retries the command every
# 2 s until it succeeds; prints its last output when it never does. The
# command runs as the condition of `until`, so it must return its own
# status (errexit is off inside it).
eventually() {
  local timeout=$1 what=$2
  shift 2
  local end=$((SECONDS + timeout))
  until "$@" >"$WORK/eventually.out" 2>&1; do
    if [ "$SECONDS" -ge "$end" ]; then
      echo "FAIL $what (gave up after ${timeout}s); last output:"
      sed 's/^/    /' "$WORK/eventually.out"
      return 1
    fi
    sleep 2
  done
  echo "ok $what"
}

pod_names() {
  kubectl -n "$NS" get pods -l "$SELECTOR" -o json |
    jq -r '.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name'
}

# ready_pods: exactly $REPLICAS pods, all Ready, none terminating.
ready_pods() {
  kubectl -n "$NS" get pods -l "$SELECTOR" -o json | jq -e --argjson n "$REPLICAS" '
    (.items | length) == $n and
    all(.items[]; .metadata.deletionTimestamp == null and
      ((.status.conditions // []) | any(.type == "Ready" and .status == "True")))' >/dev/null
}

wait_ready() {
  kubectl -n "$NS" rollout status deployment/kudurru --timeout=300s
  eventually 120 "$REPLICAS kudurru pods ready, none terminating" ready_pods
}

# api <pod> <METHOD> <path> [json]: one request to the API of a pod, through
# kubectl port-forward (the pod, not the Service, so every replica is asked
# for its own view). Prints the body; fails unless the status is 2xx.
api() {
  local pod=$1 method=$2 path=$3 body=${4:-}
  local pf="$WORK/pf.$RANDOM" out="$WORK/body.$RANDOM" pid port="" code
  kubectl -n "$NS" port-forward "pod/$pod" :8080 >"$pf" 2>&1 &
  pid=$!
  for _ in $(seq 1 50); do
    port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) ->.*/\1/p' "$pf" | head -1)
    if [ -n "$port" ] || ! kill -0 "$pid" 2>/dev/null; then break; fi
    sleep 0.2
  done
  if [ -z "$port" ]; then
    cat "$pf"
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    return 1
  fi
  local args=(-gsS -m 20 -o "$out" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $TOKEN")
  if [ -n "$body" ]; then args+=(-H 'Content-Type: application/json' --data "$body"); fi
  code=$(curl "${args[@]}" "http://127.0.0.1:$port/api/v1$path") || code=000
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  cat "$out" 2>/dev/null || true
  [[ "$code" == 2* ]]
}

# check <pod> <path> <jq args...> <filter>: GET the path from the pod and
# succeed when the jq filter is true; prints the response when it is not.
check() {
  local pod=$1 path=$2 json
  shift 2
  if ! json=$(api "$pod" GET "$path"); then
    echo "GET $path on $pod failed: $json"
    return 1
  fi
  if ! jq -e "$@" <<<"$json" >/dev/null; then
    echo "GET $path on $pod:"
    head -c 1500 <<<"$json"
    return 1
  fi
}

# on_every_pod <seconds> <description> <function> [args]: the function is
# called with each pod's name first, until it succeeds on that pod.
on_every_pod() {
  local timeout=$1 what=$2 fn=$3 pod
  shift 3
  for pod in $(pod_names); do
    eventually "$timeout" "$what on $pod" "$fn" "$pod" "$@"
  done
}

# --- the assertions ---

# lists_all_nodes: GET /nodes names every pod, each ready with a version,
# and one of them is the leader (#8: a follower used to list itself and the
# leader only).
lists_all_nodes() {
  local names
  names=$(pod_names | sort | paste -sd, -)
  check "$1" /nodes --arg names "$names" '
    ([.items[].name] | sort) == ($names | split(",")) and
    all(.items[]; .ready == true and ((.version // "") != "")) and
    ([.items[] | select(.leader)] | length) == 1'
  check "$1" /cluster --argjson n "$REPLICAS" '.nodes == $n'
}

has_zone() { # pod name
  check "$1" /objects/dnszones --arg n "$2" 'any(.items[]; .metadata.name == $n)'
}

create_zone() { # pod name
  local body
  body=$(jq -nc --arg n "$2" '{apiVersion: "kudurru.io/v1alpha1", kind: "DNSZone", metadata: {name: $n},
    spec: {zoneName: ($n + ".example."), type: "primary", nameservers: ["ns1." + $n + ".example."],
      policy: {queryAccess: "allow", zoneTransfer: "deny", notify: "none", update: "deny"}}}')
  api "$1" POST /objects/dnszones "$body" >/dev/null
}

# audit_has <pod> <resource name> <node>: GET /audit shows the creation of a
# zone, recorded by <node> (the events of every replica, #10).
audit_has() {
  check "$1" '/audit?order=desc&limit=200' --arg n "$2" --arg node "$3" '
    any(.items[]; .action == "dnszone.create" and .resource.name == $n and .node == $node)'
}

# queries_have <pod> <central|any> <tag...>: the cluster-wide query log has a
# row for <tag>.$PROBE for every tag (#9), from the central database when
# <central>.
queries_have() {
  local pod=$1 source=$2
  shift 2
  check "$pod" "/queries?scope=cluster&domain=*.$PROBE&limit=500" --arg src "$source" --arg tags "$*" '
    .scope == "cluster" and ($src == "any" or .source == "central") and
    (. as $r | all(($tags | split(" "))[]; . as $t | any($r.queries[]; .domain | startswith($t + ".")))) '
}

# queries_from_nodes <pod> <n>: the probe queries came from at least n nodes.
queries_from_nodes() {
  check "$1" "/queries?scope=cluster&domain=*.$PROBE&limit=500" --argjson n "$2" '[.queries[].node] | unique | length >= $n'
}

no_central_message() {
  check "$1" /info/messages 'all(.items[]; .source != "central")'
}

central_unreachable() {
  check "$1" /info/messages 'any(.items[]; .source == "central" and .reason == "CentralUnreachable")'
}

# --- DNS through the LoadBalancer ---

# send_queries <tag>: names the zone does not hold, so they are logged
# whatever the answer is. Only the node that announces the address answers.
send_queries() {
  local flag
  for _ in 1 2 3; do
    for flag in +notcp +tcp; do
      dig @"$LB_IP" "$flag" +time=2 +tries=1 +short "$1.$PROBE." A >/dev/null 2>&1 || true
    done
  done
}

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

# dns_for <seconds>: every query is answered for that long.
dns_for() {
  local end=$((SECONDS + $1)) flag total=0 fail=0
  while [ "$SECONDS" -lt "$end" ]; do
    for flag in +notcp +tcp; do
      total=$((total + 1))
      if ! dig @"$LB_IP" "$flag" +time=2 +tries=1 +short www.kind.example. A | grep -qx 192.0.2.10; then
        fail=$((fail + 1))
        echo "unanswered ${flag} query at $(date -u +%T)"
      fi
    done
    sleep 0.2
  done
  echo "$fail of $total queries unanswered in $1 s"
  [ "$fail" -eq 0 ]
}

rolling_restart_under_load() {
  local fail=0 total=0 rollout end=0 flag
  kubectl -n "$NS" rollout restart deployment/kudurru
  kubectl -n "$NS" rollout status deployment/kudurru --timeout=300s &
  rollout=$!
  # Query until the rollout is done, then 5 s more for the old pods' drain.
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
}

# --- the cluster ---

log "build $IMAGE"
docker build -q -t "$IMAGE" --build-arg VERSION=kind-e2e --build-arg COMMIT="$(git -C "$ROOT" rev-parse --short HEAD)" "$ROOT"

log "kind cluster $CLUSTER (control-plane and three workers)"
kind create cluster --name "$CLUSTER" --config "$ROOT/hack/kind/cluster.yaml" --wait 180s
# kind's workers carry no label that keeps MetalLB's speaker away, but the
# control-plane node does; remove it everywhere so the address can be
# announced from any node that has a ready endpoint.
kubectl label nodes --all node.kubernetes.io/exclude-from-external-load-balancers-
kind load docker-image "$IMAGE" --name "$CLUSTER"
kubectl get nodes -o wide

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

log "helm install kudurru-crds and kudurru ($REPLICAS replicas, vip.provider=metallb, address $LB_IP)"
kubectl create namespace "$NS"
helm install kudurru-crds "$ROOT/charts/kudurru-crds" --namespace "$NS" --wait
if [ "$DATABASE" = 1 ]; then
  # Started now, used in part 2.
  kubectl -n "$NS" apply -f "$ROOT/hack/kind/postgres.yaml"
fi
# install_kudurru <splitProtocols> [database secret]
install_kudurru() {
  local db=()
  if [ -n "${2:-}" ]; then db=(--set database.existingSecret="$2"); fi
  helm upgrade --install kudurru "$ROOT/charts/kudurru" --namespace "$NS" \
    --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" --set image.pullPolicy=Never \
    --set replicaCount="$REPLICAS" --set auth.bootstrapToken.enabled=true \
    --set vip.provider=metallb --set vip.address="$LB_IP" --set vip.metallbPool=kind \
    --set service.dns.splitProtocols="$1" "${db[@]}" \
    --wait --timeout 5m
  wait_ready
}
install_kudurru false
# An admin-role token for the API calls (auth.bootstrapToken).
TOKEN=$(kubectl -n "$NS" get secret kudurru-bootstrap-token -o jsonpath='{.data.token}' | base64 -d)

log "replicas spread over the nodes"
kubectl -n "$NS" get pods -l "$SELECTOR" -o wide
spread=$(kubectl -n "$NS" get pods -l "$SELECTOR" -o json | jq '[.items[].spec.nodeName] | unique | length')
if [ "$spread" -lt "$REPLICAS" ]; then
  echo "FAIL $REPLICAS replicas run on $spread nodes"
  exit 1
fi
echo "ok $REPLICAS replicas on $spread nodes"

log "every replica lists the whole cluster (#8)"
on_every_pod 120 "all $REPLICAS nodes listed" lists_all_nodes

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
on_every_pod 60 "zone kind-example listed" has_zone kind-example

log "mixed-protocol LoadBalancer Service (splitProtocols=false)"
check_dns

log "query log of all nodes without a database (#9)"
send_queries before
on_every_pod 90 "cluster query log has the queries" queries_have any before

log "audit log of every replica, and after a replica was replaced (#10)"
mapfile -t pods < <(pod_names)
creator=${pods[0]}
create_zone "$creator" audit-probe
on_every_pod 90 "audit has the zone created through $creator" audit_has audit-probe "$creator"
kubectl -n "$NS" delete pod "$creator" --wait=true
wait_ready
log "audit log after $creator was replaced"
on_every_pod 120 "audit keeps the events of $creator" audit_has audit-probe "$creator"
on_every_pod 120 "all $REPLICAS nodes listed" lists_all_nodes

log "split UDP/TCP Services sharing $LB_IP (splitProtocols=true)"
install_kudurru true
check_dns

log "rolling restart under load"
rolling_restart_under_load

if [ "$DATABASE" != 1 ]; then
  log "kind e2e passed (without the database)"
  exit 0
fi

# --- the central database ---

log "central PostgreSQL database"
kubectl -n "$NS" rollout status statefulset/postgres --timeout=300s
install_kudurru false kudurru-db
on_every_pod 120 "no database message" no_central_message

log "query log from the database, with the rows of replicas that no longer exist"
send_queries pre
on_every_pod 90 "central query log has pre" queries_have central pre
mapfile -t pods < <(pod_names)
creator=${pods[0]}
create_zone "$creator" audit-db
on_every_pod 90 "audit has the zone created through $creator" audit_has audit-db "$creator"
kubectl -n "$NS" rollout restart deployment/kudurru
wait_ready
send_queries post
# The pre rows were written by pods that are gone; the post rows by new ones.
on_every_pod 90 "central query log has pre and post" queries_have central pre post
on_every_pod 90 "queries from at least two nodes" queries_from_nodes 2
on_every_pod 90 "audit keeps the events of $creator, which is gone" audit_has audit-db "$creator"
on_every_pod 120 "all $REPLICAS nodes listed" lists_all_nodes

log "the database goes away"
kubectl -n "$NS" scale statefulset/postgres --replicas=0
kubectl -n "$NS" wait --for=delete pod/postgres-0 --timeout=120s
dns_for 15
ready_pods
echo "ok the pods stay ready without the database"
mapfile -t pods < <(pod_names)
create_zone "${pods[1]}" audit-outage
echo "ok a change through ${pods[1]} without the database"
send_queries outage
# A minute without the database is news (CentralUnreachable).
on_every_pod 240 "reports the database unreachable" central_unreachable
dns_for 5

log "the database comes back"
kubectl -n "$NS" scale statefulset/postgres --replicas=1
kubectl -n "$NS" rollout status statefulset/postgres --timeout=300s
on_every_pod 180 "database message gone" no_central_message
on_every_pod 120 "central query log has the queries of the outage" queries_have central pre post outage
on_every_pod 120 "audit has the change of the outage" audit_has audit-outage "${pods[1]}"

log "kind e2e passed"

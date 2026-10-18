#!/usr/bin/env bash
# Docker Compose end-to-end test, the Docker counterpart of hack/kind-e2e.sh:
# runs the compose deployments users copy from deploy/compose/ with images
# built from this checkout. The files are used as shipped; a generated
# override file only swaps the images, publishes the ports on loopback high
# ports (the runner's resolver owns :53) and adds an API token
# (KUDURRU_BOOTSTRAP_TOKEN_FILE). deploy/compose_test.go checks the same
# files structurally; this starts them.
#
#   1. single (deploy/compose/single): kudurru alone, read-only root file
#      system, every capability dropped
#      - becomes healthy through the file's own healthcheck, as uid 65532;
#      - answers a zone from objects.d/ and a forwarded name over UDP and TCP;
#      - a zone created through the API answers; a settings change through
#        the API lands in KUDURRU_SETTINGS_FILE on the data volume;
#      - the query log and the audit log have the queries and the change;
#      - `docker compose stop` drains within stop_grace_period and exits 0;
#      - after `docker compose down` and `up` on the same volume the zone,
#        the setting, the audit event and the query log rows are still there.
#   2. single-edge (deploy/compose/single-edge): dnsdist in front, kudurru
#      in its network namespace behind PROXYv2 from loopback
#      - both containers become healthy;
#      - DNS over UDP, TCP, DoT and DoH through dnsdist;
#      - the query log records the client's address, not dnsdist's loopback.
#
# Needs docker (with compose v2.24 or later), jq, curl, openssl and dig
# (9.18 or later for DoT and DoH) on PATH; runs on GitHub's runners
# (.github/workflows/compose.yml). KEEP=1 keeps the projects and volumes for
# debugging; FORWARD_CHECK=0 skips the forwarded query (no internet access
# to 1.1.1.1/9.9.9.9); EDGE=0 skips part 2. The jq filters are
# single-quoted on purpose: $name is a jq variable.
# shellcheck disable=SC2016
set -euo pipefail

IMAGE=${IMAGE:-ghcr.io/robertherbaugh/kudurru:compose-e2e}
DNSDIST_IMAGE=${DNSDIST_IMAGE:-ghcr.io/robertherbaugh/kudurru-dnsdist:compose-e2e}
FORWARD_CHECK=${FORWARD_CHECK:-1}
EDGE=${EDGE:-1}
BUILD=${BUILD:-1}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
# Loopback ports the containers are published on.
DNS_PORT=${DNS_PORT:-15353}
API_PORT=${API_PORT:-18080}
DOT_PORT=${DOT_PORT:-18853}
DOH_PORT=${DOH_PORT:-18443}
# The zone of objects.d/ and the names asked to put rows in the query log.
ZONE=compose.example
PROBE=probe.compose.example
PROJECTS=()

log() { printf '\n== %s\n' "$*"; }

# compose <project> <args...>: docker compose on a copy of a deploy/compose
# directory with its override file.
compose() {
  local p=$1
  shift
  docker compose --project-directory "$WORK/$p" -p "kudurru-e2e-$p" \
    -f "$WORK/$p/docker-compose.yml" -f "$WORK/$p/override.yml" "$@"
}

cleanup() {
  status=$?
  local p
  for p in "${PROJECTS[@]}"; do
    if [ "$status" -ne 0 ]; then
      log "failure: $p"
      compose "$p" ps -a || true
      compose "$p" logs --no-color --tail=200 || true
    fi
    if [ "${KEEP:-0}" != 1 ]; then
      compose "$p" down -v --remove-orphans >/dev/null 2>&1 || true
    fi
  done
  if [ "${KEEP:-0}" != 1 ]; then rm -rf "$WORK"; else echo "kept $WORK"; fi
  exit "$status"
}
trap cleanup EXIT

# eventually <seconds> <description> <command...>: retries the command every
# 2 s until it succeeds; prints its last output when it never does.
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

# api <METHOD> <path> [json]: one request to the published API (a
# self-signed certificate, hence -k). Prints the body; fails unless the
# status is 2xx.
api() {
  local method=$1 path=$2 body=${3:-} out="$WORK/body.$RANDOM" code
  local args=(-gskS -m 20 -o "$out" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $TOKEN")
  if [ -n "$body" ]; then args+=(-H 'Content-Type: application/json' --data "$body"); fi
  code=$(curl "${args[@]}" "https://127.0.0.1:$API_PORT/api/v1$path") || code=000
  cat "$out" 2>/dev/null || true
  rm -f "$out"
  [[ "$code" == 2* ]]
}

# check <path> <jq args...> <filter>: GET the path and succeed when the jq
# filter is true; prints the response when it is not.
check() {
  local path=$1 json
  shift
  if ! json=$(api GET "$path"); then
    echo "GET $path failed: $json"
    return 1
  fi
  if ! jq -e "$@" <<<"$json" >/dev/null; then
    echo "GET $path:"
    head -c 1500 <<<"$json"
    return 1
  fi
}

# answer <dig flags...> <name> <want>: one query to the published DNS port;
# want is an address, or empty for any address.
answer() {
  local want=${!#} name=${*:$#-1:1} got
  got=$(dig @127.0.0.1 "${@:1:$#-2}" +time=2 +tries=1 +short "$name" A 2>&1) || true
  if [ -n "$want" ]; then
    grep -qx "$want" <<<"$got"
  else
    grep -Eq '^[0-9.]+$' <<<"$got"
  fi || { echo "$name: got '$got', want '${want:-an address}'"; return 1; }
}

# check_dns <dig port flags...>: the objects.d zone and a forwarded name.
check_dns() {
  local flags=("$@")
  eventually 60 "${flags[*]} www.$ZONE" answer "${flags[@]}" "www.$ZONE." 192.0.2.10
  if [ "$FORWARD_CHECK" = 1 ]; then
    eventually 60 "${flags[*]} example.com (forwarded)" answer "${flags[@]}" example.com. ""
  fi
}

# send_queries <tag> <dig port flags...>: names the zone does not hold, so
# they are logged whatever the answer is.
send_queries() {
  local tag=$1 flag
  shift
  for flag in +notcp +tcp; do
    dig @127.0.0.1 "$@" "$flag" +time=2 +tries=1 +short "$tag.$PROBE." A >/dev/null 2>&1 || true
  done
}

queries_have() { # tag...
  check "/queries?domain=*.$PROBE&limit=500" --arg tags "$*" '
    . as $r | all(($tags | split(" "))[]; . as $t | any($r.queries[]; .domain | startswith($t + ".")))'
}

# queries_from_client <tag>: the rows of <tag> name a client that is not
# loopback (the address dnsdist passed in the PROXYv2 header).
queries_from_client() {
  check "/queries?domain=$1.$PROBE&limit=50" '
    (.queries | length) > 0 and all(.queries[]; (.client // "") != "" and .client != "127.0.0.1" and .client != "::1")'
}

audit_has() { # zone object name
  check '/audit?order=desc&limit=200' --arg n "$1" 'any(.items[]; .action == "dnszone.create" and .resource.name == $n)'
}

has_setting() { # value of cache.maxEntries
  check /settings --argjson v "$1" '.. | objects | select(has("maxEntries")) | .maxEntries == $v'
}

create_zone() { # name
  local body
  body=$(jq -nc --arg n "$1" '{apiVersion: "kudurru.io/v1alpha1", kind: "DNSZone", metadata: {name: $n},
    spec: {zoneName: ($n + ".example."), type: "primary", nameservers: ["ns1." + $n + ".example."],
      policy: {queryAccess: "allow", zoneTransfer: "deny", notify: "none", update: "deny"}}}')
  api POST /objects/dnszones "$body" >/dev/null
}

# authoritative <dig port flags...> <zone>: the zone's SOA with the AA flag.
authoritative() {
  local out
  out=$(dig @127.0.0.1 "${@:1:$#-1}" +norec +time=2 +tries=1 SOA "${*: -1}." 2>&1) || true
  if ! grep -q 'status: NOERROR' <<<"$out" || ! grep -Eq 'flags:[^;]* aa' <<<"$out"; then
    echo "$out"
    return 1
  fi
}

# prepare <deploy/compose dir> <project>: copies the deployment, creates the
# secrets its header comment asks for, the bootstrap token and the
# objects.d zone.
prepare() {
  local src=$ROOT/deploy/compose/$1 dir=$WORK/$2
  cp -r "$src" "$dir"
  mkdir -p "$dir/secrets" "$dir/certs"
  openssl rand -base64 32 >"$dir/secrets/encryption_key"
  openssl rand -base64 18 >"$dir/secrets/admin_password"
  printf '%s\n' "$TOKEN" >"$dir/secrets/bootstrap_token"
  # The containers run as uid 65532 (kudurru) and 953 (dnsdist): Compose
  # file secrets and bind mounts keep the host owner and mode.
  chmod 0644 "$dir"/secrets/*
  cat >"$dir/objects.d/compose-example.yaml" <<EOF
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: compose-example }
spec:
  zoneName: $ZONE.
  type: primary
  soa: { mname: ns1.$ZONE., rname: hostmaster.$ZONE. }
  nameservers: [ns1.$ZONE.]
---
apiVersion: kudurru.io/v1alpha1
kind: DNSRecordSet
metadata: { name: www, namespace: default }
spec:
  zoneRef: { name: compose-example }
  endpoints:
    - { dnsName: www.$ZONE, recordType: A, targets: ["192.0.2.10"], recordTTL: 60 }
    - { dnsName: ns1.$ZONE, recordType: A, targets: ["192.0.2.53"], recordTTL: 60 }
EOF
  PROJECTS+=("$2")
}

# bootstrap_token_override <service>: the API token for the test, from a
# Compose secret (an override's secrets are added to the service's).
bootstrap_token_override() {
  cat <<EOF
  $1:
    image: $IMAGE
    environment:
      KUDURRU_BOOTSTRAP_TOKEN_FILE: /run/secrets/bootstrap_token
    secrets: [bootstrap_token]
EOF
}

secrets_override() {
  cat <<EOF
secrets:
  bootstrap_token:
    file: ./secrets/bootstrap_token
EOF
}

# --- the images ---

if [ "$BUILD" = 1 ]; then
  log "build $IMAGE"
  # -tags licensetest: the edge supervisor refuses to start without a key
  # that covers the edge, and CI has no signing key. The image trusts the
  # throwaway key of internal/license/testdata and is never pushed.
  docker build -q -t "$IMAGE" --build-arg VERSION=compose-e2e \
    --build-arg COMMIT="$(git -C "$ROOT" rev-parse --short HEAD)" --build-arg GOTAGS=licensetest "$ROOT"
  if [ "$EDGE" = 1 ]; then
    log "build $DNSDIST_IMAGE"
    docker build -q -t "$DNSDIST_IMAGE" --build-arg KUDURRU_IMAGE="$IMAGE" "$ROOT/images/dnsdist"
  fi
fi
TOKEN=$(docker run --rm "$IMAGE" token generate)
# The audit trail and the edge supervisor need a license key: the test key,
# which the licensetest images trust (the compose files read
# KUDURRU_LICENSE_KEY), as a user's free personal key would be.
KUDURRU_LICENSE_KEY=$(tr -d '[:space:]' <"$ROOT/internal/license/testdata/e2e-license.key")
export KUDURRU_LICENSE_KEY

# --- 1. single ---

log "single: deploy/compose/single"
prepare single single
{
  echo "services:"
  bootstrap_token_override kudurru
  cat <<EOF
    ports: !override
      - "127.0.0.1:$DNS_PORT:53/udp"
      - "127.0.0.1:$DNS_PORT:53/tcp"
      - "127.0.0.1:$API_PORT:8080"
EOF
  secrets_override
} >"$WORK/single/override.yml"
compose single up -d --wait --wait-timeout 180
echo "ok healthy (the compose file's healthcheck)"
cid=$(compose single ps -q kudurru)
docker inspect "$cid" | jq -e '.[0] | .Config.User == "65532:65532" and .HostConfig.ReadonlyRootfs and (.HostConfig.CapDrop | index("ALL")) != null' >/dev/null
echo "ok uid 65532, read-only root file system, every capability dropped"

log "single: DNS over UDP and TCP"
check_dns -p "$DNS_PORT" +notcp
check_dns -p "$DNS_PORT" +tcp

log "single: the API"
eventually 60 "one ready node" check /nodes '(.items | length) == 1 and all(.items[]; .ready == true and ((.version // "") != ""))'
create_zone api-probe
eventually 60 "the zone created through the API answers" authoritative -p "$DNS_PORT" api-probe.example
api PATCH /settings '{"cache":{"maxEntries":123457}}' >/dev/null
eventually 30 "the settings change is served" has_setting 123457
compose single cp kudurru:/var/lib/kudurru/settings.yaml "$WORK/settings.yaml"
grep -q 123457 "$WORK/settings.yaml"
echo "ok the change is in KUDURRU_SETTINGS_FILE on the data volume"
send_queries before -p "$DNS_PORT"
eventually 90 "the query log has the queries" queries_have before
eventually 60 "the audit log has the change" audit_has api-probe

log "single: stop drains within stop_grace_period and exits 0"
start=$SECONDS
compose single stop kudurru
took=$((SECONDS - start))
code=$(docker inspect -f '{{.State.ExitCode}}' "$cid")
if [ "$code" != 0 ] || [ "$took" -ge 30 ]; then
  echo "FAIL stopped with exit code $code after ${took}s (want 0 within the 30 s stop_grace_period)"
  exit 1
fi
echo "ok exit code 0 after ${took}s"

log "single: down and up on the same volume"
compose single down
compose single up -d --wait --wait-timeout 180
check_dns -p "$DNS_PORT" +notcp
eventually 60 "the zone created through the API still answers" authoritative -p "$DNS_PORT" api-probe.example
eventually 30 "the setting is kept" has_setting 123457
eventually 30 "the audit event is kept" audit_has api-probe
send_queries after -p "$DNS_PORT"
eventually 90 "the query log has the queries from before and after" queries_have before after
compose single down -v

# --- 2. single-edge ---

if [ "$EDGE" != 1 ]; then
  log "compose e2e passed (without single-edge)"
  exit 0
fi

log "single-edge: deploy/compose/single-edge"
prepare single-edge edge
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj "/CN=$ZONE" \
  -keyout "$WORK/edge/certs/tls.key" -out "$WORK/edge/certs/tls.crt" 2>/dev/null
chmod 0644 "$WORK/edge/certs/tls.key"
{
  echo "services:"
  cat <<EOF
  dnsdist:
    image: $DNSDIST_IMAGE
    ports: !override
      - "127.0.0.1:$DNS_PORT:53/udp"
      - "127.0.0.1:$DNS_PORT:53/tcp"
      - "127.0.0.1:$DOT_PORT:853/tcp"
      - "127.0.0.1:$DOH_PORT:443/tcp"
      - "127.0.0.1:$API_PORT:8080"
EOF
  bootstrap_token_override kudurru
  secrets_override
} >"$WORK/edge/override.yml"
compose edge up -d --wait --wait-timeout 180
echo "ok dnsdist and kudurru healthy"

log "single-edge: DNS through dnsdist over UDP, TCP, DoT and DoH"
check_dns -p "$DNS_PORT" +notcp
check_dns -p "$DNS_PORT" +tcp
check_dns -p "$DOT_PORT" +tls
check_dns -p "$DOH_PORT" +https

log "single-edge: the query log has the client's address (PROXYv2)"
send_queries edge -p "$DNS_PORT"
eventually 90 "the query log has the queries" queries_have edge
eventually 30 "the rows name the client, not dnsdist's loopback" queries_from_client edge

log "compose e2e passed"

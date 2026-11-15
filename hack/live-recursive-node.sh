#!/usr/bin/env bash
# The on-node half of the live recursion release check (D170,
# dev-docs/release-checklist.md "Recursive groups against the real root
# servers"): start a throwaway node whose default upstream group is
# recursive, on loopback ports with a scratch data directory, and check
# against the real root servers that
#
#   - example.com is answered with the AD flag (validated),
#   - dnssec-failed.org is SERVFAIL with EDE 6 (DNSSEC Bogus),
#   - dnssec-failed.org with +cd returns the data,
#   - kudurru_recursor_outgoing_queries_total{result="timeout"} stays low
#     (printed with the other results; at most 10% of all outgoing queries).
#
#   hack/live-recursive-node.sh [binary]     (default bin/kudurru; needs dig,
#                                             outbound port 53)
set -euo pipefail

cd "$(dirname "$0")/.."
bin=${1:-bin/kudurru}
[ -x "$bin" ] || { echo "no executable $bin (make build)" >&2; exit 2; }
command -v dig >/dev/null || { echo "dig is required" >&2; exit 2; }

dir=$(mktemp -d)
pid=""
cleanup() {
	[ -z "$pid" ] || { kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null || true; }
	rm -rf "$dir"
}
trap cleanup EXIT

# The node's structured log lines and its exit error only: the first-boot
# banner on stderr holds a generated admin password.
logs() {
	echo "## serve.log (JSON lines and the error line, last 40)"
	grep -E '^(\{|error: )' "$dir/serve.log" | tail -40
}

port() { python3 -I -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }
dns=$(port) api=$(port) ops=$(port) metrics=$(port) internal=$(port)
cat >"$dir/kudurru.yaml" <<EOF
node:
  dataDir: $dir/data
store:
  driver: sqlite
listeners:
  dns:
    - address: "127.0.0.1:$dns"
      protocols: [udp, tcp]
  api: { address: "127.0.0.1:$api" }
  ops: { address: "127.0.0.1:$ops" }
  metrics: { address: "127.0.0.1:$metrics" }
  internal: { address: "127.0.0.1:$internal" }
upstreams:
  groups:
    # The documented form (docs/guide/admin/07-zones-and-upstreams.md).
    default: { mode: recursive, timeout: 4s }
log:
  level: info
EOF
"$bin" version
# A throwaway admin password, never printed, so no generated one is either.
KUDURRU_ADMIN_PASSWORD=$(python3 -I -c 'import secrets; print(secrets.token_urlsafe(24))') \
	"$bin" --config "$dir/kudurru.yaml" serve >"$dir/serve.log" 2>&1 &
pid=$!
# The ops listener's readiness route (internal/health); 60 s at most.
ready=0
for _ in $(seq 1 120); do
	if curl -sf "http://127.0.0.1:$ops/healthz/ready" >/dev/null 2>&1; then ready=1; break; fi
	kill -0 "$pid" 2>/dev/null || { logs; echo "the node exited" >&2; exit 1; }
	sleep 0.5
done
[ "$ready" = 1 ] || { logs; echo "the node was not ready after 60 s (/healthz/ready)" >&2; exit 1; }
echo "ready: /healthz/ready answered 200"

fail=0
# A dig that gets no answer fails its checks (and prints the node log)
# instead of ending the script under set -e.
q() { dig +time=5 +tries=2 -p "$dns" @127.0.0.1 "$@" || true; }
check() { if eval "$2"; then echo "ok   $1"; else echo "FAIL $1"; fail=1; fi; }

out=$(q +dnssec example.com A)
echo "$out" | grep -E 'status:|flags:|^example\.com\.' || true
check "example.com has the AD flag" 'echo "$out" | grep -q "flags:.* ad[ ;]"'

out=$(q +dnssec dnssec-failed.org A)
echo "$out" | grep -E 'status:|EDE' || true
check "dnssec-failed.org is SERVFAIL" 'echo "$out" | grep -q "status: SERVFAIL"'
check "dnssec-failed.org carries EDE 6" 'echo "$out" | grep -q "EDE: 6"'

out=$(q +dnssec +cd dnssec-failed.org A)
echo "$out" | grep -E 'status:|^dnssec-failed\.org\.' || true
check "dnssec-failed.org +cd returns the data" 'echo "$out" | grep -q "status: NOERROR" && echo "$out" | grep -qE "^dnssec-failed\.org\.[[:space:]].*IN[[:space:]]+A[[:space:]]"'

metrics_out=$(curl -sf "http://127.0.0.1:$metrics/metrics" | grep '^kudurru_recursor_outgoing_queries_total' || true)
echo "$metrics_out"
total=$(awk '{s += $2} END {printf "%d\n", s}' <<<"$metrics_out")
timeouts=$(awk '/result="timeout"/ {s += $2} END {printf "%d\n", s}' <<<"$metrics_out")
echo "outgoing queries $total, timeouts $timeouts"
check "outgoing queries were counted" '[ "$total" -gt 0 ]'
check "timeouts at most 10% of outgoing queries" '[ $((timeouts * 10)) -le "$total" ]'

[ "$fail" = 0 ] || logs
exit "$fail"

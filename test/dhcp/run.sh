#!/usr/bin/env bash
# Opt-in, Linux-only interoperability test. No host links, routes, bridges,
# sysctls, firewall rules, client state, DNS files, or DHCP sockets are modified.
# Usage: DHCPCD=/absolute/path/to/dhcpcd bash test/dhcp/run.sh
set -euo pipefail

if [[ ${1:-} != --private-lab ]]; then
  [[ $(uname -s) == Linux ]] || { echo 'Linux required' >&2; exit 1; }
  for tool in ip unshare mount jq tcpdump setpriv setcap getcap go grep; do
    command -v "$tool" >/dev/null || { echo "Missing required tool: $tool" >&2; exit 1; }
  done
  client_path=${DHCPCD:-$(command -v dhcpcd || true)}
  [[ -n $client_path && -x $client_path ]] || { echo 'Set DHCPCD to a stock dhcpcd executable (BSD-2-Clause).' >&2; exit 1; }
  client=$(realpath "$client_path")
  repo=$(cd -- "$(dirname -- "$0")/../.." && pwd)
  output=$(mktemp -d "$repo/.context/dhcp-acceptance.XXXXXX")
  # Run a fixed script snapshot so edits in the shared workspace cannot alter
  # the commands of an acceptance run that is already in progress.
  cp -- "$repo/test/dhcp/run.sh" "$output/run.sh"
  "$client" --version > "$output/client-version.txt"
  go build -o "$output/fixture" ./test/dhcp
  host_ns=$(readlink /proc/self/ns/net)
  printf 'Evidence: %s\n' "$output"
  # unshare itself creates the safety boundary. Nothing privileged happens in
  # the original namespace. The private PID namespace kills every descendant
  # on exit, including clients/captures following an interrupted test.
  exec sudo unshare --net --mount --pid --uts --fork --kill-child \
    bash "$output/run.sh" --private-lab "$output" "$client" "$host_ns"
fi

output=$2
client=$3
host_ns=$4
[[ $(readlink /proc/self/ns/net) != "$host_ns" ]] || { echo 'Refusing host network namespace' >&2; exit 1; }
[[ $(ip -j link | jq length) == 1 && $(ip -j link | jq -r '.[0].ifname') == lo ]] || { echo 'Expected empty namespace' >&2; exit 1; }
[[ -z $(ip route show table all) ]] || { echo 'Unexpected routes in private namespace' >&2; exit 1; }
mount --make-rprivate /
mount -t tmpfs tmpfs /run
# ip netns names and bind mounts now live only in the private /run.
mkdir -p /run/netns /run/lab
mount --bind "$output" /run/lab
output=/run/lab
export KUDURRU_DHCP_PRIVATE_LAB=1
export KUDURRU_DHCP_HOST_NETNS="$host_ns"
pids=()
capture_pids=()
cleanup() {
  trap - EXIT INT TERM
  for pid in "${pids[@]}"; do kill -TERM "$pid" 2>/dev/null || true; done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  for ns in srv relay a a2 a3 b c d conflict; do ip netns del "$ns" 2>/dev/null || true; done
  # Evidence stays accessible to the invoking user after root-only execution.
  chown -R "${SUDO_UID:-0}:${SUDO_GID:-0}" "$output"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for ns in srv relay a a2 a3 b c d conflict; do
  ip netns add "$ns"
  ip -n "$ns" link set lo up
done
# Every veth endpoint is born in the disconnected lab and moved only between
# its namespaces. No interface is ever moved out of the host namespace.
pair() {
  ip link add "$2" type veth peer name "$4"
  ip link set dev "$2" netns "$1"
  ip link set dev "$4" netns "$3"
  ip -n "$1" link set dev "$2" up
  ip -n "$3" link set dev "$4" up
}
ip -n srv link add sa type bridge
ip -n srv link set sa up
for n in a a2 a3 conflict; do
  pair srv "p$n" "$n" eth0
  ip -n srv link set "p$n" master sa
done
pair srv sb b eth0
pair srv st relay up
ip -n relay link add lan0 type bridge
ip -n relay link set lan0 up
for n in c d; do
  pair relay "r$n" "$n" eth0
  ip -n relay link set "r$n" master lan0
done
ip -n srv addr add 192.0.2.1/24 dev sa
ip -n srv addr add 198.18.0.1/24 dev sb
ip -n srv addr add 198.51.100.2/24 dev st
ip -n srv addr add 198.51.100.3/24 dev st
# A real ARP responder occupies the first candidate before any DISCOVER.
ip -n conflict addr add 192.0.2.9/24 dev eth0
ip -n relay addr add 198.51.100.1/24 dev up
ip -n relay addr add 203.0.113.1/24 dev lan0
# A routed positive ICMP response must also quarantine before an offer.
ip -n relay addr add 203.0.113.9/24 dev lan0
ip -n srv route add 203.0.113.0/24 via 198.51.100.1 dev st
ip netns exec relay sysctl -q -w net.ipv4.ip_forward=1
# Deterministic Ethernet identities; b has the configured reservation.
for spec in 'a 1:01' 'a2 1:02' 'a3 1:03' 'b 2:01' 'c 3:01' 'd 3:02'; do
  read -r n suffix <<< "$spec"
  ip -n "$n" link set eth0 address "02:00:00:00:0$suffix"
done
mkdir -p "$output/server"
chown 65532:65532 "$output/server"
chmod 755 "$output"
fixture=$output/fixture
# Verify the process credentials used by the systemd ambient-capability drop-in
# and the container image's file-capability executable. These execute in srv,
# never in the host namespace, and the fixture verifies effective kernel state.
KUDURRU_DHCP_CAPABILITY_PROFILE=ambient ip netns exec srv setpriv \
  --reuid=65532 --regid=65532 --clear-groups \
  --bounding-set=-all,+net_bind_service,+net_raw \
  --inh-caps=+net_bind_service,+net_raw --ambient-caps=+net_bind_service,+net_raw \
  --no-new-privs "$fixture" capabilities > "$output/capabilities-ambient.log"
setcap cap_net_bind_service,cap_net_raw=ep "$fixture"
getcap "$fixture" > "$output/capabilities-file.log"
KUDURRU_DHCP_CAPABILITY_PROFILE=file ip netns exec srv setpriv \
  --reuid=65532 --regid=65532 --clear-groups \
  --bounding-set=-all,+net_bind_service,+net_raw \
  --inh-caps=+net_bind_service,+net_raw --ambient-caps=-all \
  --no-new-privs "$fixture" capabilities >> "$output/capabilities-file.log"
setcap -r "$fixture"
printf 'PASS unprivileged ambient and file capability profiles with no-new-privileges\n'
start_server() {
  local profile=${1:-ambient} ambient=+net_bind_service,+net_raw
  if [[ $profile == file ]]; then
    setcap cap_net_bind_service,cap_net_raw=ep "$fixture"
    ambient=-all
  fi
  rm -f "$output/server/ready"
  KUDURRU_DHCP_CAPABILITY_PROFILE="$profile" ip netns exec srv setpriv --reuid=65532 --regid=65532 --clear-groups \
    --bounding-set=-all,+net_bind_service,+net_raw \
    --inh-caps=+net_bind_service,+net_raw --ambient-caps="$ambient" \
    --no-new-privs "$fixture" server "$output/server" >>"$output/server.log" 2>&1 &
  server_pid=$!
  pids+=("$server_pid")
  for ((i=0;i<100;i++)); do [[ ! -f $output/server/ready ]] || return 0; kill -0 "$server_pid"; sleep .1; done
  echo 'Server readiness timed out' >&2; exit 1
}
for ns in srv relay; do
  ip netns exec "$ns" tcpdump -U -i any -n -w "$output/$ns.pcap" 'arp or icmp or udp port 67 or udp port 68 or udp port 1067' >"$output/$ns-capture.log" 2>&1 &
  pids+=("$!")
  capture_pids+=("$!")
done
ip netns exec relay "$fixture" relay >"$output/relay.log" 2>&1 &
pids+=("$!")
start_server

cat > "$output/client.conf" <<'CONF'
noipv4ll
noipv6
nohook resolv.conf,hostname,ntp.conf
option subnet_mask,routers,domain_name_servers,domain_name
option ntp_servers,domain_search,classless_static_routes,tftp_server_name,bootfile_name
hostname labclient.default.example
fqdn both
CONF
cat > "$output/hook" <<'HOOK'
#!/bin/sh
printf '%s %s %s\n' "$reason" "${new_ip_address:-}" "${new_dhcp_server_identifier:-}" >> "$LAB_EVENTS"
printf 'ntp=%s search=%s routes=%s bootserver=%s bootfile=%s\n' "${new_ntp_servers:-}" "${new_domain_search:-}" "${new_classless_static_routes:-}" "${new_tftp_server_name:-}" "${new_bootfile_name:-}" >> "$LAB_EVENTS.options"
HOOK
chmod +x "$output/hook"
# Private /run and /var isolate pidfiles, control sockets, DUID and lease files.
# The explicit hook prevents host DNS/hostname changes; routes are netns-local.
start_client() {
  local ns=$1
  ip netns exec "$ns" unshare --mount bash -c '
    set -eu
    mount --make-rprivate /
    mount -t tmpfs tmpfs /tmp
    mkdir /tmp/lab
    mount --bind "$1" /tmp/lab
    mount -t tmpfs tmpfs /run
    mount -t tmpfs tmpfs /var
    mkdir -p /run/dhcpcd /var/lib/dhcpcd
    export LAB_EVENTS="/tmp/lab/$2.events"
    exec "$3" -4 -B -d -e "LAB_EVENTS=/tmp/lab/$2.events" -f /tmp/lab/client.conf -c /tmp/lab/hook -t 0 eth0
  ' bash "$output" "$ns" "$client" >"$output/$ns.log" 2>&1 &
  last_client_pid=$!
  pids+=("$last_client_pid")
  printf '%s\n' "$last_client_pid" > "$output/$ns.pid"
}
address() { ip -n "$1" -j -4 addr show dev eth0 | jq -r '.[0].addr_info[]? | select(.scope=="global") | .local'; }
wait_address() {
  local ns=$1 expected=$2
  for ((i=0;i<200;i++)); do
    [[ $(address "$ns") != "$expected" ]] || return 0
    sleep .1
  done
  echo "Client $ns failed to acquire $expected" >&2; cat "$output/$ns.log" >&2; exit 1
}
lease_count() { jq -r --arg scope "$1" '[.[$scope][]? | select(.State=="bound")] | length' "$output/server/leases.json"; }
wait_lease_count() {
  local scope=$1 expected=$2
  for ((i=0;i<100;i++)); do [[ $(lease_count "$scope") != "$expected" ]] || return 0; sleep .1; done
  echo "Wrong lease count for $scope" >&2; cat "$output/server/leases.json" >&2; exit 1
}
start_client a; wait_address a 192.0.2.10
start_client a2; wait_address a2 192.0.2.11
start_client b; wait_address b 198.18.0.50
start_client c; wait_address c 203.0.113.10
start_client d; wait_address d 203.0.113.11
wait_lease_count default 2
wait_lease_count remote 2
jq -e '.default[] | select(.Address=="192.0.2.9" and .State=="declined")' "$output/server/leases.json" >/dev/null
jq -e '.remote[] | select(.Address=="203.0.113.9" and .State=="declined")' "$output/server/leases.json" >/dev/null
jq -e '.default[] | select(.State=="bound" and .Hostname=="labclient.default.example")' "$output/server/leases.json" >/dev/null
for ((i=0;i<100;i++)); do
  [[ $(ip -n a route show 10.123.0.0/16) == *'via 192.0.2.1'* ]] && break
  sleep .1
done
[[ $(ip -n a route show 10.123.0.0/16) == *'via 192.0.2.1'* ]]
grep -q 'ntp=192.0.2.1 search=default.example lab.example' "$output/a.events.options"
grep -q 'bootserver=boot.default.example bootfile=netboot.efi' "$output/a.events.options"
printf 'PASS actual ARP and routed ICMP conflicts quarantined; RFC3527/5107/8357 relay acquisition\n'
printf 'PASS stock-client NTP, search, classless route, boot options and client FQDN\n'
printf 'PASS acquisition, distinct clients, reservation, two direct subnets, relayed scope\n'
# The third client cannot steal either live address.
start_client a3
sleep 6
[[ -z $(address a3) ]] || { echo 'Exhausted pool allocated a live address' >&2; exit 1; }
printf 'PASS pool exhaustion\n'
# SIGALRM is dhcpcd --release; SIGUSR1 is dhcpcd --renew.
kill -ALRM "$(cat "$output/a.pid")"
# dhcpcd can keep its userspace ARP defender alive briefly after deleting the
# kernel address. Wait for full exit before probing this address for reuse.
wait "$(cat "$output/a.pid")"
# Let the releasing client finish removing its address before starting DAD on
# the next client; the RELEASE can precede local deconfiguration briefly.
for ((i=0;i<100;i++)); do [[ -n $(address a) ]] || break; sleep .1; done
[[ -z $(address a) ]] || { echo "Released client kept its address" >&2; exit 1; }
wait_lease_count default 1
kill -HUP "$(cat "$output/a3.pid")"
wait_address a3 192.0.2.10
printf 'PASS release and safe address reuse\n'
# Capture expiration lower bound and ensure routed T1 renew persists after restart.
before=$(jq -r '.remote[] | select(.Address=="203.0.113.10") | .ExpiresAt' "$output/server/leases.json")
kill -TERM "$server_pid"
wait "$server_pid"
start_server file
jq -e '.default[] | select(.Address=="192.0.2.9" and .State=="declined")' "$output/server/leases.json" >/dev/null
jq -e '.remote[] | select(.Address=="203.0.113.9" and .State=="declined")' "$output/server/leases.json" >/dev/null
sleep 1
kill -USR1 "$(cat "$output/c.pid")"
for ((i=0;i<100;i++)); do
  after=$(jq -r '.remote[] | select(.Address=="203.0.113.10") | .ExpiresAt' "$output/server/leases.json")
  [[ $after == "$before" ]] || break
  sleep .1
done
[[ $after > "$before" ]] || { echo 'Routed renewal did not extend persisted binding' >&2; exit 1; }
printf 'PASS restart persistence and routed renewal\n'
# SIGKILL models disappearance without RELEASE. The lease must expire before
# reuse; a fresh process must recover that address using the real client.
kill -KILL "$(cat "$output/a2.pid")"
wait "$(cat "$output/a2.pid")" 2>/dev/null || true
sleep 31
sleep 31
wait_lease_count default 1
ip -n a2 addr flush dev eth0
start_client a2
wait_address a2 192.0.2.11
printf 'PASS expiration and reacquisition\n'
# Packet evidence proves ordinary T1 giaddr=0 traffic took the routed path and
# that a trusted relay received responses at its giaddr server port.
kill -TERM "$server_pid"
wait "$server_pid"
for pid in "${capture_pids[@]}"; do kill -TERM "$pid"; wait "$pid"; done
for ns in srv relay; do tcpdump -nn -vv -r "$output/$ns.pcap" > "$output/$ns-packets.txt" 2>/dev/null; done
grep -Eq '203\.0\.113\.10\.68 > 198\.51\.100\.[23]\.67' "$output/srv-packets.txt"
grep -Eq '198\.51\.100\.2\.67 > 198\.51\.100\.1\.1067' "$output/srv-packets.txt"
grep -Eq 'Server-ID.*198.51.100.3' "$output/relay-packets.txt"
grep -Eq 'ARP, .*Request who-has 192.0.2.9' "$output/srv-packets.txt"
grep -Eq '203.0.113.9 > 198.51.100.2: ICMP echo reply' "$output/srv-packets.txt"
printf 'PASS wire routing evidence; all test resources remain inside the disconnected lab\n'
printf 'PASS\n' > "$output/result"

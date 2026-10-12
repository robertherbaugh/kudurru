#!/usr/bin/env bash
# hostnetwork-check: the -hostnetwork image (images/hostnetwork/Dockerfile)
# binds :53 as uid 65532 where the ip_unprivileged_port_start sysctl does
# not help, with exactly the capabilities the chart's hostNetwork mode
# grants (drop ALL, add NET_BIND_SERVICE, no_new_privs), and does not
# without them. `make hostnetwork-check`; CI runs it in the edge workflow.
#
#   hack/hostnetwork-check.sh <hostnetwork image> <plain image>
#
# HOSTNETWORK_CHECK_NETWORK=netns (default) runs each container in its own
# network namespace with net.ipv4.ip_unprivileged_port_start=1024, which is
# what a hostNetwork pod sees on a default host; =host uses --network host
# and the host's :53, which must be free.
set -euo pipefail

image=${1:?usage: hack/hostnetwork-check.sh <hostnetwork image> <plain image>}
plain=${2:?usage: hack/hostnetwork-check.sh <hostnetwork image> <plain image>}
mode=${HOSTNETWORK_CHECK_NETWORK:-netns}
probe=${HOSTNETWORK_CHECK_PROBE_IMAGE:-alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0}
docker=${DOCKER:-docker}

dir=$(mktemp -d)
names=()
cleanup() {
	for n in "${names[@]}"; do $docker rm -f "$n" >/dev/null 2>&1 || true; done
	rm -rf "$dir"
}
trap cleanup EXIT

# Loopback ports for everything but DNS, above the usual ones so a host-mode
# run does not collide with a local kudurru.
base=$((20000 + RANDOM % 20000))
cat >"$dir/kudurru.yaml" <<EOF
node: {name: hostnetwork-check, dataDir: /var/lib/kudurru}
listeners:
  dns: [{address: "0.0.0.0:53", protocols: [udp, tcp]}]
  api: {address: "127.0.0.1:$base"}
  ops: {address: "127.0.0.1:$((base + 1))"}
  metrics: {address: "127.0.0.1:$((base + 2))"}
  internal: {address: "127.0.0.1:$((base + 3))"}
auth: {adminSocket: {path: /run/kudurru/admin.sock}}
upstreams: {groups: {default: {servers: ["127.0.0.1:9"], strategy: strict, timeout: 1s}}}
log: {level: info, format: text}
EOF
head -c 32 /dev/urandom | base64 >"$dir/encryption_key"
echo hostnetwork-check-password >"$dir/admin_password"
chmod 0755 "$dir"
chmod 0644 "$dir"/*

net=(--sysctl net.ipv4.ip_unprivileged_port_start=1024)
if [ "$mode" = host ]; then
	net=(--network host)
fi

# run <name> <image> <docker run flags...>: starts kudurru serve detached.
run() {
	local name=$1 img=$2
	shift 2
	names+=("$name")
	$docker rm -f "$name" >/dev/null 2>&1 || true
	$docker run -d --name "$name" "${net[@]}" --user 65532:65532 --read-only \
		--security-opt no-new-privileges --cap-drop ALL "$@" \
		--tmpfs /var/lib/kudurru:uid=65532,gid=65532 --tmpfs /run/kudurru:uid=65532,gid=65532 --tmpfs /tmp \
		-v "$dir:/etc/kudurru:ro" \
		-e KUDURRU_ENCRYPTION_KEY_FILE=/etc/kudurru/encryption_key -e KUDURRU_ADMIN_PASSWORD_FILE=/etc/kudurru/admin_password \
		"$img" serve --config /etc/kudurru/kudurru.yaml >/dev/null 2>"$dir/$name.err" || true
}

# answers <name>: the container's :53 answers health.kudurru.internal. TXT.
answers() {
	local target=(--network "container:$1")
	if [ "$mode" = host ]; then
		target=(--network host)
	fi
	$docker run --rm "${target[@]}" "$probe" nslookup -type=TXT health.kudurru.internal. 127.0.0.1 >/dev/null 2>&1
}

# wait_state <name>: "answering" once :53 answers, "exited" when the
# container stops first, "timeout" after 60 s.
wait_state() {
	for _ in $(seq 60); do
		if [ "$($docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" != true ]; then
			echo exited
			return
		fi
		if answers "$1"; then
			echo answering
			return
		fi
		sleep 1
	done
	echo timeout
}

why() {
	{ cat "$dir/$1.err"; $docker logs "$1" 2>&1; } | grep -iE 'permission denied|operation not permitted|bind' | head -3 | sed 's/^/    /'
}

fail=0
check() {
	local want=$1 name=$2 img=$3 desc=$4
	shift 4
	run "$name" "$img" "$@"
	local got
	got=$(wait_state "$name")
	if [ "$got" = "$want" ]; then
		echo "ok   $desc: $got"
	else
		echo "FAIL $desc: $got, want $want"
		fail=1
	fi
	if [ "$got" != answering ]; then
		why "$name"
	fi
	$docker rm -f "$name" >/dev/null 2>&1 || true
}

echo "hostnetwork-check ($mode): uid 65532, --cap-drop ALL, no-new-privileges, :53"
check answering kudurru-hncheck-ok "$image" "-hostnetwork image, NET_BIND_SERVICE added" --cap-add NET_BIND_SERVICE
check exited kudurru-hncheck-nocap "$image" "-hostnetwork image, no capability added (exec refused)"
check exited kudurru-hncheck-plain "$plain" "plain image, NET_BIND_SERVICE added (no file capability)" --cap-add NET_BIND_SERVICE
exit $fail

#!/usr/bin/env bash
# edge-root-check: `kudurru edge run` in the kudurru-dnsdist image started
# the way the chart starts it for eBPF DynBlocks and for host networking
# (charts/kudurru, kudurru.edge.rootStart): root with group 953, every
# capability dropped except NET_BIND_SERVICE, SETUID, SETGID and KILL (plus
# BPF and SYS_ADMIN for eBPF), no_new_privs, a read-only root, and
# net.ipv4.ip_unprivileged_port_start=1024 (what a hostNetwork pod sees).
# dnsdist must create the BPF maps, bind :53, switch to 953 keeping only
# capabilities_to_retain, and stop on SIGTERM. The old non-root eBPF shape
# (uid 953 with the same capabilities added) must fail to create the maps.
# Part of `make edge-check`.
#
#   hack/edge-root-check.sh <kudurru-dnsdist image>
set -euo pipefail

image=${1:?usage: hack/edge-root-check.sh <kudurru-dnsdist image>}
# The supervisor needs a license key that covers the edge. The image of
# `make edge-check` is built with -tags licensetest, which trusts the throwaway
# key that signed this file (all features; the private half was discarded).
root=$(cd "$(dirname "$0")/.." && pwd)
license_key=$(cat "$root/internal/license/testdata/e2e-license.key")
docker=${DOCKER:-docker}
name=kudurru-edge-root-check-$$
base_caps=(--cap-drop ALL --cap-add NET_BIND_SERVICE --cap-add SETUID --cap-add SETGID --cap-add KILL)
ebpf_caps=(--cap-add BPF --cap-add SYS_ADMIN)
common=(--security-opt no-new-privileges --read-only --sysctl net.ipv4.ip_unprivileged_port_start=1024
	--tmpfs /etc/dnsdist:mode=1777 --tmpfs /run/kudurru:mode=1777 --tmpfs /tmp
	-e "KUDURRU_LICENSE_KEY=$license_key")
args=(/usr/local/bin/kudurru edge run --backends=node-a=127.0.0.1:5353
	--api-key-out=/run/kudurru/dnsdist-api.key --poll-zones=false --log-format=text)
trap '$docker rm -f "$name" >/dev/null 2>&1 || true' EXIT

fail() {
	echo "FAIL $*"
	$docker logs "$name" 2>&1 | tail -20 | sed 's/^/    /'
	exit 1
}

# root_start <label> <want CapEff of dnsdist> <docker flags...> -- <supervisor flags...>
root_start() {
	local label=$1 want=$2
	shift 2
	local flags=()
	while [ "$1" != -- ]; do
		flags+=("$1")
		shift
	done
	shift
	echo "edge-root-check ($label): root start, --dnsdist-user=953:953, :53 without the sysctl"
	$docker run -d --name "$name" --user 0:953 "${flags[@]}" "${common[@]}" "$image" "${args[@]}" "$@" --dnsdist-user=953:953 >/dev/null

	# The dnsdist child, once it has switched (the image has no pgrep).
	local status="" pid
	for _ in $(seq 30); do
		# shellcheck disable=SC2016 # expanded by the shell in the container
		pid=$($docker exec "$name" sh -c 'for p in /proc/[0-9]*; do [ "$(cat "$p/comm" 2>/dev/null)" = dnsdist ] && echo "${p#/proc/}"; done | tail -1' 2>/dev/null || true)
		if [ -n "$pid" ] && status=$($docker exec "$name" cat "/proc/$pid/status" 2>/dev/null) &&
			echo "$status" | grep -q '^Uid:[[:space:]]*953[[:space:]]'; then
			break
		fi
		status=""
		sleep 1
	done
	[ -n "$status" ] || fail "dnsdist did not run as uid 953"
	local uid gid eff
	uid=$(echo "$status" | awk '/^Uid:/ {print $2"/"$3"/"$4"/"$5}')
	gid=$(echo "$status" | awk '/^Gid:/ {print $2"/"$3"/"$4"/"$5}')
	eff=$(echo "$status" | awk '/^CapEff:/ {print $2}')
	[ "$uid" = 953/953/953/953 ] && [ "$gid" = 953/953/953/953 ] || fail "dnsdist uid $uid gid $gid, want 953 everywhere"
	[ "$eff" = "$want" ] || fail "dnsdist CapEff $eff, want $want"
	echo "ok   dnsdist runs as 953:953 with CapEff $eff"
	$docker exec "$name" grep -qiE '^ *[0-9]+: 00000000:0035 ' /proc/net/udp || fail "nothing listens on udp :53"
	$docker exec "$name" grep -qiE '^ *[0-9]+: 00000000:0035 00000000:0000 0A ' /proc/net/tcp || fail "nothing listens on tcp :53"
	echo "ok   :53 bound (udp and tcp) with ip_unprivileged_port_start=1024"
	if $docker logs "$name" 2>&1 | grep -qi 'error creating a BPF map'; then
		fail "BPF map creation failed"
	fi
	$docker stop -t 15 "$name" >/dev/null
	local code
	code=$($docker inspect -f '{{.State.ExitCode}}' "$name")
	[ "$code" = 0 ] || fail "the supervisor exited $code after SIGTERM, want 0 (it must be able to signal dnsdist)"
	echo "ok   SIGTERM stops the supervisor and dnsdist (exit 0)"
	$docker rm -f "$name" >/dev/null
}

# eBPF: CapEff 0x200000 is CAP_SYS_ADMIN alone (capabilities_to_retain).
root_start ebpf 0000000000200000 "${base_caps[@]}" "${ebpf_caps[@]}" -- --dynblocks --ebpf
# Host networking without eBPF: nothing retained.
root_start hostnetwork 0000000000000000 "${base_caps[@]}" --

# The pre-fix eBPF shape: uid 953 with the capabilities added, which
# Kubernetes and Docker leave out of the permitted set of a non-root process.
out=$($docker run --rm --user 953:953 "${base_caps[@]}" "${ebpf_caps[@]}" "${common[@]}" "$image" timeout 10 "${args[@]}" --dynblocks --ebpf 2>&1 || true)
if echo "$out" | grep -qi 'error creating a BPF map'; then
	echo "ok   uid 953 with BPF/SYS_ADMIN added cannot create the BPF maps (the old chart shape)"
else
	echo "FAIL uid 953 with added capabilities did not fail as expected:"
	echo "$out" | tail -10 | sed 's/^/    /'
	exit 1
fi

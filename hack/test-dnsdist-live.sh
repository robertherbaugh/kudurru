#!/usr/bin/env bash
# Run the opt-in live dnsdist suites against the actual dnsdist program
# (D383). The program is the upstream image of the FROM line in
# images/dnsdist/Dockerfile, by registry digest (kudurrutest.DnsdistImage;
# TestSupervisorVersions keeps the two equal). The suites talk to the private
# daemon at unix:///tmp/kudurru-lab-docker.sock only: no default Docker socket
# and no mutable tag is used. Every container they start is named by the test
# and removed by it.
#
# The daemon runs without a default bridge (--bridge=none --iptables=false);
# the suites use --network host and bind loopback ports only.
#
#   hack/test-dnsdist-live.sh [suite...]
#
# Suites: f43-yaml f43-lua f28 f39 anchor f18c (default: all, in this order).
# Set KUDURRU_DNSDIST_LIVE_WORKDIR to keep the scratch directory; it is
# removed otherwise. Extra go test flags go in KUDURRU_DNSDIST_LIVE_GOFLAGS
# (default: -race -v). The privilege-drop variant of F43
# (KUDURRU_F43_CHILD_USER) needs a root test process and is not run here.
set -euo pipefail
cd "$(dirname "$0")/.."

socket=unix:///tmp/kudurru-lab-docker.sock
docker=(docker --host "$socket")
image=$(sed -n 's/^FROM \(powerdns\/dnsdist-[0-9]*:[^@ ]*@sha256:[0-9a-f]\{64\}\)$/\1/p' images/dnsdist/Dockerfile)
if [ -z "$image" ]; then
  echo "images/dnsdist/Dockerfile has no digest-pinned upstream FROM line" >&2
  exit 1
fi
if ! grep -qF "\"$image\"" internal/testutil/kudurrutest/dnsdist.go; then
  echo "kudurrutest.DnsdistImage is not $image (run go test ./deploy -run TestSupervisorVersions)" >&2
  exit 1
fi
if ! "${docker[@]}" version --format '{{.Server.Version}}' >/dev/null 2>&1; then
  echo "no private Docker daemon at $socket" >&2
  exit 1
fi
# Pull by digest only when it is missing: the registry cannot substitute
# other bytes for a digest reference.
if ! "${docker[@]}" image inspect --format '{{.Id}}' "$image" >/dev/null 2>&1; then
  "${docker[@]}" pull "$image"
fi
echo "dnsdist image: $image ($("${docker[@]}" image inspect --format '{{.Id}}' "$image"))"
# Read the whole version output before printing its first line: with
# pipefail, `| head -n 1` can close the pipe while docker still writes and
# fail the script with "write /dev/stdout: broken pipe".
version=$("${docker[@]}" run --rm --network none --entrypoint dnsdist "$image" --version)
echo "${version%%$'\n'*}"

work=${KUDURRU_DNSDIST_LIVE_WORKDIR:-}
if [ -z "$work" ]; then
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
fi
mkdir -p "$work"
read -r -a flags <<<"${KUDURRU_DNSDIST_LIVE_GOFLAGS:--race -v}"
export GOMAXPROCS="${GOMAXPROCS:-2}"
suites=("$@")
[ ${#suites[@]} -gt 0 ] || suites=(f43-yaml f43-lua f28 f39 anchor f18c)
status=0
for suite in "${suites[@]}"; do
  echo "=== suite $suite"
  case $suite in
    f43-yaml) env -u GORACE KUDURRU_F43_LIVE=1 KUDURRU_F43_WORKDIR="$work" \
      go test -p 2 -count=1 -timeout=5m "${flags[@]}" -run '^TestConsoleLiveSupervisor$' ./ee/edge/dnsdist || status=1 ;;
    f43-lua) env -u GORACE KUDURRU_F43_LIVE=1 KUDURRU_F43_RENDERER=lua KUDURRU_F43_WORKDIR="$work" \
      go test -p 2 -count=1 -timeout=5m "${flags[@]}" -run '^TestConsoleLiveSupervisor$' ./ee/edge/dnsdist || status=1 ;;
    f28) KUDURRU_F28_EDGE_LIVE=1 KUDURRU_F28_EDGE_WORKDIR="$work" \
      go test -p 2 -count=1 -timeout=5m "${flags[@]}" -run '^TestManagedCacheLivePinnedTransition$' ./ee/edge/dnsdist || status=1 ;;
    f39) KUDURRU_F39_LIVE=1 KUDURRU_F39_WORKDIR="$work" \
      go test -p 2 -count=1 -timeout=8m "${flags[@]}" -run '^TestRunServeRemoteEdge(RegexAndLateFill|AuthoritativeCacheACL|ClientGroupViews)$' ./cmd/kudurru || status=1 ;;
    anchor) KUDURRU_ANCHOR_EDGE_LIVE=1 KUDURRU_ANCHOR_EDGE_WORKDIR="$work" \
      go test -p 2 -count=1 -timeout=6m "${flags[@]}" -run '^TestRunServeManagedEdgePinnedDaemonRestart$' ./cmd/kudurru || status=1 ;;
    f18c) KUDURRU_F18C_DNSDIST=1 \
      go test -p 2 -count=1 -timeout=5m "${flags[@]}" -run '^TestRunServeUniFiHostnamePolicyLifecycle$' ./cmd/kudurru || status=1 ;;
    *) echo "unknown suite $suite" >&2; exit 2 ;;
  esac
done
exit $status

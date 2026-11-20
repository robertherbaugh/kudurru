#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source hack/pinned-interop.sh

suite=${1:-}
if [[ $# != 2 || ( $suite != knot && $suite != dnsdist ) ]]; then
  echo 'Usage: hack/lab-dockerd.sh {knot|dnsdist} results-directory (disposable Linux host with sudo)' >&2
  exit 2
fi
mkdir -p "$2"
results=$(realpath "$2")
if [[ -n $(find "$results" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
  echo "Results directory must be empty: $results" >&2
  exit 2
fi
host=unix:///tmp/kudurru-lab-docker.sock
socket=${host#unix://}
docker=(docker --host "$host")
lab=$(mktemp -d)
lock=/tmp/kudurru-lab-docker.lock
cleanup() {
  local status=0
  if [[ -n ${daemon_job:-} ]]; then
    if sudo -n test -s "$lab/docker.pid"; then
      sudo -n kill -TERM "$(sudo -n cat "$lab/docker.pid")" 2>/dev/null || true
    else
      sudo -n kill -TERM "$daemon_job" 2>/dev/null || true
    fi
    wait "$daemon_job" || true
    if sudo -n mountpoint -q "$lab/exec/netns/default"; then
      sudo -n umount "$lab/exec/netns/default" || status=1
    fi
  fi
  sudo -n rm -rf --one-file-system -- "$lab" || status=1
  if [[ ${locked:-} == 1 ]]; then
    sudo -n rmdir "$lock" || status=1
  fi
  return "$status"
}
finish() {
  local status=$?
  cleanup || status=1
  exit "$status"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if ! sudo -n mkdir "$lock"; then
  echo "Private daemon lock $lock is unavailable; check for another lab run" >&2
  exit 1
fi
locked=1
if [[ -e $socket || -L $socket ]]; then
  echo "Refusing to replace existing $socket" >&2
  exit 1
fi

export DOCKER_CONFIG="$lab/client"
mkdir "$DOCKER_CONFIG"
unset DOCKER_CONTEXT DOCKER_HOST DOCKER_TLS_VERIFY KUDURRU_KNOT_LOG_DIR
printf '{}\n' > "$lab/daemon.json"
sudo -n dockerd --config-file "$lab/daemon.json" --host "$host" \
  --data-root "$lab/data" --exec-root "$lab/exec" --pidfile "$lab/docker.pid" \
  --group "$(id -gn)" --bridge=none --iptables=false --ip6tables=false \
  > "$results/dockerd.log" 2>&1 &
daemon_job=$!
for ((attempt=0; attempt<60; attempt++)); do
  if "${docker[@]}" info >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$daemon_job" 2>/dev/null; then
    break
  fi
  sleep 1
done
if ! "${docker[@]}" info >/dev/null; then
  tail -n 40 "$results/dockerd.log" >&2
  exit 1
fi

export KUDURRU_INTEROP_RESULTS_DIR="$results"
record_image() {
  local image=$1 entrypoint=$2
  shift 2
  "${docker[@]}" pull "$image" >&2 || return 1
  printf 'image=%s\n' "$image"
  "${docker[@]}" image inspect --format 'id={{.Id}} platform={{.Os}}/{{.Architecture}} repoDigests={{json .RepoDigests}}' "$image" || return 1
  "${docker[@]}" run --pull=never --rm --network none --entrypoint "$entrypoint" "$image" "$@" 2>&1
}

{
  printf 'source=%s\nrun_id=%s\nrun_attempt=%s\n' "$(git rev-parse HEAD)" "${GITHUB_RUN_ID:-local}" "${GITHUB_RUN_ATTEMPT:-local}"
  go version
  "${docker[@]}" version
} | tee "$results/environment.txt"

status=0
case $suite in
  knot)
    knot=$(interop_image_pin knotImage test/interop/knot_catalog_test.go)
    base=$(interop_image_pin knotBase test/interop/knot_catalog_test.go)
    rollover=$(interop_image_pin knotImage internal/trustanchors/rfc5011_capture_knot_test.go)
    if [[ -z $knot || -z $base || $knot != "$rollover" ]]; then
      echo 'Knot live fixtures must agree on the registry digest and specify a digest-pinned base' >&2
      exit 1
    fi
    record_image "$knot" knotd -V | tee "$results/knot-version.txt"
    record_image "$base" cat /etc/alpine-release | tee "$results/alpine-version.txt"
    export KUDURRU_KNOT_DOCKER_HOST="$host"
    hack/test-knot-catalog.sh || status=1
    hack/test-knot-managed-root.sh || status=1
    ;;
  dnsdist)
    image=$(interop_dnsdist_image)
    record_image "$image" dnsdist --version | tee "$results/dnsdist-version.txt"
    unset KUDURRU_DNSDIST_LIVE_WORKDIR KUDURRU_DNSDIST_LIVE_GOFLAGS KUDURRU_F43_CHILD_USER KUDURRU_F43_RENDERER
    hack/test-dnsdist-live.sh || status=1
    ;;
esac
exit "$status"

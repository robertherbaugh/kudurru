#!/usr/bin/env bash
# volume-check: each image comes up on fresh Docker named volumes at
# /var/lib/kudurru{,-querylog,-local} and /run/kudurru that uid 65532 can write
# (the volumes
# take the owner and mode of the image's directories; images/base/Dockerfile).
# `make volume-check`; CI runs it in the edge workflow, publish.yml on the
# released images.
#
#   hack/volume-check.sh <image>...
set -euo pipefail

[ $# -gt 0 ] || { echo "usage: hack/volume-check.sh <image>..." >&2; exit 2; }
probe=${VOLUME_CHECK_PROBE_IMAGE:-alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6}
docker=${DOCKER:-docker}

vols=()
# shellcheck disable=SC2329 # the EXIT trap runs it
cleanup() {
	for v in "${vols[@]}"; do $docker volume rm -f "$v" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT

fail=0
for img in "$@"; do
	data="kudurru-volcheck-data-$RANDOM$RANDOM" run="kudurru-volcheck-run-$RANDOM$RANDOM"
	query="kudurru-volcheck-query-$RANDOM$RANDOM" local_state="kudurru-volcheck-local-$RANDOM$RANDOM"
	vols+=("$data" "$run" "$query" "$local_state")
	# Creating a container with the empty volumes seeds them from the
	# image's directories (nothing runs, so a base image without kudurru
	# works too; the command is never run).
	$docker rm "$($docker create --network none -v "$data:/var/lib/kudurru" -v "$query:/var/lib/kudurru-querylog" -v "$local_state:/var/lib/kudurru-local" -v "$run:/run/kudurru" "$img" volume-check)" >/dev/null
	got=$($docker run --rm --network none --user 65532:65532 -v "$data:/d" -v "$query:/q" -v "$local_state:/l" -v "$run:/r" "$probe" \
		sh -c 'stat -c "%u:%g %a" /d /q /l /r | tr "\n" " "; touch /d/w /q/w /l/w /r/w 2>/dev/null && echo writable || echo read-only')
	want="65532:65532 755 65532:65532 755 65532:65532 755 65532:65532 1777 writable"
	if [ "$got" = "$want" ]; then
		echo "ok   $img: $got"
	else
		echo "FAIL $img: $got, want $want"
		fail=1
	fi
	$docker volume rm -f "$data" "$run" "$query" "$local_state" >/dev/null
done
exit $fail

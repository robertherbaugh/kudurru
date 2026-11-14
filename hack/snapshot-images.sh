#!/usr/bin/env bash
# The image half of the full goreleaser snapshot (hack/release-local-checks.sh
# --full-snapshot). `goreleaser release --snapshot` does not push the ko
# images: it loads the linux/amd64 image of each into the Docker daemon under
# ko's local domain, goreleaser.ko.local (goreleaser v2.18.3
# internal/pipe/ko getLocalDomain; with `bare: true` the repository is the
# domain itself), tagged with the snapshot version and with the image digest:
#
#   goreleaser.ko.local:<version>          (the kudurru ko entry)
#   goreleaser.ko.local:<version>-debug    (kudurru-debug)
#   goreleaser.ko.local:<digest hex>       (one per image)
#
#   hack/snapshot-images.sh list                    the refs under the domain now
#   hack/snapshot-images.sh check BEFORE VERSION    after the snapshot
#
# check fails unless both version-tagged images exist and `kudurru version` in
# each prints VERSION; it runs hack/volume-check.sh on them (reported, not
# fatal: a local snapshot uses the plain distroless bases, which lack the
# image directories) and removes every ref under the domain that is not in
# the BEFORE list (the output of `list` taken before goreleaser ran), plus the
# two version tags, so other users of a shared daemon keep their images.
#
# DOCKER (default docker), VOLUME_CHECK (default hack/volume-check.sh) and
# KO_LOCAL_DOMAIN (default goreleaser.ko.local) are for tests.
set -uo pipefail

docker=${DOCKER:-docker}
volume_check=${VOLUME_CHECK:-$(dirname "$0")/volume-check.sh}
domain=${KO_LOCAL_DOMAIN:-goreleaser.ko.local}

# The refs under the domain, one per line (none is empty output, not a
# failure); fails only when the daemon cannot be asked.
list() {
	local out
	out=$($docker images --format '{{.Repository}}:{{.Tag}}' "$domain") || return
	grep -v -e ':<none>$' -e '^$' <<<"$out" | sort -u
	return 0
}

case "${1:-}" in
list)
	list
	exit
	;;
check)
	[ $# -eq 3 ] && [ -n "$3" ] || { echo "usage: $0 check BEFORE VERSION" >&2; exit 2; }
	before=$2 version=$3
	;;
*)
	echo "usage: $0 list | check BEFORE VERSION" >&2
	exit 2
	;;
esac

fail=0
after=$(list) || { echo "FAIL could not list the images under $domain"; exit 1; }
echo "## images under $domain after the snapshot"
echo "$after"
wanted=("$domain:$version" "$domain:$version-debug")
present=()
for ref in "${wanted[@]}"; do
	if grep -qxF "$ref" <<<"$after"; then
		present+=("$ref")
	else
		echo "FAIL $ref: not in the daemon (goreleaser loads snapshot ko images there)"
		fail=1
	fi
done

echo "## kudurru version in each image (want $version)"
for ref in "${present[@]}"; do
	got=$($docker run --rm --network none "$ref" version 2>&1 | head -1)
	if [[ "$got" == "kudurru $version "* ]]; then
		echo "ok   $ref: $got"
	else
		echo "FAIL $ref: $got"
		fail=1
	fi
done

echo "## hack/volume-check.sh on the snapshot images"
echo "(a snapshot uses the plain distroless bases unless KUDURRU_BASE_IMAGE is set;"
echo " publish.yml builds images/base first, so a FAIL here on the plain bases is expected)"
if [ ${#present[@]} -gt 0 ]; then
	"$volume_check" "${present[@]}" || echo "(volume-check exit $?, not counted)"
fi

echo "## removing the images this snapshot created"
created=$({ comm -13 <(sort -u "$before") <(echo "$after"); printf '%s\n' "${present[@]}"; } | grep -v '^$' | sort -u)
while read -r ref; do
	[ -n "$ref" ] || continue
	if $docker rmi "$ref" >/dev/null; then
		echo "removed $ref"
	else
		echo "FAIL could not remove $ref"
		fail=1
	fi
done <<<"$created"
exit "$fail"

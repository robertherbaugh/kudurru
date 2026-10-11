#!/usr/bin/env bash
# Work out the git tag for a release candidate or a release from the VERSION
# file and the existing tags (docs/release-checklist.md, "Release
# pipeline"). Read-only: it never creates or pushes a tag.
#
#   hack/release-tag.sh rc        the next v<VERSION>-rc.<N> for HEAD
#   hack/release-tag.sh release   v<VERSION> for HEAD
#
# Prints key=value lines, and appends them to $GITHUB_OUTPUT when set:
#
#   version   VERSION without the v (also the image tag base)
#   tag       the git tag to create or reuse
#   exists    true when tag already points at HEAD (a re-run: reuse it)
#   previous  the tag the changelog starts from (empty for the first release)
#   rc        release only: the release candidate HEAD's tree matches
#
# Exits non-zero, with the reason on stderr, when VERSION is malformed, v<VERSION>
# is already released, or (release) HEAD is not the tree of the latest
# v<VERSION>-rc.<N>: a release ships exactly what its last candidate shipped.
#
# Needs the full history with tags (actions/checkout fetch-depth: 0).
set -euo pipefail

cd "$(dirname "$0")/.."

die() {
	echo "release-tag: $*" >&2
	exit 1
}

mode=${1:-}
case "$mode" in
rc | release) ;;
*) die "usage: $0 rc|release" ;;
esac

[ -f VERSION ] || die "no VERSION file in the repository root"
version=$(tr -d '[:space:]' <VERSION)
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
	die "VERSION is \"$version\", want MAJOR.MINOR.PATCH (no v, no suffix)"

head=$(git rev-parse HEAD)
release_tag="v$version"

# tag_commit TAG: the commit TAG points at, empty when TAG does not exist.
tag_commit() {
	git rev-parse -q --verify "refs/tags/$1^{commit}" 2>/dev/null || true
}

# rc_numbers: the N of every v<version>-rc.<N> tag, ascending.
rc_numbers() {
	git tag -l "$release_tag-rc.*" | sed -n "s/^v${version//./\\.}-rc\.\([1-9][0-9]*\)$/\1/p" | sort -n
}

# latest_release_before VERSION: the highest vX.Y.Z tag (no pre-release
# suffix) below VERSION, empty when there is none.
latest_release_before() {
	git tag -l 'v*' | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/' | { cat; echo "v$1"; } | sort -uV |
		awk -v want="v$1" '$0 == want { found = prev } { prev = $0 } END { print found }'
}

released=$(tag_commit "$release_tag")

case "$mode" in
rc)
	[ -z "$released" ] ||
		die "$release_tag is already released; bump VERSION on main before the next release candidate"
	n=$(rc_numbers | tail -n1)
	exists=false
	for t in $(git tag --points-at HEAD -l "$release_tag-rc.*"); do
		# HEAD is already a candidate of this version: a re-run.
		if [[ "$t" =~ ^v.*-rc\.[1-9][0-9]*$ ]]; then
			tag=$t
			exists=true
		fi
	done
	if [ "$exists" = false ]; then
		tag="$release_tag-rc.$((${n:-0} + 1))"
	fi
	this=${tag##*-rc.}
	prev_n=$(rc_numbers | awk -v n="$this" '$1 < n' | tail -n1)
	if [ -n "$prev_n" ]; then
		previous="$release_tag-rc.$prev_n"
	else
		previous=$(latest_release_before "$version")
	fi
	out=("version=$version" "tag=$tag" "exists=$exists" "previous=$previous")
	;;
release)
	exists=false
	if [ -n "$released" ]; then
		[ "$released" = "$head" ] ||
			die "$release_tag is already released at ${released:0:12}; bump VERSION on main for the next release"
		exists=true
	fi
	n=$(rc_numbers | tail -n1)
	[ -n "$n" ] ||
		die "no release candidate for $version: merge main into release-candidate and let it publish $release_tag-rc.1 first"
	rc="$release_tag-rc.$n"
	if [ "$(git rev-parse "HEAD^{tree}")" != "$(git rev-parse "$rc^{tree}")" ]; then
		die "HEAD does not match $rc, the latest candidate for $version. The release branch takes merges from release-candidate only, after its candidate is published (if the release-candidate workflow is still running, re-run this once it has tagged)"
	fi
	out=("version=$version" "tag=$release_tag" "exists=$exists" "previous=$(latest_release_before "$version")" "rc=$rc")
	;;
esac

printf '%s\n' "${out[@]}"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
	printf '%s\n' "${out[@]}" >>"$GITHUB_OUTPUT"
fi

#!/usr/bin/env bash
# candidate-published: the release candidate tag has a published GitHub
# pre-release (publish.yml finished): not missing, not a draft, with
# checksums.txt among its assets. promotion.yml and release.yml run it
# before a release, which ships exactly its last candidate.
#
#   GH_TOKEN=... hack/candidate-published.sh v<VERSION>-rc.<N>
set -euo pipefail

rc=${1:?usage: hack/candidate-published.sh v<VERSION>-rc.<N>}
if ! info=$(gh release view "$rc" --json isDraft,isPrerelease,assets --jq '[.isDraft, .isPrerelease, ([.assets[].name] | index("checksums.txt") != null)] | @tsv' 2>&1); then
	echo "::error::release candidate ${rc} has no GitHub release: its publish run failed or has not finished (${info})"
	exit 1
fi
read -r draft pre checksums <<<"$info"
if [ "$draft" != false ] || [ "$pre" != true ] || [ "$checksums" != true ]; then
	echo "::error::release candidate ${rc} is not a finished pre-release (draft=${draft} prerelease=${pre} checksums.txt=${checksums}); re-run its publish first"
	exit 1
fi
echo "release candidate ${rc} was published"

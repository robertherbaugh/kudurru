#!/usr/bin/env bash
# candidate-published: the release candidate tag has a published GitHub
# pre-release (publish.yml finished): not missing, not a draft, with
# checksums.txt among its assets; and the release-candidate.yml run that
# published it succeeded, including publish.yml's Trivy scan of the pushed
# images. promotion.yml and release.yml run it before a release, which
# ships exactly its last candidate. Needs the tag fetched and a token that
# can read releases and Actions runs.
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

# A re-run of the failed jobs updates the run's conclusion; a hand-run
# publish.yml does not.
sha=$(git rev-parse "${rc}^{commit}")
run=$(gh run list --workflow release-candidate.yml --event push --commit "$sha" --limit 1 --json status,conclusion,url --jq '.[0] // empty | [.status, .conclusion, .url] | @tsv')
if [ -z "$run" ]; then
	echo "::error::release candidate ${rc} has no release-candidate workflow run for ${sha}"
	exit 1
fi
IFS=$'\t' read -r status conclusion url <<<"$run"
if [ "$status" != completed ] || [ "$conclusion" != success ]; then
	echo "::error::the release-candidate run that published ${rc} is ${status}/${conclusion:-none}, not completed/success (a failed image scan fails it); fix the findings on main and cut a new candidate, or re-run its failed jobs: ${url}"
	exit 1
fi
echo "release candidate ${rc} was published"

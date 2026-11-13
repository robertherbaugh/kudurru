#!/usr/bin/env bash
# The release checks that no pull request workflow runs (dev-docs/release-checklist.md
# sections 1 to 3), run on the current commit with one log per step:
#
#   hack/release-local-checks.sh [--out DIR] [--rc N] [--full-snapshot] [--live] [--skip-generated]
#
#   --out DIR         where the logs go (default .context/release-checks-<date>-<commit>)
#   --rc N            the candidate number for release-manifests (default: the
#                     next one, from hack/release-tag.sh rc)
#   --full-snapshot   also `goreleaser release --snapshot --clean --skip=publish,sign`
#                     (needs Docker and syft on PATH; images go to $DOCKER_HOST's
#                     daemon and are removed afterwards)
#   --live            also the live-internet checks (port 53 and HTTPS egress):
#                     TestLiveInternet, TestLiveIANATrustAnchors, TestLiveRootManagedBootstrap
#   --skip-generated  skip `make check-generated` (minutes; CI runs both halves)
#
# Steps: version-check, the version-sync unit tests, hack/release-tag.sh rc,
# make check-generated, make release-manifests at <VERSION>-rc.<N> with an
# image-reference inspection of bin/release, the real dashboard bundle with
# go test ./dashboard/... and make size, the single-target goreleaser
# snapshot with `kudurru version`, then the optional steps. Nothing is
# published, signed, tagged or pushed.
#
# The tree must be clean when it starts (a built dashboard/dist/index.html
# counts: `git checkout -- dashboard/dist/index.html`). Afterwards it
# restores the placeholder index, removes dist/, bin/release, the dashboard
# build output and the goreleaser binary it built, and fails if
# `git status --short` is not empty. summary.txt has one line per step:
# name, exit status and seconds. The exit status is non-zero if any step failed.
set -uo pipefail

cd "$(dirname "$0")/.."
root=$(pwd)

GORELEASER_VERSION=${GORELEASER_VERSION:-v2.18.3}
out="" rc="" full=0 live=0 generated=1
while [ $# -gt 0 ]; do
	case "$1" in
	--out) out=$2; shift 2 ;;
	--rc) rc=$2; shift 2 ;;
	--full-snapshot) full=1; shift ;;
	--live) live=1; shift ;;
	--skip-generated) generated=0; shift ;;
	*) echo "usage: $0 [--out DIR] [--rc N] [--full-snapshot] [--live] [--skip-generated]" >&2; exit 2 ;;
	esac
done

if [ -n "$(git status --short)" ]; then
	git status --short >&2
	echo "release-local-checks: the tree is not clean; commit or restore first (a built index: git checkout -- dashboard/dist/index.html)" >&2
	exit 2
fi
commit=$(git rev-parse HEAD)
short=$(git rev-parse --short=8 HEAD)
version=$(tr -d '[:space:]' <VERSION)
out=${out:-.context/release-checks-$(date -u +%Y%m%d)-$short}
mkdir -p "$out"
out=$(cd "$out" && pwd)
summary=$out/summary.txt
: >"$summary"
n=0 failed=0
scratch=$(mktemp -d)

cleanup() {
	git checkout -- dashboard/dist/index.html 2>/dev/null
	git clean -fdXq -- dashboard/dist 2>/dev/null
	rm -rf dist bin/release "$scratch"
}
trap cleanup EXIT

# step NAME CMD...: run CMD with its output in NN-NAME.log, headed by the
# commit, and record the exit status and duration.
step() {
	local name=$1 log start status
	shift
	n=$((n + 1))
	log=$(printf '%s/%02d-%s.log' "$out" "$n" "$name")
	start=$(date +%s)
	{
		echo "# commit $commit"
		echo "# $(date -u +%Y-%m-%dT%H:%M:%SZ): $*"
		"$@"
	} >"$log" 2>&1
	status=$?
	echo "# exit $status" >>"$log"
	printf '%-28s exit %-3s %5ss  %s\n' "$name" "$status" "$(($(date +%s) - start))" "$(basename "$log")" | tee -a "$summary"
	[ "$status" -eq 0 ] || failed=1
	return "$status"
}

environment() {
	go version
	helm version --short || true
	bun --version || true
	python3 --version
	docker version --format 'docker server {{.Server.Version}}' 2>/dev/null || echo "docker: no daemon at ${DOCKER_HOST:-the default socket}"
	command -v syft >/dev/null && syft version 2>/dev/null | head -3 || echo "syft: not on PATH"
	df -h /
}

# The image references in the staged manifests and Compose bundle: every
# kudurru image must carry the candidate tag (<tag> or <supervisor>-k<tag>).
inspect_release() {
	local tag=$1
	ls -la bin/release bin/release/compose
	python3 -I - "$tag" bin/release <<'EOF'
import pathlib, re, sys
tag, base = sys.argv[1], pathlib.Path(sys.argv[2])
for want in ("crds.yaml", "install.yaml", "compose"):
    if not (base / want).exists():
        sys.exit(f"missing {base / want}")
ref = re.compile(r"ghcr\.io/robertherbaugh/(kudurru[a-z0-9-]*):([A-Za-z0-9._-]+)")
bad, seen = [], {}
for path in sorted(p for p in base.rglob("*") if p.is_file()):
    for m in ref.finditer(path.read_text(errors="replace")):
        seen.setdefault(m.group(0), set()).add(str(path.relative_to(base)))
        t = m.group(2)
        if t != tag and not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+-k" + re.escape(tag), t):
            bad.append(f"{path}: {m.group(0)}")
for r, files in sorted(seen.items()):
    print(f"{r}  ({len(files)} files)")
if not seen:
    sys.exit("no kudurru image references found")
if bad:
    sys.exit("references without the candidate tag:\n" + "\n".join(bad))
print(f"all {len(seen)} distinct kudurru image references carry {tag}")
EOF
}

snapshot_single() {
	"$scratch/goreleaser" build --snapshot --single-target --clean || return
	./dist/kudurru_linux_amd64_v1/kudurru version || return
	sha256sum dist/kudurru_linux_amd64_v1/kudurru
	wc -c <dist/kudurru_linux_amd64_v1/kudurru
}

snapshot_full() {
	command -v syft >/dev/null || { echo "syft is not on PATH (the sboms step needs it)"; return 1; }
	docker info >/dev/null 2>&1 || { echo "no Docker daemon at ${DOCKER_HOST:-the default socket}"; return 1; }
	"$scratch/goreleaser" release --snapshot --clean --skip=publish,sign
	local status=$?
	echo "## dist/ (artifacts, without the build directories)"
	find dist -maxdepth 1 -type f -printf '%s %p\n' | sort -k2
	echo "## dist/checksums.txt"
	cat dist/checksums.txt 2>/dev/null
	echo "## images created by this run"
	local images
	images=$(docker images --format '{{.Repository}}:{{.Tag}} {{.ID}}' | grep '^ghcr.io/robertherbaugh/kudurru:.*SNAPSHOT' || true)
	echo "$images"
	local snapshot
	snapshot=$(python3 -I -c 'import json, sys; print(json.load(open(sys.argv[1]))["version"])' dist/metadata.json 2>/dev/null)
	echo "## kudurru version in each image ($snapshot)"
	while read -r ref _; do
		[ -n "$ref" ] || continue
		echo "$ref: $(docker run --rm --network none "$ref" version 2>&1 | head -1)"
	done <<<"$images"
	echo "## hack/volume-check.sh on the snapshot images"
	echo "(a snapshot uses the plain distroless bases unless KUDURRU_BASE_IMAGE is set;"
	echo " publish.yml builds images/base first, so a FAIL here on the plain bases is expected)"
	# shellcheck disable=SC2046 # one argument per image
	hack/volume-check.sh $(awk '{print $1}' <<<"$images") || true
	# Only the snapshot tags: other users of a shared daemon keep their images.
	echo "## removing the snapshot images"
	awk '{print $1}' <<<"$images" | xargs -r docker rmi
	return "$status"
}

step environment environment
step version-check make version-check
step sync-version-tests python3 -B -m unittest discover -s hack -p test_sync_version.py -v
step release-tag-rc hack/release-tag.sh rc
if [ -z "$rc" ]; then
	rc=$(hack/release-tag.sh rc 2>/dev/null | sed -n 's/^tag=v.*-rc\.//p')
fi
tag=$version-rc.${rc:-1}
if [ "$generated" = 1 ]; then
	step check-generated make check-generated
fi
step release-manifests make release-manifests RELEASE_ARTIFACT_VERSION="$tag"
step release-manifests-inspect inspect_release "$tag"
step tree-after-manifests bash -c 'git status --short; [ -z "$(git status --short)" ]'
step dashboard-build make dashboard-build
step dashboard-go-test go test -count=1 ./dashboard/...
step size make size
step goreleaser-install env GOBIN="$scratch" go install "github.com/goreleaser/goreleaser/v2@$GORELEASER_VERSION"
step goreleaser-check "$scratch/goreleaser" check
step snapshot-single-target snapshot_single
if [ "$full" = 1 ]; then
	step snapshot-full snapshot_full
fi
if [ "$live" = 1 ]; then
	step live-recursor env KUDURRU_LIVE_DNS=1 go test -count=1 -v -run '^(TestLiveInternet|TestLiveIANATrustAnchors)$' ./internal/recursor
	step live-managed-root env KUDURRU_LIVE_DNS=1 go test -count=1 -v -run '^TestLiveRootManagedBootstrap$' ./cmd/kudurru
fi
cleanup
trap - EXIT
step tree-clean bash -c 'git status --short; [ -z "$(git status --short)" ] && grep -c kudurru:placeholder dashboard/dist/index.html'
step disk df -h /
echo "logs in $out"
exit "$failed"

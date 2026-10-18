#!/usr/bin/env bash
# ci-changes: the path filters of the workflows' `changes` jobs, in one place
# (ci.yml, compose.yml, dev-image.yml, kind.yml, license-server.yml,
# security.yml). Prints <filter>=true|false for each filter named, for
# $GITHUB_OUTPUT:
#
#   code         anything outside docs/, dev-docs/, docs-site/, website/,
#                branding/ and license-server/: the product and its tests
#   portal       what can change the license portal (license-server.yml):
#                license-server/, branding/ (the portal embeds the logos,
#                palette and email artwork), internal/license/ and
#                hack/licensegen/ (the golden vectors, trusted keyring and
#                shared/catalog.ts follow them) and its workflow
#   portal-scan  what can change the portal image's scan (security.yml):
#                the portal's paths and the scan policy
#
# The jobs are skipped rather than the workflows filtered with paths, because
# a skipped job satisfies a required status check and a workflow that never
# runs does not. Every filter is true unless git can list the changes: a
# tag, a scheduled or manual run, a new branch or an unreachable
# force-pushed base runs everything. A change to this script runs every
# filter.
#
#   hack/ci-changes.sh <filter>...
#
# The base and head come from the event (GITHUB_EVENT_NAME,
# GITHUB_EVENT_PATH); BASE and HEAD override them, e.g. to try a filter
# locally: BASE=origin/main HEAD=HEAD hack/ci-changes.sh code portal
set -euo pipefail

[ $# -gt 0 ] || { echo "usage: hack/ci-changes.sh <filter>..." >&2; exit 2; }
for f in "$@"; do
  case "$f" in
    code | portal | portal-scan) ;;
    *) echo "hack/ci-changes.sh: unknown filter $f" >&2; exit 2 ;;
  esac
done

docs='^(docs|dev-docs|docs-site|website|license-server|branding)/'
portal='^(license-server/|branding/|internal/license/|hack/licensegen/)|^\.github/workflows/license-server\.yml$'
policy='^(Makefile|trivy\.yaml|\.trivyignore\.yaml|\.github/workflows/security\.yml)$'
self='^hack/ci-changes\.sh$'

if [ -z "${BASE+x}" ] && [ -n "${GITHUB_EVENT_PATH:-}" ]; then
  case "${GITHUB_EVENT_NAME:-}" in
    pull_request | pull_request_target)
      BASE=$(jq -r '.pull_request.base.sha // ""' "$GITHUB_EVENT_PATH")
      HEAD=$(jq -r '.pull_request.head.sha // ""' "$GITHUB_EVENT_PATH") ;;
    push)
      if [[ "${GITHUB_REF:-}" != refs/tags/* ]]; then
        BASE=$(jq -r '.before // ""' "$GITHUB_EVENT_PATH")
        HEAD=$(jq -r '.after // ""' "$GITHUB_EVENT_PATH")
      fi ;;
  esac
fi
BASE=${BASE:-}
HEAD=${HEAD:-HEAD}

known=false
files=""
if [[ -n "$BASE" && ! "$BASE" =~ ^0+$ ]] && files=$(git diff --name-only "$BASE...$HEAD") && [ -n "$files" ]; then
  known=true
fi

# any <regex>: some changed file matches.
any() { grep -Eq "$1" <<<"$files"; }

for f in "$@"; do
  v=true
  if [ "$known" = true ] && ! any "$self"; then
    case "$f" in
      code) grep -Eqv "$docs" <<<"$files" || v=false ;;
      portal) any "$portal" || v=false ;;
      portal-scan) any "$portal|$policy" || v=false ;;
    esac
  fi
  echo "$f=$v"
done

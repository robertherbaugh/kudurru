#!/bin/bash
# Cherry-pick a lane's hand-written commits onto the current branch of the main
# checkout, skipping regenerated-artifact commits (subject "chore(gen): ...").
# Usage: integrate.sh <lane> <base-commit> [<end-commit>]
set -euo pipefail
lane="$1"; base="$2"; end="${3:-wip/v020/$lane}"
cd /home/vercel-sandbox/kudurru
[ "$(git rev-parse --abbrev-ref HEAD)" = "robertherbaugh/v020-features-review" ] || { echo "not on the PR branch" >&2; exit 2; }
mapfile -t commits < <(git log --reverse --format='%H %s' "$base..$end")
for line in "${commits[@]}"; do
  sha="${line%% *}"; subject="${line#* }"
  case "$subject" in
    "chore(gen)"*) echo "skip  ${sha:0:10} $subject"; continue;;
  esac
  echo "pick  ${sha:0:10} $subject"
  if ! git cherry-pick --allow-empty "$sha" >/tmp/v020-cherry.log 2>&1; then
    echo "CONFLICT at ${sha:0:10}: resolve, then 'git cherry-pick --continue' and rerun with base=$sha" >&2
    git status --short | grep -E '^(UU|AA|DU|UD|AU|UA)' >&2 || true
    exit 1
  fi
done
echo "done: $(git rev-parse --short HEAD)"

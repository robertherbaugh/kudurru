#!/bin/bash
# Keep the remote copies of the v0.2 completion work current, so that nothing
# exists only on this machine:
#   wip/v020/<lane>              the lane branch as committed
#   wip/v020/<lane>-uncommitted  a snapshot commit of uncommitted lane work
#   wip/v020/handoff-notes       integrator notes, maps and the lanes' out/ drafts
# Only refs under wip/v020/ are ever written; they are backup refs and are
# force-updated. The PR branch is never pushed from here.
# Usage: backup.sh once | backup.sh loop [seconds]
set -u
REPO=/home/vercel-sandbox/kudurru
S=$REPO/.context/v020-final-20261110
W=/home/vercel-sandbox/.claude/projects/-home-vercel-sandbox-kudurru/c8702f26-f312-4c72-b527-f2f104c61581/workflows/scripts
MSG_TRAILER=$'Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>\nSigned-off-by: Robert Herbaugh <contact@robertherbaugh.me>'
cd "$REPO" || exit 1

snapshot_lane() { # lane -> prints snapshot sha, or nothing when the worktree is clean
  local lane="$1" wt="$REPO/.context/wt/$1" idx tree head
  [ -d "$wt" ] || return 0
  [ -n "$(git -C "$wt" status --porcelain 2>/dev/null)" ] || return 0
  idx=$(mktemp -u /tmp/v020-snap-idx-XXXXXX)
  GIT_INDEX_FILE=$idx git -C "$wt" read-tree HEAD || return 0
  GIT_INDEX_FILE=$idx git -C "$wt" add -A -- . ':!dashboard/dist/index.html' 2>/dev/null
  tree=$(GIT_INDEX_FILE=$idx git -C "$wt" write-tree); rm -f "$idx"
  head=$(git -C "$wt" rev-parse HEAD)
  [ "$tree" = "$(git -C "$wt" rev-parse HEAD^{tree})" ] && return 0
  git -C "$wt" commit-tree "$tree" -p "$head" -m "wip($lane): snapshot of uncommitted in-progress work

Unfinished work of an agent on lane $lane, preserved as a snapshot so
that nothing exists only on one machine. It may not build or pass tests,
is not reviewed, and is not part of pull request 54.

$MSG_TRAILER"
}

notes_commit() {
  local idx tree f blob
  idx=$(mktemp -u /tmp/v020-notes-idx-XXXXXX)
  ( cd "$S" && find PLAN.md AGENT-BRIEF.md heavy integrate.sh prepush.sh diskguard.sh backup.sh out -type f; ls map-*.json postverify-*.json 2>/dev/null ) | sort -u | while read -r f; do
    blob=$(git hash-object -w "$S/$f") && GIT_INDEX_FILE=$idx git update-index --add --cacheinfo "100644,$blob,.handoff/v020/$f"
  done
  for f in "$W"/v020-lane-*.js "$W"/v020-post-verify-*.js; do
    [ -f "$f" ] || continue
    blob=$(git hash-object -w "$f") && GIT_INDEX_FILE=$idx git update-index --add --cacheinfo "100644,$blob,.handoff/v020/workflow-scripts/$(basename "$f")"
  done
  tree=$(GIT_INDEX_FILE=$idx git write-tree); rm -f "$idx"
  git commit-tree "$tree" -m "wip(handoff): integrator notes for the v0.2.0 completion pass

Working notes, read-only code maps, the agent brief and the decision,
ledger and changelog drafts that the lanes wrote for integration. Not
part of pull request 54; evidence logs stay local.

$MSG_TRAILER"
}

once() {
  local specs=() lane sha remote
  for wt in "$REPO"/.context/wt/*/; do
    lane=$(basename "$wt")
    git show-ref --verify -q "refs/heads/wip/v020/$lane" || continue
    specs+=("+refs/heads/wip/v020/$lane:refs/heads/wip/v020/$lane")
    sha=$(snapshot_lane "$lane")
    if [ -n "$sha" ]; then
      git update-ref "refs/snapshots/v020/$lane" "$sha"
      specs+=("+refs/snapshots/v020/$lane:refs/heads/wip/v020/$lane-uncommitted")
    fi
  done
  sha=$(notes_commit) && git update-ref refs/snapshots/v020/handoff-notes "$sha" && specs+=("+refs/snapshots/v020/handoff-notes:refs/heads/wip/v020/handoff-notes")
  if git push -q origin "${specs[@]}" 2>/tmp/v020-backup-push.err; then
    echo "$(date -u +%FT%TZ) pushed ${#specs[@]} refs"
  else
    echo "$(date -u +%FT%TZ) PUSH FAILED: $(tail -2 /tmp/v020-backup-push.err | tr '\n' ' ')"
    return 1
  fi
}

case "${1:-once}" in
  once) once ;;
  loop) while true; do once; sleep "${2:-900}"; done ;;
esac

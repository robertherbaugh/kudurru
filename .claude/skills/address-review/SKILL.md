---
name: address-review
description: Act on a completed code review of the current kudurru pull request. Triage every review comment, fix what is valid, run the local checks, commit with sign-off, push, reply to and resolve the threads, wait for CI, and merge. Use when a review has finished and the user wants the PR landed.
argument-hint: "[PR number]"
---

# Address a code review and land the PR

Goal: every review comment ends up **fixed**, **declined with a reason** or
**deferred**, CI is green on the final commit, and the PR is merged into
`main`. Work on autopilot. Stop and ask only at the gates under "Stop and ask".

## 1. Find the PR and the feedback

Use the PR number passed as an argument. If none was given, use the PR for the
current branch.

```sh
N=<number>   # or: N=$(gh pr view --json number -q .number)
gh pr view "$N" --json number,title,state,isDraft,baseRefName,headRefName,mergeable,url
git status --short && git fetch origin && git status -sb   # clean tree, in sync with the remote
```

Collect the feedback from every source. Reviews here arrive as one review
summary (Summary / Checks / Findings / Verdict) plus inline comments tagged
`[blocker]`, `[bug]`, `[suggestion]` or `[nit]`.

- **This conversation:** findings from a `/code-review` that ran in this session.
- **Unresolved review threads** (each thread ID is needed later to resolve it):
  ```sh
  gh api graphql -F n="$N" -f query='query($n:Int!){repository(owner:"robertherbaugh",name:"kudurru"){pullRequest(number:$n){reviewThreads(first:100){nodes{id isResolved isOutdated path line comments(first:20){nodes{databaseId author{login} body}}}}}}}' \
    -q '.data.repository.pullRequest.reviewThreads.nodes[] | select(.isResolved|not)'
  ```
- **Review summaries:** `gh api "repos/{owner}/{repo}/pulls/$N/reviews" -q '.[] | select(.body!="") | {id,state,submitted_at,body}'`.
  Read the latest summary's **Checks** section too, since it lists failures
  and checks that were not run.
- **PR conversation:** `gh pr view "$N" --json comments`. Ignore bot comments
  (Cloudflare preview deployments).
- **Conductor diff comments:** `mcp__conductor__GetDiffComments`, if it is available.

## 2. Triage before editing

Check each comment against the code; reviewers are sometimes wrong. Classify:

| Tag | Default action |
|---|---|
| `[blocker]`, `[bug]` | Fix, and add a test that would have caught it. |
| `[suggestion]` | Fix it if it is correct and in scope for this PR. |
| `[nit]` | Fix it if it is cheap and correct. Otherwise decline briefly. |
| Out of scope but real | Defer it with a row in `docs/followups.md` (name the source and the target). |
| Wrong or already handled | Decline, citing the code or the test that shows why. |

Before editing, write the triage as a short list in the chat.

## 3. Implement

- Follow the repo rules in `CLAUDE.md`: American English; for `web/` and
  `license-server/` UI, read AGENTS.md first and use Astryx components.
  Keep `docs/architecture/` diagrams in step when you change a flow they show.
- If you change `api/` types, run `make generate`.
- Run the checks for every area you touched, plus any check the review marked
  as failing or not run:

| Area | Command |
|---|---|
| Go (`cmd/`, `internal/`, `ee/`, `api/`) | `make lint test` (`make check-generated` if `api/` changed) |
| `web/` | `make web-lint web-test` and `cd web && bun run build` |
| `license-server/` | `make license-server-check` |
| `www/` | `make www-check` |
| `charts/`, `deploy/` | `make helm-check` |
| `site/` | `make docs-check-links` |

  A failure that also happens on `origin/main` is pre-existing; note it and move on.

## 4. Commit and push

- Use conventional commits with DCO sign-off, which the repo requires:
  `git commit -s -m "fix(scope): ..."` with a subject under 72 characters.
  Write one commit per logical fix; group nits into one commit.
- Run `git push`. Never force-push reviewed history. If the branch is behind
  `main` or conflicts with it, run `git merge origin/main`, re-run the checks,
  then push.

## 5. Close the loop on GitHub

For each thread, reply with one line: either `Fixed in <sha>: <what changed>`,
or the reason it was declined or deferred.

```sh
gh api "repos/{owner}/{repo}/pulls/$N/comments/<databaseId>/replies" -f body="Fixed in abc1234: ..."
gh api graphql -f query='mutation($t:ID!){resolveReviewThread(input:{threadId:$t}){thread{isResolved}}}' -F t=<thread id>
```

Resolve threads that are fixed or deferred. Leave declined threads open, so the
reviewer sees the reply.

## 6. Wait for CI

Run this as a **background** Bash command, with a timeout of about 45 minutes
(`kind-metallb` alone takes about 10 minutes):

```sh
until [ "$(gh pr checks "$N" --json name -q length 2>/dev/null || echo 0)" -gt 0 ]; do sleep 15; done
gh pr checks "$N" --watch --fail-fast --interval 30
```

`skipping` counts as passing: `ci.yml` skips jobs on purpose when their paths
did not change. On a failure:

1. Run `gh run view <run-id> --log-failed`, find the cause, fix it, commit, push, and wait again.
2. If it looks flaky (a timeout, a runner problem, or a test that passes
   locally and is not touched by the diff), run `gh run rerun <run-id> --failed`
   **once**.
3. If it is still red after 3 fix attempts, stop and ask.

## 7. Merge

```sh
gh pr view "$N" --json mergeable,mergeStateStatus,reviewDecision,headRefOid
gh pr merge "$N" --merge --match-head-commit "$(git rev-parse HEAD)"
```

- Use `--merge`: the repo uses merge commits. Never use `--admin` or bypass a
  ruleset. Do not pass `--delete-branch`, because the Conductor workspace keeps
  using it.
- After merging, list the `main` runs that the merge started
  (`gh run list --branch main --limit 5`). Merges to `main` deploy the license
  portal, `www` and the docs site. Report their URLs; do not wait for them.

## Stop and ask

Stop and ask the user before acting if:

- the PR is a draft, is closed, or targets anything other than `main`. PRs to
  `release-candidate` and `release` follow `docs/release-checklist.md`.
- you are declining a `[blocker]`/`[bug]`, or the review verdict is "request
  changes" and you disagree with it.
- a fix needs an architecture decision, which means a `docs/architecture.md`
  §16 row, or it would widen the PR's scope substantially.
- CI fails for reasons you can't fix here (secrets, quota, external services).

## Report

End with a single message containing:

- a table of comments, each with its action: fixed `<sha>`, declined (reason), or deferred
- the local checks that ran
- the CI result
- the merge commit and the URLs of the `main` runs

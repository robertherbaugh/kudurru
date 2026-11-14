# Brief for v0.2.0 completion agents

You are one of several agents finishing the kudurru v0.2.0 release (PR #54,
branch `robertherbaugh/v020-features-review`). An integrator (the parent
session) cherry-picks your commits onto the PR branch. Read this file fully
before you start. It is the contract between you and the integrator.

## 1. Where you work

- You work ONLY in the lane worktree named in your prompt
  (`/home/vercel-sandbox/kudurru/.context/wt/<lane>`, branch `wip/v020/<lane>`).
  `cd` there first and use absolute paths.
- Never edit the main checkout `/home/vercel-sandbox/kudurru`, another lane's
  worktree, or anything under `.context/worktrees/` (old retained checkouts;
  never read them as current source either).
- Shared, writable scratch: `/home/vercel-sandbox/kudurru/.context/v020-final-20261110/`
  (called `$SHARED` below). Your package output goes in `$SHARED/out/<PKG>/`
  and your test logs in `$SHARED/evidence/<PKG>/`.
- The map for your package is a JSON file in `$SHARED/map-*.json` (named in your
  prompt). Find your package IDs in its `remaining[]` array: `detail`,
  `acceptance` and `files` there are your starting specification. The file's
  `summary`, `alreadyDone`, `designOptions`, `recommendation`, `testInfra` and
  `risks` fields are required reading. The map was produced by reading code, not
  by running it: verify what it says before relying on it.

## 2. Project rules that are easy to get wrong

- American English everywhere (code, comments, docs, commit messages).
- Licensing: never copy or port code from GPL/EUPL projects (AdGuard Home,
  Pi-hole, Technitium, dnsdist, keepalived, Gravity). Reimplement from the
  specification. No new dependency unless it is unavoidable; if you add one it
  must be permissively licensed, must not need cgo (`CGO_ENABLED=0` is a release
  requirement) and must keep `make size` under 100,000,000 bytes. Say so in your
  result.
- `ee/` code may not be imported from `internal/dnsserver`, `internal/pipeline`
  or `internal/stages`. Nothing on the DNS query path may read the store or
  depend on a leader. DNS answering never depends on license state.
- Follow existing patterns instead of inventing parallel ones. This codebase is
  strict about: capturing the original request source and credential once and
  re-checking it at commit; bounded lifetimes; exact UID/resourceVersion
  compare-and-swap; effect + audit row committed together; explicit "outcome
  unknown" answers instead of retries; no replay of a refused or uncertain
  mutation. Read the neighboring code and the decision-log rows cited in your
  map before designing.
- Never weaken an existing guard, deadline, assertion or test to make something
  pass. Never add a skip. If an existing test fails after your change, find out
  why. If you believe an existing test or guard is wrong, say so in your result
  instead of changing it silently.
- Benchmark evidence pins source files by SHA-256: never edit a file listed
  under `workload_files` in any `bench/calibration/*/provenance.json` (for
  example `internal/dnsserver/quic_socket_test.go`), and never change a
  baseline or limits file under `bench/`. Run the guards before you commit:
  `python3 -B -m unittest discover -s hack -p 'test_bench_*.py'`.
- Every API route declares its permission in `internal/rbac/permissions.yaml`
  (a lint enforces it). Settings keys live in `internal/config/schema.go`.
- UI work (dashboard/): read `dashboard/AGENTS.md` and
  `dev-docs/milestones/m3/ui.md` first, and look up every Astryx component with
  `cd dashboard && bun run astryx -- component <Name> --dense` before using it.
- Never print or log secret values. Never commit `clusterconfig/`, kubeconfigs,
  talosconfigs or decrypted secrets.

## 3. Files you must not edit (the integrator owns them)

- `dev-docs/milestones/v0.2/*.md` (tracker ledgers)
- `dev-docs/design-record.md` (decision log), `dev-docs/followups.md`,
  `dev-docs/open-questions.md`
- `CHANGELOG.md`, the root `README.md`
- `dashboard/dist/index.html` (never stage it)

Instead write these files (plain Markdown, American English, no secrets):

- `$SHARED/out/<PKG>/decision.md`: only if your prompt assigns a decision number.
  One decision-log table row in the existing format:
  `| D<nnn> | <topic> | <decision> | <why> | <alternatives rejected> |`
  (look at the last rows of `dev-docs/design-record.md` for tone and length).
  You may reference `D<nnn>` in code comments and docs.
- `$SHARED/out/<PKG>/ledger.md`: the evidence section for the tracker, in the
  style of the last sections of `dev-docs/milestones/v0.2/completion-plan.md`
  (heading `### <ID> <short title> — November 10, 2026 (D<nnn>)`, what now
  happens in plain prose, then "Evidence under `.context/v020-final-20261110/evidence/<PKG>/`:"
  with exact test names, pass/skip counts and durations, retained failed runs and
  what they showed, and a last paragraph stating honestly what remains open).
- `$SHARED/out/<PKG>/changelog.md`: bullet lines for `CHANGELOG.md` `[0.2.0]`
  (user-facing wording, grouped under Added / Changed / Fixed).

You MAY edit operator and architecture docs that describe your feature
(`docs/architecture/*.md`, `docs/deploy/*.md`, `docs/guide/*.md`,
`docs-site/content/**` except the generated `docs-site/content/reference/`).
CLAUDE.md requires `docs/architecture/` pages and diagrams to stay in step with
the code when you change a flow they show.

## 4. Commits

- Conventional commits, subject under 72 characters, body explains what and why.
- Always `git commit -s` and end the message body with exactly one
  `Co-Authored-By` trailer line before the sign-off, naming the model you are
  actually running as (your own system context states it). For example
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` if you are Claude
  Opus 5.5, or `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>` if you
  are Claude Fable 5.2. Earlier commits in a lane may carry a different model's
  trailer; leave them as they are.
- Stage explicit paths only. Never `git add -A`, `git add .` or `git commit -a`.
- Split your work into: (a) one or more SOURCE commits (hand-written code, tests,
  docs), then (b) at most one FINAL commit that contains ONLY regenerated
  artifacts, with the subject `chore(gen): regenerate for <PKG>`. The integrator
  drops (b) and regenerates after cherry-picking, so (a) must never contain
  generated files and (b) must never contain hand-written ones.
- Generated artifacts and their generators (run only the ones you need):
  - `make generate` (controller-gen deepcopy + CRDs): `api/**/zz_generated.deepcopy.go`,
    `deploy/kustomize/crd/**`, `deploy/helm/kudurru-crds/templates/**`,
    `internal/store/validator/crds/**`. Needed after changing `api/v1alpha1` etc.
  - `make openapi` then `make client-go`: `api/openapi.json`, `api/openapi-3.0.json`,
    `internal/apiclient/openapi/**`. Needed after changing any API route or DTO.
  - `cd dashboard && bun run gen:api`: `dashboard/src/api/generated.ts`.
  - `make docs-gen`: `docs-site/content/reference/**` and the CLI reference
    (settings table, CRD reference, CLI reference, OpenAPI copy).
  - `make rbac-manifests` then `make deploy-assets`: after changing
    `internal/rbac/permissions.yaml` (chart ClusterRoles and kustomize bases).
  Drift tests (`TestOpenAPIDrift`, `TestReferenceDrift`, `TestCLIReferenceDrift`,
  `TestAggregatedRolesDrift`) must pass in your worktree after commit (b).
- Do not push. Do not rebase or reset the lane branch below the commit you
  started from. Do not touch other branches.

## 5. Building and testing on this shared machine

Three lanes share 8 cores and 16 GB. Follow this so that timing-sensitive tests
do not fail from CPU starvation and the machine does not run out of memory:

```sh
export KUBEBUILDER_ASSETS=/home/vercel-sandbox/kudurru/bin/k8s/1.37.0-linux-amd64
export CGO_ENABLED=1            # the race detector needs it; release builds stay CGO_ENABLED=0
H=/home/vercel-sandbox/kudurru/.context/v020-final-20261110/heavy   # machine-wide wrapper: at most four heavy runs at a time
```

- HEAVY runs must go through the lock wrapper: anything that compiles or runs the
  tests of `./cmd/kudurru`, `./test/e2e`, `./test/interop`, any `-race` run over
  more than three packages, `golangci-lint`, `make` generators, and dashboard
  builds or Playwright. Example:
  `$H go test -race -count=1 -run 'TestRootMember' -timeout 20m ./cmd/kudurru 2>&1 | tee $SHARED/evidence/<PKG>/race01.log`
  The wrapper waits for the lock, so a command may sit idle for many minutes
  before it starts; run long ones with `run_in_background` and wait for the
  notification instead of polling with sleep.
- Light runs (one to three small packages, `go build ./...`, `go vet` on a few
  packages, `gofmt -l`) need no lock; add `-p 2` to keep them polite.
- `./cmd/kudurru` has about 3,000 tests and a 200 MB race test binary: always
  select with `-run`. The full package only runs through `python3 hack/go-test.py`
  (the integrator does that).
- Process-level cluster tests live in `test/e2e` and need `KUDURRU_E2E=1`.
- Lint: `$H go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./path/to/pkg/...`
  on the packages you touched (scoped; zero issues required). `gofmt -l` must be
  empty for touched files.
- Docker: a private daemon is running (started by the integrator). Use it only
  when your package needs a container (dnsdist, Knot, PostgreSQL, keepalived):
  `export DOCKER_HOST=unix:///tmp/kudurru-lab-docker.sock`. It runs with
  `--bridge=none --iptables=false`, so containers need `--network host` (or
  `--network none`) and must bind loopback ports only. The pinned dnsdist image
  the live tests use is present as `kudurru-dnsdist:vip-fix-b8fd96f` (image ID
  sha256:89c7f196a871...). Rules: never prune, never remove or retag existing
  images, never restart or reconfigure the daemon; pull only digest-pinned
  images that a test in the repository already names; give every container you
  start a unique name with your package ID, and remove your containers when
  done. Runs that start containers are HEAVY (use the wrapper). If the socket
  is missing, say so in your result instead of starting a daemon.
- Keep numbered logs of every meaningful run in `$SHARED/evidence/<PKG>/`
  (`race01.log`, `race02.log`, ...), including failed runs. Do not delete or
  overwrite a failed log; the ledger records retained failures and what they
  showed.
- Runs can be cut off without warning (the machine has already been restarted
  once mid-run). Commit each coherent, building, tested step as its own source
  commit as soon as it is ready instead of saving everything for the end, and
  write your `out/<PKG>/` drafts early. Uncommitted work survives a cutoff, but
  the next agent has to rediscover what it meant.
- Disk is limited (about 11 GB free, shared; a background guard trims stale Go
  build cache entries). Do not create extra worktrees or
  copies of the repository. Remove large temporary files you create.

## 6. Definition of done for an implementation package

1. The behavior in your map entry is implemented and mounted in the production
   path (not only a library), with API/CLI/schema/permission/doc surfaces that
   the acceptance text requires. UI surfaces are handled by a later dashboard
   lane unless your prompt says otherwise; expose what the UI will need (API
   fields, capability flags) and list it in your result.
2. Tests prove the acceptance criteria through the real seams the neighboring
   tests use (real SQLite, envtest, real TLS/HTTP doubles, mounted `runServe`),
   and they fail without your change. Unit tests alone are not acceptance when
   the criterion names a mounted behavior.
3. `gofmt`, `go vet`, scoped lint and race tests of every touched package pass;
   drift tests pass after your generated commit.
4. Source commits + optional generated commit exist on the lane branch, and the
   three `$SHARED/out/<PKG>/` files are written.
5. Your structured result is honest: what is done, what is not, every test
   command with its outcome, and anything you decided that the integrator
   should know. "Partial" with a precise remainder is far more useful than an
   optimistic "done".

If the specification turns out to be wrong or the right design differs from the
map's recommendation, use your judgment, do the better thing, and explain it in
the decision row and result. Do not stop to ask; nobody can answer during the run.

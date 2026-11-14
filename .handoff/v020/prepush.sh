#!/bin/bash
# Composed pre-push checks for an integration batch. Each step runs through the
# heavy wrapper and logs to the evidence directory given as $1.
# Usage: prepush.sh <evidence-dir> <base-commit>
set -u
cd /home/vercel-sandbox/kudurru
OUT="$1"; BASE="$2"
H=${HEAVY:-/home/vercel-sandbox/kudurru/.context/v020-final-20261110/heavy}
export KUBEBUILDER_ASSETS=/home/vercel-sandbox/kudurru/bin/k8s/1.37.0-linux-amd64
export CGO_ENABLED=1
mkdir -p "$OUT"
fail=0
step() { # name, command...
  local name="$1"; shift
  "$H" "$@" > "$OUT/$name.log" 2>&1
  local rc=$?
  echo "exit=$rc" >> "$OUT/$name.log"
  echo "$name exit=$rc"
  [ $rc -eq 0 ] || fail=1
}
pkgs=$(git diff --name-only "$BASE"..HEAD | grep '\.go$' | xargs -r -n1 dirname | sort -u | grep -v -E '^(cmd/kudurru|test/e2e|test/interop)$' | sed 's|^|./|' | tr '\n' ' ')
newtests=$(git diff "$BASE"..HEAD -- cmd/kudurru | grep -E '^\+func Test' | sed 's/^+func \(Test[A-Za-z0-9_]*\).*/\1/' | sort -u | paste -sd'|')
gofiles=$(git diff --name-only --diff-filter=AM "$BASE"..HEAD | grep '\.go$' | grep -v zz_generated)
fmt=""; [ -n "$gofiles" ] && fmt=$(gofmt -l $gofiles 2>&1)
echo "gofmt: ${fmt:-clean}"; [ -z "$fmt" ] || fail=1
step pyguards bash -c "python3 -m unittest discover -s hack -p test_go_test.py && python3 -B -m unittest discover -s hack -p 'test_bench_*.py' && python3 -B -m unittest discover -s hack -p test_sync_version.py"
step build bash -c 'go build ./... && CGO_ENABLED=0 go build -o /dev/null ./cmd/kudurru'
step vet go vet ./...
step drift go test -count=1 -run '^(TestOpenAPIDrift|TestReferenceDrift|TestAggregatedRolesDrift|TestRoutePermissionLint|TestSettings.*)$' ./internal/api ./hack/docsgen ./ee/rbac/... ./internal/config
step lint go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run --allow-parallel-runners ./...
[ -n "$pkgs" ] && step libs go test -race -count=1 -p 2 -timeout 40m $pkgs
step cmd go test -race -count=1 -timeout 40m -run "^(TestCLIReferenceDrift|TestDeadcode.*|TestRootMemberFollowerRefusesUnforwardedMutations|${newtests})\$" ./cmd/kudurru
echo "prepush fail=$fail"
exit $fail

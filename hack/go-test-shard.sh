#!/usr/bin/env bash
# go-test-shard: print the packages of shard <i> of <n>, for
# `make test TEST_PKGS="$(hack/go-test-shard.sh <i> <n>)"` (ci.yml's go job
# runs one shard per runner). Every package of `go list ./...` lands in
# exactly one shard. Packages are placed heaviest first on the lightest
# shard, weighted by the seconds `go test -race` took on a 4-vCPU runner
# (WEIGHTS below, from a go job's log; a package not listed counts 1 s), so
# the shards take about as long. Refresh WEIGHTS when one shard's Test step
# becomes much slower than the other's.
#
#   hack/go-test-shard.sh <i> <n>      1 <= i <= n
set -euo pipefail

if [ $# -ne 2 ] || ! [[ "$1" =~ ^[0-9]+$ && "$2" =~ ^[0-9]+$ ]] || (($1 < 1 || $1 > $2)); then
  echo "usage: hack/go-test-shard.sh <i> <n> (1 <= i <= n)" >&2
  exit 2
fi
cd "$(dirname "$0")/.."

# Seconds per package (relative to the module path), October 2026.
WEIGHTS='
cmd/kudurru 1120
internal/store/sqlite 326
internal/servingbuild 140
internal/auth/security 93
internal/api 84
internal/store/kube 82
internal/querylog/sqlitelog 70
internal/primaryruntime 56
internal/servingauth 52
internal/primaryruntime/network 46
internal/auth/providerhost 39
internal/iammigrate 39
internal/zonestate 35
ee/edge 31
internal/controller 31
ee/edge/dnsdist 29
internal/dnsserver 29
internal/audit/spool 27
internal/central/pgquerylog 27
internal/auth/providercoordinator 25
internal/dhcp 25
internal/dhcpha 25
internal/primaryruntime/sqliteadapter 25
deploy 23
ee/edge/keepalived 19
internal/primaryruntime/driver 19
internal/zonecache 19
ee/integration/inbound 17
internal/central/pgaudit 15
internal/primaryruntime/currentguard 15
internal/primaryzone 15
internal/auth/stack 14
internal/unblock 14
ee/integration/cloudflare 13
internal/dhcpkube 12
internal/audit/auditlog 11
internal/auth/users 10
ee/integration/host 9
internal/config 9
internal/dhcpdns 9
internal/listdata/catalogue 9
internal/store/validator 9
'

mod=$(go list -m)
go list ./... | awk -v mod="$mod/" -v shard="$1" -v shards="$2" -v weights="$WEIGHTS" '
  BEGIN {
    n = split(weights, lines, "\n")
    for (i = 1; i <= n; i++) if (split(lines[i], f, " ") == 2) w[f[1]] = f[2]
  }
  {
    rel = substr($0, 1, length(mod)) == mod ? substr($0, length(mod) + 1) : $0
    pkg[++np] = $0; wt[np] = (rel in w) ? w[rel] : 1
  }
  END {
    # Stable order: heaviest first, then by import path.
    for (i = 1; i <= np; i++) idx[i] = i
    for (i = 2; i <= np; i++) {
      j = i
      while (j > 1 && (wt[idx[j-1]] < wt[idx[j]] || (wt[idx[j-1]] == wt[idx[j]] && pkg[idx[j-1]] > pkg[idx[j]]))) {
        t = idx[j]; idx[j] = idx[j-1]; idx[j-1] = t; j--
      }
    }
    for (s = 1; s <= shards; s++) load[s] = 0
    for (i = 1; i <= np; i++) {
      best = 1
      for (s = 2; s <= shards; s++) if (load[s] < load[best]) best = s
      load[best] += wt[idx[i]]
      if (best == shard) print pkg[idx[i]]
    }
  }'

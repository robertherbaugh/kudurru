#!/usr/bin/env bash
# Run kudurru's Go fuzz targets (docs/m5/e2e.md, "Fuzzing").
#
#   hack/fuzz.sh                 every target for FUZZTIME (default 60s)
#   hack/fuzz.sh --list          print "package target" pairs (CI matrix)
#   hack/fuzz.sh FuzzDoHRequest  only entries ("<package> <target>") matching the regex
#   hack/fuzz.sh --one ./internal/zone FuzzImportBIND   exactly one target (CI)
#
# FUZZTIME     per-target budget (go test -fuzztime), default 60s
# FUZZ_JOBS    targets fuzzed at the same time, default 1
# FUZZ_PARALLEL workers per target (go test -parallel), default 2
#
# Each target runs from its seed corpus (f.Add plus testdata/fuzz/<target>/)
# and writes new crashers to testdata/fuzz/<target>/ in its package: commit
# a crasher together with the fix so it stays a regression test. Logs go to
# bin/fuzz/<target>.log.
set -euo pipefail

cd "$(dirname "$0")/.."

TARGETS=(
	"./internal/blocking/parser FuzzParse"
	"./internal/dnsserver/proxyproto FuzzParse"
	"./internal/dnsserver/proxyproto FuzzDatagram"
	"./internal/dnsserver/proxyproto FuzzStream"
	"./internal/zone FuzzImportBIND"
	"./internal/zone FuzzCanonicalKey"
	"./test/fuzz FuzzConfigLoad"
	"./test/fuzz FuzzConfigEnv"
	"./test/fuzz FuzzConfigValues"
	"./test/fuzz FuzzDoHRequest"
	"./test/fuzz FuzzTSIGVerify"
	"./test/fuzz FuzzQueryLogFilter"
	"./test/fuzz FuzzQueryLogNames"
	"./test/fuzz FuzzDnsmasqImport"
	"./test/fuzz FuzzKeepalivedRender"
	"./test/fuzz FuzzKeepalivedFIFOLine"
	"./test/fuzz FuzzDNSDistRender"
)

if [[ "${1:-}" == "--list" ]]; then
	printf '%s\n' "${TARGETS[@]}"
	exit 0
fi

fuzztime="${FUZZTIME:-60s}"
jobs="${FUZZ_JOBS:-1}"
parallel="${FUZZ_PARALLEL:-2}"
mkdir -p bin/fuzz

run_one() {
	local pkg="$1" target="$2" log="bin/fuzz/$2-$(basename "$1").log"
	local start=$SECONDS
	if go test -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime" -parallel "$parallel" "$pkg" >"$log" 2>&1; then
		echo "ok    $pkg $target ($((SECONDS - start))s, $(grep -Eo 'execs: [0-9]+' "$log" | tail -1))"
	else
		echo "FAIL  $pkg $target ($((SECONDS - start))s): see $log"
		tail -n 30 "$log"
		return 1
	fi
}

if [[ "${1:-}" == "--one" ]]; then
	run_one "$2" "$3"
	exit $?
fi

filter="${1:-.}"
failed=0
pids=()
for entry in "${TARGETS[@]}"; do
	read -r pkg target <<<"$entry"
	[[ "$entry" =~ $filter ]] || continue
	run_one "$pkg" "$target" &
	pids+=($!)
	if ((${#pids[@]} >= jobs)); then
		wait "${pids[0]}" || failed=1
		pids=("${pids[@]:1}")
	fi
done
for pid in "${pids[@]}"; do
	wait "$pid" || failed=1
done
exit $failed

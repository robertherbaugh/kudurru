#!/usr/bin/env bash
# go-modules: fetch Go modules with a bounded retry on transient network and
# proxy errors, so a dropped HTTP/2 stream or a 5xx from proxy.golang.org
# does not fail a CI job. Only a failure whose output names a network or
# proxy error is retried (3 attempts, 10 s then 20 s apart); any other
# failure, and every failure that names a checksum mismatch, an unknown
# revision or a missing module, fails on the first attempt. GOPROXY and
# GOSUMDB stay as configured: nothing here falls back to `direct` or skips
# the checksum database. A retry resumes where the last attempt stopped,
# because the module cache keeps what was already fetched.
#
#   hack/go-modules.sh download [MODULE...]      go mod download (no MODULE:
#                                                the current module's build list)
#   hack/go-modules.sh install PKG@VERSION...    go install, into $GOBIN when set
#   hack/go-modules.sh run PKG@VERSION [ARG...]  go run PKG@VERSION: installs the
#                                                tool (retried), then runs it once
#
# `run` keeps a tool pinned to a release version in
# bin/go-tools/<go version>/<PKG>@<VERSION>/ (GO_MODULES_TOOLS moves it), so
# a later run neither downloads nor links it again; any other version query
# (@latest, a branch) is installed into a temporary directory every time.
#
# CI downloads the module's dependencies once per job with `download` and
# then builds from the module cache: the Dockerfile and .goreleaser.yaml
# build with GOPROXY=off, so a build can never fetch a module that the
# retried download did not. Tools run with `go run PKG@VERSION` in the
# Makefile go through `run`.
#
# GO_MODULES_ATTEMPTS (default 3) and GO_MODULES_BACKOFF (seconds before the
# first retry, doubled for each later one; default 10) tune the retry.
set -euo pipefail

usage='usage: hack/go-modules.sh download [MODULE...] | install PKG@VERSION... | run PKG@VERSION [ARG...]'
attempts=${GO_MODULES_ATTEMPTS:-3}
backoff=${GO_MODULES_BACKOFF:-10}
go=${GO:-go}

# Failures that another attempt cannot fix. They win over a transient match
# in the same output.
permanent='checksum mismatch|SECURITY ERROR|unknown revision|invalid version|404 Not Found|410 Gone|not found: |missing go\.sum entry|malformed'
# Network and proxy failures: HTTP/2 resets, dropped connections, timeouts,
# DNS and dial errors, and server-side 5xx or 429 responses.
transient='stream error|INTERNAL_ERROR|GOAWAY|http2: |connection reset|connection refused|broken pipe|unexpected EOF|i/o timeout|TLS handshake timeout|context deadline exceeded|Client\.Timeout|timeout awaiting response headers|dial tcp|: 5[0-9][0-9] [A-Z]|: 429 Too Many Requests'

out=$(mktemp)
tools=
cleanup() {
	rm -f "$out"
	if [ -n "$tools" ]; then rm -rf "$tools"; fi
}
trap cleanup EXIT

# fetch COMMAND...: run a download-only go command, retrying transient errors.
fetch() {
	local attempt=1 status delay
	while :; do
		status=0
		"$@" >"$out" 2>&1 || status=$?
		cat "$out" >&2
		if [ "$status" -eq 0 ]; then
			return 0
		fi
		if grep -Eq "$permanent" "$out" || ! grep -Eq "$transient" "$out"; then
			return "$status"
		fi
		if [ "$attempt" -ge "$attempts" ]; then
			echo "go-modules: transient network or proxy error on attempt $attempt of $attempts; giving up" >&2
			return "$status"
		fi
		delay=$((backoff << (attempt - 1)))
		echo "go-modules: transient network or proxy error on attempt $attempt of $attempts; retrying in ${delay}s" >&2
		sleep "$delay"
		attempt=$((attempt + 1))
	done
}

mode=${1:-}
[ $# -gt 0 ] && shift
case $mode in
download)
	fetch "$go" mod download "$@"
	;;
install)
	[ $# -gt 0 ] || { echo "$usage" >&2; exit 2; }
	fetch "$go" install "$@"
	;;
run)
	pkg=${1:?$usage}
	shift
	case $pkg in
	*@*) ;;
	*) echo "go-modules: run takes PKG@VERSION, got $pkg" >&2; exit 2 ;;
	esac
	dir=
	if [[ ${pkg##*@} =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?$ ]]; then
		cache=${GO_MODULES_TOOLS:-$(cd "$(dirname "$0")/.." && pwd)/bin/go-tools}
		dir=$cache/$("$go" env GOVERSION)/$pkg
	fi
	if [ -z "$dir" ] || ! compgen -G "$dir/*" >/dev/null; then
		if [ -n "$dir" ]; then
			mkdir -p "$dir"
			tools=$(mktemp -d "$cache/.install.XXXXXX")
		else
			tools=$(mktemp -d)
		fi
		fetch env GOBIN="$tools" "$go" install "$pkg"
		if [ -n "$dir" ]; then
			# A rename within the cache, so a concurrent run sees either no
			# program or a whole one.
			for bin in "$tools"/*; do mv -f "$bin" "$dir/"; done
		else
			dir=$tools
		fi
	fi
	bins=("$dir"/*)
	if [ "${#bins[@]}" -ne 1 ] || [ ! -x "${bins[0]}" ]; then
		echo "go-modules: go install $pkg did not produce one program in $dir" >&2
		exit 1
	fi
	status=0
	"${bins[0]}" "$@" || status=$?
	exit "$status"
	;;
*)
	echo "$usage" >&2
	exit 2
	;;
esac

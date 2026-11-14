#!/usr/bin/env bash
# Run the first-party grammar corpus against a separate official executable.
# No AdGuard Home source, rules or executable enter the repository/artifacts.
set -euo pipefail

cd "$(dirname "$0")/.."
version=v0.107.79
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64)
    platform=linux_amd64
    checksum=c48f4a43000665484c5ec28177de11a004759b620dae8f77b2aabefc9ef3687f
    ;;
  Linux/aarch64|Linux/arm64)
    platform=linux_arm64
    checksum=3f7893c18e8aaadc456d0452839190561c306ca95175a2254958be80a769c1ae
    ;;
  *)
    echo 'test-adguard requires Linux amd64 or arm64; other hosts can set KUDURRU_ADGUARD_BINARY when running the Go test directly.' >&2
    exit 1
    ;;
esac

# SHA-256 values are from the official v0.107.79 release asset metadata and
# checksums.txt. Updating the version requires reviewing comparator differences.
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
archive="$scratch/AdGuardHome.tar.gz"
curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 \
  --connect-timeout 15 --max-time 120 --retry 2 --max-filesize 100000000 \
  "https://github.com/AdguardTeam/AdGuardHome/releases/download/$version/AdGuardHome_$platform.tar.gz" \
  --output "$archive"
printf '%s  %s\n' "$checksum" "$archive" | sha256sum --check --status
tar -xzf "$archive" -C "$scratch" ./AdGuardHome/AdGuardHome
export KUDURRU_ADGUARD_BINARY="$scratch/AdGuardHome/AdGuardHome"
"$KUDURRU_ADGUARD_BINARY" --version
CGO_ENABLED=1 go test -race -count=1 -run '^TestAdGuardBlackBox$' -v ./internal/blocking

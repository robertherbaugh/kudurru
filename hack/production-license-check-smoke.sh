#!/usr/bin/env bash
set -euo pipefail

binary=${1:?pass a kudurru binary built with -tags=licensetest}
private=$(mktemp -d /tmp/kudurru-license-smoke.XXXXXXXX)
trap 'if [[ -f "$private/synthetic.key" ]]; then unlink "$private/synthetic.key"; fi; rmdir "$private"' EXIT
install -m 600 internal/license/testdata/e2e-license.key "$private/synthetic.key"
sha256=$(sha256sum "$binary" | cut -d ' ' -f1)

bun --no-env-file hack/production-license-check.ts active "$binary" "$sha256" "$private/synthetic.key" |
  jq -e --arg sha256 "$sha256" '.passed and (.humanVisualApproval | not) and .binarySha256 == $sha256 and .mode == "active" and (.checks | length == 3)' >/dev/null
bun --no-env-file hack/production-license-check.ts community "$binary" "$sha256" |
  jq -e --arg sha256 "$sha256" '.passed and (.humanVisualApproval | not) and .binarySha256 == $sha256 and .mode == "community" and (.checks | length == 1)' >/dev/null

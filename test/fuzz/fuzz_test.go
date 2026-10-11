// Package fuzz holds fuzz targets for parsers and renderers that take
// untrusted or operator input through exported APIs: the configuration
// loader, DoH request bodies, TSIG verification, query-log filters, the
// dnsmasq import and the keepalived/dnsdist renderers. In-package targets
// (blocking parser, PROXYv2, zone file import) stay next to their code;
// hack/fuzz.sh and `make fuzz` run all of them (docs/m5/e2e.md).
//
// Without -fuzz, `go test` runs every target over its seed corpus (f.Add
// and testdata/fuzz/<target>/), so the corpus is a regression suite.
package fuzz

import (
	"os"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// skipKnownBug reports whether inputs that trigger the recorded bug id
// (docs/m5/e2e.md) should be skipped: true unless KUDURRU_E2E_KNOWN_BUGS
// is set, in which case the target reproduces the bug.
func skipKnownBug(id string) bool {
	_ = id
	return os.Getenv(kudurrutest.EnvKnownBugs) == ""
}

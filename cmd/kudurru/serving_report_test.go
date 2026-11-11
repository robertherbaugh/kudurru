package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// The publisher tries a failing candidate again every second. The log names
// what it failed on once, again when the reason changes, and says when a
// generation is published again; a candidate that lost the race against a
// newer one is not reported.
func TestServingFailureLogNamesTheFailureOnce(t *testing.T) {
	var out bytes.Buffer
	l := &servingFailureLog{logger: slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	lines := func() []string {
		t.Helper()
		text := strings.TrimSpace(out.String())
		out.Reset()
		if text == "" {
			return nil
		}
		return strings.Split(text, "\n")
	}
	l.report("current", nil)
	l.report("candidate_changed", replication.ErrCAS)
	if got := lines(); got != nil {
		t.Fatalf("a publication and a superseded candidate were logged: %v", got)
	}
	zone := errors.New(`servingzones: nonprimary build failed: DNSZone "lab": spec.type: Unsupported value: "external"`)
	for range 3 {
		l.report("prepare_failed", zone)
		l.report("candidate_changed", replication.ErrCAS)
	}
	got := lines()
	if len(got) != 1 || !strings.Contains(got[0], "level=WARN") || !strings.Contains(got[0], "complete serving generation not published") ||
		!strings.Contains(got[0], "reason=prepare_failed") || !strings.Contains(got[0], `DNSZone \"lab\"`) {
		t.Fatalf("a repeated failure was logged as %v", got)
	}
	l.report("prepare_failed", errors.New("querylog: unsupported sink"))
	if got := lines(); len(got) != 1 || !strings.Contains(got[0], "querylog: unsupported sink") {
		t.Fatalf("a different failure was logged as %v", got)
	}
	l.report("current", nil)
	l.report("dns_only", nil)
	if got := lines(); len(got) != 1 || !strings.Contains(got[0], "level=INFO") || !strings.Contains(got[0], "published again") {
		t.Fatalf("the recovery was logged as %v", got)
	}
	// A dependency that changed under the candidate keeps the generation
	// unpublished until it settles: that is reported.
	l.report("candidate_changed", fmt.Errorf("capture: %w", serving.ErrDependency))
	if got := lines(); len(got) != 1 || !strings.Contains(got[0], "reason=candidate_changed") {
		t.Fatalf("a dependency failure was logged as %v", got)
	}
	l.report("prepare_failed", errors.New(strings.Repeat("x", 3*maxServingFailureText)))
	if got := lines(); len(got) != 1 || len(got[0]) > maxServingFailureText+300 {
		t.Fatalf("an oversized error was logged with %d bytes", len(got[0]))
	}
}

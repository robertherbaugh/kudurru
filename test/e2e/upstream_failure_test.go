package e2e

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// TestUpstreamFailureAnsweredAcrossCommits: a node whose upstream fails
// answers SERVFAIL at once, and keeps doing so while a configuration commit
// replaces what it serves from. Each API token created through the admin
// socket is such a commit. A query that arrived around one used to get no
// answer at all (the node logged "write failed: serving: dependency
// changed" at debug level) and its client waited for its own timeout, while
// a forwarded answer in the same position was delivered.
func TestUpstreamFailureAnsweredAcrossCommits(t *testing.T) {
	kudurrutest.RequireE2E(t)
	// The harness default: the upstream is the discard port, where nothing
	// listens.
	spec, cfg := kudurrutest.NewNodeSpec(t, t.TempDir(), "upstream-failure")
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{
		Env: []string{"KUDURRU_ADMIN_PASSWORD=upstream-failure-e2e-password-1"}, // synthetic fixture; avoid generated credentials in failure logs
	})
	socket := filepath.Join(spec.DataDir, "admin.sock")

	const rounds = 5
	c := &dns.Client{Timeout: 3 * time.Second}
	sent := 0
	ask := func(round int) {
		t.Helper()
		m := new(dns.Msg)
		m.SetQuestion(fmt.Sprintf("q%d.upstream-failure.example.", sent), dns.TypeA)
		sent++
		r, _, err := c.Exchange(m, spec.Ports.DNS)
		if err != nil {
			t.Fatalf("commit %d of %d, query %d: no answer: %v", round, rounds, sent, err)
		}
		if r.Rcode != dns.RcodeServerFailure {
			t.Fatalf("commit %d of %d, query %d: %s, want SERVFAIL", round, rounds, sent, dns.RcodeToString[r.Rcode])
		}
	}
	ask(0)
	for round := 1; round <= rounds; round++ {
		created := make(chan error, 1)
		go func() {
			out, err := exec.Command(kudurruBin, "admin", "create-token", "--socket", socket).Output() //nolint:gosec // the test's binary
			if err == nil && strings.TrimSpace(string(out)) == "" {
				err = errors.New("no token printed")
			}
			created <- err
		}()
		// Queries go out one after another from before the commit until
		// well after the command returned, so that some are in flight when
		// the commit lands and some arrive before its successor is served.
		var until time.Time
		for until.IsZero() || time.Now().Before(until) {
			ask(round)
			if until.IsZero() {
				select {
				case err := <-created:
					if err != nil {
						t.Fatalf("create-token %d: %v", round, err)
					}
					until = time.Now().Add(250 * time.Millisecond)
				default:
				}
			}
			time.Sleep(time.Millisecond)
		}
	}
	t.Logf("%d queries answered SERVFAIL across %d commits", sent, rounds)
}

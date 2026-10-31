package dnsdist

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/internal/cluster"
)

func TestConsoleBoundsAndAcknowledgment(t *testing.T) {
	for _, mode := range []string{"ack", "preamble", "trailing", "echo", "unacknowledged", "failed", "overflow", "stderr-overflow", "sleep"} {
		t.Run(mode, func(t *testing.T) {
			s, err := New(Config{Source: &cluster.StaticSource{}, Binary: fakeBinary(t), ConfigDir: t.TempDir(), ConsoleTimeout: 500 * time.Millisecond, ChildEnv: []string{"KUDURRU_FAKE_CONSOLE_MODE=" + mode}})
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			err = s.console(t.Context(), "do end")
			if mode == "ack" || mode == "preamble" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unacknowledged/failed command accepted")
			}
			if mode == "sleep" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout classification: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "sensitive") {
				t.Fatal("upstream diagnostic leaked")
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("console deadline was not bounded")
			}
		})
	}
	s := &Supervisor{cfg: Config{Binary: "must-not-execute"}}
	if !errors.Is(s.console(t.Context(), strings.Repeat("x", maxConsoleCommand)), errConsole) {
		t.Fatal("oversized command not rejected before exec")
	}
}

func TestPrivateStagingDoesNotSatisfyReadiness(t *testing.T) {
	status := &ServerStatus{Servers: []BackendStatus{{State: "up", Pools: []string{"kudurru-pending-test"}}, {State: "down", Pools: []string{edge.PoolResolver}}}}
	if status.UpCount() != 0 {
		t.Fatal("staging backend satisfied readiness")
	}
}

func TestSupervisorRemovalAndUncertainMutation(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Source = &chanSource{initial: []cluster.Endpoint{ep("node-a", "10.0.0.11"), ep("node-b", "10.0.0.12")}, ch: make(chan []cluster.Endpoint, 4)}
	})
	// newHarness keeps its channel source, so use the configured source here.
	source := h.sup.cfg.Source.(*chanSource)
	h.waitFor("ready", h.ready)
	if err := os.WriteFile(filepath.Join(h.dir, "fail-remove"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	source.ch <- []cluster.Endpoint{ep("node-b", "10.0.0.12")}
	h.waitFor("uncertain removal convergence", func() bool { return h.count("start") == 2 && strings.Contains(h.log(), "backends=node-b") && h.ready() })
	data, _ := os.ReadFile(h.sup.MainPath())
	if strings.Contains(string(data), "name: node-a") {
		t.Fatal("lost-ack removal restarted obsolete membership")
	}
	if h.count("console down") != 1 || h.count("console remove") != 1 {
		t.Fatal("removal ordering commands missing")
	}
}

func TestNamedStagingHealthFailureRetainsAcceptedFiles(t *testing.T) {
	h := newHarness(t, nil)
	h.waitFor("ready", h.ready)
	if err := os.WriteFile(filepath.Join(h.dir, "reject-health"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.src.ch <- []cluster.Endpoint{ep("node-b", "10.0.0.12")}
	h.waitFor("health rejection restored old membership", func() bool { return h.count("start") >= 3 && h.ready() })
	data, _ := os.ReadFile(h.sup.MainPath())
	if !strings.Contains(string(data), "name: node-a") || strings.Contains(string(data), "name: node-b") {
		t.Fatal("unqualified new member replaced accepted restart target")
	}
	h.sup.mu.Lock()
	message := h.sup.updateError
	h.sup.mu.Unlock()
	if message == "" {
		t.Fatal("rejected health stage was not visible")
	}
}

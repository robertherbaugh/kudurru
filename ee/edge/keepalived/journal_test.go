package keepalived

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
)

func TestConsumerBoundedJournalAndPrivateAtomicState(t *testing.T) {
	dir := t.TempDir()
	c := &Consumer{RunDir: dir, Node: "node-a", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	initial, err := ReadState(dir)
	if err != nil || initial.Session == "" || initial.Sequence != 0 {
		t.Fatal("missing initial source session")
	}
	for i := 0; i < TransitionLimit+6; i++ {
		line := `INSTANCE "VI_dns" MASTER 200`
		if i%2 != 0 {
			line = `INSTANCE "VI_dns" BACKUP 180`
		}
		if err := c.Handle(line); err != nil {
			t.Fatal(err)
		}
	}
	state, err := ReadState(dir)
	if err != nil || len(state.Transitions) != TransitionLimit || state.Sequence != TransitionLimit+6 || state.Transitions[0].Sequence != 7 || state.Session != initial.Session {
		t.Fatal("journal bounds/session/sequence lost")
	}
	if state.Transitions[0].Previous.State != StateBackup || state.Transitions[0].Next.State != StateMaster || state.Transitions[0].Next.Since.IsZero() {
		t.Fatal("journal lost original transition")
	}
	if err := c.Handle(`INSTANCE "VI_dns" BACKUP 180`); err != nil {
		t.Fatal(err)
	}
	if err := c.Handle(`INSTANCE "VI_dns" BACKUP_PRIORITY 190`); err != nil {
		t.Fatal(err)
	}
	updated, err := ReadState(dir)
	if err != nil || updated.Sequence != state.Sequence || !updated.VIPs["dns"].Since.Equal(state.VIPs["dns"].Since) {
		t.Fatal("duplicate/priority update invented a transition")
	}
	info, err := os.Stat(StatePath(dir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("state file is not private")
	}
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	reset, err := ReadState(dir)
	if err != nil || reset.Session == state.Session || reset.Sequence != 0 || len(reset.VIPs) != 0 || len(reset.Transitions) != 0 {
		t.Fatal("Consumer restart reused source evidence")
	}
	// Additive fields remain compatible with the original state-file shape.
	var legacy struct {
		Node string              `json:"node"`
		VIPs map[string]VIPState `json:"vips"`
	}
	wire, err := json.Marshal(state)
	if err != nil || json.Unmarshal(wire, &legacy) != nil || legacy.Node != state.Node || legacy.VIPs["dns"].State != state.VIPs["dns"].State {
		t.Fatal("legacy state projection changed")
	}
}

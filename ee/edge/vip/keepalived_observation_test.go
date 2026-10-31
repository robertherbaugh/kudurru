package vip

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge/keepalived"
)

func nextNotice(t *testing.T, notices <-chan KeepalivedNotice) KeepalivedNotice {
	t.Helper()
	select {
	case notice := <-notices:
		return notice
	case <-time.After(3 * time.Second):
		t.Fatal("missing bounded notification")
		return KeepalivedNotice{}
	}
}

func TestKeepalivedObservationJournalTransitionsAndRestart(t *testing.T) {
	dir := t.TempDir()
	c := &keepalived.Consumer{RunDir: dir, Node: "node-a", Log: slog.New(slog.DiscardHandler)}
	apply := func(line string) {
		t.Helper()
		if err := c.Handle(line); err != nil {
			t.Fatal(err)
		}
	}
	apply(`INSTANCE "VI_dns" BACKUP 180`)
	notices := make(chan KeepalivedNotice, 128)
	start := func() func() {
		k := NewKeepalived(dir, "node-a", []VIP{{Name: "dns", VRID: 53}})
		k.Poll = 5 * time.Millisecond
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- k.Observe(ctx, time.Minute, func(_ context.Context, n KeepalivedNotice) { notices <- n })
		}()
		return func() {
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Error("observer did not join cancellation")
			}
		}
	}
	stop := start()
	if n := nextNotice(t, notices); n.Issue != "source_ready" || n.Event != nil || n.Sequence != 1 {
		t.Fatal("startup replayed existing transition")
	}
	// Back-to-back updates between polls must both survive in the journal.
	apply(`INSTANCE "VI_dns" MASTER 200`)
	apply(`INSTANCE "VI_dns" FAULT 0`)
	for i, want := range []State{Master, FaultS} {
		n := nextNotice(t, notices)
		previous := Backup
		if i == 1 {
			previous = Master
		}
		if n.Event == nil || n.Event.State != want || n.Event.Previous != previous || n.Event.VRID != 53 || n.Event.Node != "node-a" || n.Sequence != uint64(i+2) {
			t.Fatal("journal transition attribution lost")
		}
	}
	stop()
	stop = start()
	defer stop()
	if n := nextNotice(t, notices); n.Issue != "source_ready" || n.Sequence != 3 || n.Event != nil {
		t.Fatal("observer restart duplicated old events")
	}
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	if n := nextNotice(t, notices); n.Issue != "source_restart" || n.Event != nil || n.Sequence != 0 {
		t.Fatal("producer restart manufactured transition")
	}
	apply(`INSTANCE "VI_dns" MASTER 200`)
	if n := nextNotice(t, notices); n.Event == nil || n.Event.Previous != "" || n.Event.State != Master {
		t.Fatal("new episode borrowed previous state")
	}
}

func TestKeepalivedObservationGapStaleAndForeignEvidence(t *testing.T) {
	for _, mode := range []string{"journal_gap", "source_stale", "source_node_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			c := &keepalived.Consumer{RunDir: dir, Node: "node-a", Log: slog.New(slog.DiscardHandler)}
			if err := c.Reset(); err != nil {
				t.Fatal(err)
			}
			k := NewKeepalived(dir, "node-a", []VIP{{Name: "dns", VRID: 53}})
			k.Poll = 5 * time.Millisecond
			notices := make(chan KeepalivedNotice, 128)
			resume := make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- k.Observe(ctx, time.Second, func(_ context.Context, n KeepalivedNotice) {
					notices <- n
					if n.Issue == "source_ready" {
						select {
						case <-resume:
						case <-ctx.Done():
						}
					}
				})
			}()
			defer func() {
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Error("observer cancellation failed")
				}
			}()
			if nextNotice(t, notices).Issue != "source_ready" {
				t.Fatal("baseline missing")
			}
			count := 1
			if mode == "journal_gap" {
				count = keepalived.TransitionLimit + 1
			}
			if mode == "source_stale" {
				c.Now = func() time.Time { return time.Now().Add(-time.Hour) }
			}
			if mode == "source_node_mismatch" {
				c = &keepalived.Consumer{RunDir: dir, Node: "foreign", Log: slog.New(slog.DiscardHandler)}
			}
			for i := 0; i < count; i++ {
				line := `INSTANCE "VI_dns" MASTER 200`
				if i%2 != 0 {
					line = `INSTANCE "VI_dns" BACKUP 180`
				}
				if err := c.Handle(line); err != nil {
					t.Fatal(err)
				}
			}
			close(resume)
			if n := nextNotice(t, notices); n.Issue != mode || n.Event != nil {
				t.Fatal("invalid source invented transition")
			}
		})
	}
}

func TestKeepalivedObservationFileBoundsAndLegacy(t *testing.T) {
	dir := t.TempDir()
	path := keepalived.StatePath(dir)
	if err := os.WriteFile(path, []byte(`{"node":"node-a","vips":{"dns":{"state":"MASTER"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := readObservationJournal(dir)
	if err != nil || legacy.Session != "" || legacy.VIPs["dns"].State != "MASTER" {
		t.Fatal("legacy file no longer readable")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", (1<<20)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readObservationJournal(dir); err == nil {
		t.Fatal("oversized journal accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := readObservationJournal(dir); err == nil || time.Since(started) > time.Second {
		t.Fatal("FIFO journal blocked/accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "other")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := readObservationJournal(dir); err == nil {
		t.Fatal("symlink source accepted")
	}
}

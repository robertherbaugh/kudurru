package vip

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/edge/keepalived"
)

var (
	_ Provider = None{}
	_ Provider = (*Keepalived)(nil)
	_ Provider = (*ServiceProvider)(nil)
)

var consumers = map[string]*keepalived.Consumer{}

// feed applies FIFO lines through one Consumer per directory, as the
// single long-lived `kudurru vip fifo` process does.
func feed(t *testing.T, dir string, lines ...string) {
	t.Helper()
	c, ok := consumers[dir]
	if !ok {
		c = &keepalived.Consumer{RunDir: dir, Node: "node-a", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		consumers[dir] = c
	}
	for _, l := range lines {
		if err := c.Handle(l); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKeepalivedProvider(t *testing.T) {
	dir := t.TempDir()
	k := NewKeepalived(dir, "node-a", []VIP{{Name: "dns", Address: "10.0.0.53", VRID: 53}, {Name: "dns2", Address: "10.0.0.54", VRID: 54}})
	k.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if k.Name() != "keepalived" {
		t.Fatal(k.Name())
	}
	if _, ok := k.Owner("dns"); ok {
		t.Error("owner known before any FIFO state")
	}
	feed(t, dir, `INSTANCE "VI_dns" BACKUP 200`, `INSTANCE "VI_dns" MASTER 200`, `INSTANCE "VI_dns2" BACKUP 180`)
	if o, ok := k.Owner("dns"); !ok || o != "node-a" {
		t.Errorf("Owner(dns) = %q %v", o, ok)
	}
	if _, ok := k.Owner("dns2"); ok {
		t.Error("Owner(dns2) known while BACKUP")
	}
	if _, ok := k.Owner("nope"); ok {
		t.Error("unknown VIP has an owner")
	}

	ctx := context.Background()
	if err := k.SetWeight(ctx, "dns", PreferNot); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "weight-dns")); string(b) != "60\n" { //nolint:gosec // test temp dir
		t.Errorf("track file %q", b)
	}
	// D51: lameduck never writes 300.
	lame := WithReason(ctx, ReasonLameduck)
	if err := k.SetWeight(lame, "dns", Fault); !errors.Is(err, ErrLameduckFault) {
		t.Errorf("lameduck Fault: %v", err)
	}
	if err := k.SetWeight(lame, "dns", PreferNot); err != nil {
		t.Errorf("lameduck PreferNot: %v", err)
	}
	if err := k.SetWeight(WithReason(ctx, ReasonDrain), "dns", Fault); err != nil {
		t.Fatal(err)
	}
	if err := k.SetWeight(ctx, "dns", Weight(7)); err == nil {
		t.Error("arbitrary weight accepted")
	}
	if err := k.SetWeight(ctx, "nope", Serving); !errors.Is(err, ErrUnknownVIP) {
		t.Errorf("unknown VIP: %v", err)
	}
	vs := k.VIPs()
	if vs[0].Owner != "node-a" || vs[0].Weight != Fault || vs[1].Owner != "" || vs[1].Weight != Serving {
		t.Errorf("VIPs = %+v", vs)
	}
}

func TestKeepalivedEvents(t *testing.T) {
	dir := t.TempDir()
	k := NewKeepalived(dir, "node-a", []VIP{{Name: "dns", VRID: 53}})
	k.Poll = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = k.Run(ctx) }()
	feed(t, dir, `INSTANCE "VI_dns" BACKUP 200`)
	want := []State{Backup, Master, FaultS}
	got := []State{}
	deadline := time.After(5 * time.Second)
	sent := 1
	for len(got) < len(want) {
		select {
		case ev := <-k.Events():
			if ev.VIP != "dns" || ev.Node != "node-a" {
				t.Fatalf("event %+v", ev)
			}
			got = append(got, ev.State)
			switch sent {
			case 1:
				feed(t, dir, `INSTANCE "VI_dns" MASTER 200`)
			case 2:
				feed(t, dir, `INSTANCE "VI_dns" FAULT 0`)
			}
			sent++
		case <-deadline:
			t.Fatalf("events %v, want %v", got, want)
		}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("events %v, want %v", got, want)
		}
	}
}

func TestServiceProvider(t *testing.T) {
	if _, err := NewServiceProvider("f5", nil); err == nil {
		t.Error("unknown kind accepted")
	}
	vs := []VIP{{Name: "dns", Address: "192.0.2.53"}}
	kv, _ := NewServiceProvider("kube-vip", vs)
	if a := kv.Annotations("dns", ""); a["kube-vip.io/loadbalancerIPs"] != "192.0.2.53" {
		t.Errorf("kube-vip %v", a)
	}
	mlb, _ := NewServiceProvider("metallb", vs)
	if a := mlb.Annotations("dns", "kudurru-dns"); a["metallb.io/loadBalancerIPs"] != "192.0.2.53" || a["metallb.io/allow-shared-ip"] != "kudurru-dns" {
		t.Errorf("metallb %v", a)
	}
	if mlb.Annotations("other", "") != nil {
		t.Error("annotations for an unknown VIP")
	}
	if err := mlb.SetWeight(context.Background(), "dns", Fault); err != nil || mlb.Events() != nil {
		t.Error("Service providers have no weights or events")
	}
	if _, ok := mlb.Owner("dns"); ok || mlb.Name() != "metallb" || len(mlb.VIPs()) != 1 {
		t.Error("stub surface")
	}
}

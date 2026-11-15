package main

import (
	"context"
	"log/slog"
	"net/netip"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

func TestReloadExclusionsKeepsPseudonym(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Querylog.Privacy = "anonymize_client"
	cfg.Querylog.Anonymize = "pseudonym"
	p, err := querylog.PrivacyFrom(cfg.Querylog)
	if err != nil {
		t.Fatal(err)
	}
	collector := new(querylog.Async)
	collector.SetPrivacy(p)
	addr := netip.MustParseAddr("192.0.2.10")
	original := collector.Pseudonym(addr)
	logger := slog.New(slog.DiscardHandler)
	reload := config.NewReloadable(cfg)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	subscribeReload(ctx, reload, reloadTargets{logger: logger, qlAsync: collector})
	next := *cfg
	next.Querylog.Exclude.Domains = []string{"example.test"}
	reload.Set(&next)
	if collector.Pseudonym(addr) != original {
		t.Fatal("production exclusion reload rotated identity")
	}
	full := next
	full.Querylog.Privacy = "full"
	reload.Set(&full)
	if collector.Pseudonym(addr) != "" {
		t.Fatal("production privacy change not applied")
	}
}

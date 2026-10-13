package main

import (
	"context"
	"log/slog"
	"net/netip"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/unblock"
)

func TestUnblockBookNamespace(t *testing.T) {
	for _, namespace := range []string{"default", "dns"} {
		t.Run(namespace, func(t *testing.T) {
			cfg, err := config.Load("", nil)
			if err != nil {
				t.Fatal(err)
			}
			st := memory.New()
			t.Cleanup(func() { _ = st.Close() })
			book, err := newUnblockBook(st, config.NewReloadable(cfg), namespace, "node-a", make([]byte, 32), slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			req, err := book.Submit(context.Background(), unblock.Submission{Client: netip.MustParseAddr("192.0.2.1"), Domain: "ads.example"})
			if err != nil {
				t.Fatal(err)
			}
			if req.Namespace != namespace {
				t.Fatalf("request namespace = %q, want %q", req.Namespace, namespace)
			}
		})
	}
}

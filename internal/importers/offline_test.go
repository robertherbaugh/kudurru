package importers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestOfflineYAMLAdmission(t *testing.T) {
	tests := []string{"", "[]", "a: [", "a: 1\na: 2", "a: &x [1]\nb: *x", "a: !!binary c2VjcmV0", "a: 1\n---\nb: 2", "a: 1\n---", "1: value", "a: {<<: value}", "a: " + strings.Repeat("s", 65537), strings.Repeat(" ", maxOfflineBytes+1)}
	for i, input := range tests {
		_, err := previewOffline(t.Context(), "adguard", OfflineRequest{}, strings.NewReader(input), func(context.Context, OfflineRequest, *offlineMap, *Plan) error { return nil })
		if err == nil {
			t.Fatalf("case %d accepted", i)
		}
		if strings.Contains(err.Error(), "value") {
			t.Fatal("input value leaked")
		}
	}
}
func TestOfflineWarningsBoundedAndRedacted(t *testing.T) {
	p, err := previewOffline(t.Context(), "adguard", OfflineRequest{}, strings.NewReader("password: secret-marker"), func(_ context.Context, _ OfflineRequest, _ *offlineMap, p *Plan) error {
		for range 1000 {
			p.loss("unsupported setting omitted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "secret-marker") || len(p.Warnings) != maxOfflineWarnings+1 {
		t.Fatal("unbounded or unredacted preview")
	}
}
func TestOfflineCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := previewOffline(ctx, "adguard", OfflineRequest{}, strings.NewReader("a: b"), func(context.Context, OfflineRequest, *offlineMap, *Plan) error {
		t.Fatal("converted canceled input")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestOfflineNeverBroadensLiveRouting(t *testing.T) {
	for _, source := range []string{"adguard", "blocky"} {
		_, err := Preview(t.Context(), MigrationRequest{Source: source, URL: "http://127.0.0.1", Credential: "secret-marker"})
		if err == nil || strings.Contains(err.Error(), "secret-marker") {
			t.Fatal("offline source accepted as a live credential destination")
		}
	}
}

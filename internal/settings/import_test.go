package settings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportedUpstreamsOwnershipAndRestart(t *testing.T) {
	dir := t.TempDir()
	node, routes := filepath.Join(dir, "node.yaml"), filepath.Join(dir, "routes.conf")
	write(t, node, "store: {driver: kube}\nupstreams: {importFile: routes.conf}\n")
	write(t, routes, "server=/corp.example/192.0.2.1\n")
	backend := &memoryBackend{}
	m := newManager(t, backend, node, nil)
	reviewed := revision(m)
	write(t, routes, "server=/corp.example/192.0.2.2\n")
	if err := m.Transition(t.Context(), "upstreams", "adopt", reviewed); !errors.Is(err, ErrConflict) {
		t.Fatal("changed imported source admitted with a stale revision", err)
	}
	if _, err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(t.Context(), "upstreams", "adopt", revision(m)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(backend.raw), "importFile") || strings.Contains(string(backend.raw), "routes.conf") {
		t.Fatal("host-only source entered the durable public section")
	}
	if err := os.Remove(routes); err != nil {
		t.Fatal(err)
	}
	restarted := newManager(t, backend, node, nil)
	if restarted.Reload.Current().Upstreams.Groups["dnsmasq-1"].Servers[0] != "192.0.2.2" {
		t.Fatal("restart discarded adopted canonical routing")
	}
	if err := restarted.Transition(t.Context(), "upstreams", "release", revision(restarted)); !errors.Is(err, ErrHandoff) {
		t.Fatal("release accepted missing authoritative source", err)
	}
	write(t, routes, "server=/corp.example/192.0.2.3\n")
	if err := restarted.Transition(t.Context(), "upstreams", "release", revision(restarted)); !errors.Is(err, ErrHandoff) {
		t.Fatal("release silently changed imported routes", err)
	}
	write(t, routes, "server=/corp.example/192.0.2.2\n")
	if err := restarted.Transition(t.Context(), "upstreams", "release", revision(restarted)); err != nil {
		t.Fatal(err)
	}
}

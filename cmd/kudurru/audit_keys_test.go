package main

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit/auditkeys"
)

func TestAuditKeysCLIRegisteredOfflineLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ring.json")
	run := func(args ...string) (string, error) {
		t.Helper()
		return runAuditVerifyCLI(t.Context(), t, "http://127.0.0.1:1", "", append([]string{"audit", "keys"}, args...)...)
	}
	out, err := run("init", "--file", path, "--cluster-id", "original-cluster", "--namespace", "default")
	if err != nil || !strings.Contains(out, "retained public keys: 1") {
		t.Fatal("registered offline initialization failed")
	}
	raw, err := auditkeys.ReadProtected(path)
	if err != nil {
		t.Fatal("read initialized file")
	}
	ring, err := auditkeys.Parse(raw)
	if err != nil {
		t.Fatal("parse initialized ring")
	}
	old := ring.ActiveID()
	for range 4 {
		if _, err := run("rotate", "--file", path); err != nil {
			t.Fatal("offline rotation failed")
		}
	}
	if _, err := run("revoke", "--file", path, "--key-id", old); err != nil {
		t.Fatal("offline revoke failed")
	}
	raw, err = auditkeys.ReadProtected(path)
	if err != nil {
		t.Fatal("read updated file")
	}
	ring, err = auditkeys.Parse(raw)
	if err != nil || ring.Count() != 5 {
		t.Fatal("retention/restart failed")
	}
	if _, err := run("init", "--file", path, "--cluster-id", "other", "--namespace", "default"); err == nil {
		t.Fatal("silently replaced persisted trust")
	}
	if _, err := run("revoke", "--file", path, "--key-id", ring.ActiveID()); err == nil {
		t.Fatal("revoked active signer")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = runAuditVerifyCLI(canceled, t, "http://127.0.0.1:1", "", "audit", "keys", "rotate", "--file", path)
	if err == nil {
		t.Fatal("ignored global command cancellation")
	}
}
func TestAuditKeysCLIExplicitLegacyImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ring.json")
	master := filepath.Join(dir, "master")
	if os.WriteFile(master, []byte(hex.EncodeToString(make([]byte, 32))), 0o600) != nil {
		t.Fatal("create protected fixture")
	}
	args := []string{"audit", "keys", "init", "--file", path, "--cluster-id", "cluster", "--namespace", "default", "--legacy-master-file", master}
	if _, err := runAuditVerifyCLI(t.Context(), t, "http://127.0.0.1:1", "", args...); err == nil {
		t.Fatal("legacy import without cutoff")
	}
	args = append(args, "--legacy-before", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	out, err := runAuditVerifyCLI(t.Context(), t, "http://127.0.0.1:1", "", args...)
	if err != nil || !strings.Contains(out, "retained public keys: 1") {
		t.Fatal("explicit legacy import failed")
	}
	if _, err := runAuditVerifyCLI(t.Context(), t, "http://127.0.0.1:1", "", "audit", "keys", "revoke", "--file", path, "--legacy"); err != nil {
		t.Fatal("legacy revocation command failed")
	}
}

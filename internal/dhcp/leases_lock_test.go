//go:build unix

package dhcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLeaseExclusiveOwnerAndPermissions(t *testing.T) {
	cfg := leaseTestConfig()
	s, path := leaseTestOpen(t, cfg)
	leaseTestBind(t, s, leaseTestClient(1))
	for _, suffix := range []string{"", "-wal", "-shm"} {
		st, err := os.Stat(path + suffix)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("lease file %q is not private: %v %v", suffix, st, err)
		}
	}
	if second, err := OpenLeases(path, cfg); err == nil {
		_ = second.Close()
		t.Fatal("second owner accepted")
	}
	aliasDir := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(path), aliasDir); err != nil {
		t.Fatal(err)
	}
	if second, err := OpenLeases(filepath.Join(aliasDir, filepath.Base(path)), cfg); err == nil {
		_ = second.Close()
		t.Fatal("second owner through parent alias accepted")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // #nosec G302 -- tests repair of existing insecure permissions
		t.Fatal(err)
	}
	s, err := OpenLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("existing lease file permissions not tightened: %v %v", st, err)
	}
}

func TestLeaseRefusesSymlinksHardlinksAndSpecialFiles(t *testing.T) {
	cfg := leaseTestConfig()
	s, path := leaseTestOpen(t, cfg)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "symlink.db")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if second, err := OpenLeases(alias, cfg); err == nil {
		_ = second.Close()
		t.Fatal("database symlink accepted")
	}
	hardlink := filepath.Join(filepath.Dir(path), "hardlink.db")
	if err := os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}
	if second, err := OpenLeases(hardlink, cfg); err == nil {
		_ = second.Close()
		t.Fatal("database with multiple hard links accepted")
	}
	if second, err := OpenLeases(t.TempDir(), cfg); err == nil {
		_ = second.Close()
		t.Fatal("directory database accepted")
	}
}

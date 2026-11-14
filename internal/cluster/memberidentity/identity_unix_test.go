//go:build unix

package memberidentity

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func testIdentity(t *testing.T) (*Identity, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "member.json")
	i, err := Create(path, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	return i, path
}

func TestIdentityRestartRetainsIndependentTrustAndRotatesLeaf(t *testing.T) {
	i, path := testIdentity(t)
	first, err := i.certificate()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Load(path, i.Trust().MemberID())
	if err != nil || back.Trust().Fingerprint() != i.Trust().Fingerprint() {
		t.Fatal("restart changed enrollment", err)
	}
	next, err := back.certificate()
	if err != nil || bytes.Equal(next.Certificate[0], first.Certificate[0]) {
		t.Fatal("restarted node did not issue its own fresh leaf", err)
	}
	public := back.Trust().Certificate()
	trust, err := ParseTrust(back.Trust().MemberID(), public)
	if err != nil || trust.Fingerprint() != i.Trust().Fingerprint() {
		t.Fatal("public enrollment did not round trip", err)
	}
	public[0] ^= 0xff
	if trust.Fingerprint() != i.Trust().Fingerprint() {
		t.Fatal("caller mutated retained public trust")
	}
	if _, err := Load(path, uuid.NewString()); !errors.Is(err, ErrIdentity) {
		t.Fatal("wrong local member adopted an existing key", err)
	}
	before, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(path, i.Trust().MemberID()); !errors.Is(err, ErrIdentity) {
		t.Fatal("creation replaced an existing identity", err)
	}
	after, err := readFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused creation changed original file", err)
	}
}

func TestIdentityConcurrentCreationHasOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "member.json")
	member := uuid.NewString()
	var winners atomic.Int32
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if _, err := Create(path, member); err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrIdentity) {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if _, err := Load(path, member); winners.Load() != 1 || err != nil {
		t.Fatal("exclusive creation did not preserve one durable winner", winners.Load(), err)
	}
}

func TestIdentityRefusesUnsafeFilesWithoutRepair(t *testing.T) {
	for _, kind := range []string{"missing", "empty", "directory", "symlink", "hardlink", "fifo", "permissions", "oversized", "duplicate", "unknown", "key-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			i, original := testIdentity(t)
			path := filepath.Join(t.TempDir(), "bad.json")
			raw, err := readFile(original)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "symlink":
				err = os.Symlink(original, path)
			case "hardlink":
				err = os.Link(original, path)
			case "fifo":
				err = unix.Mkfifo(path, 0o600)
			default:
				switch kind {
				case "empty":
					raw = nil
				case "oversized":
					raw = bytes.Repeat([]byte("x"), maxFileBytes+1)
				case "duplicate":
					raw = append([]byte(`{"version":1,`), raw[1:]...)
				case "unknown":
					raw = append([]byte(`{"ignored":true,`), raw[1:]...)
				case "key-mismatch":
					other, err := Create(filepath.Join(t.TempDir(), "other.json"), i.Trust().MemberID())
					if err != nil {
						t.Fatal(err)
					}
					var disk diskIdentity
					if err := json.Unmarshal(raw, &disk); err != nil {
						t.Fatal(err)
					}
					disk.Certificate = other.Trust().Certificate()
					// #nosec G117 -- intentionally malformed private key file in a test-owned temporary directory
					raw, err = json.Marshal(disk)
					if err != nil {
						t.Fatal(err)
					}
				}
				err = os.WriteFile(path, raw, 0o600) // #nosec G703 -- test-owned path
				if err == nil && kind == "permissions" {
					err = os.Chmod(path, 0o644) // #nosec G302 -- deliberately unsafe test input
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path, i.Trust().MemberID()); !errors.Is(err, ErrIdentity) {
				t.Fatal("unsafe identity accepted", err)
			}
			if kind == "missing" {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("load created a missing identity", err)
				}
			}
		})
	}
}

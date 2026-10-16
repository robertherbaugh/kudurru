//go:build unix

package dhcpsetup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	"golang.org/x/sys/unix"
)

func TestSetupRefusesAliasedAndSpecialFiles(t *testing.T) {
	for _, filename := range []string{lockFilename, settingsFilename} {
		for _, kind := range []string{"symlink", "hardlink", "directory", "fifo"} {
			t.Run(filename+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, filename)
				target := filepath.Join(t.TempDir(), "original")
				original := []byte("preserve original bytes")
				if err := os.WriteFile(target, original, 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(target, path)
				case "hardlink":
					err = os.Link(target, path)
				case "directory":
					err = os.Mkdir(path, 0o700)
				case "fifo":
					err = unix.Mkfifo(path, 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
				if m, err := Open(dir, config.DHCP{}, nil); err == nil {
					_ = m.Close()
					t.Fatal("unsafe setup file accepted")
				}
				if string(setupDiskBytes(t, target)) != string(original) {
					t.Fatal("refusing setup damaged aliased file")
				}
			})
		}
	}
}

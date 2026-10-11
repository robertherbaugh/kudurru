package keepalived

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/edge"
)

// LegacyMarkerPath is the file in runDir that `kudurru vip run --legacy`
// keeps while it renders without auth_hmac, so that `kudurru doctor` in
// the kudurru container (which shares the run volume, not
// /etc/keepalived) can warn about unauthenticated VRRP adverts.
func LegacyMarkerPath(runDir string) string { return filepath.Join(runDir, "vrrp-legacy") }

// writeLegacyMarker creates the marker in legacy mode and removes it
// otherwise.
func writeLegacyMarker(runDir string, legacy bool) error {
	p := LegacyMarkerPath(runDir)
	if !legacy {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return edge.WriteFileAtomic(p, []byte("keepalived renders without auth_hmac (kudurru vip run --legacy): VRRP adverts are unauthenticated\n"), 0o644) //nolint:gosec // a public flag that other containers read
}

// UnauthenticatedInstances returns the vrrp_instance names of a
// keepalived.conf that carry neither an auth_hmac nor an authentication
// block, and the number of instances in total.
func UnauthenticatedInstances(conf []byte) (unauth []string, total int) {
	sc := bufio.NewScanner(bytes.NewReader(conf))
	var (
		name   string
		depth  int
		authed bool
	)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexAny(line, "#!"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch {
		case depth == 0 && len(f) >= 2 && f[0] == "vrrp_instance":
			name, authed = f[1], false
			total++
		case depth == 1 && name != "" && (f[0] == "auth_hmac" || f[0] == "authentication"):
			authed = true
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth <= 0 {
			depth = 0
			if name != "" && strings.Contains(line, "}") {
				if !authed {
					unauth = append(unauth, name)
				}
				name = ""
			}
		}
	}
	return unauth, total
}

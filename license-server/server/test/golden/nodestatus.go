//go:build ignore

// nodestatus prints what a kudurru node reports for license keys at chosen
// instants, using the node's own code: internal/license.New(Options{Keyring,
// Now}), Set(key, "test") and Status(), as JSON with an extra "expires"
// field formatted the way `kudurru license status` prints it
// (cmd/kudurru/license.go printStatus: the UTC date, time.DateOnly) and
// "graceEnds" while in grace. The license server's tests feed it keys the
// server signed (server/test/node-oracle.test.ts), so the portal's keys are
// judged by the real node code, not by a TypeScript copy of it.
//
//	go run ./license-server/server/test/golden/nodestatus.go -trusted DIR < cases.json
//
// DIR holds <kid>.pub files (the format of internal/license/keys); cases.json
// is a JSON array of {"key": "<compact JWS>", "now": <Unix seconds>}. The
// output is a JSON array with one status per case, in order.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/license"
)

type input struct {
	Key string `json:"key"`
	Now int64  `json:"now"`
}

type output struct {
	license.Status
	// Expires is what `kudurru license status` prints after "expires:".
	Expires string `json:"expires,omitempty"`
	// GraceEnds is what it prints after "grace ends:" (grace only).
	GraceEnds string `json:"graceEnds,omitempty"`
	// SetError is the error of Manager.Set (a key that did not verify).
	SetError string `json:"setError,omitempty"`
}

func keyring(dir string) (license.Keyring, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	ring := license.Keyring{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		pub, err := license.ParsePublicKey(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		ring[strings.TrimSuffix(e.Name(), ".pub")] = pub
	}
	if len(ring) == 0 {
		return nil, fmt.Errorf("no .pub files in %s", dir)
	}
	return ring, nil
}

func main() {
	trusted := flag.String("trusted", "", "directory of <kid>.pub files")
	flag.Parse()
	ring, err := keyring(*trusted)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nodestatus:", err)
		os.Exit(2)
	}
	var cases []input
	if err := json.NewDecoder(os.Stdin).Decode(&cases); err != nil {
		fmt.Fprintln(os.Stderr, "nodestatus: reading cases:", err)
		os.Exit(2)
	}
	out := make([]output, 0, len(cases))
	for _, c := range cases {
		now := time.Unix(c.Now, 0)
		m, err := license.New(license.Options{Keyring: ring, Now: func() time.Time { return now }})
		if err != nil {
			fmt.Fprintln(os.Stderr, "nodestatus:", err)
			os.Exit(2)
		}
		var o output
		if err := m.Set(c.Key, "test"); err != nil {
			o.SetError = err.Error()
		}
		o.Status = m.Status()
		if o.Status.ExpiresAt != nil {
			o.Expires = o.Status.ExpiresAt.Format(time.DateOnly)
		}
		if o.Status.State == license.StateGrace && o.Status.GraceEndsAt != nil {
			o.GraceEnds = o.Status.GraceEndsAt.Format(time.DateOnly)
		}
		out = append(out, o)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "nodestatus:", err)
		os.Exit(1)
	}
}

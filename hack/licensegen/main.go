// Command licensegen is the licensor's tool for kudurru license keys
// (docs/architecture/licensing.md). It is not part of the kudurru binary.
//
//	licensegen keygen -kid kudurru-2026-1 -dir .            writes <kid>.pub and <kid>.key
//	licensegen sign -key <kid>.key -kid <kid> -licensee ... issues a key on stdout
//	licensegen inspect -pub <kid>.pub KEY                   verifies a key and prints its claims
//
// The .key file is the private signing key. Keep it in Infisical and never
// commit it; commit only <kid>.pub, into internal/license/keys/. To sign
// without the key touching disk, pass -key - and pipe it in (or set
// KUDURRU_LICENSE_SIGNING_KEY), for example:
//
//	infisical secrets get LICENSE_SIGNING_KEY --plain | licensegen sign -key - -kid ... -licensee ...
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/license"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "licensegen:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: licensegen keygen|sign|inspect [flags]")
	}
	switch args[0] {
	case "keygen":
		return keygen(args[1:])
	case "sign":
		return sign(args[1:])
	case "inspect":
		return inspect(args[1:])
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	kid := fs.String("kid", "", "key id, e.g. kudurru-2026-1")
	dir := fs.String("dir", ".", "directory for <kid>.pub and <kid>.key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *kid == "" {
		return errors.New("-kid is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, *kid+".pub"), []byte(license.EncodePublicKey(pub)+"\n"), 0o644); err != nil { //nolint:gosec // a public key
		return err
	}
	keyPath := filepath.Join(*dir, *kid+".key")
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the licensor names the directory
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s.pub (commit it to internal/license/keys/) and %s (private: keep it in Infisical, then delete this copy)\n", *kid, keyPath)
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyFile := fs.String("key", "", "private key file written by keygen; - reads it from stdin (default: $KUDURRU_LICENSE_SIGNING_KEY)")
	kid := fs.String("kid", "", "key id the public key is embedded under")
	licensee := fs.String("licensee", "", "who the license is for")
	plan := fs.String("plan", "personal", "personal, business, enterprise or trial")
	nodes := fs.Int("nodes", 3, "node limit")
	features := fs.String("features", "", "comma-separated features (oidc,edge,integrations); empty grants all")
	days := fs.Int("days", 365, "term in days")
	id := fs.String("id", "", "license id (default: random)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *kid == "" || *licensee == "" {
		return errors.New("-kid and -licensee are required")
	}
	var raw []byte
	var err error
	switch {
	case *keyFile == "-":
		raw, err = io.ReadAll(os.Stdin)
	case *keyFile != "":
		raw, err = os.ReadFile(*keyFile) //nolint:gosec // G304: the licensor names the file
	case os.Getenv("KUDURRU_LICENSE_SIGNING_KEY") != "":
		raw = []byte(os.Getenv("KUDURRU_LICENSE_SIGNING_KEY"))
	default:
		return errors.New("-key (a file, or - for stdin) or KUDURRU_LICENSE_SIGNING_KEY is required")
	}
	if err != nil {
		return err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("-key is not a licensegen private key")
	}
	if *id == "" {
		b := make([]byte, 9)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		*id = "lic_" + base64.RawURLEncoding.EncodeToString(b)
	}
	now := time.Now()
	c := license.Claims{
		ID: *id, Licensee: *licensee, Plan: license.Plan(*plan), Nodes: *nodes,
		IssuedAt: now.Unix(), ExpiresAt: now.AddDate(0, 0, *days).Unix(),
	}
	for _, f := range strings.Split(*features, ",") {
		if f = strings.TrimSpace(f); f != "" {
			c.Features = append(c.Features, license.Feature(f))
		}
	}
	key, err := license.Sign(ed25519.NewKeyFromSeed(seed), *kid, c)
	if err != nil {
		return err
	}
	fmt.Println(key)
	return nil
}

func inspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	pubFile := fs.String("pub", "", "public key file (default: the keys embedded in this build)")
	kid := fs.String("kid", "", "key id for -pub (default: its file name)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: licensegen inspect [-pub file] KEY")
	}
	ring, err := license.EmbeddedKeyring()
	if err != nil {
		return err
	}
	if *pubFile != "" {
		b, err := os.ReadFile(*pubFile) //nolint:gosec // G304: the licensor names the file
		if err != nil {
			return err
		}
		pub, err := license.ParsePublicKey(string(b))
		if err != nil {
			return err
		}
		if *kid == "" {
			*kid = strings.TrimSuffix(filepath.Base(*pubFile), ".pub")
		}
		ring = license.Keyring{*kid: pub}
	}
	c, err := license.Verify(fs.Arg(0), ring)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(c, "", "  ")
	fmt.Println(string(out))
	fmt.Fprintln(os.Stderr, "expires", c.Expires().Format(time.RFC3339))
	return nil
}

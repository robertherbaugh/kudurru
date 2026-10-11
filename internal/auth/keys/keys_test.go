package keys

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadFromEnvironment(t *testing.T) {
	k1 := bytes.Repeat([]byte{1}, 32)
	k2 := bytes.Repeat([]byte{2}, 32)
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, []byte(hex.EncodeToString(k2)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		env       map[string]string
		cur, prev []byte
		err       bool
	}{
		{"base64", map[string]string{EnvKey: base64.StdEncoding.EncodeToString(k1)}, k1, nil, false},
		{"file wins and hex works", map[string]string{EnvKey: base64.StdEncoding.EncodeToString(k1), EnvKey + "_FILE": file}, k2, nil, false},
		{"rotation window", map[string]string{EnvKey: base64.StdEncoding.EncodeToString(k1), EnvPreviousKey + "_FILE": file}, k1, k2, false},
		{"short key", map[string]string{EnvKey: base64.StdEncoding.EncodeToString(k1[:16])}, nil, nil, true},
		{"previous without current", map[string]string{EnvPreviousKey: base64.StdEncoding.EncodeToString(k1)}, nil, nil, true},
		{"missing file", map[string]string{EnvKey + "_FILE": file + ".missing"}, nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, err := Load(Options{Getenv: envOf(tc.env), Driver: "kube"})
			if tc.err {
				if err == nil {
					t.Fatal("want an error")
				}
				return
			}
			if err != nil || !bytes.Equal(k.Current, tc.cur) || !bytes.Equal(k.Previous, tc.prev) || k.Generated {
				t.Fatalf("Load = %+v, %v", k, err)
			}
		})
	}
}

func TestKubeRefusesToGenerate(t *testing.T) {
	_, err := Load(Options{Getenv: envOf(nil), Driver: "kube", DataDir: t.TempDir()})
	if !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("kube without a key: %v", err)
	}
}

func TestDockerGeneratesOnce(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	got := make([]Keys, 4)
	errs := make([]error, 4)
	for i := range got { // concurrent first starts agree on one key
		wg.Go(func() { got[i], errs[i] = Load(Options{Getenv: envOf(nil), Driver: "sqlite", DataDir: dir}) })
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || len(got[i].Current) != 32 || !got[i].Generated || !bytes.Equal(got[i].Current, got[0].Current) {
			t.Fatalf("load %d: %+v, %v", i, got[i], errs[i])
		}
	}
	path := filepath.Join(dir, Dir, FileName)
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v, %v", fi.Mode(), err)
	}
	di, _ := os.Stat(filepath.Join(dir, Dir))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("key dir mode %v", di.Mode())
	}
	again, err := Load(Options{Getenv: envOf(nil), DataDir: dir})
	if err != nil || !bytes.Equal(again.Current, got[0].Current) || again.Path != path {
		t.Fatalf("restart: %+v, %v", again, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, Dir))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

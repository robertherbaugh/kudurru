package license

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

var t0 = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

type fixture struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	ring Keyring
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{pub, priv, Keyring{"test-1": pub}}
}

func (f fixture) key(t *testing.T, mutate func(*Claims)) string {
	t.Helper()
	c := Claims{
		ID: "lic_1", Licensee: "Homelab Hank", Plan: PlanPersonal, Nodes: 3,
		IssuedAt: t0.Add(-24 * time.Hour).Unix(), ExpiresAt: t0.Add(365 * 24 * time.Hour).Unix(),
	}
	if mutate != nil {
		mutate(&c)
	}
	k, err := Sign(f.priv, "test-1", c)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (f fixture) manager(t *testing.T, now time.Time) *Manager {
	t.Helper()
	m, err := New(Options{Keyring: f.ring, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestVerifyRoundTrip(t *testing.T) {
	f := newFixture(t)
	c, err := Verify("  "+f.key(t, nil)+"\n", f.ring) // whitespace from an email or a mounted file
	if err != nil {
		t.Fatal(err)
	}
	if c.Licensee != "Homelab Hank" || c.Plan != PlanPersonal || c.Nodes != 3 || c.Issuer != Issuer {
		t.Errorf("claims = %+v", c)
	}
}

func TestVerifyRefusals(t *testing.T) {
	f := newFixture(t)
	good := f.key(t, nil)
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	forged, _ := Sign(otherPriv, "test-1", Claims{ID: "x", Licensee: "Eve", Plan: PlanEnterprise, Nodes: 99, IssuedAt: 1, ExpiresAt: 2})
	unknownKid, _ := Sign(f.priv, "other-kid", Claims{ID: "x", Licensee: "Eve", Plan: PlanEnterprise, Nodes: 99, IssuedAt: 1, ExpiresAt: 2})
	parts := strings.Split(good, ".")
	tampered := parts[0] + "." + strings.Replace(parts[1], parts[1][:4], "AAAA", 1) + "." + parts[2]
	_ = otherPub

	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"empty", "", ErrMalformed},
		{"garbage", "not-a-key", ErrMalformed},
		{"signed by a stranger", forged, ErrBadSignature},
		{"unknown kid", unknownKid, ErrUnknownKey},
		{"tampered payload", tampered, ErrBadSignature},
	}
	for _, tc := range cases {
		if _, err := Verify(tc.raw, f.ring); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestSignRejectsBadClaims(t *testing.T) {
	f := newFixture(t)
	for name, mutate := range map[string]func(*Claims){
		"no id":       func(c *Claims) { c.ID = "" },
		"no licensee": func(c *Claims) { c.Licensee = " " },
		"bad plan":    func(c *Claims) { c.Plan = "free-for-all" },
		"no nodes":    func(c *Claims) { c.Nodes = 0 },
		"exp<=iat":    func(c *Claims) { c.ExpiresAt = c.IssuedAt },
	} {
		c := Claims{ID: "x", Licensee: "a", Plan: PlanPersonal, Nodes: 1, IssuedAt: 1, ExpiresAt: 2}
		mutate(&c)
		if _, err := Sign(f.priv, "test-1", c); err == nil {
			t.Errorf("%s: Sign accepted the claims", name)
		}
	}
}

func TestCommunityWithoutKey(t *testing.T) {
	f := newFixture(t)
	for name, m := range map[string]*Manager{"manager": f.manager(t, t0), "nil": nil, "zero": {}} {
		s := m.Status()
		if s.State != StateCommunity || s.Nodes != CommunityNodes || len(s.Features) != 0 {
			t.Errorf("%s: status = %+v", name, s)
		}
		if err := m.Require(FeatureOIDC); !errors.Is(err, ErrFeature) {
			t.Errorf("%s: Require = %v", name, err)
		}
		if err := m.CheckNodes(1); err != nil {
			t.Errorf("%s: one node must be free: %v", name, err)
		}
		if err := m.CheckNodes(2); !errors.Is(err, ErrNodeLimit) {
			t.Errorf("%s: CheckNodes(2) = %v", name, err)
		}
	}
}

func TestLifecycle(t *testing.T) {
	f := newFixture(t)
	key := f.key(t, nil)
	exp := time.Unix(Claims{ExpiresAt: t0.Add(365 * 24 * time.Hour).Unix()}.ExpiresAt, 0)
	steps := []struct {
		name  string
		now   time.Time
		state State
		nodes int
		feat  bool
	}{
		{"active", t0, StateActive, 3, true},
		{"last second", exp.Add(-time.Second), StateActive, 3, true},
		{"just expired: grace", exp, StateGrace, 3, true},
		{"end of grace", exp.Add(Grace - time.Second), StateGrace, 3, true},
		{"after grace", exp.Add(Grace), StateExpired, 1, false},
	}
	for _, st := range steps {
		m := f.manager(t, st.now)
		if err := m.Set(key, "test"); err != nil {
			t.Fatal(err)
		}
		s := m.Status()
		if s.State != st.state || s.Nodes != st.nodes || m.Allows(FeatureEdge) != st.feat {
			t.Errorf("%s: state=%s nodes=%d edge=%v", st.name, s.State, s.Nodes, m.Allows(FeatureEdge))
		}
	}
}

func TestNotYetValid(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, t0)
	if err := m.Set(f.key(t, func(c *Claims) { c.NotBefore = t0.Add(time.Hour).Unix() }), "test"); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.State != StateInvalid || s.Nodes != 1 || !strings.Contains(s.Error, "not valid before") {
		t.Errorf("status = %+v", s)
	}
}

func TestFeatureList(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, t0)
	if err := m.Set(f.key(t, func(c *Claims) { c.Features = []Feature{FeatureOIDC, "from-the-future"} }), "test"); err != nil {
		t.Fatal(err)
	}
	if !m.Allows(FeatureOIDC) || m.Allows(FeatureEdge) {
		t.Errorf("features = %v", m.Status().Features)
	}
	// An empty list grants every feature this build knows.
	if err := m.Set(f.key(t, nil), "test"); err != nil {
		t.Fatal(err)
	}
	for _, ft := range Features() {
		if !m.Allows(ft) {
			t.Errorf("empty feature list does not grant %s", ft)
		}
	}
}

func TestInvalidKeyDropsThePreviousOne(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, t0)
	if err := m.Set(f.key(t, nil), "file"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("typo", "file"); err == nil {
		t.Fatal("a bad key was accepted")
	}
	if s := m.Status(); s.State != StateInvalid || s.Nodes != 1 || m.Allows(FeatureOIDC) {
		t.Errorf("status after a bad key = %+v", s)
	}
	if err := m.Set("", ""); err != nil || m.Status().State != StateCommunity {
		t.Errorf("clearing the key: %v, %s", err, m.Status().State)
	}
}

func TestLoadOrderAndFiles(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "license.key")
	m := f.manager(t, t0)

	// No source holds a key: community. A missing file is no key.
	if err := m.Load([]Source{Literal("flag", ""), File("file", path)}); err != nil || m.Status().State != StateCommunity {
		t.Fatalf("empty sources: %v, %s", err, m.Status().State)
	}
	// The file wins when nothing is earlier.
	if err := os.WriteFile(path, []byte(f.key(t, nil)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Load([]Source{Literal("flag", ""), File("file", path)}); err != nil || m.Status().Source != "file" || m.Status().State != StateActive {
		t.Fatalf("file source: %v, %+v", err, m.Status())
	}
	// An earlier source wins, and a bad one does not fall through.
	if err := m.Load([]Source{Literal("flag", "typo"), File("file", path)}); err == nil || m.Status().State != StateInvalid {
		t.Fatalf("bad flag: %v, %s", err, m.Status().State)
	}
	// An unreadable source is an error, not "no key".
	if err := m.Load([]Source{File("dir", dir)}); err == nil || m.Status().State != StateInvalid {
		t.Fatalf("unreadable source: %v, %s", err, m.Status().State)
	}
}

func TestEmbeddedKeyring(t *testing.T) {
	ring, err := EmbeddedKeyring()
	if err != nil {
		t.Fatal(err)
	}
	if len(ring) == 0 {
		t.Fatal("no embedded keys")
	}
	for kid, pub := range ring {
		if len(pub) != ed25519.PublicKeySize || kid == "" {
			t.Errorf("key %q: %d bytes", kid, len(pub))
		}
	}
	if _, err := New(Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestUnrestricted(t *testing.T) {
	m := Unrestricted()
	if err := m.CheckNodes(1000); err != nil {
		t.Error(err)
	}
	for _, f := range Features() {
		if err := m.Require(f); err != nil {
			t.Error(err)
		}
	}
}

func TestCollector(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, t0)
	if err := m.Set(f.key(t, nil), "test"); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(NewCollector(m)); n != 3 {
		t.Errorf("metrics = %d, want 3", n)
	}
}

func TestRunLogsAndReloads(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(t.TempDir(), "license.key")
	m := f.manager(t, t0)
	ctx, cancel := contextWithCancel(t)
	done := make(chan error, 1)
	go func() {
		done <- m.Run(ctx, []Source{File("file", path)}, 10*time.Millisecond, slog.New(slog.DiscardHandler))
	}()
	if err := os.WriteFile(path, []byte(f.key(t, nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Status().State != StateActive {
		if time.Now().After(deadline) {
			t.Fatalf("Run never picked up the key file: %+v", m.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}

// A release build trusts the licensor's keys and nothing else: the throwaway
// key of the dashboard suite exists only under -tags licensetest.
func TestDefaultBuildTrustsNoTestKey(t *testing.T) {
	if len(testKeys) != 0 {
		t.Skip("built with -tags licensetest")
	}
	ring, err := EmbeddedKeyring()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ring["e2e-test"]; ok {
		t.Fatal("the e2e test key is trusted by a default build")
	}
	raw, err := os.ReadFile("testdata/e2e-license.key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(string(raw), ring); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("the e2e key verifies in a default build: %v", err)
	}
}

package auditkeys

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
)

func newTestRing(t *testing.T) *Ring {
	t.Helper()
	r, err := New("cluster-original", "default", nil, nil, time.Now())
	if err != nil {
		t.Fatal("initialize ring")
	}
	return r
}
func TestIndependentRingRetainsPublicHistory(t *testing.T) {
	ring := newTestRing(t)
	original := ring.ActiveID()
	specs := []auditv1.AuditLogSpec{}
	for i := 0; i < 5; i++ {
		spec := auditv1.AuditLogSpec{Node: "node", Source: "source", Count: i + 1}
		var err error
		spec.Seal, err = ring.Sign(spec)
		if err != nil {
			t.Fatal("sign")
		}
		specs = append(specs, spec)
		ring, err = ring.Rotate()
		if err != nil {
			t.Fatal("rotate")
		}
		data, _ := ring.EncodePrivate()
		ring, err = Parse(data) // restart from protected persisted bytes
		if err != nil {
			t.Fatal("restart")
		}
	}
	if ring.Count() != 6 {
		t.Fatal("lost retained public key")
	}
	for _, spec := range specs {
		if err := ring.VerificationOnly().Verify(spec, time.Time{}); err != nil {
			t.Fatal("retained history did not verify")
		}
	}
	if _, err := ring.VerificationOnly().Sign(specs[0]); err == nil {
		t.Fatal("verification cut kept private seed")
	}
	revoked, err := ring.Revoke(original)
	if err != nil || !errors.Is(revoked.Verify(specs[0], time.Time{}), ErrUnavailable) {
		t.Fatal("revoked history trusted")
	}
	if err := revoked.Verify(specs[4], time.Time{}); err != nil {
		t.Fatal("unrelated history lost trust")
	}
	if _, err := ring.Revoke(ring.ActiveID()); err == nil {
		t.Fatal("revoked active signer")
	}
	if !ring.ScopeMatches("cluster-original", "default") || ring.ScopeMatches("other", "default") || ring.ScopeMatches("cluster-original", "other") {
		t.Fatal("scope binding")
	}
	tampered := specs[0]
	tampered.Count++
	if !errors.Is(ring.Verify(tampered, time.Time{}), ErrIntegrity) {
		t.Fatal("changed spec accepted")
	}
	parts := strings.Split(specs[0].Seal, ":")
	for _, field := range []int{0, 1, 2, 3} {
		changed := append([]string(nil), parts...)
		changed[field] = "unknown"
		bad := specs[0]
		bad.Seal = strings.Join(changed, ":")
		if ring.Verify(bad, time.Time{}) == nil {
			t.Fatal("changed signature field accepted")
		}
	}
	data, _ := ring.EncodePrivate()
	var doc document
	if json.Unmarshal(data, &doc) != nil {
		t.Fatal("decode private fixture")
	}
	next, _ := ring.Rotate()
	nextData, _ := next.EncodePrivate()
	if bytes.Contains(nextData, []byte(base64.StdEncoding.EncodeToString(doc.Seed))) {
		t.Fatal("obsolete private seed retained")
	}
	// Actual codec is base64: ensure only the new active seed exists in the DTO.
	if len(next.doc.Seed) != 32 || bytes.Equal(doc.Seed, next.doc.Seed) {
		t.Fatal("signer did not change")
	}
	encoded, _ := json.Marshal(ring)
	if string(encoded) != `{"private":true}` || fmt.Sprint(ring) != "private audit key ring" || fmt.Sprintf("%#v", ring) != "private audit key ring" || fmt.Sprintf("%#v", *ring) != "private audit key ring" {
		t.Fatal("ring formatting is not redacted")
	}
}
func TestRingStrictCodecAndCapacity(t *testing.T) {
	ring := newTestRing(t)
	raw, _ := ring.EncodePrivate()
	cases := [][]byte{nil, bytes.Repeat([]byte(" "), MaxBytes+1), append(bytes.Clone(raw), []byte("{}")...), []byte(`{"version":1,"version":1}`), []byte(`{"version":1,"Version":2}`), []byte(`{"version":1,"vers\u0069on":1}`)}
	for _, mutate := range []func(*document){
		func(d *document) { d.Version = 2 }, func(d *document) { d.Seed = nil }, func(d *document) { d.Keys[0].ID = "bad" }, func(d *document) { d.Keys = append(d.Keys, d.Keys[0]) }, func(d *document) { d.Keys[0].Revoked = true }, func(d *document) { d.Namespace = "INVALID" }, func(d *document) { d.Legacy = [][]byte{{1}} },
	} {
		var d document
		_ = json.Unmarshal(raw, &d)
		mutate(&d)
		data, _ := json.Marshal(d)
		cases = append(cases, data)
	}
	cases = append(cases, bytes.Replace(raw, []byte(`"version":1`), []byte(`"unsupported":1,"version":1`), 1))
	for i, data := range cases {
		if _, err := Parse(data); err == nil {
			t.Fatalf("invalid codec case %d accepted", i)
		}
	}
	for ring.Count() < MaxKeys {
		var err error
		ring, err = ring.Rotate()
		if err != nil {
			t.Fatal("bounded rotation failed early")
		}
	}
	if _, err := ring.Rotate(); err == nil {
		t.Fatal("silently pruned full ring")
	}
}
func TestLegacyExplicitCutoffAndNoDowngrade(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	cutoff := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	ring, err := New("cluster-original", "default", [][]byte{key}, &cutoff, time.Now())
	if err != nil {
		t.Fatal("import")
	}
	spec := auditv1.AuditLogSpec{Node: "node", Count: 1}
	body, _ := json.Marshal(spec)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	spec.Seal = hex.EncodeToString(mac.Sum(nil))
	if ring.Verify(spec, cutoff.Add(-time.Second)) != nil {
		t.Fatal("original HMAC bytes not accepted")
	}
	for _, created := range []time.Time{{}, cutoff, cutoff.Add(time.Second)} {
		if !errors.Is(ring.Verify(spec, created), ErrUnavailable) {
			t.Fatal("legacy cutoff bypass")
		}
	}
	for range 3 {
		ring, err = ring.Rotate()
		if err != nil {
			t.Fatal("rotate imported ring")
		}
	}
	raw, _ := ring.EncodePrivate()
	ring, err = Parse(raw)
	if err != nil || ring.Verify(spec, cutoff.Add(-time.Second)) != nil {
		t.Fatal("restart extended/lost original legacy trust")
	}
	noImport := newTestRing(t)
	if !errors.Is(noImport.Verify(spec, cutoff.Add(-time.Second)), ErrUnavailable) {
		t.Fatal("implicit legacy fallback")
	}
	revoked, _ := ring.RevokeLegacy()
	if !errors.Is(revoked.Verify(spec, cutoff.Add(-time.Second)), ErrUnavailable) {
		t.Fatal("legacy revocation ignored")
	}
	// A new-format failure may not select the legacy verifier.
	spec.Seal = "ed25519-v1:bad:bad:" + spec.Seal
	if ring.Verify(spec, cutoff.Add(-time.Second)) == nil {
		t.Fatal("algorithm downgrade")
	}
	fractional := cutoff.Add(time.Nanosecond)
	if _, err := New("cluster-original", "default", [][]byte{key}, &fractional, time.Now()); err == nil {
		t.Fatal("fractional cutoff permits same-second downgrade")
	}
	future := time.Now().Add(time.Hour)
	if _, err := New("cluster-original", "default", [][]byte{key}, &future, time.Now()); err == nil {
		t.Fatal("future legacy admission")
	}
}
func TestProtectedFileLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ring.json")
	init := func(*Ring) (*Ring, error) { return New("cluster-original", "default", nil, nil, time.Now()) }
	if count, err := UpdateFile(t.Context(), path, true, init); err != nil || count != 1 {
		t.Fatal("initialize file")
	}
	if _, err := UpdateFile(t.Context(), path, true, init); err == nil {
		t.Fatal("reinitialized existing trust")
	}
	for range 3 {
		if _, err := UpdateFile(t.Context(), path, false, (*Ring).Rotate); err != nil {
			t.Fatal("rotate persisted file")
		}
	}
	raw, err := ReadProtected(path)
	if err != nil {
		t.Fatal("protected read")
	}
	ring, err := Parse(raw)
	if err != nil || ring.Count() != 4 {
		t.Fatal("lost persisted public keys")
	}
	// #nosec G302 -- intentionally exposed fixture verifies refusal before reading private files
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal("chmod fixture")
	}
	if _, err := ReadProtected(path); err == nil {
		t.Fatal("accepted exposed file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal("chmod fixture")
	}
	link := filepath.Join(t.TempDir(), "link")
	if os.Symlink(path, link) != nil {
		t.Fatal("symlink fixture")
	}
	if _, err := ReadProtected(link); err == nil {
		t.Fatal("followed private symlink")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := UpdateFile(ctx, path, false, (*Ring).Rotate); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored canceled operator command")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := UpdateFile(t.Context(), path, false, func(r *Ring) (*Ring, error) { close(entered); <-release; return r.Rotate() })
		result <- err
	}()
	<-entered
	_, err = UpdateFile(t.Context(), path, false, (*Ring).Rotate)
	close(release)
	if err == nil {
		t.Fatal("concurrent update did not refuse")
	}
	if <-result != nil {
		t.Fatal("original operator update failed")
	}
}

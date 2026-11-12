package replication

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// uncachedDigest is Digest without the memo, as it was computed before D378.
func uncachedDigest(t *testing.T, i Image) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Manifest              []Kind      `json:"manifest"`
		Payload               Payload     `json:"payload"`
		ObjectResourceVersion uint64      `json:"objectResourceVersion,string"`
		Parent                *Generation `json:"parent"`
	}{i.KindManifest, i.Payload, i.ObjectResourceVersion, i.Parent})
	if err != nil {
		t.Fatal(err)
	}
	b, err := canonical(raw, 64)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A remembered digest or object identity is exactly what the uncached
// function returns for the same bytes; any changed byte is computed again,
// and a rejected input is rejected every time.
func TestMemoizedDigestAndIdentityMatchUncached(t *testing.T) {
	i := sample(t)
	want := uncachedDigest(t, i)
	for range 3 {
		got, err := Digest(i)
		if err != nil || got != want {
			t.Fatalf("digest %q, %v; want %q", got, err, want)
		}
	}
	changed := sample(t)
	changed.Payload.Security[0].Data = json.RawMessage(`{"ciphertext":"opaque-encrypted-by-owner!"}`)
	if got, err := Digest(changed); err != nil || got == want || got != uncachedDigest(t, changed) {
		t.Fatalf("a changed payload did not get its own digest: %q, %v", got, err)
	}
	// The sealed image still validates, and a tampered copy of it is still
	// refused after its original digest was remembered.
	if _, err := Validate(i, validation(i)); err != nil {
		t.Fatal(err)
	}
	tampered := i
	tampered.Payload.Security = []Entry{{Key: "sealed", Data: json.RawMessage(`{"ciphertext":"different"}`)}}
	if _, err := Validate(tampered, validation(i)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered image after a remembered digest: %v", err)
	}

	object := json.RawMessage(`{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSZone","metadata":{"namespace":"ns","name":"memo","resourceVersion":"3"}}`)
	for range 2 {
		key, h, err := objectKey(object)
		if err != nil || key != "DNSZone\x00ns\x00memo" || h.APIVersion != "kudurru.io/v1alpha1" || h.Metadata.ResourceVersion != "3" {
			t.Fatalf("object identity %q %+v %v", key, h, err)
		}
	}
	for range 2 {
		if _, _, err := objectKey(json.RawMessage(`{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSZone","metadata":{}}`)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("an object without a name was accepted: %v", err)
		}
	}
}

// A remembered canonical image is the uncached canonical encoding of the
// same bytes under the same depth bound; another bound is checked again.
func TestMemoizedCanonicalImage(t *testing.T) {
	i := sample(t)
	b, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	want, err := canonical(b, 64)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, err := canonicalImage(b, 64)
		if err != nil || string(got) != string(want) {
			t.Fatalf("canonical image differs: %v", err)
		}
	}
	if _, err := canonicalImage(b, 2); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a remembered image passed a smaller depth bound: %v", err)
	}
	for range 2 {
		if _, err := Decode(bytes.NewReader(append(b[:len(b):len(b)], '{')), validation(i)); err == nil {
			t.Fatal("trailing JSON was accepted")
		}
	}
}

// The memo never holds more than its bound.
func TestMemoBound(t *testing.T) {
	c := newMemo[int](4, 1<<30, func(int) int { return 1 })
	for n := range 10 {
		c.put(sha256.Sum256(fmt.Appendf(nil, "%d", n)), n)
		if c.len() > 4 {
			t.Fatalf("memo holds %d entries, bound 4", c.len())
		}
	}
	if v, ok := c.get(sha256.Sum256([]byte("9"))); !ok || v != 9 {
		t.Fatal("the last entry is missing", v, ok)
	}
}

// Concurrent readers of one generation share the memo safely.
func TestMemoizedDigestConcurrent(t *testing.T) {
	i := sample(t)
	want := uncachedDigest(t, i)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				if got, err := Digest(i); err != nil || got != want {
					t.Error("concurrent digest", got, err)
					return
				}
				if _, err := Validate(i, validation(i)); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// The depth bound is part of the canonical image key in a form that no input
// bytes can imitate: a remembered image under one bound is never returned for
// other bytes under another bound.
func TestMemoizedCanonicalImageDepthIsSeparate(t *testing.T) {
	if _, err := canonicalImage([]byte(`{"a":1}`), 64); err != nil {
		t.Fatal(err)
	}
	want, wantErr := canonical([]byte(`4{"a":1}`), 6)
	got, err := canonicalImage([]byte(`4{"a":1}`), 6)
	if wantErr == nil || err == nil {
		t.Fatalf("depth 6 with a leading digit: memoized %q, %v; uncached %q, %v", got, err, want, wantErr)
	}
}

// An object whose identity strings are large is not remembered, so the
// identity memo stays within its byte budget whatever its input, including
// objects of images that are rejected later.
func TestIdentityMemoSkipsLargeObjects(t *testing.T) {
	name := bytes.Repeat([]byte("n"), 1<<20)
	for n := range 4 {
		raw := json.RawMessage(fmt.Sprintf(`{"apiVersion":"x/v1","kind":"Unknown%d","metadata":{"name":%q}}`, n, name))
		if _, _, err := objectKey(raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := identities.get(sha256.Sum256(raw)); ok {
			t.Fatalf("an object identity of %d bytes was remembered", len(raw))
		}
	}
	if b := identities.size(); b > identities.maxBytes {
		t.Fatalf("identity memo holds %d bytes, budget %d", b, identities.maxBytes)
	}
}

// A store with more objects than the memo holds still gets hits on repeated
// passes in the same order, and the identity memo holds a store of the
// default maximum object count.
func TestMemoHitsBeyondBound(t *testing.T) {
	c := newMemo[int](1000, 1<<30, func(int) int { return 1 })
	keys := make([][sha256.Size]byte, 1300)
	for n := range keys {
		keys[n] = sha256.Sum256(fmt.Appendf(nil, "%d", n))
	}
	for pass := range 3 {
		hits := 0
		for n, k := range keys {
			if _, ok := c.get(k); ok {
				hits++
				continue
			}
			c.put(k, n)
		}
		t.Logf("pass %d: %d hits of %d", pass, hits, len(keys))
		if pass > 0 && hits < len(keys)/4 {
			t.Fatalf("pass %d: %d hits of %d with a bound of 1000", pass, hits, len(keys))
		}
		if c.len() > 1000 {
			t.Fatalf("memo holds %d entries, bound 1000", c.len())
		}
	}
	if identities.bound < 100_000 {
		t.Fatalf("identity memo holds %d entries, fewer than the default 100,000 objects of an image", identities.bound)
	}
}

// The byte budget bounds the memo even when every entry is under the count.
func TestMemoByteBudget(t *testing.T) {
	c := newMemo[int](1000, 100, func(v int) int { return v })
	for n := range 50 {
		c.put(sha256.Sum256(fmt.Appendf(nil, "%d", n)), 30)
		if c.size() > 100 {
			t.Fatalf("memo holds %d bytes, budget 100", c.size())
		}
	}
	c.put(sha256.Sum256([]byte("large")), 101)
	if _, ok := c.get(sha256.Sum256([]byte("large"))); ok {
		t.Fatal("an entry larger than the budget was remembered")
	}
}

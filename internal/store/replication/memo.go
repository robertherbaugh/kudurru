package replication

import (
	"crypto/sha256"
	"sync"
)

// memo is a small process-wide cache of a pure function of exact input bytes,
// keyed by the SHA-256 of those bytes (D378). Every reader of one generation
// (the publisher's candidate, the provider coordinator, the primary driver,
// the TSIG and integration runtimes and each command's own commit) seals and
// validates the same complete image, and an ordinary commit changes only a few
// of its objects. Under the race detector, recomputing the canonical encoding
// and every object's identity for each of them made one publication of 120
// record sets take seconds. A hit returns exactly what the function returned
// for the same bytes; errors are never stored, so a rejected input is checked
// again every time. The cache is bounded by entry count and is cleared, not
// evicted piecemeal, when full.
type memo[V any] struct {
	mu    sync.Mutex
	bound int
	m     map[[sha256.Size]byte]V
}

func newMemo[V any](bound int) *memo[V] {
	return &memo[V]{bound: bound, m: make(map[[sha256.Size]byte]V)}
}

func (c *memo[V]) get(key [sha256.Size]byte) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *memo[V]) put(key [sha256.Size]byte, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.bound {
		clear(c.m)
	}
	c.m[key] = v
}

func (c *memo[V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

type objectIdentity struct {
	key    string
	header objectHeader
}

var (
	// digests maps the exact encoding Digest hashes (before canonicalization)
	// to its payload digest: 64 hex characters per entry.
	digests = newMemo[string](64)
	// identities maps one object document to its key and header (a few
	// hundred bytes per entry, a few megabytes when full). A store with more
	// objects than the bound still gets correct results, only fewer hits.
	identities = newMemo[objectIdentity](1 << 15)
	// canonicals maps one encoded complete image (with its depth bound) to
	// its canonical encoding. Only two images of at most maxRememberedImage
	// bytes are kept, so it holds at most 16 MiB; a larger image is
	// canonicalized every time.
	canonicals = newMemo[[]byte](2)
)

const maxRememberedImage = 8 << 20

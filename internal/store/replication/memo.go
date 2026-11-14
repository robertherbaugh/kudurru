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
// record sets take seconds. A hit returns what the function returned for the
// same input bytes and the same parameters; errors are never stored, so an
// input the function rejects is checked again every time. The cache is
// bounded by entry count and by the bytes its values hold, as the size
// function counts them; a value larger than the byte budget is not stored.
// When a new value would exceed either bound, entries chosen at random are
// evicted until it fits, so a working set larger than the bound still gets a
// share of hits on repeated passes instead of none.
type memo[V any] struct {
	mu       sync.Mutex
	bound    int
	maxBytes int
	sizeOf   func(V) int
	bytes    int
	m        map[[sha256.Size]byte]V
}

func newMemo[V any](bound, maxBytes int, sizeOf func(V) int) *memo[V] {
	return &memo[V]{bound: bound, maxBytes: maxBytes, sizeOf: sizeOf, m: make(map[[sha256.Size]byte]V)}
}

func (c *memo[V]) get(key [sha256.Size]byte) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *memo[V]) put(key [sha256.Size]byte, v V) {
	n := c.sizeOf(v)
	if n > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.m[key]; ok {
		c.bytes -= c.sizeOf(old)
		delete(c.m, key)
	}
	// Map iteration starts at a random position, so this evicts arbitrary
	// entries.
	for k, old := range c.m {
		if len(c.m) < c.bound && c.bytes+n <= c.maxBytes {
			break
		}
		c.bytes -= c.sizeOf(old)
		delete(c.m, k)
	}
	c.m[key] = v
	c.bytes += n
}

func (c *memo[V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func (c *memo[V]) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

type objectIdentity struct {
	key    string
	header objectHeader
}

// entryOverhead approximates the map slot, key and string headers of one
// entry beyond the bytes of its strings.
const entryOverhead = 128

// maxRememberedIdentity is the largest object identity, in bytes of its
// strings, that is remembered. Ordinary identities are a few hundred bytes;
// a larger one is decoded every time.
const maxRememberedIdentity = 4 << 10

func (id objectIdentity) size() int {
	h := id.header
	return entryOverhead + len(id.key) + len(h.APIVersion) + len(h.Kind) + len(h.Metadata.Namespace) + len(h.Metadata.Name) + len(h.Metadata.ResourceVersion)
}

var (
	// digests maps the exact encoding Digest hashes (before canonicalization)
	// to its payload digest: 64 hex characters per entry.
	digests = newMemo(64, 64*(entryOverhead+64), func(d string) int { return entryOverhead + len(d) })
	// identities maps one object document to its key and header. It holds
	// the default maximum of 100,000 objects of an image with room to spare
	// and at most 32 MiB; an identity above maxRememberedIdentity is never
	// stored. Identities are remembered when an object is decoded, before the
	// image it came from is validated, so objects of a rejected image may be
	// remembered too; the bounds hold for them as well.
	identities = newMemo(1<<17, 32<<20, objectIdentity.size)
	// canonicals maps one encoded complete image (with its depth bound) to
	// its canonical encoding. Only two images of at most maxRememberedImage
	// bytes, input and output, are kept, so it holds at most 16 MiB; a larger
	// image is canonicalized every time and its key is not computed.
	canonicals = newMemo(2, 2*maxRememberedImage, func(b []byte) int { return len(b) })
)

const maxRememberedImage = 8 << 20

package servingauth

import (
	"crypto/sha256"
	"strconv"
	"sync"
)

// memo remembers a pure function of exact input bytes, keyed by the SHA-256
// of those bytes, as replication's memo does for whole-image work (D378,
// D422). Every reader of one generation (the publisher's candidate, its
// primary, integration and provider captures, provider reconciliation, the
// primary driver and each command) validates and decodes the same security
// rows, and a commit changes only a few of them. Only successes are stored,
// so a row the function rejects is checked again every time. The cache is
// bounded by entry count and by the bytes its values hold; when a new value
// would exceed either bound, arbitrary entries are evicted until it fits.
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

// rowKey is the memo key of one owner row read as kind at a root format
// version. Every part is length-prefixed, so no two inputs share a key.
func rowKey(kind string, version int, key string, data []byte) [sha256.Size]byte {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(kind), []byte(strconv.Itoa(version)), []byte(key), data} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte{':'})
		h.Write(part)
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}

// rowOverhead approximates the map slot and value headers of one entry.
const rowOverhead = 128

var (
	// validRows holds the security rows ValidateEntry accepted: two complete
	// sections of MaxRows rows.
	validRows = newMemo(2*MaxRows, 2*MaxRows*rowOverhead, func(struct{}) int { return rowOverhead })
	// floorRows holds the revocation floors decodeSecurity accepted, with
	// their decoded values: two complete sections of MaxRows floors of at
	// most MaxRowBytes, though floors are a few hundred bytes.
	floorRows = newMemo(2*MaxRows, 2*MaxRows*(rowOverhead+1024), floorSize)
)

func floorSize(v any) int {
	switch f := v.(type) {
	case GrantFloor:
		return rowOverhead + len(f.Grant)
	case AccountFloor:
		return rowOverhead + len(f.Account.Kind) + len(f.Account.Namespace) + len(f.Account.Name) + len(f.Account.UID)
	case ProviderFloor:
		return rowOverhead + len(f.Provider) + len(f.UID)
	}
	return rowOverhead
}

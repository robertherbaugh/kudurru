package matcher

import "hash/maphash"

// seed is shared by every matcher in the process so one hash of a query name
// can be reused across the exact and suffix filters of every list.
var seed = maphash.MakeSeed()

// Hash returns the process-wide maphash of s. Pass it to the *Hash lookup
// variants to avoid hashing the same name once per matcher.
func Hash(s string) uint64 {
	return maphash.String(seed, s)
}

// hashBytes is Hash for a byte slice; maphash guarantees that both agree for
// equal contents.
func hashBytes(b []byte) uint64 {
	return maphash.Bytes(seed, b)
}

const (
	// filterBitsPerKey sizes the filter. With filterProbes bits set per key
	// inside one 64-bit word this gives a false-positive rate of about 1 %
	// (asserted in TestFilterFalsePositiveRate).
	filterBitsPerKey = 12
	filterProbes     = 4
)

// filter is a blocked Bloom filter: each key selects one 64-bit word from the
// high half of its hash and sets filterProbes bits inside that word from the
// low bits, so a probe costs exactly one memory access. A zero filter
// contains nothing.
type filter struct {
	words []uint64
}

func newFilter(keys int) filter {
	if keys <= 0 {
		return filter{}
	}
	n := (keys*filterBitsPerKey + 63) / 64
	return filter{words: make([]uint64, n)}
}

// word maps the high 32 bits of h onto [0, len(words)) without a division
// (multiply-shift range reduction).
func (f *filter) word(h uint64) int {
	return int((h >> 32) * uint64(len(f.words)) >> 32) //nolint:gosec // result < len(words) by construction
}

func filterMask(h uint64) uint64 {
	var m uint64
	for i := range filterProbes {
		m |= 1 << ((h >> (6 * i)) & 63)
	}
	return m
}

func (f *filter) add(h uint64) {
	f.words[f.word(h)] |= filterMask(h)
}

// mayContain reports whether h may have been added. False means definitely
// not; true is wrong with a probability of about 1 %.
func (f *filter) mayContain(h uint64) bool {
	if len(f.words) == 0 {
		return false
	}
	m := filterMask(h)
	return f.words[f.word(h)]&m == m
}

func (f *filter) bytes() int {
	return 8 * len(f.words)
}

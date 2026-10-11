package matcher

import (
	"bytes"
	"cmp"
	"math"
	"slices"
	"strings"
)

// bucket is a sorted set of fixed-width records stored as one concatenated
// string, so a set of n records costs n*width bytes plus one string header
// and is searched with a binary search that never allocates.
type bucket struct {
	data string
	// tags holds one tag per record, or is empty when every record carries
	// tag (the common case: a list written in one syntax).
	tags string
	tag  uint8
}

// find reports the index of key in b; every record in b has width len(key).
func (b *bucket) find(key string) (int, bool) {
	w := len(key)
	if w == 0 || len(b.data) == 0 {
		return 0, false
	}
	n := len(b.data) / w
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if b.data[mid*w:mid*w+w] < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < n && b.data[lo*w:lo*w+w] == key {
		return lo, true
	}
	return 0, false
}

func (b *bucket) tagAt(i int) uint8 {
	if b.tags == "" {
		return b.tag
	}
	return b.tags[i]
}

func (b *bucket) bytes() int {
	return len(b.data) + len(b.tags)
}

// arena collects the unsorted records of one width while a builder runs.
// Records are appended as raw bytes rather than as strings so a list of a
// million entries costs one growing buffer instead of a million headers.
type arena struct {
	data []byte
	tags []byte
}

func (a *arena) add(rec string, tag uint8) {
	a.data = append(a.data, rec...)
	a.tags = append(a.tags, tag)
}

// build sorts and de-duplicates the arena into a bucket. When a record was
// added more than once the tag of the first addition wins, which keeps the
// result independent of sort stability. It returns the bucket and the number
// of unique records, and releases the arena.
func (a *arena) build(w int) (bucket, int) {
	n := len(a.tags)
	if n == 0 || w == 0 {
		return bucket{}, 0
	}
	if n > math.MaxUint32 {
		panic("matcher: more than 2^32 records of one width")
	}
	idx := make([]uint32, n)
	for i := range idx {
		idx[i] = uint32(i) //nolint:gosec // n <= MaxUint32 checked above
	}
	data := a.data
	slices.SortFunc(idx, func(x, y uint32) int {
		ox, oy := int(x)*w, int(y)*w
		if c := bytes.Compare(data[ox:ox+w], data[oy:oy+w]); c != 0 {
			return c
		}
		return cmp.Compare(x, y)
	})

	var sb strings.Builder
	sb.Grow(n * w)
	tags := make([]byte, 0, n)
	var prev []byte
	for k, i := range idx {
		o := int(i) * w
		rec := data[o : o+w]
		if k > 0 && bytes.Equal(rec, prev) {
			continue
		}
		sb.Write(rec)
		tags = append(tags, a.tags[i])
		prev = rec
	}
	a.data, a.tags = nil, nil

	b := bucket{data: sb.String(), tag: tags[0]}
	for _, t := range tags[1:] {
		if t != b.tag {
			b.tags = string(tags)
			break
		}
	}
	return b, len(tags)
}

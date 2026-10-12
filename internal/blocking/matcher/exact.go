package matcher

// MaxNameLen is the longest name, without the trailing dot, that a matcher
// stores (RFC 1035: 255 octets on the wire, 253 in presentation form).
const MaxNameLen = 253

// Exact is a set of names matched exactly. Names are bucketed by length;
// each bucket is one sorted concatenated string, and a bit filter in front
// of the buckets turns most misses into a single memory access.
type Exact struct {
	buckets []bucket // index = name length
	filter  filter
	n       int
}

// Len returns the number of unique names.
func (e *Exact) Len() int {
	if e == nil {
		return 0
	}
	return e.n
}

// Match reports whether name is in the set and returns its tag.
func (e *Exact) Match(name string) (tag uint8, ok bool) {
	if e == nil || len(name) >= len(e.buckets) || e.buckets[len(name)].data == "" {
		return 0, false
	}
	return e.MatchHash(name, Hash(name))
}

// MatchHash is Match with a precomputed h == Hash(name).
func (e *Exact) MatchHash(name string, h uint64) (tag uint8, ok bool) {
	if e == nil || len(name) == 0 || len(name) >= len(e.buckets) {
		return 0, false
	}
	b := &e.buckets[len(name)]
	if b.data == "" || !e.filter.mayContain(h) {
		return 0, false
	}
	i, ok := b.find(name)
	if !ok {
		return 0, false
	}
	return b.tagAt(i), true
}

// Bytes returns the approximate heap footprint of the set.
func (e *Exact) Bytes() int {
	if e == nil {
		return 0
	}
	n := e.filter.bytes() + len(e.buckets)*40
	for i := range e.buckets {
		n += e.buckets[i].bytes()
	}
	return n
}

// ExactBuilder accumulates names for an Exact set. The zero value is ready
// to use.
type ExactBuilder struct {
	arenas [MaxNameLen + 1]arena
	added  int
}

// Add adds a normalised name with its tag. It returns false, and adds
// nothing, when the name is empty or longer than MaxNameLen.
func (b *ExactBuilder) Add(name string, tag uint8) bool {
	if len(name) == 0 || len(name) > MaxNameLen {
		return false
	}
	b.arenas[len(name)].add(name, tag)
	b.added++
	return true
}

// Added returns the number of successful Add calls, duplicates included.
func (b *ExactBuilder) Added() int { return b.added }

// Build sorts, de-duplicates and indexes the names and resets the builder.
// It returns nil when nothing was added.
func (b *ExactBuilder) Build() *Exact {
	if b.added == 0 {
		return nil
	}
	maxLen := 0
	for w := range b.arenas {
		if len(b.arenas[w].tags) > 0 {
			maxLen = w
		}
	}
	e := &Exact{buckets: make([]bucket, maxLen+1)}
	for w := 1; w <= maxLen; w++ {
		bk, n := b.arenas[w].build(w)
		e.buckets[w] = bk
		e.n += n
	}
	e.filter = newFilter(e.n)
	for w := 1; w <= maxLen; w++ {
		data := e.buckets[w].data
		for o := 0; o < len(data); o += w {
			e.filter.add(Hash(data[o : o+w]))
		}
	}
	*b = ExactBuilder{}
	return e
}

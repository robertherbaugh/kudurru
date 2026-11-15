package matcher

import "strings"

// Suffix is a set of base names with subdomain semantics: base matches the
// name base itself and every name below it. It serves ||x^ and *.x rules and
// hosts-style entries declared to cover subdomains.
//
// Entries are bucketed by TLD; within a TLD the rest of the name ("ads.example"
// for "ads.example.com") lives in length buckets of sorted concatenated
// strings, so the TLD is stored once rather than once per entry. A blocked
// bit filter over the full base names sits in front of the buckets: a lookup
// hashes each parent of the query once and touches a bucket only on a filter
// hit.
type Suffix struct {
	tldIndex map[string]int32
	tlds     []tldSet
	filter   filter
	n        int
}

type tldSet struct {
	byLen    []bucket // index = len(rest)
	whole    bool     // the TLD itself is an entry
	wholeTag uint8
}

// Len returns the number of unique base names.
func (s *Suffix) Len() int {
	if s == nil {
		return 0
	}
	return s.n
}

// Match reports whether name equals, or is a subdomain of, a base in the set.
// It walks the parents of name broadest-first and returns the first base
// that matches, which is always a suffix of name (no allocation), plus its
// tag.
func (s *Suffix) Match(name string) (base string, tag uint8, ok bool) {
	if s == nil || s.n == 0 {
		return "", 0, false
	}
	return s.match(name, 0, false)
}

// MatchHash is Match with a precomputed h == Hash(name), which is reused for
// the last (narrowest) candidate, the name itself.
func (s *Suffix) MatchHash(name string, h uint64) (base string, tag uint8, ok bool) {
	if s == nil || s.n == 0 {
		return "", 0, false
	}
	return s.match(name, h, true)
}

func (s *Suffix) match(name string, h uint64, haveHash bool) (string, uint8, bool) {
	dot := strings.LastIndexByte(name, '.')
	tld := name[dot+1:]
	ti, found := s.tldIndex[tld]
	if !found {
		return "", 0, false
	}
	t := &s.tlds[ti]
	if t.whole {
		return tld, t.wholeTag, true
	}
	if dot <= 0 {
		return "", 0, false
	}
	// Candidates are name[start:] for every label boundary start, from the
	// registrable-domain level ("example.com") down to the full name.
	end := dot
	for {
		j := strings.LastIndexByte(name[:end], '.')
		start := j + 1
		rest := name[start:dot]
		if len(rest) > 0 && len(rest) < len(t.byLen) && t.byLen[len(rest)].data != "" {
			var ch uint64
			if start == 0 && haveHash {
				ch = h
			} else {
				ch = Hash(name[start:])
			}
			if s.filter.mayContain(ch) {
				b := &t.byLen[len(rest)]
				if i, ok := b.find(rest); ok {
					return name[start:], b.tagAt(i), true
				}
			}
		}
		if j < 0 {
			return "", 0, false
		}
		end = j
	}
}

// Bytes returns the approximate heap footprint of the set.
func (s *Suffix) Bytes() int {
	if s == nil {
		return 0
	}
	n := s.filter.bytes()
	for tld := range s.tldIndex {
		n += len(tld) + 48 // key bytes, string header, value, map overhead
	}
	for i := range s.tlds {
		n += 40 + len(s.tlds[i].byLen)*40
		for j := range s.tlds[i].byLen {
			n += s.tlds[i].byLen[j].bytes()
		}
	}
	return n
}

// SuffixBuilder accumulates base names for a Suffix set. The zero value is
// ready to use.
type SuffixBuilder struct {
	// MergeTags ORs tags of duplicate entries instead of keeping the first.
	MergeTags bool
	tldIndex  map[string]int32
	tlds      []tldArenas
	added     int
}

type tldArenas struct {
	byLen    []arena
	whole    bool
	wholeTag uint8
}

// Add adds a normalised base name with its tag. It returns false, and adds
// nothing, when base is empty, longer than MaxNameLen, or has an empty label
// at either end.
func (b *SuffixBuilder) Add(base string, tag uint8) bool {
	if len(base) == 0 || len(base) > MaxNameLen {
		return false
	}
	dot := strings.LastIndexByte(base, '.')
	tld := base[dot+1:]
	if tld == "" || dot == 0 {
		return false
	}
	if b.tldIndex == nil {
		b.tldIndex = make(map[string]int32)
	}
	ti, ok := b.tldIndex[tld]
	if !ok {
		ti = int32(len(b.tlds)) //nolint:gosec // TLD count is bounded by distinct strings < 2^31
		b.tldIndex[strings.Clone(tld)] = ti
		b.tlds = append(b.tlds, tldArenas{})
	}
	t := &b.tlds[ti]
	b.added++
	if dot < 0 {
		if !t.whole {
			t.whole, t.wholeTag = true, tag
		} else if b.MergeTags {
			t.wholeTag |= tag
		}
		return true
	}
	rest := base[:dot]
	if len(rest) >= len(t.byLen) {
		t.byLen = append(t.byLen, make([]arena, len(rest)+1-len(t.byLen))...)
	}
	t.byLen[len(rest)].add(rest, tag)
	return true
}

// Added returns the number of successful Add calls, duplicates included.
func (b *SuffixBuilder) Added() int { return b.added }

// Build sorts, de-duplicates and indexes the base names and resets the
// builder. It returns nil when nothing was added.
func (b *SuffixBuilder) Build() *Suffix {
	if b.added == 0 {
		return nil
	}
	s := &Suffix{
		tldIndex: b.tldIndex,
		tlds:     make([]tldSet, len(b.tlds)),
	}
	for ti := range b.tlds {
		ta := &b.tlds[ti]
		t := &s.tlds[ti]
		t.whole, t.wholeTag = ta.whole, ta.wholeTag
		if ta.whole {
			s.n++
		}
		t.byLen = make([]bucket, len(ta.byLen))
		for w := range ta.byLen {
			bk, n := ta.byLen[w].build(w, b.MergeTags)
			t.byLen[w] = bk
			s.n += n
		}
		ta.byLen = nil
	}
	s.filter = newFilter(s.n)
	var buf []byte
	for tld, ti := range s.tldIndex {
		t := &s.tlds[ti]
		for w := range t.byLen {
			data := t.byLen[w].data
			for o := 0; o < len(data); o += w {
				buf = append(buf[:0], data[o:o+w]...)
				buf = append(buf, '.')
				buf = append(buf, tld...)
				s.filter.add(hashBytes(buf))
			}
		}
	}
	*b = SuffixBuilder{}
	return s
}

// Visit yields every matching base broadest first. Returning false stops.
// It is used only when cancellation needs to inspect alternative matches.
func (s *Suffix) Visit(name string, visit func(string, uint8) bool) {
	if s == nil {
		return
	}
	dot := strings.LastIndexByte(name, '.')
	tld := name[dot+1:]
	ti, ok := s.tldIndex[tld]
	if !ok {
		return
	}
	t := &s.tlds[ti]
	if t.whole && !visit(tld, t.wholeTag) {
		return
	}
	if dot <= 0 {
		return
	}
	for end := dot; ; {
		j := strings.LastIndexByte(name[:end], '.')
		start := j + 1
		rest := name[start:dot]
		if len(rest) > 0 && len(rest) < len(t.byLen) {
			b := &t.byLen[len(rest)]
			if i, ok := b.find(rest); ok && !visit(name[start:], b.tagAt(i)) {
				return
			}
		}
		if j < 0 {
			return
		}
		end = j
	}
}

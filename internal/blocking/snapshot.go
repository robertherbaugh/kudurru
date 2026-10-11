package blocking

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/blocking/matcher"
)

// ErrUnknownList is returned by Snapshot.Select for list names the
// snapshot does not contain.
var ErrUnknownList = errors.New("blocking: unknown list")

// Snapshot is an immutable set of compiled lists. The owner publishes it
// through an atomic.Pointer[Snapshot] and swaps in the result of Rebuild;
// readers load the pointer once per query and never lock.
type Snapshot struct {
	lists      []*List // sorted by name
	byName     map[string]*List
	generation uint64
}

// NewSnapshot returns a snapshot of the given lists. Names must be unique.
func NewSnapshot(generation uint64, lists ...*List) (*Snapshot, error) {
	s := &Snapshot{byName: make(map[string]*List, len(lists)), generation: generation}
	for _, l := range lists {
		if l == nil {
			continue
		}
		if _, dup := s.byName[l.name]; dup {
			return nil, fmt.Errorf("blocking: duplicate list %q in snapshot", l.name)
		}
		s.byName[l.name] = l
		s.lists = append(s.lists, l)
	}
	slices.SortFunc(s.lists, func(a, b *List) int { return strings.Compare(a.name, b.name) })
	return s, nil
}

// Generation returns the generation number given to NewSnapshot or Rebuild.
func (s *Snapshot) Generation() uint64 { return s.generation }

// List returns the named list or nil.
func (s *Snapshot) List(name string) *List { return s.byName[name] }

// Lists returns all lists sorted by name.
func (s *Snapshot) Lists() []*List { return slices.Clone(s.lists) }

// Entries returns the number of compiled entries over all lists.
func (s *Snapshot) Entries() int {
	n := 0
	for _, l := range s.lists {
		n += l.stats.Entries
	}
	return n
}

// Bytes approximates the heap footprint of all compiled matchers.
func (s *Snapshot) Bytes() int {
	n := 0
	for _, l := range s.lists {
		n += l.stats.Bytes
	}
	return n
}

// Contains reports which block-mode lists block name, as list name -> the
// rule that matched (§7.3 "Contains(name) -> map[list]rule"). A deny match
// cancelled by the same list's @@ exception is not reported. With no list
// names every block-mode list is checked; unknown names are ignored. It
// returns nil when nothing matches and allocates only on a match. The query
// path uses Selection.Evaluate instead.
func (s *Snapshot) Contains(name string, lists ...string) map[string]string {
	n := NormalizeName(name)
	h := matcher.Hash(n)
	var out map[string]string
	for _, l := range s.pick(lists) {
		if l.mode != ModeBlock {
			continue
		}
		m, ok := l.primary.match(n, h)
		if !ok {
			continue
		}
		if _, excepted := l.except.match(n, h); excepted {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[l.name] = m.Rule()
	}
	return out
}

// ContainsAllow reports which lists allow name: allow-mode lists whose
// entries match it, and block-mode lists whose own @@ exception matches it
// (rendered with the @@ prefix). Arguments and allocation behave as for
// Contains.
func (s *Snapshot) ContainsAllow(name string, lists ...string) map[string]string {
	n := NormalizeName(name)
	h := matcher.Hash(n)
	var out map[string]string
	for _, l := range s.pick(lists) {
		c := &l.except
		if l.mode == ModeAllow {
			c = &l.primary
		}
		m, ok := c.match(n, h)
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[l.name] = m.Rule()
	}
	return out
}

func (s *Snapshot) pick(names []string) []*List {
	if len(names) == 0 {
		return s.lists
	}
	out := make([]*List, 0, len(names))
	for _, n := range names {
		if l := s.byName[n]; l != nil {
			out = append(out, l)
		}
	}
	return out
}

// Select resolves the lists one policy applies (its block and allow list
// references together; each list's own mode decides its role) in the given
// order. The stage computes one Selection per policy per snapshot and
// reuses it for every query. Unknown names are skipped and reported with
// ErrUnknownList; the returned Selection is usable either way, so a
// dangling reference never disables the rest of a policy.
func (s *Snapshot) Select(lists []string, opts SelectOptions) (*Selection, error) {
	sel := &Selection{opts: opts}
	var missing []string
	seen := make(map[string]bool, len(lists))
	for _, name := range lists {
		if seen[name] {
			continue
		}
		seen[name] = true
		l := s.byName[name]
		switch {
		case l == nil:
			missing = append(missing, name)
		case l.mode == ModeAllow:
			sel.allow = append(sel.allow, l)
		default:
			sel.block = append(sel.block, l)
		}
	}
	if len(missing) > 0 {
		return sel, fmt.Errorf("%w: %s", ErrUnknownList, strings.Join(missing, ", "))
	}
	return sel, nil
}

// gcAfterBytes is the compiled size from which Rebuild collects the
// compile's garbage before the next list; small lists (the catalogue's
// services, users' own inline lists) leave too little to be worth a GC.
const gcAfterBytes = 1 << 20

// ListReport describes what Rebuild did with one list.
type ListReport struct {
	Name string
	// Reused is true when the checksums were unchanged and the previous
	// compiled list was carried over without parsing.
	Reused bool
	// KeptOld is true when compiling failed and the previous compiled list
	// was kept (blocky's keep-old-group-on-failure).
	KeptOld bool
	// Err is the compile error, if any.
	Err   error
	Stats Stats
}

// Rebuild compiles specs into a new snapshot with generation
// prev.Generation()+1, reusing unchanged lists from prev (CRC-32 skip) and
// keeping prev's version of a list whose compile failed. Lists of prev that
// are not in specs are dropped. prev may be nil. Lists are compiled one at a
// time to bound peak memory.
func Rebuild(prev *Snapshot, specs []Spec, opts CompileOptions) (*Snapshot, []ListReport) {
	var gen uint64
	if prev != nil {
		gen = prev.generation
	}
	reports := make([]ListReport, 0, len(specs))
	lists := make([]*List, 0, len(specs))
	seen := make(map[string]bool, len(specs))
	for _, spec := range specs {
		r := ListReport{Name: spec.Name}
		if seen[spec.Name] {
			r.Err = fmt.Errorf("blocking: duplicate list %q", spec.Name)
			reports = append(reports, r)
			continue
		}
		seen[spec.Name] = true
		var old *List
		if prev != nil {
			old = prev.byName[spec.Name]
		}
		l, err := Compile(spec, old, opts)
		r.Err = err
		switch {
		case err != nil && old != nil:
			l, r.KeptOld = old, true
		case err == nil && l == old:
			r.Reused = true
		}
		if !r.Reused && len(specs) > 1 && l != nil && l.stats.Bytes >= gcAfterBytes {
			// A compile leaves its builders' garbage (about twice the list's
			// final size) behind; collecting it before the next list keeps
			// the peak near "compiled so far + one list in flight" instead of
			// letting garbage from several lists pile up.
			runtime.GC()
		}
		if l != nil {
			lists = append(lists, l)
			r.Stats = l.stats
		}
		reports = append(reports, r)
	}
	s, err := NewSnapshot(gen+1, lists...)
	if err != nil {
		// Unreachable: duplicate names are filtered above.
		panic(err)
	}
	return s, reports
}

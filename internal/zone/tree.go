package zone

import "strings"

// The zone tree is a left-leaning red-black tree (Sedgewick, 2008) of owner
// names in canonical order. Its shape follows CoreDNS plugin/file/tree
// (Apache-2.0; itself derived from the bíogo LLRB, BSD-3-Clause): one node
// per owner name holding every RRset of that name. Two things differ: nodes
// are keyed by a precomputed canonical key (name.go) so a comparison is a
// single bytes compare, and the tree is built once per snapshot and never
// mutated afterwards, so it has no delete path and needs no locks. Lookups
// probe Zone.index, a hash map over the same nodes, for exact matches; the
// tree supplies canonical order and the build-time searches.

// node is one owner name. Empty non-terminals (names that have descendants
// but no records) are materialised as nodes without RRsets so that "does
// this name exist" is one search.
type node struct {
	key         string
	name        string // lowercased presentation format, absolute
	sets        []RRset
	left, right *node
	red         bool
}

// get returns the RRset of type t at n.
func (n *node) get(t uint16) *RRset {
	for i := range n.sets {
		if n.sets[i].Type == t {
			return &n.sets[i]
		}
	}
	return nil
}

// empty reports whether n is an empty non-terminal.
func (n *node) empty() bool { return len(n.sets) == 0 }

// tree is the per-zone index. The zero value is an empty tree.
type tree struct {
	root  *node
	count int
}

func isRed(n *node) bool { return n != nil && n.red }

func rotateLeft(h *node) *node {
	x := h.right
	h.right = x.left
	x.left = h
	x.red = h.red
	h.red = true
	return x
}

func rotateRight(h *node) *node {
	x := h.left
	h.left = x.right
	x.right = h
	x.red = h.red
	h.red = true
	return x
}

func flipColors(h *node) {
	h.red = !h.red
	h.left.red = !h.left.red
	h.right.red = !h.right.red
}

// search returns the node with key k or nil.
func (t *tree) search(k string) *node {
	n := t.root
	for n != nil {
		switch c := strings.Compare(k, n.key); {
		case c < 0:
			n = n.left
		case c > 0:
			n = n.right
		default:
			return n
		}
	}
	return nil
}

// upsert returns the node for key k, inserting a new empty node named name
// when it does not exist yet.
func (t *tree) upsert(k, name string) *node {
	var found *node
	t.root = t.insert(t.root, k, name, &found)
	t.root.red = false
	return found
}

func (t *tree) insert(h *node, k, name string, found **node) *node {
	if h == nil {
		n := &node{key: k, name: name, red: true}
		*found = n
		t.count++
		return n
	}
	switch {
	case k < h.key:
		h.left = t.insert(h.left, k, name, found)
	case k > h.key:
		h.right = t.insert(h.right, k, name, found)
	default:
		*found = h
		return h
	}
	// Bottom-up 2-3 variant: fix right-leaning reds, split 4-nodes.
	if isRed(h.right) && !isRed(h.left) {
		h = rotateLeft(h)
	}
	if isRed(h.left) && isRed(h.left.left) {
		h = rotateRight(h)
	}
	if isRed(h.left) && isRed(h.right) {
		flipColors(h)
	}
	return h
}

// walk calls fn for every node in canonical order until fn returns false.
func (t *tree) walk(fn func(*node) bool) {
	walkNode(t.root, fn)
}

func walkNode(n *node, fn func(*node) bool) bool {
	for n != nil {
		if !walkNode(n.left, fn) {
			return false
		}
		if !fn(n) {
			return false
		}
		n = n.right
	}
	return true
}

// height returns the height of the tree (for tests).
func (t *tree) height() int { return heightOf(t.root) }

func heightOf(n *node) int {
	if n == nil {
		return 0
	}
	return 1 + max(heightOf(n.left), heightOf(n.right))
}

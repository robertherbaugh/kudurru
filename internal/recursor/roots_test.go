package recursor

import (
	"net/netip"
	"testing"
)

func TestDefaultRootHintsDetachedAndSharedWithRecursion(t *testing.T) {
	addresses := DefaultRootHints()
	cut := hintCut(nil)
	if len(addresses) != 2*len(cut.ns) {
		t.Fatal("default endpoints differ from recursive hints")
	}
	for i, name := range cut.ns {
		for j, address := range cut.glue[name] {
			if addresses[2*i+j] != address {
				t.Fatal("default endpoint order differs from recursive hints")
			}
		}
	}
	original := addresses[0]
	addresses[0] = netip.AddrPort{}
	if DefaultRootHints()[0] != original || hintCut(nil).glue[cut.ns[0]][0] != original {
		t.Fatal("caller changed shared recursive hints")
	}
}

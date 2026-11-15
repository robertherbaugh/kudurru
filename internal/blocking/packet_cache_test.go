package blocking

import (
	"net/netip"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

func TestPacketCacheCompiledClientModifiers(t *testing.T) {
	if (*Snapshot)(nil).ClientIndependent() {
		t.Fatal("missing original rules qualified")
	}
	for _, tc := range []struct {
		rule        string
		independent bool
	}{
		{"||ads.example^", true}, {"/^ads\\.example$/", true}, {"||ads.example^$dnstype=A", true},
		{"||ads.example^$client=192.0.2.1", false}, {"||ads.example^$client=~192.0.2.1", false},
		{"||ads.example^$client='device'", false}, {"||ads.example^$ctag=device_pc", false},
		{"@@||ads.example^$client=192.0.2.1", false}, {"||ads.example^$ctag=~device_pc", false},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			l := mustCompile(t, Spec{Name: "rules", Mode: ModeBlock, Sources: []Source{LinesSource("original", parser.FormatAdblock, []string{tc.rule})}})
			s, err := NewSnapshot(1, l)
			if err != nil {
				t.Fatal(err)
			}
			if l.Stats().Unsupported != 0 || s.ClientIndependent() != tc.independent {
				t.Fatal("compiled modifier proof incorrect")
			}
		})
	}
	l := mustCompile(t, Spec{Name: "rules", Mode: ModeBlock, Sources: []Source{LinesSource("original", parser.FormatAdblock, []string{"||ads.example^$client=192.0.2.1"})}})
	s, err := NewSnapshot(1, l)
	if err != nil {
		t.Fatal(err)
	}
	sel, err := s.Select([]string{"rules"}, SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		d := sel.EvaluateContext(Context{Name: "ads.example.", Type: dns.TypeA, ClientIP: netip.MustParseAddr(ip)})
		if d.Blocked() != (ip == "192.0.2.1") || s.ClientIndependent() {
			t.Fatal("same question lost original client verdict")
		}
	}
}

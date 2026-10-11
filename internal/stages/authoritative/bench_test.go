package authoritative

import (
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// BenchmarkServe measures the stage end to end for an authoritative answer
// (zone routing, ACL, lookup and reply construction) and for a query that
// is passed on to the rest of the pipeline.
func BenchmarkServe(b *testing.B) {
	st := New(source(b, true))
	next := func(*pipeline.Request) (*pipeline.Response, error) { return &pipeline.Response{}, nil }
	for _, bc := range []struct{ name, qname string }{
		{"authoritative", "web.example.org."},
		{"cname-chain", "www.example.org."},
		{"nxdomain", "nope.example.org."},
		{"not-local", "www.external.test."},
	} {
		b.Run(bc.name, func(b *testing.B) {
			tc := Case{Qname: bc.qname, Qtype: dns.TypeA}
			req := tc.request()
			b.ReportAllocs()
			for b.Loop() {
				if _, err := st.Serve(req, next); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

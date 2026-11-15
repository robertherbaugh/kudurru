package dynamicupdate

import (
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestPlanPolicyPreflight(t *testing.T) {
	s := base(t)
	tx, e := Parse(request(t, nil, nil), Limits{})
	if e != nil {
		t.Fatal(e)
	}
	types := make([]uint16, 65535)
	s.Grants = []Grant{{Key: "update-key.", Owner: "*.example.", Types: types}, {Key: "update-key.", Owner: "example.", Types: types}}
	// A bad current record must never be decoded before policy amplification is refused.
	s.Records = []dns.RR{nil}
	if _, e := Plan(s, tx, Principal{Key: "update-key.", Version: "v1"}, time.Now(), Limits{}); e == nil || e.Reason != "policy capacity exceeded" {
		t.Fatalf("policy preflight: %v", e)
	}
	if _, err := PolicyAllocation(nil, nil, []Reservation{{Name: "a.example.", Source: strings.Repeat("x", 513)}}, 1<<20); err == nil {
		t.Fatal("unbounded reservation identity")
	}
	if _, err := PolicyAllocation(nil, []Grant{{Key: "key.", Owner: "*.example.", Types: types}}, nil, 1024); err == nil {
		t.Fatal("types workspace not charged")
	}
}

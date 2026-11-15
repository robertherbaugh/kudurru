package postprocess

import (
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestFinalResponseAuthenticity(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	valid := pipeline.Security{State: pipeline.ValidationSecure, ValidatedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Minute)}
	for _, tc := range []struct {
		name           string
		origin         pipeline.ResponseOrigin
		security       pipeline.Security
		modified, cd   bool
		do, ad, wantAD bool
	}{
		{"secure DO", pipeline.OriginForwarded, valid, false, false, true, false, true},
		{"secure AD interest", pipeline.OriginForwarded, valid, false, false, false, true, true},
		{"no interest", pipeline.OriginForwarded, valid, false, false, false, false, false},
		{"checking disabled", pipeline.OriginForwarded, valid, false, true, true, true, false},
		{"modified", pipeline.OriginForwarded, valid, true, false, true, true, false},
		{"mixed alias", pipeline.OriginMixed, valid, false, false, true, true, false},
		{"authoritative", pipeline.OriginAuthoritative, valid, false, false, true, true, false},
		{"unknown origin", pipeline.OriginUnknown, valid, false, false, true, true, false},
		{"upstream AD only", pipeline.OriginForwarded, pipeline.Security{}, false, false, true, true, false},
		{"expired proof", pipeline.OriginForwarded, pipeline.Security{State: pipeline.ValidationSecure, ValidUntil: now}, false, false, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := new(dns.Msg)
			q.SetQuestion("example.", dns.TypeA)
			q.SetEdns0(1232, tc.do)
			q.CheckingDisabled, q.AuthenticatedData = tc.cd, tc.ad
			m := new(dns.Msg)
			m.SetReply(q)
			m.AuthenticatedData = true // never sufficient evidence by itself
			a, _ := dns.NewRR("example. 60 IN A 192.0.2.1")
			proof := &dns.RRSIG{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 60}, TypeCovered: dns.TypeA}
			m.Answer = []dns.RR{a, proof}
			resp := &pipeline.Response{Msg: m, Origin: tc.origin, Security: tc.security, Modified: tc.modified}
			s := &Stage{Now: func() time.Time { return now }}
			got, err := s.Serve(&pipeline.Request{Msg: q}, func(*pipeline.Request) (*pipeline.Response, error) { return resp, nil })
			if err != nil || got.Msg.AuthenticatedData != tc.wantAD || got.Msg.CheckingDisabled != tc.cd {
				t.Fatalf("AD/CD result: %+v, %v", got, err)
			}
			wantRecords := 1
			if tc.do {
				wantRecords = 2
			}
			if len(got.Msg.Answer) != wantRecords || len(m.Answer) != 2 || !m.AuthenticatedData {
				t.Fatal("finalization must remove unsolicited proofs only from the private client response")
			}
		})
	}
}

func TestPostprocessDoesNotAlterProtocolDispatch(t *testing.T) {
	for _, opcode := range []int{dns.OpcodeNotify, dns.OpcodeUpdate} {
		q := new(dns.Msg)
		q.SetQuestion("example.", dns.TypeSOA)
		q.Opcode = opcode
		r := &pipeline.Response{Msg: new(dns.Msg)}
		got, err := (&Stage{}).Serve(&pipeline.Request{Msg: q}, func(*pipeline.Request) (*pipeline.Response, error) { return r, nil })
		if err != nil || got != r {
			t.Fatal("NOTIFY/UPDATE must retain their protocol response")
		}
	}
}

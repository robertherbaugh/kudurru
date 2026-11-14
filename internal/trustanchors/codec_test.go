package trustanchors

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestStrictCodecRoundTripAndDetachedDocuments(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a)
	original, err := s.Version()
	if err != nil {
		t.Fatal(err)
	}
	d := s.Inspect()
	d.Keys[0].DNSKEY = "mutated"
	for i := range d.Keys {
		if len(d.Keys[i].Support) > 0 {
			d.Keys[i].Support[0] = "mutated"
		}
	}
	if unchanged, err := s.Version(); err != nil || unchanged != original {
		t.Fatal("diagnostic mutation altered accepted state", err)
	}
	b, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	v, err := restored.Version()
	if err != nil || v != original {
		t.Fatal("round trip restamped authority", err)
	}
	var spaced bytes.Buffer
	if err := json.Indent(&spaced, b, "", "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(spaced.Bytes()); err != nil {
		t.Fatal("insignificant whitespace refused", err)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"duplicate": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"format":1`), []byte(`"format":1,"format":1`), 1)
		},
		"alias":   func(b []byte) []byte { return bytes.Replace(b, []byte(`"clusterID"`), []byte(`"ClusterID"`), 1) },
		"omitted": func(b []byte) []byte { return bytes.Replace(b, []byte(`"clockUncertain":false,`), nil, 1) },
		"null": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"clockUncertain":false`), []byte(`"clockUncertain":null`), 1)
		},
		"unknown":     func(b []byte) []byte { return append(append([]byte{}, b[:len(b)-1]...), []byte(`,"future":true}`)...) },
		"trailing":    func(b []byte) []byte { return append(append([]byte{}, b...), []byte(`{}`)...) },
		"version":     func(b []byte) []byte { return bytes.Replace(b, []byte(`"format":1`), []byte(`"format":2`), 1) },
		"empty_owner": func([]byte) []byte { return []byte(`{}`) },
		"deep":        func([]byte) []byte { return []byte(strings.Repeat("[", 20) + strings.Repeat("]", 20)) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(mutate(b)); err == nil {
				t.Fatal("invalid history accepted")
			}
		})
	}
	if _, err := Decode(bytes.Repeat([]byte(" "), MaxStateBytes+1)); !errors.Is(err, ErrBounds) {
		t.Fatal("oversized document accepted", err)
	}
}

func TestCodecRejectsInventedAuthorityAndSchedulerTimes(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a)
	for name, mutate := range map[string]func(*Snapshot){
		"uninitialized":      func(d *Snapshot) { d.Generation = 0 },
		"missing_history":    func(d *Snapshot) { d.Keys = nil },
		"fallback":           func(d *Snapshot) { d.Quarantined = true },
		"unknown_state":      func(d *Snapshot) { d.Keys[0].Status = "Removed" },
		"future_observation": func(d *Snapshot) { d.Keys[0].LastSeen = d.Checkpoint.Wall.Add(time.Second) },
		"reset_poll":         func(d *Snapshot) { d.Schedule.NextRefresh = d.Schedule.NextRefresh.Add(time.Hour) },
		"lost_support": func(d *Snapshot) {
			for i := range d.Keys {
				if d.Keys[i].Status == AddPending {
					d.Keys[i].Support = []string{}
				}
			}
		},
		"invented_acceptance": func(d *Snapshot) {
			for i := range d.Keys {
				if d.Keys[i].Status == AddPending {
					d.Keys[i].Status, d.Keys[i].AcceptedAt = Valid, d.Keys[i].FirstSeen
				}
			}
		},
		"forged_identity": func(d *Snapshot) { d.Keys[0].ID = strings.Repeat("0", 64) },
		"lost_session":    func(d *Snapshot) { d.Checkpoint.Session = "" },
	} {
		t.Run(name, func(t *testing.T) {
			d := s.Inspect()
			mutate(&d)
			if _, err := Encode(State{doc: d}); err == nil {
				t.Fatal("invented state accepted")
			}
		})
	}
}

func TestProtocolRefreshAndRetryUseOriginalBasis(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		ttl            uint32
		expires        time.Duration
		refresh, retry time.Duration
	}{
		{60, time.Minute, time.Hour, time.Hour},
		{86400, 10 * 24 * time.Hour, 12 * time.Hour, 8640 * time.Second},
		{40 * 86400, 90 * 24 * time.Hour, 15 * 24 * time.Hour, 24 * time.Hour},
		{40 * 86400, 4 * time.Hour, 2 * time.Hour, time.Hour},
	} {
		refresh, retry, err := RefreshIntervals(tc.ttl, f.base, f.base.Add(tc.expires))
		if err != nil || refresh != tc.refresh || retry != tc.retry {
			t.Fatal("RFC interval mismatch", refresh, retry, err)
		}
	}
	s := f.initial([]testKey{f.a}, f.a)
	v, err := s.Version()
	if err != nil {
		t.Fatal(err)
	}
	at := f.point(2 * time.Hour)
	due, err := s.RetryDue(at)
	if err != nil || !due.Equal(at.wall.Add(time.Hour)) {
		t.Fatal("retry scheduler renewed original proof basis", err)
	}
	if after, err := s.Version(); err != nil || after != v {
		t.Fatal("scheduler mutated authority", err)
	}
	if _, _, err := RefreshIntervals(3600, f.base, f.base); err == nil {
		t.Fatal("expired interval accepted")
	}
}

func TestOriginalWireAuthenticationAndKeyEligibility(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a)
	for name, mutate := range map[string]func(*dns.Msg){
		"candidate_only": func(m *dns.Msg) { *m = *f.wire([]testKey{f.a, f.b}, f.b) },
		"ad_only":        func(m *dns.Msg) { m.Answer = m.Answer[:2]; m.AuthenticatedData = true },
		"tampered":       func(m *dns.Msg) { m.Answer[0].(*dns.DNSKEY).Flags &^= dns.SEP },
		"foreign_signer": func(m *dns.Msg) { m.Answer[2].(*dns.RRSIG).SignerName = "example." },
		"wrong_question": func(m *dns.Msg) { m.Question[0].Name = "example." },
		"truncated":      func(m *dns.Msg) { m.Truncated = true },
		"duplicate":      func(m *dns.Msg) { m.Answer = append(m.Answer, dns.Copy(m.Answer[0])) },
		"bad_protocol":   func(m *dns.Msg) { m.Answer[0].(*dns.DNSKEY).Protocol = 2 },
		"typed_nil":      func(m *dns.Msg) { m.Answer[0] = (*dns.DNSKEY)(nil) },
		"too_many": func(m *dns.Msg) {
			for len(m.Answer) <= MaxLiveKeys+MaxSignatures {
				m.Answer = append(m.Answer, dns.Copy(m.Answer[0]))
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := f.wire([]testKey{f.a, f.b}, f.a)
			mutate(m)
			if _, err := Verify(s, m, f.point(time.Hour)); err == nil {
				t.Fatal("unauthenticated or malformed proof accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*dns.DNSKEY){
		"unsupported":     func(k *dns.DNSKEY) { k.Algorithm = dns.ED448 },
		"non_sep":         func(k *dns.DNSKEY) { k.Flags &^= dns.SEP },
		"non_zone":        func(k *dns.DNSKEY) { k.Flags &^= dns.ZONE },
		"malformed_point": func(k *dns.DNSKEY) { k.PublicKey = "AA==" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := testKey{key: dns.Copy(f.c.key).(*dns.DNSKEY), signer: f.c.signer}
			mutate(bad.key)
			next := f.step(s, f.wire([]testKey{f.a, f.b, bad}, f.a), f.point(time.Hour))
			if len(next.Inspect().Keys) != 2 {
				t.Fatal("ineligible key entered managed trust history")
			}
		})
	}
}

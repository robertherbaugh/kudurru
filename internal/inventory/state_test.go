package inventory

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func stateClient(id, ip string, at time.Time) Client {
	return Client{ID: id, IPs: []netip.Addr{netip.MustParseAddr(ip)}, MACs: []string{"02:00:00:00:00:01"}, Hostname: id + ".test", ObservedAt: at, ExpiresAt: at.Add(time.Hour)}
}
func roundTripState(t *testing.T, h *Holder, at time.Time) *Holder {
	t.Helper()
	s, err := h.ExportState()
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeState(s)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeState(data)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreState(decoded, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	got, err := restored.ExportState()
	if err != nil {
		t.Fatal(err)
	}
	again, _ := EncodeState(got)
	if string(again) != string(data) {
		t.Fatal("state changed on restore")
	}
	return restored
}
func TestStateRestartRemovalExpiryAndAmbiguity(t *testing.T) {
	at := time.Unix(100000, 0).UTC()
	ip := netip.MustParseAddr("192.0.2.1")
	for _, mode := range []string{"removed", "expired", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			h := New(func() time.Time { return at.Add(2 * time.Minute) })
			if err := h.Replace("a", 1, []Client{stateClient("older", ip.String(), at)}); err != nil {
				t.Fatal(err)
			}
			newer := stateClient("newer", ip.String(), at.Add(time.Minute))
			if mode == "ambiguous" {
				newer.ObservedAt = at
				newer.ExpiresAt = at.Add(time.Hour)
			}
			if err := h.Replace("b", 1, []Client{newer}); err != nil {
				t.Fatal(err)
			}
			check := at.Add(3 * time.Minute)
			if mode == "removed" {
				if err := h.Replace("b", 2, nil); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "expired" {
				check = newer.ExpiresAt
			}
			restored := roundTripState(t, h, check)
			if got := restored.Snapshot().Lookup(ip, check); got != nil {
				t.Fatalf("resurrected %s", got.ID)
			}
			s, _ := restored.ExportState()
			next, err := PrepareReplacement(s, "c", 1, nil, check)
			if err != nil {
				t.Fatal(err)
			}
			again, err := RestoreState(next, func() time.Time { return check })
			if err != nil {
				t.Fatal(err)
			}
			if again.Snapshot().Lookup(ip, check) != nil {
				t.Fatal("replacement resurrected older owner")
			}
		})
	}
}
func TestStateCopiesPrepareAndOriginalTimes(t *testing.T) {
	at := time.Unix(100000, 0).UTC()
	h := New(func() time.Time { return at })
	row := stateClient("phone", "192.0.2.1", at)
	if err := h.Replace("a", 4, []Client{row}); err != nil {
		t.Fatal(err)
	}
	s, _ := h.ExportState()
	original, _ := json.Marshal(s)
	next, err := PrepareReplacement(s, "a", 5, nil, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(s)
	if string(after) != string(original) {
		t.Fatal("prepare mutated input")
	}
	if !next.Watermarks[0].ObservedAt.Equal(at.Add(time.Minute)) || !next.Watermarks[0].ExpiresAt.Equal(at.Add(time.Minute)) || next.Watermarks[0].Live {
		t.Fatal("bad removal time")
	}
	restored, err := RestoreState(next, func() time.Time { return at.Add(48 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	again, _ := restored.ExportState()
	if !reflect.DeepEqual(next, again) {
		t.Fatal("restart restamped/pruned original evidence")
	}
	s.Sources[0].Clients[0].MACs[0] = "changed"
	s.Watermarks[0].ID = "changed"
	if h.Snapshot().Lookup(row.IPs[0], at).MACs[0] == "changed" {
		t.Fatal("export aliases holder")
	}
	if _, err := PrepareReplacement(next, "a", 5, nil, at.Add(2*time.Minute)); err == nil {
		t.Fatal("revision replay accepted")
	}
	if _, err := RestoreState(next, func() time.Time { return at }); err == nil {
		t.Fatal("clock rollback accepted future tombstone")
	}
}
func TestStateRejectsIncompleteOrMalformedEvidence(t *testing.T) {
	at := time.Unix(100000, 0).UTC()
	h := New(func() time.Time { return at })
	if err := h.Replace("a", 1, []Client{stateClient("phone", "192.0.2.1", at)}); err != nil {
		t.Fatal(err)
	}
	s, _ := h.ExportState()
	data, _ := EncodeState(s)
	for name, mutate := range map[string]func(*State){
		"missing": func(s *State) { s.Watermarks = nil }, "duplicate": func(s *State) { s.Watermarks = append(s.Watermarks, s.Watermarks[0]) },
		"live owner": func(s *State) { s.Watermarks[0].ID = "other" }, "expiry": func(s *State) { s.Watermarks[0].ExpiresAt = at.Add(25 * time.Hour) },
		"newer": func(s *State) { s.Watermarks[0].ObservedAt = at.Add(-time.Minute) }, "old invalid client": func(s *State) { s.Sources[0].Clients[0].IPs[0] = netip.Addr{} },
		"revision": func(s *State) { s.Sources[0].Revision = 0 }, "generation": func(s *State) { s.Generation = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			decoded, err := DecodeState(data)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&decoded)
			if _, err := RestoreState(decoded, func() time.Time { return at.Add(48 * time.Hour) }); err == nil {
				t.Fatal("accepted malformed state")
			}
		})
	}
	for _, bad := range []string{string(data) + "{}", `{"formatVersion":1,"unknown":true}`, `null`} {
		if _, err := DecodeState([]byte(bad)); err == nil {
			t.Errorf("accepted encoding %q", bad)
		}
	}
}
func TestStateConcurrentExportReplacement(t *testing.T) {
	at := time.Unix(100000, 0).UTC()
	h := New(func() time.Time { return at })
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := uint64(1); i <= 40; i++ {
			if err := h.Replace("a", i, []Client{stateClient("phone", "192.0.2.1", at)}); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Go(func() {
		for range 40 {
			s, err := h.ExportState()
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := RestoreState(s, func() time.Time { return at }); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Wait()
}

func TestStateStrictOriginalJSON(t *testing.T) {
	at := time.Unix(100000, 0).UTC()
	h := New(func() time.Time { return at })
	if err := h.Replace("a", 1, []Client{stateClient("phone", "192.0.2.1", at)}); err != nil {
		t.Fatal(err)
	}
	s, err := h.ExportState()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeState(s)
	if err != nil {
		t.Fatal(err)
	}
	base := string(encoded)
	bad := map[string]string{
		"duplicate generation":     strings.Replace(base, `"generation":"1"`, `"generation":"1","generation":"2"`, 1),
		"duplicate nested boolean": strings.Replace(base, `"live":true`, `"live":false,"live":true`, 1),
		"escaped duplicate":        strings.Replace(base, `"live":true`, `"live":true,"\u006cive":false`, 1),
		"case alias":               strings.Replace(base, `"live":true`, `"Live":true`, 1),
		"case duplicate":           strings.Replace(base, `"id":"phone"`, `"id":"phone","ID":"other"`, 1),
		"missing boolean":          strings.Replace(base, `,"live":true`, "", 1),
		"null boolean":             strings.Replace(base, `"live":true`, `"live":null`, 1),
		"string boolean":           strings.Replace(base, `"live":true`, `"live":"true"`, 1),
		"missing source clients":   strings.Replace(base, `"clients":`, `"unknown":`, 1),
		"missing ips":              strings.Replace(base, `"ips":`, `"unknown":`, 1),
		"missing observed":         strings.Replace(base, `"observedAt":`, `"unknown":`, 1),
		"null source id":           strings.Replace(base, `"id":"a"`, `"id":null`, 1),
		"numeric generation":       strings.Replace(base, `"generation":"1"`, `"generation":1`, 1),
		"leading zero":             strings.Replace(base, `"generation":"1"`, `"generation":"01"`, 1),
		"null generation":          strings.Replace(base, `"generation":"1"`, `"generation":null`, 1),
		"null optional tags":       strings.Replace(base, `"hostname":`, `"tags":null,"hostname":`, 1),
		"isolated high surrogate":  strings.Replace(base, `"phone"`, `"\ud800"`, 1),
		"isolated low surrogate":   strings.Replace(base, `"phone"`, `"\udfff"`, 1),
		"bad surrogate pair":       strings.Replace(base, `"phone"`, `"\ud800\u0041"`, 1),
		"invalid utf8":             strings.Replace(base, `"phone"`, "\"\xff\"", 1),
	}
	for name, wire := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeState([]byte(wire)); err == nil {
				t.Fatal("accepted ambiguous historical payload")
			}
		})
	}
	for _, wire := range []string{
		`{"formatVersion":1,"generation":"0","sources":null,"watermarks":null}`,
		`{"formatVersion":1,"generation":"0","sources":[],"watermarks":[]}`,
		strings.Replace(base, `"phone"`, `"\ud83d\ude00"`, 1),
		strings.Replace(base, `"phone"`, `"\\ud800"`, 1),
		strings.Replace(base, `"live":true`, `"live":false`, 1),
		strings.Replace(base, `"ips":["192.0.2.1"]`, `"ips":null`, 1),
	} {
		if _, err := DecodeState([]byte(wire)); err != nil {
			t.Fatalf("valid original JSON rejected: %v", err)
		}
	}
	for _, wire := range []string{
		`{"formatVersion":1,"generation":"0","sources":null}`,
		`{"formatVersion":1,"generation":"0","watermarks":null}`,
		`{"formatVersion":1,"generation":"0","sources":[{"id":"a","revision":"1"}],"watermarks":null}`,
	} {
		if _, err := DecodeState([]byte(wire)); err == nil {
			t.Fatal("missing required field accepted")
		}
	}
}

func TestStatePreallocationExactBudget(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.FixedZone("offset", 330*60))
	states := []State{
		{FormatVersion: 1},
		{FormatVersion: 1, Sources: []SourceState{}, Watermarks: []Watermark{}},
		{FormatVersion: 1, Generation: 19, Sources: []SourceState{{ID: "<>&\"\\\n\x01\u2028\u2029😀", Revision: 25, Clients: nil}}, Watermarks: []Watermark{}},
		{FormatVersion: 1, Generation: 21, Sources: []SourceState{{ID: "a", Revision: 1, Clients: []Client{{Source: "a", ID: "b", Name: "<>&\"\\\b\f\n\r\t\x00\u2028\u2029😀", Hostname: "device.test", IPs: []netip.Addr{netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("192.0.2.1")}, MACs: []string{"02:00:00:00:00:01"}, Tags: []string{"a", "b"}, ObservedAt: at, ExpiresAt: at.Add(time.Hour)}}}}, Watermarks: []Watermark{{IP: netip.MustParseAddr("192.0.2.1"), Source: "a", ID: "b", ObservedAt: at, ExpiresAt: at.Add(time.Hour), Live: true}, {IP: netip.MustParseAddr("2001:db8::1"), ObservedAt: at, ExpiresAt: at, Live: false}}},
	}
	for i, s := range states {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckStateBounds(s, len(b)); err != nil {
			t.Fatalf("case %d exact budget failed: %v", i, err)
		}
		if err := CheckStateBounds(s, len(b)-1); err == nil {
			t.Fatalf("case %d over-budget allocation allowed", i)
		}
	}
	base := states[3]
	b, err := EncodeState(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*State){
		"source string":    func(s *State) { s.Sources[0].ID = strings.Repeat("x", 257) },
		"client string":    func(s *State) { s.Sources[0].Clients[0].ID = strings.Repeat("x", 257) },
		"invalid utf8":     func(s *State) { s.Sources[0].Clients[0].Name = "\xff" },
		"mac string":       func(s *State) { s.Sources[0].Clients[0].MACs = []string{strings.Repeat("x", 18)} },
		"tag count":        func(s *State) { s.Sources[0].Clients[0].Tags = make([]string, 65) },
		"addresses":        func(s *State) { s.Sources[0].Clients[0].IPs = make([]netip.Addr, 17) },
		"watermark string": func(s *State) { s.Watermarks[0].Source = strings.Repeat("x", 257) },
	} {
		t.Run(name, func(t *testing.T) {
			var s State
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatal(err)
			}
			change(&s)
			if _, err := EncodeState(s); err == nil {
				t.Fatal("unbounded allocation accepted")
			}
		})
	}
}

package querylog

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	testclock "k8s.io/utils/clock/testing"
)

func TestPrivacyReloadDailyIdentity(t *testing.T) {
	fc := testclock.NewFakeClock(time.Date(2026, 10, 11, 23, 0, 0, 0, time.UTC))
	makePrivacy := func(mode Mode, pseudonym bool, excluded ...string) *Privacy {
		t.Helper()
		p, err := NewPrivacy(PrivacyOptions{Mode: mode, Pseudonym: pseudonym, Clock: fc, ExcludeDomains: excluded})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := new(Async)
	before := makePrivacy(PrivacyAnonymizeClient, true)
	a.SetPrivacy(before)
	addr := netip.MustParseAddr("192.0.2.10")
	original := a.Pseudonym(addr)
	if original == "" {
		t.Fatal("missing original pseudonym")
	}
	supplied := makePrivacy(PrivacyAnonymizeClient, true, "excluded.example")
	independent := supplied.Pseudonym(addr)
	a.SetPrivacy(supplied)
	if a.Pseudonym(addr) != original || before.Pseudonym(addr) != original {
		t.Fatal("exclusion reload changed daily identity")
	}
	if supplied.Pseudonym(addr) != independent {
		t.Fatal("reload mutated caller's filter")
	}
	event := &Event{Name: "excluded.example.", Client: addr}
	if a.admit(event) {
		t.Fatal("new exclusion not applied")
	}
	event = &Event{Name: "kept.example.", Client: addr}
	if !a.admit(event) || event.ClientName != original || event.Client.IsValid() {
		t.Fatal("collector and pseudonym lookup disagree after reload")
	}
	fc.Step(2 * time.Hour)
	nextDay := a.Pseudonym(addr)
	if nextDay == original || before.Pseudonym(addr) != nextDay {
		t.Fatal("old and new filter did not share midnight rotation")
	}
	a.SetPrivacy(makePrivacy(PrivacyAnonymizeClient, false))
	if a.Pseudonym(addr) != "" {
		t.Fatal("prefix mode retained pseudonyms")
	}
	a.SetPrivacy(makePrivacy(PrivacyAnonymizeClient, true))
	afterModeChange := a.Pseudonym(addr)
	if afterModeChange == nextDay {
		t.Fatal("anonymization change reused old identity")
	}
	a.SetPrivacy(makePrivacy(PrivacyFull, true))
	a.SetPrivacy(before)
	if a.Pseudonym(addr) == afterModeChange || a.Pseudonym(addr) == nextDay {
		t.Fatal("privacy mode change reused old identity")
	}
	fresh := new(Async)
	fresh.SetPrivacy(makePrivacy(PrivacyAnonymizeClient, true))
	if fresh.Pseudonym(addr) == a.Pseudonym(addr) {
		t.Fatal("new collector reused process-local identity")
	}
	a.SetPrivacy(nil)
	if a.Pseudonym(addr) != "" {
		t.Fatal("removed privacy filter retained identity")
	}
}

func TestPrivacyReloadConcurrentMidnight(t *testing.T) {
	fc := testclock.NewFakeClock(time.Date(2026, 10, 11, 23, 59, 0, 0, time.UTC))
	p, err := NewPrivacy(PrivacyOptions{Mode: PrivacyAnonymizeClient, Pseudonym: true, Clock: fc})
	if err != nil {
		t.Fatal(err)
	}
	a := new(Async)
	a.SetPrivacy(p)
	addr := netip.MustParseAddr("2001:db8::1")
	original := a.Pseudonym(addr)
	fc.Step(2 * time.Minute)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				next, err := NewPrivacy(PrivacyOptions{Mode: PrivacyAnonymizeClient, Pseudonym: true, Clock: fc})
				if err != nil {
					t.Error(err)
					return
				}
				a.SetPrivacy(next)
				if got := a.Pseudonym(addr); got == original || got != p.Pseudonym(addr) {
					t.Error("concurrent reload split daily identity")
					return
				}
			}
		})
	}
	wg.Wait()
}

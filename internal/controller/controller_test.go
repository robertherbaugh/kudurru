package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestZoneStatus: recordCount, contentHash, serial, observedGeneration and
// Ready are computed from the record sets referencing the zone.
func TestZoneStatus(t *testing.T) {
	e := newEnv(t)
	e.apply(zoneObj("example", "example.com."))
	e.apply(rrsetObj("apps", "web", "example", 0, ep("www.example.com", "A", "192.0.2.1", "192.0.2.2")))
	e.apply(rrsetObj("apps", "mail", "example", time.Second, ep("example.com", "MX", "10 mail.example.com")))
	e.apply(rrsetObj("apps", "elsewhere", "other", 0, ep("www.other.org", "A", "192.0.2.3")))
	e.start()

	want := zoneHash(t, "example.com.", []RecordTuple{
		{Name: "www.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.1", "192.0.2.2"}},
		{Name: "example.com.", Type: "MX", TTL: 300, Targets: []string{"10 mail.example.com"}},
	})
	e.eventually("zone status", func() bool { return e.zone("example").Status.ContentHash == want })
	z := e.zone("example")
	if z.Status.RecordCount != 3 || z.Status.Serial != unix32(t0) || z.Status.ObservedGeneration != 1 {
		t.Fatalf("status %+v", z.Status)
	}
	if got := cond(z.Status.Conditions, v1alpha1.ConditionReady); got != "True/Serving" {
		t.Fatalf("Ready %s", got)
	}
	if got := cond(z.Status.Conditions, v1alpha1.ConditionDegraded); got != "False/AsExpected" {
		t.Fatalf("Degraded %s", got)
	}
	if len(z.Status.Published) != 0 {
		t.Fatal("Published must stay empty")
	}
	if n := e.eventCount(EventSerialBumped, key(KindDNSZone, "", "example")); n != 1 {
		t.Fatalf("SerialBumped events %d", n)
	}
	e.eventually("orphan record set", func() bool {
		return cond(e.rrset("apps", "elsewhere").Status.Conditions, v1alpha1.ConditionReady) == "False/ZoneNotFound"
	})
	e.eventually("record set names", func() bool { return e.rrset("apps", "web").Status.Names == 1 })
}

// TestSerialRule (D12): monotonic, no bump on identical content, bump on
// change, max(prev+1, unix_now).
func TestSerialRule(t *testing.T) {
	e := newEnv(t)
	e.apply(zoneObj("example", "example.com."))
	web := rrsetObj("apps", "web", "example", 0, ep("www.example.com", "A", "192.0.2.1", "192.0.2.2"))
	e.apply(web)
	e.start()
	start := unix32(t0)
	e.eventually("first serial", func() bool { return e.zone("example").Status.Serial == start })
	e.settle()

	// Identical content: re-apply, a label-only change, and a target
	// reorder (same RRset) never bump.
	e.apply(web.DeepCopy())
	relabelled := web.DeepCopy()
	relabelled.Labels = map[string]string{"team": "web"}
	e.apply(relabelled)
	reordered := relabelled.DeepCopy()
	reordered.Spec.Endpoints[0].Targets = []string{"192.0.2.2", "192.0.2.1"}
	e.apply(reordered)
	e.settle()
	if s := e.zone("example").Status.Serial; s != start {
		t.Fatalf("serial moved on identical content: %d", s)
	}

	// Change within the same second: prev+1.
	changed := relabelled.DeepCopy()
	changed.Spec.Endpoints[0].Targets = []string{"192.0.2.9"}
	e.apply(changed)
	e.eventually("serial prev+1", func() bool { return e.zone("example").Status.Serial == start+1 })

	// Clock moves on: unix_now wins.
	e.clock.Advance(time.Hour)
	changed2 := changed.DeepCopy()
	changed2.Spec.Endpoints[0].Targets = []string{"192.0.2.10"}
	e.apply(changed2)
	e.eventually("serial unix_now", func() bool { return e.zone("example").Status.Serial == unix32(t0.Add(time.Hour)) })

	// Clock goes backwards (NTP step): still monotonic.
	e.clock.Advance(-2 * time.Hour)
	changed3 := changed2.DeepCopy()
	changed3.Spec.Endpoints[0].Targets = []string{"192.0.2.11"}
	e.apply(changed3)
	e.eventually("monotonic serial", func() bool { return e.zone("example").Status.Serial == unix32(t0.Add(time.Hour))+1 })

	// A zone spec change that alters served content (default TTL) bumps.
	z := e.zone("example")
	z.Spec.DefaultTTL = 600
	z.ResourceVersion = ""
	e.apply(z)
	e.eventually("serial on TTL change", func() bool { return e.zone("example").Status.Serial == unix32(t0.Add(time.Hour))+2 })
	e.settle()
	if n := e.eventCount(EventSerialBumped, key(KindDNSZone, "", "example")); n != 5 {
		t.Fatalf("SerialBumped events %d, want 5", n)
	}
}

// TestDuplicateOrdering: the later object by creationTimestamp gets
// Duplicated, regardless of the order in which they were written; removing
// the earlier object clears the condition.
func TestDuplicateOrdering(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.apply(zoneObj("example", "example.com."))
	// Written first, but created later.
	e.apply(rrsetObj("team-b", "web", "example", time.Hour, ep("www.example.com", "A", "192.0.2.99"), ep("b.example.com", "A", "192.0.2.2")))
	e.eventually("b serving alone", func() bool {
		return cond(e.rrset("team-b", "web").Status.Conditions, v1alpha1.ConditionDegraded) == "False/AsExpected"
	})
	earlier := rrsetObj("team-a", "web", "example", 0, ep("www.example.com", "A", "192.0.2.1"))
	e.apply(earlier)

	e.eventually("later object duplicated", func() bool {
		return cond(e.rrset("team-b", "web").Status.Conditions, v1alpha1.ConditionDegraded) == "True/Duplicated"
	})
	b := e.rrset("team-b", "web")
	if got := cond(b.Status.Conditions, v1alpha1.ConditionReady); got != "True/Serving" {
		t.Fatalf("partially served record set Ready %s", got)
	}
	if msg := condition(b.Status.Conditions, v1alpha1.ConditionDegraded).Message; !strings.Contains(msg, "www.example.com. A (held by team-a/web)") {
		t.Fatalf("Degraded message %q", msg)
	}
	if got := cond(e.rrset("team-a", "web").Status.Conditions, v1alpha1.ConditionDegraded); got != "False/AsExpected" {
		t.Fatalf("earlier object Degraded %s", got)
	}
	e.eventually("zone degraded", func() bool {
		return cond(e.zone("example").Status.Conditions, v1alpha1.ConditionDegraded) == "True/Duplicated"
	})
	if n := e.eventCount(EventDuplicated, key(KindDNSRecordSet, "team-b", "web")); n != 1 {
		t.Fatalf("Duplicated events %d", n)
	}
	want := zoneHash(t, "example.com.", []RecordTuple{
		{Name: "www.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}},
		{Name: "b.example.com.", Type: "A", TTL: 300, Targets: []string{"192.0.2.2"}},
	})
	e.eventually("zone serves the earlier record", func() bool { return e.zone("example").Status.ContentHash == want })

	e.del(earlier)
	e.eventually("duplicate cleared", func() bool {
		return cond(e.rrset("team-b", "web").Status.Conditions, v1alpha1.ConditionDegraded) == "False/AsExpected"
	})
	e.eventually("zone healthy", func() bool {
		return cond(e.zone("example").Status.Conditions, v1alpha1.ConditionDegraded) == "False/AsExpected"
	})
}

// TestRecordSetConditions covers OutsideZone, Invalid and the zone
// appearing after its record sets.
func TestRecordSetConditions(t *testing.T) {
	tests := []struct {
		name      string
		rrset     *v1alpha1.DNSRecordSet
		ready     string
		degraded  string
		event     string
		inMessage string
	}{
		{
			name:     "partly outside",
			rrset:    rrsetObj("apps", "x", "example", 0, ep("www.example.com", "A", "192.0.2.1"), ep("www.example.org", "A", "192.0.2.2")),
			ready:    "True/Serving",
			degraded: "True/OutsideZone", event: EventOutsideZone, inMessage: "not in zone example.com.: www.example.org. A",
		},
		{
			name:     "entirely outside",
			rrset:    rrsetObj("apps", "x", "example", 0, ep("www.example.org", "A", "192.0.2.2")),
			ready:    "False/OutsideZone",
			degraded: "True/OutsideZone", event: EventOutsideZone,
		},
		{
			name:     "invalid target",
			rrset:    rrsetObj("apps", "x", "example", 0, ep("www.example.com", "A", "999.0.0.1")),
			ready:    "False/Invalid",
			degraded: "False/AsExpected", event: EventInvalid, inMessage: "not an IPv4 address",
		},
		{
			name:     "healthy",
			rrset:    rrsetObj("apps", "x", "example", 0, ep("www.example.com", "A", "192.0.2.1")),
			ready:    "True/Serving",
			degraded: "False/AsExpected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.start()
			e.apply(tt.rrset)
			e.eventually("zone not found", func() bool {
				return cond(e.rrset("apps", "x").Status.Conditions, v1alpha1.ConditionReady) == "False/ZoneNotFound"
			})
			e.apply(zoneObj("example", "example.com."))
			e.eventually("conditions", func() bool {
				rs := e.rrset("apps", "x")
				return cond(rs.Status.Conditions, v1alpha1.ConditionReady) == tt.ready &&
					cond(rs.Status.Conditions, v1alpha1.ConditionDegraded) == tt.degraded
			})
			rs := e.rrset("apps", "x")
			if rs.Status.ObservedGeneration != rs.Generation {
				t.Fatalf("observedGeneration %d, generation %d", rs.Status.ObservedGeneration, rs.Generation)
			}
			for _, c := range rs.Status.Conditions {
				if c.ObservedGeneration != rs.Generation {
					t.Fatalf("condition %s observedGeneration %d", c.Type, c.ObservedGeneration)
				}
			}
			if tt.inMessage != "" {
				all := conditionText(rs.Status.Conditions)
				if !strings.Contains(all, tt.inMessage) {
					t.Fatalf("messages %q lack %q", all, tt.inMessage)
				}
			}
			e.settle()
			k := key(KindDNSRecordSet, "apps", "x")
			for _, reason := range []string{EventOutsideZone, EventInvalid, EventDuplicated} {
				want := 0
				if reason == tt.event {
					want = 1
				}
				if n := e.eventCount(reason, k); n != want {
					t.Errorf("%s events %d, want %d", reason, n, want)
				}
			}
		})
	}
}

// TestPolicyReferences: missing ClientGroups and BlockLists are reported by
// name and resolve when the objects appear or go away again.
func TestPolicyReferences(t *testing.T) {
	e := newEnv(t)
	e.start()
	pol := &v1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "kids"},
		Spec: v1alpha1.PolicySpec{
			ClientGroupRefs: []string{"kids-devices"},
			BlockListRefs:   []string{"ads"},
			AllowListRefs:   []string{"school"},
		},
	}
	e.apply(pol)
	e.eventually("missing references", func() bool {
		return cond(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded) == "True/MissingReference"
	})
	msg := condition(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded).Message
	for _, want := range []string{`ClientGroup "kids-devices"`, `BlockList "ads"`, `BlockList "school" (allowListRefs)`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q lacks %s", msg, want)
		}
	}
	if got := cond(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionReady); got != "True/Serving" {
		t.Fatalf("Ready %s", got)
	}

	cg := &v1alpha1.ClientGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "kids-devices"},
		Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "192.168.1.0/28"}, {IP: "192.168.1.50"}}}}
	e.apply(cg)
	// Same name in another namespace does not resolve the reference.
	e.apply(&v1alpha1.BlockList{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "ads"},
		Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Inline: []string{"ads.example"}}}}})
	for _, n := range []string{"ads", "school"} {
		e.apply(&v1alpha1.BlockList{ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: n},
			Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Inline: []string{"x.example"}}}}})
	}
	e.eventually("resolved", func() bool {
		return cond(e.policy("home", "kids").Status.Conditions, v1alpha1.ConditionDegraded) == "False/AsExpected"
	})
	var got v1alpha1.ClientGroup
	e.eventually("members", func() bool {
		_ = e.st.Get(context.Background(), store.KeyOf(cg), &got)
		return got.Status.Members == 2 && cond(got.Status.Conditions, v1alpha1.ConditionReady) == "True/Serving"
	})
	e.del(cg)
	e.eventually("missing again", func() bool {
		p := e.policy("home", "kids")
		c := condition(p.Status.Conditions, v1alpha1.ConditionDegraded)
		return c != nil && c.Status == metav1.ConditionTrue && strings.Contains(c.Message, "kids-devices") && !strings.Contains(c.Message, "ads")
	})
}

// TestBlockListConditions: Ready by default; fetch errors reported by the
// listfetcher in status.sources surface as Degraded{ListFetchFailed}, and
// the listfetcher's fields survive the controller's status writes.
func TestBlockListConditions(t *testing.T) {
	e := newEnv(t)
	e.start()
	bl := &v1alpha1.BlockList{ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "ads"},
		Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{URL: "https://lists.example/ads.txt", Name: "ads"}}}}
	e.apply(bl)
	get := func() *v1alpha1.BlockList {
		var out v1alpha1.BlockList
		if err := e.st.Get(context.Background(), store.KeyOf(bl), &out); err != nil {
			t.Fatal(err)
		}
		return &out
	}
	e.eventually("ready", func() bool { return cond(get().Status.Conditions, v1alpha1.ConditionReady) == "True/Serving" })
	e.settle()

	// The listfetcher reports a failure.
	fetched := get()
	fetched.Status.Sources = []v1alpha1.SourceStatus{{Name: "ads", Error: "HTTP 503", Entries: 10}}
	fetched.Status.Entries = 10
	if err := e.st.UpdateStatus(context.Background(), fetched); err != nil {
		t.Fatal(err)
	}
	e.eventually("degraded", func() bool {
		return cond(get().Status.Conditions, v1alpha1.ConditionDegraded) == "True/ListFetchFailed"
	})
	out := get()
	if out.Status.Entries != 10 || len(out.Status.Sources) != 1 || !strings.Contains(condition(out.Status.Conditions, v1alpha1.ConditionDegraded).Message, "ads: HTTP 503") {
		t.Fatalf("status %+v", out.Status)
	}
}

// TestStatusWriteSuppression: once converged, re-applying identical
// objects, status-only echoes and resyncs write nothing.
func TestStatusWriteSuppression(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.ResyncPeriod = 20 * time.Millisecond })
	objs := []store.Object{
		zoneObj("example", "example.com."),
		rrsetObj("apps", "web", "example", 0, ep("www.example.com", "A", "192.0.2.1")),
		rrsetObj("apps", "dup", "example", time.Second, ep("www.example.com", "A", "192.0.2.2")),
		rrsetObj("apps", "bad", "example", 0, ep("bad name", "A", "192.0.2.3")),
		&v1alpha1.ClientGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "cg"}},
		&v1alpha1.BlockList{ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "bl"}, Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Inline: []string{"a.example"}}}}},
		&v1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "p"}, Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"cg", "nope"}}},
	}
	for _, o := range objs {
		e.apply(o)
	}
	e.start()
	e.settle()
	// One write per object: every status was computed once.
	if n := e.st.Writes(""); n != len(objs) {
		t.Fatalf("converging wrote %d statuses, want %d", n, len(objs))
	}
	before := e.st.Writes("")
	resyncs := func() float64 {
		return sumCounter(t, e.ctrl.metrics.resyncs)
	}
	r0 := resyncs()
	for _, o := range objs {
		e.apply(o.DeepCopyObject().(store.Object))
	}
	e.eventually("several resyncs", func() bool { return resyncs() >= r0+10 })
	e.settle()
	if n := e.st.Writes(""); n != before {
		t.Fatalf("%d status writes after convergence", n-before)
	}
	if n := e.eventCount(EventSerialBumped, key(KindDNSZone, "", "example")); n != 1 {
		t.Fatalf("SerialBumped events %d", n)
	}
}

// TestResyncRepairsMissedEvents: a change whose watch event never arrives
// is picked up by the next resync.
func TestResyncRepairsMissedEvents(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.ResyncPeriod = 200 * time.Millisecond })
	e.apply(zoneObj("example", "example.com."))
	e.start()
	e.eventually("first serial", func() bool { return e.zone("example").Status.Serial != 0 })
	e.st.hide.Store(true)
	e.apply(rrsetObj("apps", "web", "example", 0, ep("www.example.com", "A", "192.0.2.1")))
	time.Sleep(50 * time.Millisecond)
	if e.zone("example").Status.RecordCount != 0 {
		t.Fatal("event was not hidden")
	}
	e.st.hide.Store(false)
	e.eventually("resync picked up the record set", func() bool {
		return e.zone("example").Status.RecordCount == 1 &&
			cond(e.rrset("apps", "web").Status.Conditions, v1alpha1.ConditionReady) == "True/Serving"
	})
	// Deletions missed while hidden are pruned at the next list.
	e.st.hide.Store(true)
	e.del(e.rrset("apps", "web"))
	e.st.hide.Store(false)
	e.eventually("resync pruned the deleted record set", func() bool { return e.zone("example").Status.RecordCount == 0 })
}

// TestRetriesFailedWrites: UpdateStatus errors are retried with the rate
// limiter until they succeed; conflicts too.
func TestRetriesFailedWrites(t *testing.T) {
	for _, failErr := range []error{errors.New("apiserver unavailable"), store.ErrConflict} {
		t.Run(failErr.Error(), func(t *testing.T) {
			e := newEnv(t)
			e.st.failErr = failErr
			e.st.failN.Store(3)
			e.apply(zoneObj("example", "example.com."))
			e.start()
			e.eventually("written after retries", func() bool { return e.zone("example").Status.Serial != 0 })
			if e.st.failN.Load() != 0 {
				t.Fatal("failures not consumed")
			}
		})
	}
}

// TestNewTermIsIdempotent: a second run (a new leader term, or a new
// leader) over converged objects writes nothing and bumps no serial.
func TestNewTermIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.apply(zoneObj("example", "example.com."))
	e.apply(rrsetObj("apps", "web", "example", 0, ep("www.example.com", "A", "192.0.2.1")))
	e.apply(rrsetObj("apps", "dup", "example", time.Second, ep("www.example.com", "A", "192.0.2.2")))
	e.start()
	e.settle()
	serial := e.zone("example").Status.Serial
	writes := e.st.Writes("")
	e.stop()

	e.clock.Advance(24 * time.Hour)
	e.start()
	e.settle()
	if n := e.st.Writes(""); n != writes {
		t.Fatalf("new term wrote %d statuses", n-writes)
	}
	if s := e.zone("example").Status.Serial; s != serial {
		t.Fatalf("new term moved the serial %d -> %d", serial, s)
	}
}

// TestZoneCoalescing: a burst of record-set changes inside the window is
// stamped as one serial.
func TestZoneCoalescing(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.ZoneCoalesce = 300 * time.Millisecond })
	e.apply(zoneObj("example", "example.com."))
	e.start()
	e.eventually("first serial", func() bool { return e.zone("example").Status.Serial != 0 })
	e.settle()
	first := e.zone("example").Status.Serial
	zw := e.st.Writes(KindDNSZone)
	var tuples []RecordTuple
	for i := range 50 {
		name := "h" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".example.com"
		e.apply(rrsetObj("apps", "rs-"+name, "example", 0, ep(name, "A", "192.0.2.1")))
		tuples = append(tuples, RecordTuple{Name: name + ".", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}})
	}
	want := zoneHash(t, "example.com.", tuples)
	e.eventually("converged", func() bool { return e.zone("example").Status.ContentHash == want })
	e.settle()
	if n := e.st.Writes(KindDNSZone) - zw; n > 2 {
		t.Fatalf("burst of 50 record sets wrote the zone %d times", n)
	}
	if s := e.zone("example").Status.Serial; s-first > 2 {
		t.Fatalf("burst bumped the serial %d times", s-first)
	}
}

func TestRunTwiceConcurrently(t *testing.T) {
	e := newEnv(t)
	e.start()
	if err := e.ctrl.Run(context.Background()); err == nil {
		t.Fatal("concurrent Run must fail")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("nil store accepted")
	}
}

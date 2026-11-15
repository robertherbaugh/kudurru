package querylog

import (
	"reflect"
	"testing"
)

// TestRawSinkReceivesTheAdmittedEventOnly is the raw override of D372 at
// the collector seam. A raw sink receives every admitted event as it was
// before the transform, with its wire messages, under every privacy mode;
// the other sinks, the primary sink and the live tail receive the
// transformed event without them; an excluded client or domain reaches no
// sink, raw or not; and only a route with a raw sink asks for wire messages.
func TestRawSinkReceivesTheAdmittedEventOnly(t *testing.T) {
	hide, err := NewPrivacy(PrivacyOptions{Mode: PrivacyHideDomainsClients, ExcludeDomains: []string{"*.skip.example"}, ExcludeClients: []string{"192.168.1.99"}})
	if err != nil {
		t.Fatal(err)
	}
	f := newFanFixture(t, nil, hide)
	raw := f.sink("raw", &fanSink{raw: true})
	plain := f.route(hide, nil, spec("out"))
	if plain.RetainsWire() || f.async.RetainsWire() {
		t.Fatal("a route without a raw sink asks for wire messages")
	}
	route := f.route(hide, plain, spec("out"), spec("raw"))
	plain.Release()
	if !route.RetainsWire() || !f.async.RetainsWire() {
		t.Fatal("a route with a raw sink does not ask for wire messages")
	}
	if st := f.status("raw"); !st.Raw {
		t.Fatalf("status of the raw sink: %+v", st)
	}
	wire := &Wire{Query: []byte("query"), Response: []byte("response"), ClientPort: 5353}
	withWire := func(e *Event) *Event { e.Wire = wire; return e }
	route.Enqueue(withWire(ev("192.168.1.77", "secret.example.")))
	route.Enqueue(withWire(ev("192.168.1.77", "a.skip.example.")))
	route.Enqueue(withWire(ev("192.168.1.99", "visible.example.")))
	waitFor(t, "both sinks", func() bool { return len(raw.names()) == 1 && len(f.get("out").names()) == 1 })

	raw.mu.Lock()
	original := raw.batches[0][0]
	raw.mu.Unlock()
	if original.Name != "secret.example." || original.Client.String() != "192.168.1.77" || original.ClientName != "laptop" || original.Wire != wire || original.ID == 0 {
		t.Fatalf("the raw sink did not receive the event as it was: %+v", original)
	}
	out := f.get("out")
	out.mu.Lock()
	transformed := out.batches[0][0]
	out.mu.Unlock()
	if transformed == original || transformed.Name != HiddenName || transformed.Client.String() != "0.0.0.0" || transformed.Wire != nil || transformed.ID != original.ID {
		t.Fatalf("the other sink did not receive the transformed event: %+v", transformed)
	}
	f.primary.mu.Lock()
	primary := f.primary.batches[0][0]
	f.primary.mu.Unlock()
	if primary != transformed || f.tail.Recent(0, 0)[0].Wire != nil {
		t.Fatal("the primary sink or the tail saw wire messages")
	}
	if got := raw.names(); !reflect.DeepEqual(got, []string{"secret.example."}) {
		t.Fatalf("raw sink received %v: excluded clients and domains are never sent", got)
	}

	// Under nothing no per-query event leaves the node, except to the raw
	// sink, whose audited setting is independent of the privacy mode.
	nothing, _ := NewPrivacy(PrivacyOptions{Mode: PrivacyNothing, ExcludeClients: []string{"192.168.1.99"}})
	second := f.route(nothing, route, spec("out"), spec("raw"))
	route.Release()
	second.Enqueue(withWire(ev("192.168.1.77", "totals.example.")))
	second.Enqueue(withWire(ev("192.168.1.99", "excluded.example.")))
	waitFor(t, "the raw sink under nothing", func() bool { return len(raw.names()) == 2 })
	waitFor(t, "primary under nothing", func() bool { _, n := f.primary.count(); return n == 2 })
	if got := out.names(); len(got) != 1 {
		t.Fatalf("a sink that is not raw received %v under nothing", got)
	}
	if got := raw.names(); got[1] != "totals.example." {
		t.Fatalf("raw sink under nothing received %v", got)
	}
	second.Release()
}

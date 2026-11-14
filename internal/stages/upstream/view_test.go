package upstream

import (
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

type viewLease struct{ v *View }

func (l viewLease) Value() any                            { return l.v }
func (l viewLease) Clone() (pipeline.ServingLease, error) { return l, nil }
func (viewLease) Release()                                {}

func TestCapturedResolverDerivesOriginalAndNeverRevivesEpoch(t *testing.T) {
	oldServer, newServer := startFake(t, 11), startFake(t, 22)
	old, err := New("default", []string{oldServer.addr}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Set().Close() }()
	live, err := New("default", []string{newServer.addr}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Set().Close() }()
	v1, err := NewView(old.Set(), nil, false, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	v2, err := NewView(old.Set(), nil, false, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	live.Capture = func(r *pipeline.Request) (*View, error) { return r.Serving.Value().(*View), nil }
	r := request("old.example.", false)
	r.Serving = viewLease{v1}
	p1, err := live.Bind(r)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := live.Serve(r, nil)
	if err != nil || resp.Msg.Answer[0].String() == "" {
		t.Fatal(err)
	}
	if oldServer.calls.Load() != 1 || newServer.calls.Load() != 0 {
		t.Fatal("captured query used live routes")
	}
	sub := *r
	sub.Msg = r.Msg.Copy()
	sub.Msg.Question[0].Name = "alias.example."
	sub.Serving = viewLease{v2}
	derived, err := live.Bind(&sub)
	if err != nil || derived.Scope().Epoch != p1.Scope().Epoch {
		t.Fatal("subquery recaptured resolver", err)
	}
	fresh := request("old.example.", false)
	fresh.Serving = viewLease{v2}
	p2, err := live.Bind(fresh)
	if err != nil || p1.Scope() == p2.Scope() || p1.Current() || !p2.Current() {
		t.Fatal("ABA resolver fence", err)
	}
	live.Capture = nil
	fresh.Resolution = nil
	if _, err = live.Bind(fresh); !errors.Is(err, pipeline.ErrServingCapture) {
		t.Fatal("live fallback", err)
	}
}

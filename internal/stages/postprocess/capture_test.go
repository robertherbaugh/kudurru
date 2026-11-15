package postprocess

import (
	"errors"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
)

type settingsLease struct{}

func (settingsLease) Value() any                              { return struct{}{} }
func (l settingsLease) Clone() (pipeline.ServingLease, error) { return l, nil }
func (settingsLease) Release()                                {}

func TestCapturedPostprocessCannotUseLiveSettings(t *testing.T) {
	s := &Stage{}
	live, err := responsepolicy.Compile(responsepolicy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetSettings(live)
	m := new(dns.Msg)
	m.SetQuestion("captured.example.", dns.TypeA)
	r := &pipeline.Request{Msg: m, Serving: settingsLease{}}
	called := 0
	next := func(req *pipeline.Request) (*pipeline.Response, error) {
		called++
		return pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusForwarded), nil
	}
	if _, err = s.Serve(r, next); !errors.Is(err, pipeline.ErrServingCapture) {
		t.Fatal("live response fallback", err)
	}
	s.Capture = func(*pipeline.Request) (*responsepolicy.Settings, error) { return nil, errors.New("owner failed") }
	if _, err = s.Serve(r, next); err == nil || called != 0 {
		t.Fatal("failed owner reached downstream")
	}
	s.Capture = func(*pipeline.Request) (*responsepolicy.Settings, error) { return live, nil }
	if _, err = s.Serve(r, next); err != nil || called != 1 {
		t.Fatal("valid typed capture failed", err)
	}
}

package authoritative

import (
	"errors"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

type zoneLease struct{ set zone.Set }

func (l zoneLease) Value() any                            { return l.set }
func (l zoneLease) Clone() (pipeline.ServingLease, error) { return l, nil }
func (zoneLease) Release()                                {}

type forbiddenZones struct{}

func (forbiddenZones) Zones() zone.Set { panic("captured transfer consulted live source") }

func TestCapturedTransferUsesOneSetForSOAAndAXFRFallback(t *testing.T) {
	h, z := xfrSource(t, v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessAllow}, 20)
	st := New(forbiddenZones{})
	st.Capture = func(r *pipeline.Request) zone.Set { return r.Serving.Value().(zone.Set) }
	req := ixfrRequest("example.org.", z.Serial()-1, pipeline.ProtoTCP, lanClient, "")
	req.Serving = zoneLease{h.Zones()}
	resp, err := st.Serve(req, nil)
	if err != nil || resp.Transfer == nil {
		t.Fatal("captured IXFR fallback", err)
	}
	msgs := stream(resp)
	if len(msgs) == 0 || msgs[0].Answer[0].(*dns.SOA).Serial != z.Serial() {
		t.Fatal("wrong captured serial")
	}
	chunks, err := st.TransferCaptured(req, "example.org.", z.Serial())
	if err != nil {
		t.Fatal(err)
	}
	first := <-chunks
	if len(first) != 1 || first[0].(*dns.SOA).Serial != z.Serial() {
		t.Fatal("capability did not use captured SOA")
	}
	if _, err = st.Transfer("example.org.", 0); !errors.Is(err, pipeline.ErrServingCapture) {
		t.Fatal("live capability remained available", err)
	}
	st.Capture = nil
	if _, err = st.TransferCaptured(req, "example.org.", 0); !errors.Is(err, pipeline.ErrServingCapture) {
		t.Fatal("missing transfer capture fallback", err)
	}
	resp, err = st.Serve(req, nil)
	if err != nil || resp.Msg.Rcode != dns.RcodeServerFailure || resp.Transfer != nil {
		t.Fatal("missing captured authority fallback", err)
	}
}

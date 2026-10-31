package blocking

import (
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

func TestReconfigureAppliesDefaults(t *testing.T) {
	ix := fixture(t)
	c := newChain(t, ix, OptionsFrom(config.Blocking{Enabled: true, DefaultResponse: "nxdomain", BlockTTL: config.Duration(30 * time.Second)}))
	resp, _ := c.ask(t, "10.0.0.5", "ads.example.", dns.TypeA, false)
	if resp.Msg.Rcode != dns.RcodeNameError {
		t.Fatalf("before: %s", dns.RcodeToString[resp.Msg.Rcode])
	}
	c.stage.Reconfigure(OptionsFrom(config.Blocking{Enabled: true, DefaultResponse: "customIP", BlockTTL: config.Duration(45 * time.Second),
		CustomIP: config.BlockingCustomIP{V4: "192.0.2.53"}}))
	resp, _ = c.ask(t, "10.0.0.5", "ads.example.", dns.TypeA, false)
	if len(resp.Msg.Answer) != 1 || resp.Msg.Answer[0].(*dns.A).A.String() != "192.0.2.53" || resp.Msg.Answer[0].Header().Ttl != 45 {
		t.Fatalf("after Reconfigure: %v", resp.Msg.Answer)
	}
	c.stage.Reconfigure(OptionsFrom(config.Blocking{Enabled: false}))
	resp, _ = c.ask(t, "10.0.0.5", "ads.example.", dns.TypeA, false)
	if resp.Status == pipeline.StatusBlockedList || c.stage.Enabled() {
		t.Fatalf("blocking.enabled false must pass: %v", resp.Status)
	}
}

func TestReconfigureDegradedReadiness(t *testing.T) {
	s, err := New(Options{Index: policy.Static{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true, false} {
		s.Reconfigure(Options{AllowDegraded: allow})
		if ready, _ := s.Ready(); ready != allow {
			t.Fatal("readiness retained startup degraded setting")
		}
	}
	// Readiness and replacement can run concurrently; neither may mutate the
	// original policy index or any query's captured response settings.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			_, _ = s.Ready()
		}
	}()
	for range 100 {
		s.Reconfigure(Options{AllowDegraded: true})
	}
	<-done
}

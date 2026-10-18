package blocking

import (
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
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

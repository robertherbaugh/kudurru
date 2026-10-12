package dnsserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// dropper drops queries for drop-ctl.* with pipeline.ErrDrop (the control
// stage's form) and drop-rl.* with a nil-message rate-limit Response (the
// ingress limiter's form); everything else gets the local answer.
type dropper struct{}

func (dropper) Name() string { return "local" }

func (dropper) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	switch name := req.Question().Name; {
	case strings.HasPrefix(name, "drop-ctl."):
		return nil, pipeline.ErrDrop
	case strings.HasPrefix(name, "drop-rl."):
		return &pipeline.Response{Status: pipeline.StatusBlockedRateLimit}, nil
	}
	return localAnswer{}.Serve(req, next)
}

// TestDropsSendNothing: a dropped query is never answered (no SERVFAIL) on
// any transport: udp writes nothing, tcp/dot close the connection, doq
// resets the stream and doh/doh3 abort the exchange; the next query on a
// fresh exchange is answered.
func TestDropsSendNothing(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: allSpecs(), stages: []pipeline.Stage{dropper{}}})
	for _, tt := range []struct {
		proto    Protocol
		versions []upstream.HTTPVersion
	}{
		{proto: ProtoUDP},
		{proto: ProtoTCP},
		{proto: ProtoDoT},
		{proto: ProtoDoH, versions: []upstream.HTTPVersion{upstream.HTTPVersion2}},
		{proto: ProtoDoH, versions: []upstream.HTTPVersion{upstream.HTTPVersion11}},
		{proto: ProtoDoQ},
		{proto: ProtoDoH3},
	} {
		name := string(tt.proto)
		if len(tt.versions) > 0 {
			name += "/" + string(tt.versions[0])
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, q := range []string{"drop-ctl.example", "drop-rl.example"} {
				start := time.Now()
				var resp *dns.Msg
				var err error
				if tt.proto == ProtoUDP {
					// A short wait instead of dnsproxy's retries: nothing comes.
					resp, _, err = (&dns.Client{Net: "udp", Timeout: 300 * time.Millisecond}).Exchange(query(q), f.addr(ProtoUDP))
				} else {
					resp, err = f.dnsproxyClient(t, tt.proto, tt.versions...).Exchange(context.Background(), query(q))
				}
				if err == nil {
					t.Fatalf("%s: answered %v, want nothing", q, resp)
				}
				if tt.proto != ProtoUDP && time.Since(start) > 2*time.Second {
					t.Fatalf("%s: the exchange must end at once, took %s (%v)", q, time.Since(start), err)
				}
				ok := f.dnsproxyClient(t, tt.proto, tt.versions...)
				resp, err = ok.Exchange(context.Background(), query("example.com"))
				if err != nil {
					t.Fatalf("after a drop: %v", err)
				}
				checkA(t, resp)
			}
		})
	}
}

func TestResolveDropIsServfailForCallersThatNeedAnAnswer(t *testing.T) {
	h := &Handler{Chain: pipeline.MustNew(dropper{}), Logger: quietLogger}
	for _, q := range []string{"drop-ctl.example", "drop-rl.example"} {
		m := h.Resolve(context.Background(), query(q), pipeline.Client{Proto: pipeline.ProtoUDP})
		if m == nil || m.Rcode != dns.RcodeServerFailure {
			t.Fatalf("%s: %v", q, m)
		}
		if got := h.resolveFreshID(context.Background(), query(q), pipeline.Client{Proto: pipeline.ProtoDoQ}); got != nil {
			t.Fatalf("%s: resolveFreshID must report the drop, got %v", q, got)
		}
	}
}

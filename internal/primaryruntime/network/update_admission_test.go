package network_test

import (
	"context"
	"testing"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	network "github.com/robertherbaugh/kudurru/internal/primaryruntime/network"
)

func TestUpdateRejectsUnverifiedContextsAndBoundedFramingBeforeCapture(t *testing.T) {
	f := newFixture(t, public.ZonePolicy{})
	for _, tc := range []struct {
		name                           string
		proto                          pipeline.Proto
		synthetic, canceled, oversized bool
		want                           int
	}{
		{"synthetic", pipeline.ProtoUDP, true, false, false, dns.RcodeRefused},
		{"doh", pipeline.ProtoDoH, false, false, false, dns.RcodeRefused},
		{"doq", pipeline.ProtoDoQ, false, false, false, dns.RcodeRefused},
		{"doh3", pipeline.ProtoDoH3, false, false, false, dns.RcodeRefused},
		{"unknown-transport", pipeline.Proto(255), false, false, false, dns.RcodeRefused},
		{"canceled", pipeline.ProtoTCP, false, true, false, dns.RcodeServerFailure},
		{"record-cap", pipeline.ProtoTCP, false, false, true, dns.RcodeFormatError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			callback, err := network.NewUpdate(network.UpdateOptions{Runtime: f.r, Limits: dynamicupdate.Limits{MaxUpdateRecords: 1}, Capture: func(*pipeline.Request, string) (pr.VerifiedUpdate, error) {
				called = true
				return pr.VerifiedUpdate{}, nil
			}})
			check(t, err)
			ctx := t.Context()
			if tc.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			msg := update(t, "example.", "x.example. 60 IN A 192.0.2.1", false)
			if tc.oversized {
				msg.Ns = append(msg.Ns, dns.Copy(msg.Ns[0]))
			}
			response, err := callback(&pipeline.Request{Ctx: ctx, Msg: msg, Client: pipeline.Client{Proto: tc.proto, Synthetic: tc.synthetic}})
			check(t, err)
			defer response.Close()
			if response.Msg.Rcode != tc.want || called {
				t.Fatal("invalid request reached issuer", response.Msg, called)
			}
		})
	}
}

func TestUpdateMissingCaptureAndErrorCannotEmitWireSuccess(t *testing.T) {
	f := newFixture(t, public.ZonePolicy{})
	for _, capture := range []network.CaptureUpdate{nil, func(*pipeline.Request, string) (pr.VerifiedUpdate, error) {
		return pr.VerifiedUpdate{}, &network.UpdateReject{Rcode: dns.RcodeSuccess}
	}, func(*pipeline.Request, string) (pr.VerifiedUpdate, error) {
		return pr.VerifiedUpdate{}, pr.ErrUnavailable
	}} {
		callback, err := network.NewUpdate(network.UpdateOptions{Runtime: f.r, Capture: capture})
		check(t, err)
		response, err := callback(&pipeline.Request{Ctx: t.Context(), Msg: update(t, "example.", "", false), Client: pipeline.Client{Proto: pipeline.ProtoTCP}})
		check(t, err)
		defer response.Close()
		if response.Msg.Rcode != dns.RcodeServerFailure {
			t.Fatal("missing/error capture emitted success", response.Msg)
		}
	}
}

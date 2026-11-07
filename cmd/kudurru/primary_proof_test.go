package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/miekg/dns"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

type primaryProofStage struct{ check func(*pipeline.Request) error }

func (primaryProofStage) Name() string { return "authoritative" }
func (s primaryProofStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	if err := s.check(req); err != nil {
		return nil, err
	}
	return pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusRefusedACL), nil
}

func TestPrimaryUpdateIngressUsesNativeProofAndPinsAuthority(t *testing.T) {
	keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "update-key.", Algorithm: dnsserver.TSIGHMACSHA256, Secret: []byte("test-update-key-0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	term := replication.Term{ClusterID: "cluster", PrimaryID: "node-a", Epoch: 1, PromotionID: "initial"}
	id := zonekinds.ZoneID{ClusterID: term.ClusterID, ObjectName: "home", UID: "zone-uid", Apex: "example.org."}
	reads := 0
	ingress := &primaryUpdateIngress{authority: func() replication.Authority {
		reads++
		return replication.Authority{Term: term, MemberID: term.PrimaryID, Mode: replication.Primary, Incarnation: 7}
	}}
	results := make(chan error, 1)
	commands := make(chan primaryruntime.VerifiedUpdate, 1)
	stage := primaryProofStage{check: func(req *pipeline.Request) (err error) {
		defer func() { results <- err }()
		command, err := ingress.capture(req, id)
		if err != nil {
			return err
		}
		identity, err := ingress.Verify(req.Ctx, command.Proof, command.Wire)
		if err != nil {
			return err
		}
		key, _ := keys.Identity("update-key.")
		if identity.Principal.Version != key.CredentialVersion || identity.ProviderVersion != key.ProviderVersion || identity.Term != term || identity.AuthorityIncarnation != 7 || identity.Domain != primaryUpdateDomain(id) || identity.RetryID == "" || identity.NotAfter.IsZero() || reads != 1 {
			return errors.New("proof lost pinned authority or credential identity")
		}
		badWire := append([]byte(nil), command.Wire...)
		badWire[0] ^= 1
		if _, err := ingress.Verify(req.Ctx, command.Proof, badWire); err == nil {
			return errors.New("wire change retained proof")
		}
		otherIssuer := &primaryUpdateIngress{authority: ingress.authority}
		if _, err := otherIssuer.Verify(req.Ctx, command.Proof, command.Wire); err == nil {
			return errors.New("issuer mismatch retained proof")
		}
		if _, err := ingress.Verify(req.Ctx, identity, command.Wire); err == nil {
			return errors.New("public identity manufactured a capability")
		}
		if _, err := ingress.Verify(req.Ctx, "update-key.", command.Wire); err == nil {
			return errors.New("key name manufactured a capability")
		}
		follower := &primaryUpdateIngress{authority: func() replication.Authority {
			return replication.Authority{Term: term, MemberID: "node-b", Mode: replication.Follower, Incarnation: 7}
		}}
		if _, err := follower.capture(req, id); err == nil {
			return errors.New("follower minted a writable UPDATE proof")
		}
		native, ok := dnsserver.VerifiedTSIGFor(req, time.Now())
		if !ok || !identity.NotAfter.Equal(native.Deadline()) {
			return errors.New("command lost original signature deadline")
		}
		ingress.now = func() time.Time { return identity.NotAfter }
		_, expired := ingress.Verify(req.Ctx, command.Proof, command.Wire)
		ingress.now = nil
		if expired == nil {
			return errors.New("signature deadline admitted an expired proof")
		}
		// The runtime owns a detached copy even if a later stage changes the
		// original parsed message; the sealed proof remains for original data.
		req.Msg.Ns[0].Header().Ttl++
		if _, err := ingress.Verify(req.Ctx, command.Proof, command.Wire); err != nil {
			return fmt.Errorf("aliased listener message: %w", err)
		}
		commands <- command
		return nil
	}}
	logger := slog.New(slog.DiscardHandler)
	registry, err := dnsserver.NewRegistry(dnsserver.Options{Handler: &dnsserver.Handler{Chain: pipeline.MustNew(stage), Logger: logger}, TSIG: keys, Logger: logger, Sockets: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(dnsserver.Spec{Protocol: dnsserver.ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- registry.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	for deadline := time.Now().Add(5 * time.Second); ; {
		if ready, _ := registry.Ready(); ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native listener not ready")
		}
		time.Sleep(time.Millisecond)
	}
	msg := new(dns.Msg)
	msg.SetUpdate(id.Apex)
	rr, _ := dns.NewRR("host.example.org. 60 IN A 192.0.2.2")
	msg.Insert([]dns.RR{rr})
	msg.SetTsig("update-key.", dns.HmacSHA256, 300, time.Now().Unix())
	client := &dns.Client{Net: "tcp", Timeout: time.Second, TsigProvider: dnsserver.TSIGProvider(keys)}
	response, _, err := client.Exchange(msg, registry.Addr(dnsserver.ProtoTCP).String())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("UPDATE was not dispatched")
	}
	if response.Rcode != dns.RcodeRefused {
		t.Fatalf("handler response: %d", response.Rcode)
	}
	command := <-commands
	// Socket completion cancels the original request lifetime. A retained
	// proof cannot authorize later background mutation work.
	proof := command.Proof.(*primaryUpdateProof)
	select {
	case <-proof.request.Ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("listener retained UPDATE lifetime")
	}
	if _, err := ingress.Verify(context.Background(), command.Proof, command.Wire); err == nil {
		t.Fatal("ended request retained capability")
	}
}

func TestPrimaryUpdateIngressRejectsSyntheticKey(t *testing.T) {
	p := &primaryUpdateIngress{authority: func() replication.Authority { return replication.Authority{} }}
	request := &pipeline.Request{Ctx: context.Background(), Client: pipeline.Client{TSIGKey: "update-key.", Proto: pipeline.ProtoUDP}}
	if _, err := p.capture(request, zonekinds.ZoneID{}); err == nil {
		t.Fatal("unverified key authorized capture")
	}
	if _, err := p.Verify(context.Background(), &primaryUpdateProof{}, nil); err == nil {
		t.Fatal("zero capability authorized verification")
	}
}

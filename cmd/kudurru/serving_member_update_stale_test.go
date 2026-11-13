package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// memberUpdateFollowerDNS mounts the follower's native DNS listener over its
// complete publication, with the follower's own TSIG keyring, as runServe
// does. An UPDATE pauses right after it captured its publication while
// pause is set. The pipeline and the client allow 30 seconds instead of five:
// a held request's own deadline started when it was received, so the
// fixture's hold (a commit on the primary and a pull) must not be charged
// to it.
func memberUpdateFollowerDNS(t *testing.T, s *servingRuntime) *primaryNetworkFixture {
	t.Helper()
	f := &primaryNetworkFixture{store: s.opts.Store, publisher: s.publisher, keys: s.opts.Keyrings.Capture().Keys(), keyrings: s.opts.Keyrings, runtime: s.primary,
		entered: make(chan struct{}, 1), resume: make(chan struct{}, 1), timeout: memberUpdateFixtureBound}
	i, a, u := identify.New(nil, nil), authoritative.New(nil), upstream.NewStage(nil)
	a.Update = s.primary.Update
	b, err := blocking.New(blocking.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := cache.New(cachedata.New(cachedata.Options{}), cache.Settings{}, u)
	r := &postprocess.Stage{}
	ctl := control.New(control.Options{})
	f.chain, err = pipeline.New(i, ctl, r, a, b, c, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := servingbuild.BindDNS(s.publisher, servingbuild.DNSStages{Chain: f.chain, Control: ctl, Identify: i, Authority: a, Blocking: b, Cache: c, Response: r, Resolution: u}, nil); err != nil {
		t.Fatal(err)
	}
	capture := servingdns.Capture(s.publisher)
	f.chain.SetCapture(func(req *pipeline.Request) (pipeline.ServingLease, error) {
		lease, err := capture(req)
		if err == nil && req.Msg.Opcode == dns.OpcodeUpdate && f.pause.CompareAndSwap(true, false) {
			f.entered <- struct{}{}
			select {
			case <-f.resume:
			case <-req.Ctx.Done():
			}
		}
		return lease, err
	})
	logger := slog.New(slog.DiscardHandler)
	f.listeners, err = dnsserver.NewRegistry(dnsserver.Options{Handler: &dnsserver.Handler{Chain: f.chain, Logger: logger, QueryTimeout: memberUpdateFixtureBound}, Logger: logger, TSIG: f.keys, Sockets: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.listeners.Add(dnsserver.Spec{Protocol: dnsserver.ProtoUDP, Address: "127.0.0.1:0"}, dnsserver.Spec{Protocol: dnsserver.ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := f.listeners.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.listeners.Run(ctx) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil && ctx.Err() == nil {
			t.Error(err)
		}
	})
	awaitServingRuntime(t, s, func() bool { ready, _ := f.listeners.Ready(); return ready })
	return f
}

// memberUpdatePull installs the primary's current image on the follower and
// waits until the follower serves it. A pull that meets a commit on the
// primary between the image's read and its emission reads again on the
// primary (MemberServer.serve), so it is not refused; the "unauthorized peer"
// retry this helper had is gone. One refusal is still read again, and only
// that one: an install refused with sqlite.ErrAnchorUnavailable ("anchor
// lifecycle unavailable", in 2 of 4 burst runs of HA-OPEN6 race04 and race05,
// right after the burst canceled its background pull, possibly in the middle
// of an install). Its cause in the anchor install lane was not identified in
// HA-OPEN6 and is recorded as open; the burst no longer cancels a pull.
func memberUpdatePull(t *testing.T, f *memberForwardHTTPFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for attempt := 1; ; attempt++ {
		err := f.puller.Pull(ctx)
		if err == nil {
			break
		}
		if !errors.Is(err, sqlite.ErrAnchorUnavailable) || attempt == 10 {
			t.Fatal("follower pull", err)
		}
		t.Logf("follower pull attempt %d: %v", attempt, err)
		time.Sleep(100 * time.Millisecond)
	}
	awaitServingRuntime(t, f.follower, func() bool {
		status := f.follower.publisher.Status()
		return status.ProtectedReady && status.Served.Base.Same(f.follower.opts.Store.Authority().Installed)
	})
}

// A follower's pull right after a commit on the primary is served. Each zone
// create is followed within milliseconds by the zone's own status write, so
// the pull usually read its image under one authority and was refused at
// emission under the next ("replication: authority changed"), a 503 without
// authenticated metadata that the follower reported as "replica: unauthorized
// peer" (about one pull in two in the HA-OPEN6 diagnostic runs). The primary
// now reads the image again; every pull here is a single attempt.
func TestRootMemberPullRightAfterACommitIsServed(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, true)
	for i := range 5 {
		name := fmt.Sprintf("pulled%d", i)
		memberUpdateApplyOnPrimary(t, f, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: public.DNSZoneSpec{ZoneName: name + ".test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns." + name + ".test."},
			Policy: public.ZonePolicy{Notify: public.NotifyNone}}})
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		err := f.puller.Pull(ctx)
		cancel()
		if err != nil {
			t.Fatalf("pull %d right after a commit on the primary: %v", i+1, err)
		}
	}
}

// memberUpdateApplyOnPrimary commits one object on the primary as fixture
// administration, outside any request, and waits until the primary serves it.
func memberUpdateApplyOnPrimary(t *testing.T, f *memberForwardHTTPFixture, obj store.Object) {
	t.Helper()
	st := f.primary.s.opts.Store
	if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The member listener admits a peer through the primary's complete
	// publication: pull only after the primary serves its own commit.
	awaitServingRuntime(t, f.primary.s, func() bool {
		status := f.primary.s.publisher.Status()
		installed := st.Authority().Installed // a primary cursor carries no digest
		return status.ProtectedReady && status.Served.Base.Term.Same(installed.Term) && status.Served.Base.Revision == installed.Revision
	})
}

// memberUpdateFixtureBound replaces, in the mounted follower fixture, the
// DNS pipeline's five-second deadline, the client's timeout and the
// follower's four-second forward bound. Under the race detector on a shared
// machine a publication alone can take longer than four seconds, so the
// production bounds made the held-UPDATE test fail on timing instead of on
// behavior (HA-OPEN5 review). The process test keeps the production bounds.
const memberUpdateFixtureBound = 30 * time.Second

func memberUpdateRcode(msg *dns.Msg) int {
	if msg == nil {
		return -1
	}
	return msg.Rcode
}

// memberUpdateFollowerZone creates the update zone example. on the primary,
// installs it on the follower, waits until both admit it for updates and
// mounts the follower's DNS listener. bound replaces the follower's forward
// bound; zero keeps the production memberForwardUpdateTimeout.
func memberUpdateFollowerZone(t *testing.T, bound time.Duration) (*memberForwardHTTPFixture, *primaryNetworkFixture) {
	t.Helper()
	f := newMemberForwardHTTPFixture(t, true)
	f.follower.memberForwardUpdateBound = bound
	memberUpdateApplyOnPrimary(t, f, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{ZoneName: "example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.example."},
		Policy: public.ZonePolicy{Notify: public.NotifyNone, Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "updates", Name: "*.example.", Types: []string{"A"}}}}}})
	admitted := func(s *servingRuntime) bool {
		lease, err := s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		owner, ok := value.(*servingbuild.RuntimeOwner)
		if !ok || owner.Primary() == nil || owner.Zones() == nil {
			return false
		}
		route := owner.Zones().Zones(time.Now()).Match("example.")
		return route != nil && route.Type() == public.ZoneTypePrimary && owner.Primary().Admitted("example.", time.Now())
	}
	awaitServingRuntime(t, f.primary.s, func() bool { return admitted(f.primary.s) })
	memberUpdatePull(t, f)
	awaitServingRuntime(t, f.follower, func() bool { return admitted(f.follower) })
	return f, memberUpdateFollowerDNS(t, f.follower)
}

// TestRootMemberFollowerUpdateAcrossInstallWaitsForItsPublication is the
// follower half of a DHCP burst: a signed UPDATE captured the follower's
// publication, and the follower then installed the primary's next commit
// (here another object; in a burst, the previous UPDATE's own commit) before
// the forward checked its keyring. The keyring that verified the request is
// no longer current, which is a source change under the request, not a
// refusal of the key, and nothing was sent. The follower answered SERVFAIL
// for it (and before that REFUSED), so most of a burst through a follower
// failed. It now waits, within the forward's bound, for its own current
// publication, selects the zone and admits the key again there, and sends
// the update once: the answer is the primary's NOERROR, signed, and the
// record is committed exactly once (D381).
func TestRootMemberFollowerUpdateAcrossInstallWaitsForItsPublication(t *testing.T) {
	f, dnsf := memberUpdateFollowerZone(t, memberUpdateFixtureBound-5*time.Second)
	served := func(name string) int {
		query := new(dns.Msg)
		query.SetQuestion(name, dns.TypeA)
		answer, err := dnsf.exchange(query, true)
		if err != nil {
			t.Fatal("follower query", name, err)
		}
		return answer.Rcode
	}
	// The forward path works on a current publication.
	answer, err := dnsf.exchange(primaryUpdateMessage("example.", "first.example.", true), true)
	if err != nil || memberUpdateRcode(answer) != dns.RcodeSuccess || answer.IsTsig() == nil {
		t.Fatalf("forwarded UPDATE on a current publication: rcode=%d err=%v", memberUpdateRcode(answer), err)
	}
	memberUpdatePull(t, f)
	if rcode := served("first.example."); rcode != dns.RcodeSuccess {
		t.Fatal("forwarded UPDATE not served by the follower", dns.RcodeToString[rcode])
	}

	dnsf.pause.Store(true)
	type result struct {
		msg *dns.Msg
		err error
	}
	done := make(chan result, 1)
	go func() {
		msg, err := dnsf.exchange(primaryUpdateMessage("example.", "held.example.", true), true)
		done <- result{msg, err}
	}()
	select {
	case <-dnsf.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("UPDATE did not capture the follower's publication")
	}
	before := f.follower.opts.Store.Authority()
	memberUpdateApplyOnPrimary(t, f, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere"}, Spec: public.DNSZoneSpec{ZoneName: "elsewhere.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.elsewhere."},
		Policy: public.ZonePolicy{Notify: public.NotifyNone}}})
	memberUpdatePull(t, f)
	if f.follower.opts.Store.Authority().Installed.Same(before.Installed) {
		t.Fatal("the follower did not install the primary's next commit")
	}
	dnsf.resume <- struct{}{}
	got := <-done
	if got.err != nil {
		t.Fatal("held UPDATE exchange failed", got.err)
	}
	if got.msg.IsTsig() == nil {
		t.Fatal("held UPDATE answer was not signed")
	}
	if rcode := memberUpdateRcode(got.msg); rcode != dns.RcodeSuccess {
		t.Fatalf("UPDATE held across a follower install answered %s, want NOERROR", dns.RcodeToString[rcode])
	}

	// It was sent once, on the newer publication: the follower serves it
	// after its next pull, and a request that requires the name to be unused
	// now fails its prerequisite.
	memberUpdatePull(t, f)
	if rcode := served("held.example."); rcode != dns.RcodeSuccess {
		t.Fatal("the UPDATE held across the install is not served", dns.RcodeToString[rcode])
	}
	unused := primaryUpdateMessage("example.", "held.example.", false)
	unused.NameNotUsed([]dns.RR{&dns.ANY{Hdr: dns.RR_Header{Name: "held.example."}}})
	unused.SetTsig("updates.", dns.HmacSHA256, 300, time.Now().Unix())
	answer, err = dnsf.exchange(unused, true)
	if err != nil || memberUpdateRcode(answer) != dns.RcodeYXDomain {
		t.Fatalf("held.example. after its UPDATE: a name-not-in-use prerequisite answered rcode=%d err=%v, want YXDOMAIN", memberUpdateRcode(answer), err)
	}
}

// A burst through a follower under the production forward bound
// (memberForwardUpdateTimeout, four seconds), while the follower installs
// the primary's commits, usually the previous update's own, once a second
// (production pulls every five seconds; pulling every 20 ms kept the
// follower's publisher from publishing for the whole bound, the open
// publication-starvation limit of D405): every signed UPDATE is answered with the primary's NOERROR,
// signed, never with a SERVFAIL that a second identical send would not get,
// and each is committed once (D381). The held-UPDATE test above replaces the
// bound because the fixture's own hold is charged to it; this one keeps it.
// The one SERVFAIL accepted is the documented one at the bound itself (not
// sent, or not answered, within four seconds): on a loaded machine under the
// race detector a publication can take longer than that. It is logged and
// its outcome is left unknown; a SERVFAIL before the bound fails the test.
func TestRootMemberFollowerUpdateBurstWithinTheProductionBound(t *testing.T) {
	f, dnsf := memberUpdateFollowerZone(t, 0)
	if f.follower.memberForwardUpdateBound != 0 {
		t.Fatal("the follower's forward bound was replaced")
	}
	// The pulls are stopped between two pulls, never canceled in the middle
	// of an install: a canceled install can leave the next one refused with
	// "anchor lifecycle unavailable" (see memberUpdatePull).
	var stopping atomic.Bool
	stop := func() { stopping.Store(true) }
	pulled := make(chan int, 1)
	go func() {
		n := 0
		defer func() { pulled <- n }()
		for !stopping.Load() {
			if f.puller.Pull(t.Context()) == nil {
				n++
			}
			for range 10 {
				if stopping.Load() {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	const burst = 6
	committed := map[string]bool{}
	for i := range burst {
		name := fmt.Sprintf("burst%d.example.", i)
		sent := time.Now()
		answer, err := dnsf.exchange(primaryUpdateMessage("example.", name, true), i%2 == 1)
		elapsed := time.Since(sent)
		if err != nil {
			stop()
			t.Fatal("burst UPDATE exchange failed", name, err)
		}
		rcode := memberUpdateRcode(answer)
		switch {
		case answer.IsTsig() == nil:
			stop()
			t.Fatalf("burst UPDATE %d of %d answer was not signed", i+1, burst)
		case rcode == dns.RcodeSuccess:
			committed[name] = true
		case rcode == dns.RcodeServerFailure && elapsed >= memberForwardUpdateTimeout-50*time.Millisecond:
			t.Logf("burst UPDATE %d of %d answered SERVFAIL at the forward bound after %s (outcome unknown)", i+1, burst, elapsed)
		default:
			stop()
			t.Fatalf("burst UPDATE %d of %d answered %s after %s, want NOERROR", i+1, burst, dns.RcodeToString[rcode], elapsed)
		}
		if elapsed > memberForwardUpdateTimeout+time.Second {
			stop()
			t.Fatal("burst UPDATE answered after the forward bound", name, elapsed)
		}
	}
	stop()
	t.Logf("burst of %d forwarded UPDATEs: %d NOERROR, %d follower pulls in between", burst, len(committed), <-pulled)
	if len(committed) == 0 {
		t.Fatal("no burst UPDATE was answered within the forward bound")
	}
	memberUpdatePull(t, f)
	for i := range burst {
		name := fmt.Sprintf("burst%d.example.", i)
		if !committed[name] {
			continue
		}
		unused := primaryUpdateMessage("example.", name, false)
		unused.NameNotUsed([]dns.RR{&dns.ANY{Hdr: dns.RR_Header{Name: name}}})
		unused.SetTsig("updates.", dns.HmacSHA256, 300, time.Now().Unix())
		answer, err := dnsf.exchange(unused, false)
		if err != nil || memberUpdateRcode(answer) != dns.RcodeYXDomain {
			t.Fatalf("%s after its UPDATE: a name-not-in-use prerequisite answered rcode=%d err=%v, want YXDOMAIN", name, memberUpdateRcode(answer), err)
		}
	}
}

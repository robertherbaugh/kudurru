package primaryzone

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type materialSource struct {
	m   *zonestate.Materializer
	now func() time.Time
}

func (s materialSource) Zones() zone.Set { return s.m.Capture(s.now()) }
func TestMaterializedProductionStageExpiryAndFrozenRawPrimary(t *testing.T) {
	z := desiredZone()
	rs := desiredSet("live", endpoint("host", "192.0.2.1"))
	deadline := metav1.NewTime(fixedNow.Add(time.Minute))
	rs.Spec.Endpoints[0].Kudurru = &public.EndpointExt{ExpiresAt: &deadline}
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{rs}, Complete: true}
	c := NewCompiler(CompilerOptions{})
	v := compile(t, c, blank(z), in, fixedNow)
	var clock atomic.Int64
	clock.Store(fixedNow.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	m, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted, Now: now})
	if err := m.Load(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	stage := authoritative.New(materialSource{m: m, now: now})
	stage.Now = now
	var forwards int
	next := func(req *pipeline.Request) (*pipeline.Response, error) {
		forwards++
		msg := new(dns.Msg)
		msg.SetReply(req.Msg)
		return &pipeline.Response{Msg: msg}, nil
	}
	query := func(name string) *pipeline.Response {
		t.Helper()
		msg := new(dns.Msg)
		msg.SetQuestion(name, dns.TypeA)
		msg.SetEdns0(1232, true)
		msg.AuthenticatedData = true
		req := &pipeline.Request{Ctx: context.Background(), Msg: msg, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.20:53123")}, Received: now()}
		response, err := stage.Serve(req, next)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	if got := query("host.example."); got.Msg.Rcode != dns.RcodeSuccess || len(got.Msg.Answer) != 1 || got.Msg.AuthenticatedData {
		t.Fatalf("primary response %+v", got)
	}
	captured := m.Capture(now())
	clock.Store(deadline.UnixNano())
	if model := m.Capture(now()).Match("host.example."); model.Type() != public.ZoneTypePrimary || model.SOA() != nil {
		t.Fatal("expiry coverage changed primary membership type or retained data")
	}
	for _, name := range []string{"host.example.", "missing.example."} {
		if got := query(name); got.Msg.Rcode != dns.RcodeServerFailure {
			t.Fatalf("expired %s %+v", name, got.Msg)
		}
	}
	if forwards != 0 {
		t.Fatal("expired coverage escaped to root forwarding")
	}
	if captured.Match("host.example.").Lookup("host.example.", dns.TypeA).Kind != zone.Success {
		t.Fatal("capture joined newer routing")
	}
	if got := query("external.test."); got.Msg.Rcode != dns.RcodeSuccess || forwards != 1 {
		t.Fatal("external positive control")
	}
	// An expiry materialization removes only leased data; primary SOA remains the
	// exact committed serial and next generation answers a truthful negative.
	expired := compile(t, c, v, in, now())
	if err := m.Load(context.Background(), expired); err != nil {
		t.Fatal(err)
	}
	got := query("host.example.")
	if got.Msg.Rcode != dns.RcodeNameError || got.Msg.Ns[0].(*dns.SOA).Serial != expired.Head.Spec.Serial || forwards != 1 {
		t.Fatalf("expiry commit %+v", got.Msg)
	}
}
func TestMaterializerRejectsStaleConflictingRootsAndLifecycleCallbacks(t *testing.T) {
	z := desiredZone()
	rs := desiredSet("source", endpoint("host", "192.0.2.1"))
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{rs}, Complete: true}
	c := NewCompiler(CompilerOptions{})
	first := compile(t, c, blank(z), in, fixedNow)
	m, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted})
	if err := m.Load(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	held := m.Capture(fixedNow)
	rs.Spec.Endpoints[0].Targets = []string{"192.0.2.2"}
	rs.ResourceVersion = "second"
	second := compile(t, c, first, in, fixedNow.Add(time.Second))
	if err := m.Load(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if m.Capture(fixedNow).Match("host.example.").Lookup("host.example.", dns.TypeA).Answer[0].(*dns.A).A.String() != "192.0.2.2" || held.Match("host.example.").Lookup("host.example.", dns.TypeA).Answer[0].(*dns.A).A.String() != "192.0.2.1" {
		t.Fatal("stale callback or mutable capture")
	}
	rs.Spec.Endpoints[0].Targets = []string{"192.0.2.3"}
	rs.ResourceVersion = "different-same-sequence"
	conflicting := compile(t, c, first, in, fixedNow.Add(time.Second))
	if err := m.Load(context.Background(), conflicting); !errors.Is(err, zonestate.ErrInvalid) {
		t.Fatalf("same sequence conflict %v", err)
	}
	if m.Capture(fixedNow).Match("host.example.").AvailableAt(fixedNow) {
		t.Fatal("conflicting sequence did not fail closed")
	}
	newer := compile(t, c, second, in, fixedNow.Add(2*time.Second))
	if err := m.Load(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	if err := m.Retire(newer.Head.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(context.Background(), second); !errors.Is(err, zonestate.ErrConflict) {
		t.Fatalf("retired UID resurrected: %v", err)
	}
	oldID := newer.Head.Spec.Zone
	z.UID = "recreated-uid"
	z.ResourceVersion = "recreated"
	recreated := compile(t, c, blank(z), in, fixedNow)
	if err := m.Load(context.Background(), recreated); !errors.Is(err, zonestate.ErrConflict) {
		t.Fatalf("uncoordinated UID activation %v", err)
	}
	if err := m.ReplaceRetired(context.Background(), oldID, recreated); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(context.Background(), newer); !errors.Is(err, zonestate.ErrConflict) {
		t.Fatalf("old UID after replacement %v", err)
	}
	mismatched := *recreated
	mismatched.Head = recreated.Head.DeepCopy()
	mismatched.Head.Spec.AuthEpoch++
	if _, err := m.Prepare(context.Background(), &mismatched); !errors.Is(err, zonestate.ErrInvalid) {
		t.Fatalf("head policy mismatch %v", err)
	}
}
func TestDesiredWaitersBoundCancellationWakeupAndRecovery(t *testing.T) {
	z := desiredZone()
	rs := desiredSet("source", endpoint("host", "192.0.2.1"))
	token, _ := SourceToken(rs)
	v := compile(t, NewCompiler(CompilerOptions{}), blank(z), &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{rs}, Complete: true}, fixedNow)
	target := zonestate.DesiredTarget{Zone: v.Head.Spec.Zone, Source: token, Operation: "apply"}
	for i := 0; i < 20; i++ {
		waiting, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted})
		ctx, stop := context.WithTimeout(context.Background(), time.Second)
		result := make(chan error, 1)
		go func() {
			got, err := waiting.WaitDesired(ctx, target)
			if err == nil && (!got.Published || got.Outcome != "committed") {
				err = errors.New("wrong acknowledgment")
			}
			result <- err
		}()
		if err := waiting.Load(ctx, v); err != nil {
			stop()
			t.Fatal(err)
		}
		if err := <-result; err != nil {
			stop()
			t.Fatal(err)
		}
		stop()
	}
	// Restart recovers observations solely from durable envelopes; no accepted
	// target history map or source-store lookup is needed.
	cold, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted})
	if err := cold.Load(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	equivalent := target
	equivalent.Source.ResourceVersion = "status-opaque"
	if got, err := cold.WaitDesired(context.Background(), equivalent); err != nil || got.Outcome != "committedEquivalent" || got.ObservedResourceVersion == equivalent.Source.ResourceVersion {
		t.Fatalf("restart equivalent %+v %v", got, err)
	}
}

func TestRegisteredDesiredTargetsTruthfulPendingSupersessionAndCapacity(t *testing.T) {
	z := desiredZone()
	rs := desiredSet("source", endpoint("host", "192.0.2.1"))
	c := NewCompiler(CompilerOptions{})
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{rs}, Complete: true}
	first := compile(t, c, blank(z), in, fixedNow)
	m, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted, MaxWaiters: 1})
	if err := m.Load(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	rs.Spec.Endpoints[0].Targets = []string{"192.0.2.2"}
	rs.ResourceVersion = "new-accepted"
	token, _ := SourceToken(rs)
	target := zonestate.DesiredTarget{Zone: first.Head.Spec.Zone, Source: token, Operation: "apply"}
	if got := m.Desired(target); got.Outcome != "pending" {
		t.Fatalf("older committed intent cannot supersede newly accepted target %+v", got)
	}
	if err := m.RegisterDesired(target); err != nil {
		t.Fatal(err)
	}
	other := target
	other.CorrelationID = "other-request"
	if err := m.RegisterDesired(other); !errors.Is(err, zonestate.ErrCapacity) {
		t.Fatalf("unbounded target registration %v", err)
	}
	// Capture registered targets before reading an actual newer replacement.
	in.Targets = m.PendingTargets(target.Zone)
	rs.Spec.Endpoints[0].Targets = []string{"192.0.2.3"}
	rs.ResourceVersion = "replacement-not-ordered"
	replaced := compile(t, c, first, in, fixedNow.Add(time.Second))
	if err := m.Load(context.Background(), replaced); err != nil {
		t.Fatal(err)
	}
	if got := m.Desired(target); got.Outcome != "superseded" || got.ObservedResourceVersion != "replacement-not-ordered" {
		t.Fatalf("fresh read supersession %+v", got)
	}
	m.ForgetDesired(target)
	if err := m.RegisterDesired(other); err != nil {
		t.Fatal("registration not released", err)
	}
	// A new materializer recovers registered-target evidence from the durable
	// envelope; it does not reinterpret earlier committed data by ordering RVs.
	cold, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted})
	if err := cold.Load(context.Background(), replaced); err != nil {
		t.Fatal(err)
	}
	if got := cold.Desired(target); got.Outcome != "superseded" {
		t.Fatalf("restart supersession %+v", got)
	}
}

func TestMaterializerInitialCoverageAndPinnedMembershipVersion(t *testing.T) {
	z := desiredZone()
	initial := blank(z)
	c := NewCompiler(CompilerOptions{})
	m, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted, Now: func() time.Time { return fixedNow }})
	coverage, err := zone.BuildCommittedUnavailable(z)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cover(initial.Expected.Zone, coverage); err != nil {
		t.Fatal(err)
	}
	covered := m.Capture(fixedNow)
	if got := covered.Match("host.example."); got == nil || got.Type() != public.ZoneTypePrimary || got.AvailableAt(fixedNow) {
		t.Fatal("initial no-data primary coverage")
	}
	version, ok := zonestate.CapturedVersion(covered, z.Spec.ZoneName)
	if !ok || version.Zone != initial.Expected.Zone || version.CommitSequence != 0 {
		t.Fatalf("initial membership %+v", version)
	}
	stage := authoritative.New(materialSource{m: m, now: func() time.Time { return fixedNow }})
	msg := new(dns.Msg)
	msg.SetQuestion("host.example.", dns.TypeA)
	res, err := stage.Serve(&pipeline.Request{Ctx: context.Background(), Msg: msg}, func(*pipeline.Request) (*pipeline.Response, error) {
		t.Error("uninitialized primary fell through to raw/root source")
		return nil, pipeline.ErrNoAnswer
	})
	if err != nil || res.Msg.Rcode != dns.RcodeServerFailure {
		t.Fatalf("initial coverage %v %v", res, err)
	}
	v := compile(t, c, initial, &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{desiredSet("source", endpoint("host", "192.0.2.1"))}, Complete: true}, fixedNow)
	if err := m.Load(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	current := m.Capture(fixedNow)
	live, ok := zonestate.CapturedVersion(current, z.Spec.ZoneName)
	if !ok || live.CommitSequence != v.Head.Spec.CommitSequence || live.RootDigest != v.Head.Spec.RootDigest {
		t.Fatal("captured live ID/version")
	}
	old, _ := zonestate.CapturedVersion(covered, z.Spec.ZoneName)
	if old.CommitSequence != 0 || covered.Match("host.example.").AvailableAt(fixedNow) {
		t.Fatal("old cut joined new membership/data")
	}
	if err := m.Cover(initial.Expected.Zone, coverage); err != nil {
		t.Fatal(err)
	}
	if m.Capture(fixedNow).Match("host.example.").SOA() == nil {
		t.Fatal("desired coverage overwrote committed data")
	}
}

func TestMaterializerRejectsZeroPrebuildAndNonprimaryCoverage(t *testing.T) {
	z := desiredZone()
	base := blank(z)
	m, err := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted})
	if err != nil {
		t.Fatal(err)
	}
	secondaryCoverage, err := zone.BuildUnavailable(z.Spec.ZoneName)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cover(base.Expected.Zone, secondaryCoverage); !errors.Is(err, zonestate.ErrInvalid) {
		t.Fatalf("nonprimary coverage accepted: %v", err)
	}
	if m.Capture(fixedNow).Match("host.example.") != nil {
		t.Fatal("rejected coverage changed membership")
	}
	v := compile(t, NewCompiler(CompilerOptions{}), base, &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{desiredSet("source", endpoint("host", "192.0.2.1"))}, Complete: true}, fixedNow)
	if err := m.Load(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	before, _ := zonestate.CapturedVersion(m.Capture(fixedNow), z.Spec.ZoneName)
	if err := m.Install(context.Background(), v, &zonestate.BuiltPrimary{}); !errors.Is(err, zonestate.ErrInvalid) {
		t.Fatalf("zero prebuild accepted: %v", err)
	}
	after, _ := zonestate.CapturedVersion(m.Capture(fixedNow), z.Spec.ZoneName)
	if before != after || m.Capture(fixedNow).Match("host.example.").Lookup("host.example.", dns.TypeA).Answer[0].(*dns.A).A.String() != "192.0.2.1" {
		t.Fatal("rejected prebuild changed data/version")
	}
}

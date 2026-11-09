//go:build unix

package persistence_test

import (
	"context"
	"crypto/tls"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func promotedProbeFixture(t *testing.T) acquisitionFixture {
	t.Helper()
	f := newAcquisitionFixture(t, nil)
	before := f.mirror.Authority()
	if _, err := f.mirror.Promote(finite(t), replication.PromoteRequest{ExpectedTerm: before.Term,
		ExpectedInstalled: before.Installed, RequestID: "probe-fixture-promotion"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMemberProbeActualTLSFencesOldPrimaryWithoutInstalling(t *testing.T) {
	f := promotedProbeFixture(t)
	before := f.s.Authority()
	if err := f.source.ObserveNextPeer(t.Context()); err != nil {
		t.Fatal("actual promoted peer announcement failed", err)
	}
	a := f.s.Authority()
	if a.Mode != replication.Fenced || a.Writable() || !a.Term.Same(f.mirror.Authority().Term) ||
		!a.Installed.Same(before.Installed) || a.InstallSequence != before.InstallSequence {
		t.Fatal("metadata observation failed to fence or installed data")
	}
	if _, err := f.s.WriterToken(); err == nil {
		t.Fatal("old primary retained write admission")
	}
	if err := f.source.ObserveNextPeer(t.Context()); err != nil || !reflect.DeepEqual(a, f.s.Authority()) {
		t.Fatal("fresh equal-term metadata probe changed authority", err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := open(t, f.path, replication.Standalone)
	if !reflect.DeepEqual(a, restarted.Authority()) {
		t.Fatal("physical restart lost durable peer fence")
	}
}

func TestMemberProbeBusyNeverQueuesBehindPeerExchange(t *testing.T) {
	f := promotedProbeFixture(t)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	original := carried(t, f.s)
	go func() {
		finished <- f.s.Command(original, f.s.Authority().Term, func(context.Context, *sqlite.CommandTx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	defer func() {
		close(release)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	before := f.s.Authority()
	started := time.Now()
	if err := f.source.ObserveNextPeer(t.Context()); err == nil {
		t.Fatal("busy original receiver reported success")
	}
	if time.Since(started) > time.Second || !reflect.DeepEqual(before, f.s.Authority()) {
		t.Fatal("incoming observation queued on a held writer or changed authority")
	}
}

func TestMemberProbeActualFenceSurvivesCanceledEmission(t *testing.T) {
	f := promotedProbeFixture(t)
	connection := peerConnection(t, f.local, f.remote)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	term := f.mirror.Authority().Term
	called := false
	err := f.s.ObserveMemberProbe(ctx, term.ClusterID, f.remote.Trust().MemberID(), term, &connection,
		func(scope context.Context, member string, current replication.Term, check func() error) error {
			called = true
			if member != f.local.Trust().MemberID() || !current.Same(term) || check() != nil {
				t.Fatal("emission lacked actual completed fence")
			}
			cancel()
			if check() == nil || scope.Err() == nil {
				t.Fatal("emission renewed the original canceled request")
			}
			return scope.Err()
		})
	if !called || !errors.Is(err, context.Canceled) || f.s.Authority().Mode != replication.Fenced || !f.s.Authority().Term.Same(term) {
		t.Fatal("output failure erased an actual durable fence", called, err)
	}
}

func TestMemberProbeRefusesUnboundAndForgedAuthority(t *testing.T) {
	f := promotedProbeFixture(t)
	connection := peerConnection(t, f.local, f.remote)
	term := f.mirror.Authority().Term
	before := f.s.Authority()
	called := false
	emit := func(context.Context, string, replication.Term, func() error) error { called = true; return nil }
	for _, tc := range []struct {
		name       string
		ctx        context.Context
		member     string
		connection *tls.ConnectionState
	}{
		{"unbounded", t.Context(), f.remote.Trust().MemberID(), &connection},
		{"self", finite(t), f.local.Trust().MemberID(), &connection},
		{"missing-tls", finite(t), f.remote.Trust().MemberID(), nil},
		{"forged-tls", finite(t), f.remote.Trust().MemberID(), &tls.ConnectionState{HandshakeComplete: true, Version: tls.VersionTLS13}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := f.s.ObserveMemberProbe(tc.ctx, term.ClusterID, tc.member, term, tc.connection, emit); err == nil || called || !reflect.DeepEqual(before, f.s.Authority()) {
				t.Fatal("unbound authority changed actual source", err)
			}
		})
	}
	called = false
	err := f.s.WithMemberProbe(finite(t), f.local.Trust(), "not-enrolled", func(context.Context, *sqlite.MemberAcquisition) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatal("unlisted destination entered an outgoing probe", err)
	}
}

func TestMemberProbeRevocationAfterHandshakeRefusesBothDirections(t *testing.T) {
	f := promotedProbeFixture(t)
	connection := peerConnection(t, f.local, f.remote)
	term := f.mirror.Authority().Term
	if err := f.revoke(t); err != nil {
		t.Fatal(err)
	}
	before := f.s.Authority()
	called := false
	err := f.s.ObserveMemberProbe(finite(t), term.ClusterID, f.remote.Trust().MemberID(), term, &connection,
		func(context.Context, string, replication.Term, func() error) error { called = true; return nil })
	if err == nil || called || !reflect.DeepEqual(before, f.s.Authority()) {
		t.Fatal("revoked peer used retained TLS to fence or emit", err)
	}
	err = f.s.WithMemberProbe(finite(t), f.local.Trust(), f.remote.Trust().MemberID(),
		func(context.Context, *sqlite.MemberAcquisition) error { called = true; return nil })
	if err == nil || called || !reflect.DeepEqual(before, f.s.Authority()) {
		t.Fatal("outgoing probe admitted a revoked peer", err)
	}
}

// This exercises authenticated metadata conflict semantics. It is not public
// three-process conflicting-promotion or explicit branch-recovery acceptance.
func TestMemberProbeEqualEpochConflictRetainsInstalledBranchAcrossRestart(t *testing.T) {
	f := newMemberFixture(t)
	before := f.s.Authority()
	conflicting := before.Term
	conflicting.PrimaryID = f.remote.Trust().MemberID()
	called := false
	err := f.s.ObserveMemberProbe(finite(t), conflicting.ClusterID, conflicting.PrimaryID, conflicting, &f.tls,
		func(_ context.Context, member string, current replication.Term, check func() error) error {
			called = true
			if member != before.MemberID || !current.Same(before.Term) {
				t.Fatal("conflict selected the announced branch for emission")
			}
			return check()
		})
	if err != nil || !called {
		t.Fatal("authenticated conflict observation failed", err)
	}
	got := f.s.Authority()
	if got.Mode != replication.Conflict || got.Writable() || got.Conflict == nil || !got.Conflict.Same(conflicting) ||
		!got.Term.Same(before.Term) || !got.Installed.Same(before.Installed) || got.InstallSequence != before.InstallSequence {
		t.Fatal("conflict metadata selected or installed a branch")
	}
	if _, err := f.s.WriterToken(); err == nil {
		t.Fatal("conflicting peer announcement retained writer admission")
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := open(t, f.path, replication.Standalone)
	if !reflect.DeepEqual(got, restarted.Authority()) {
		t.Fatal("restart lost the conflict or changed installed branch")
	}
}

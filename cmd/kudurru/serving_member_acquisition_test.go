package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Fixture administration installs the first complete enrolled image explicitly.
// Subsequent generations use the actual root listener and acquisition worker.
// This does not stand in for the separate initial remote-join protocol.
func TestRootRetainedMemberAcquisitionPublicationRestartAndRevocation(t *testing.T) {
	primary := newServingRuntimeFixture(t)
	primary.opts.Config.Node.DataDir = t.TempDir()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	open := func() *sqlite.Store {
		s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: primary.opts.Validator,
			InternalKinds: privateStoreKinds(), InitialMode: replication.Follower,
			ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	mirror := open()
	local, err := memberidentity.Create(filepath.Join(primary.opts.Config.Node.DataDir, memberIdentityFile), primary.opts.Store.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := memberidentity.Create(filepath.Join(dir, memberIdentityFile), mirror.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	finite := func() context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		t.Cleanup(cancel)
		return ctx
	}
	write := func(fn func(context.Context, *sqlite.CommandTx) error) {
		writer, err := primary.opts.Store.WriterToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := primary.opts.Store.Command(sqlite.WithWriterToken(finite(), writer), writer.Term(), fn); err != nil {
			t.Fatal(err)
		}
	}
	_, cut, err := primary.opts.Store.ReadServingCandidate(finite())
	if err != nil {
		t.Fatal(err)
	}
	write(func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.StageEnrollmentPrimary(ctx, cut, local, "primary", listener.Addr().String())
	})
	change := func(fn func(context.Context, *sqlite.CommandTx, *persistence.Original) error) {
		ctx := finite()
		writer, err := primary.opts.Store.WriterToken()
		if err != nil {
			t.Fatal(err)
		}
		ctx = sqlite.WithWriterToken(ctx, writer)
		image, err := primary.opts.Store.ReadGeneration(ctx)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := accepted.DecodeRows(image.Payload.Membership, image.Term.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		original, err := persistence.Capture(ctx, owner, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := primary.opts.Store.Command(ctx, writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error { return fn(ctx, tx, original) }); err != nil {
			t.Fatal(err)
		}
	}
	var invitation enrollment.Invitation
	change(func(ctx context.Context, tx *sqlite.CommandTx, original *persistence.Original) error {
		var err error
		_, invitation, err = persistence.Issue(ctx, tx, original, "receiver", "127.0.0.2:8082", time.Now().Add(time.Minute))
		return err
	})
	proposal, err := invitation.Propose(remote)
	if err != nil {
		t.Fatal(err)
	}
	change(func(ctx context.Context, tx *sqlite.CommandTx, original *persistence.Original) error {
		_, err := persistence.Accept(ctx, tx, original, proposal)
		return err
	})
	if err := mirror.Enroll(finite(), primary.opts.Store.Authority().Term); err != nil {
		t.Fatal(err)
	}
	install := captureServingReplica(t, mirror)
	image, err := primary.opts.Store.ReadGeneration(install)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := mirror.ValidateImage(install, *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.InstallReplica(install, validated, replication.InstallOptions{ExpectedTerm: image.Term}); err != nil {
		t.Fatal(err)
	}
	credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{49}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := primary.prepareMemberListener(t.Context(), credentials)
	if err != nil {
		t.Fatal(err)
	}
	server, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: credentials, Enrolled: enrolled})
	if err != nil {
		t.Fatal(err)
	}
	serverContext, stopServer := context.WithCancel(t.Context())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(serverContext, listener) }()
	t.Cleanup(func() {
		stopServer()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("actual member listener did not join")
		}
	})
	start := func() (*servingRuntime, func()) {
		opts := primary.opts
		config := *opts.Config
		config.Node.DataDir, config.Node.Name = dir, "receiver"
		opts.Config, opts.Store = &config, mirror
		s, err := newServingRuntime(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close(context.Background()) })
		setup, stopSetup := context.WithTimeout(t.Context(), 5*time.Second)
		listener, coordinator, err := s.prepareMemberServices(setup, credentials)
		s.member = coordinator
		stopSetup()
		if err != nil || listener == nil || s.member == nil {
			t.Fatal("retained follower did not mount acquisition", err)
		}
		// Setup is already canceled: the network exchange must own its own
		// original context and pure local schema validation lifetime.
		if err := s.member.Pull(finite()); err != nil {
			t.Fatal("setup lifetime leaked into actual acquisition", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("retained acquisition runtime did not join")
			}
			if err := s.Close(context.Background()); err != nil {
				t.Error(err)
			}
		}
		t.Cleanup(stop)
		return s, stop
	}
	mutate := func(key string) replication.Generation {
		write(func(ctx context.Context, tx *sqlite.CommandTx) error {
			return tx.PutSection(ctx, "dedup", replication.Entry{Key: key, Data: []byte(`{"committed":true}`)})
		})
		image, err := primary.opts.Store.ReadGeneration(finite())
		if err != nil {
			t.Fatal(err)
		}
		return image.Generation()
	}
	await := func(s *servingRuntime, expected replication.Generation) {
		awaitServingRuntime(t, s, func() bool {
			lease, err := s.publisher.CaptureHTTP(t.Context())
			if err != nil {
				return false
			}
			defer lease.Release()
			return lease.Bundle().Identity().Base.Same(expected) && mirror.Authority().Installed.Same(expected)
		})
	}
	s, stop := start()
	await(s, image.Generation())
	wanted := mutate("actual-network-successor")
	await(s, wanted) // scheduled Run must acquire, install, and publish this image
	before := mirror.Authority()
	if err := s.member.Pull(finite()); err != nil || mirror.Authority().InstallSequence != before.InstallSequence {
		t.Fatal("actual mounted 304 changed the installed image", err)
	}
	status := s.member.Status(finite())
	if status.LastContact.IsZero() || !status.Installed.Same(wanted) || !status.Served.Same(wanted) || status.Eligible {
		t.Fatal("root acquisition status lost accepted contact or granted promotion", status)
	}
	if _, err := s.member.Promote(finite(), clusterreplica.PromoteRequest{ConfirmNode: "receiver"}); !errors.Is(err, clusterreplica.ErrForbidden) {
		t.Fatal("acquisition worker granted operator promotion", err)
	}
	stop()
	if err := mirror.Close(); err != nil {
		t.Fatal(err)
	}
	wanted = mutate("actual-network-after-physical-restart")
	mirror = open()
	if mirror.Authority().MemberID != remote.Trust().MemberID() {
		t.Fatal("physical restart replaced local identity")
	}
	s, stop = start()
	await(s, wanted)
	retained, err := memberidentity.Load(filepath.Join(dir, memberIdentityFile), remote.Trust().MemberID())
	if err != nil || retained.Trust().Fingerprint() != remote.Trust().Fingerprint() {
		t.Fatal("restart replaced retained private CA", err)
	}
	before = mirror.Authority()
	lastContact := s.member.Status(finite()).LastContact
	change(func(ctx context.Context, tx *sqlite.CommandTx, original *persistence.Original) error {
		_, err := persistence.Revoke(ctx, tx, original, remote.Trust().MemberID())
		return err
	})
	if err := s.member.Pull(finite()); err == nil {
		t.Fatal("revoked member acquired a new image")
	}
	if !reflect.DeepEqual(before, mirror.Authority()) || s.member.Status(finite()).LastContact != lastContact {
		t.Fatal("revoked actual exchange changed installation or successful contact")
	}
	stop()
}

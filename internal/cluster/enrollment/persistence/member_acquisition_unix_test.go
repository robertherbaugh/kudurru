//go:build unix

package persistence_test

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type acquisitionFixture struct {
	memberFixture
	mirror *sqlite.Store
	source *replica.EnrolledSource
}

func installAcquisitionImage(t *testing.T, mirror *sqlite.Store, in *replication.Image) {
	t.Helper()
	ctx := finite(t)
	if _, err := mirror.BindAnchorLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	ctx, err := mirror.CaptureAnchorInstall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := mirror.ValidateImage(ctx, *in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.InstallReplica(ctx, checked, replication.InstallOptions{ExpectedTerm: in.Term, ExpectedInstalled: mirror.Authority().Installed}); err != nil {
		t.Fatal(err)
	}
}

func newAcquisitionFixture(t *testing.T, wrap func(http.Handler) http.Handler) acquisitionFixture {
	t.Helper()
	s, path := initialized(t)
	local := primaryIdentity(t, s)
	mirror := open(t, filepath.Join(t.TempDir(), "mirror.db"), replication.Follower)
	remote, err := memberidentity.Create(filepath.Join(t.TempDir(), "follower.json"), mirror.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	cut := primaryCut(t, s)
	if err := s.Command(carried(t, s), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.StageEnrollmentPrimary(ctx, cut, local, "primary", server.Listener.Addr().String())
	}); err != nil {
		t.Fatal(err)
	}
	ctx, o := original(t, s, func() error { return nil })
	var invitation enrollment.Invitation
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		_, invitation, err = persistence.Issue(ctx, tx, o, "follower", "127.0.0.2:8082", time.Now().Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	proposal, err := invitation.Propose(remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := accept(t, s, proposal); err != nil {
		t.Fatal("enroll retained receiver", err)
	}
	if err := mirror.Enroll(t.Context(), s.Authority().Term); err != nil {
		t.Fatal(err)
	}
	initial := image(t, s)
	installAcquisitionImage(t, mirror, initial)
	f := memberFixture{s: s, path: path, local: local, remote: remote}
	handler, credentials := memberServer(t, f, nil)
	server.Config.Handler = handler.Handler()
	if wrap != nil {
		server.Config.Handler = wrap(server.Config.Handler)
	}
	server.TLS, err = local.ServerTLS([]memberidentity.Trust{remote.Trust()})
	if err != nil {
		t.Fatal(err)
	}
	server.StartTLS()
	source, err := replica.NewEnrolledSource(replica.EnrolledSourceOptions{Store: mirror, Identity: remote, Credentials: credentials,
		Validation: func(context.Context) replication.Validation {
			return replication.Validation{SchemaVersion: initial.StoreSchemaVersion, Kinds: initial.KindManifest, ValidateEntry: runtimeowners.ValidateEntry, ValidateImage: runtimeowners.ValidateImage}
		}})
	if err != nil {
		t.Fatal(err)
	}
	return acquisitionFixture{memberFixture: f, mirror: mirror, source: source}
}

func TestEnrolledSourceActualImage304AndNewGeneration(t *testing.T) {
	f := newAcquisitionFixture(t, nil)
	response, err := f.source.Fetch(t.Context(), "")
	if err != nil || response.Image == nil || !response.Image.Generation().Same(image(t, f.s).Generation()) {
		t.Fatal("actual enrolled source did not acquire complete image", err)
	}
	puller := &follower.Puller{Mirror: f.mirror, Source: f.source}
	before := f.mirror.Authority()
	if err := puller.Pull(t.Context()); err != nil || f.mirror.Authority().InstallSequence != before.InstallSequence || puller.Status().LastContact.IsZero() {
		t.Fatal("actual enrolled 304 lost original no-op/contact", err)
	}
	if err := f.s.Command(carried(t, f.s), f.s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "acquisition-generation", Data: []byte(`{"committed":true}`)})
	}); err != nil {
		t.Fatal(err)
	}
	if err := puller.Pull(t.Context()); err != nil {
		t.Fatal("actual enrolled pull did not install successor", err)
	}
	got, want := image(t, f.mirror), image(t, f.s)
	if !got.Generation().Same(want.Generation()) || !reflect.DeepEqual(got.Payload, want.Payload) || f.mirror.Authority().InstallSequence != before.InstallSequence+1 {
		t.Fatal("actual pull changed complete successor bytes or installation count")
	}
}

func TestMemberAcquisitionActualTLSAndOriginalScope(t *testing.T) {
	f := newAcquisitionFixture(t, nil)
	connection := peerConnection(t, f.remote, f.local)
	var escaped *sqlite.MemberAcquisition
	var escapedContext context.Context
	err := f.mirror.WithMemberAcquisition(finite(t), f.remote.Trust(), func(ctx context.Context, original *sqlite.MemberAcquisition) error {
		if _, err := original.ObserveAuthority(ctx, original.Term()); err == nil {
			t.Fatal("bare term claimed peer authentication")
		}
		if _, err := original.ObserveMemberAuthority(context.Background(), original.Term(), &connection); err == nil {
			t.Fatal("foreign context entered source scope")
		}
		if _, err := original.ObserveMemberAuthority(ctx, original.Term(), &connection); err != nil {
			return err
		}
		if _, err := original.ObserveMemberAuthority(ctx, original.Term(), &connection); err == nil {
			t.Fatal("one acquisition reauthenticated a second response")
		}
		copied := *original
		escaped, escapedContext = &copied, ctx
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := escaped.ObserveMemberAuthority(escapedContext, escaped.Term(), &connection); err == nil {
		t.Fatal("copied escaped scope observed authority")
	}
	if err := f.mirror.WithMemberAcquisition(finite(t), f.remote.Trust(), func(ctx context.Context, original *sqlite.MemberAcquisition) error {
		stale := original.Term()
		stale.Epoch--
		_, _ = original.ObserveMemberAuthority(ctx, stale, &connection)
		return nil
	}); err == nil {
		t.Fatal("ignored invalid term observation granted acquisition success")
	}
	for _, connection := range []*tls.ConnectionState{nil, {HandshakeComplete: true, Version: tls.VersionTLS13}} {
		err := f.mirror.WithMemberAcquisition(finite(t), f.remote.Trust(), func(ctx context.Context, original *sqlite.MemberAcquisition) error {
			_, err := original.ObserveMemberAuthority(ctx, original.Term(), connection)
			return err
		})
		if err == nil {
			t.Fatal("caller-constructed TLS fields authenticated an acquisition")
		}
	}
	wrong, err := memberidentity.Create(filepath.Join(t.TempDir(), "wrong.json"), f.remote.Trust().MemberID())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	for _, ctx := range []context.Context{t.Context(), carried(t, f.s)} {
		if err := f.mirror.WithMemberAcquisition(ctx, f.remote.Trust(), func(context.Context, *sqlite.MemberAcquisition) error { called = true; return nil }); err == nil {
			t.Fatal("unbounded or writer context admitted acquisition")
		}
	}
	if err := f.mirror.WithMemberAcquisition(finite(t), wrong.Trust(), func(context.Context, *sqlite.MemberAcquisition) error { called = true; return nil }); err == nil || called {
		t.Fatal("same-UUID replacement identity entered acquisition", err)
	}
}

func TestMemberAcquisitionExcludesReplacementAndJoinsCanceledExchange(t *testing.T) {
	entered, ended := make(chan struct{}), make(chan struct{})
	f := newAcquisitionFixture(t, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
			close(ended)
		})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := f.source.Fetch(ctx, ""); result <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("actual source did not reach original peer")
	}
	// This independent exchange must expire in the Store queue without
	// issuing a second request or releasing the original held exchange.
	waiting, stopWaiting := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer stopWaiting()
	waited := make(chan error, 1)
	go func() { _, err := f.source.Fetch(waiting, ""); waited <- err }()
	select {
	case err := <-waited:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("queued member acquisition lost its original deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued member acquisition outwaited its original deadline")
	}
	next := f.mirror.Authority().Term
	next.Epoch++
	next.PromotionID = "interposed-observation"
	changed := make(chan error, 1)
	changeContext := finite(t)
	go func() { _, err := f.mirror.ObserveAuthority(changeContext, next); changed <- err }()
	select {
	case err := <-changed:
		t.Fatal("source replacement entered during actual exchange", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("actual exchange lost original cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("actual canceled exchange did not return")
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("actual canceled transport retained its connection")
	}
	select {
	case err := <-changed:
		if err != nil || !f.mirror.Authority().Term.Same(next) {
			t.Fatal("independent replacement failed after original scope release", err)
		}
	case <-time.After(time.Second):
		t.Fatal("independent replacement remained blocked after original scope release")
	}
}

type higherTermWriter struct{ http.ResponseWriter }

func (w higherTermWriter) WriteHeader(status int) {
	var term replication.Term
	raw, err := base64.RawURLEncoding.DecodeString(w.Header().Get(replica.HeaderTerm))
	if err == nil && json.Unmarshal(raw, &term) == nil {
		term.Epoch++
		term.PromotionID = "authenticated-higher-term"
		raw, _ = json.Marshal(term)
		w.Header().Set(replica.HeaderTerm, base64.RawURLEncoding.EncodeToString(raw))
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w higherTermWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestEnrolledSourceHigherTermFencesBeforeBodyAcceptance(t *testing.T) {
	var calls atomic.Int32
	f := newAcquisitionFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); next.ServeHTTP(higherTermWriter{w}, r) })
	})
	before := f.mirror.Authority()
	response, err := f.source.Fetch(t.Context(), "")
	var exchange *replica.ExchangeError
	if !errors.Is(err, replication.ErrCAS) || !errors.As(err, &exchange) || !exchange.Reached || response.Image != nil || calls.Load() != 1 {
		t.Fatal("authenticated higher term continued old acquisition", err)
	}
	a := f.mirror.Authority()
	if a.Mode != replication.Fenced || a.Term.Epoch != before.Term.Epoch+1 || !a.Installed.Same(before.Installed) {
		t.Fatal("higher term failed to fence or replaced installed data", a)
	}
}

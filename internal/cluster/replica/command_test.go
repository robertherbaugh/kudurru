package replica

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type fixtureDomain struct {
	executions atomic.Int64
	fail       bool
	sensitive  bool
}

func (d *fixtureDomain) Authorize(ctx context.Context, t CommandTx, c Command) (context.Context, error) {
	tx := t.State().(*sqlite.CommandTx)
	raw, e := tx.GetSection(ctx, "security", "revoked:"+c.Subject.ID)
	if e != nil && !errors.Is(e, store.ErrNotFound) {
		return nil, e
	}
	if len(raw) > 0 {
		return nil, ErrForbidden
	}
	// Real IAM owners reload the referenced credential and current grants using
	// this same tx. This fixture demonstrates current-state revocation checks.
	return sqlite.WithActor(ctx, c.Subject.ID), nil
}
func (d *fixtureDomain) Execute(ctx context.Context, t CommandTx, _ Command) (Outcome, error) {
	tx := t.State().(*sqlite.CommandTx)
	z := zoneObject("counter")
	e := tx.Get(ctx, store.KeyOf(z), z)
	if e != nil && !errors.Is(e, store.ErrNotFound) {
		return Outcome{}, e
	}
	if errors.Is(e, store.ErrNotFound) {
		z.Spec.DefaultTTL = 100
	} else {
		z.Spec.DefaultTTL++
	}
	result, e := tx.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerAPI})
	if e != nil {
		return Outcome{}, e
	}
	d.executions.Add(1)
	if d.fail {
		return Outcome{}, errors.New("fixture domain failure")
	}
	body, _ := json.Marshal(result)
	if d.sensitive {
		body = json.RawMessage(`{"token":"fixture-sensitive-token"}`)
	}
	return Outcome{Status: 200, Body: body, Sensitive: d.sensitive}, nil
}
func commandFor(s *sqlite.Store, id string) Command {
	now := time.Now().UTC()
	return Command{Format: CommandFormat, Version: 1, ID: id, Hops: 1, ExpectedTerm: s.Authority().Term, IssuedAt: now, NotAfter: now.Add(time.Minute), Operation: "fixture.increment", Subject: SubjectRef{Kind: auth.KindUser, ID: "alice", Provider: "local", CredentialKind: "session", CredentialID: "session-1", IssuedAt: now.Add(-time.Minute)}, Preconditions: []Precondition{}, Payload: json.RawMessage(`{"increment":1}`)}
}
func serviceFor(s *sqlite.Store, d *fixtureDomain) *CommandService {
	return &CommandService{Backend: SQLiteAtomic{Store: s}, Domain: d}
}

func TestProductionAtomicDedupConcurrencyBodyConflictAndCurrentRevocation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, replication.Primary)
	d := &fixtureDomain{}
	service := serviceFor(s, d)
	cmd := commandFor(s, "one-id")
	before := imageOf(t, s)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Go(func() { _, e := service.Execute(ctx, cmd); errs <- e })
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if d.executions.Load() != 1 || imageOf(t, s).Revision != before.Revision+1 || len(imageOf(t, s).Payload.Dedup) != 1 {
		t.Fatal("dedup was not atomically committed once")
	}
	changed := cmd
	changed.Payload = json.RawMessage(`{"increment":2}`)
	if _, e := service.Execute(ctx, changed); !errors.Is(e, ErrDedupConflict) {
		t.Fatalf("changed body accepted under ID: %v", e)
	}
	changed = cmd
	changed.Subject.ID = "bob"
	if _, e := service.Execute(ctx, changed); !errors.Is(e, ErrDedupConflict) {
		t.Fatalf("changed subject accepted under ID: %v", e)
	}
	if e := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "security", replication.Entry{Key: "revoked:alice", Data: json.RawMessage(`{"revoked":true}`)})
	}); e != nil {
		t.Fatal(e)
	}
	if _, e := service.Execute(ctx, cmd); !errors.Is(e, ErrForbidden) {
		t.Fatalf("cached result disclosed after revocation: %v", e)
	}
	if d.executions.Load() != 1 {
		t.Fatal("revoked dedup request executed again")
	}
}

func TestDomainFailureSensitiveResultAndStorageFailureRollback(t *testing.T) {
	for _, kind := range []string{"domain", "sensitive", "storage"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s := openStore(t, replication.Primary)
			before := imageOf(t, s)
			d := &fixtureDomain{fail: kind == "domain", sensitive: kind == "sensitive"}
			service := serviceFor(s, d)
			if kind == "storage" {
				service.MaxResultBytes = 8
			}
			if _, e := service.Execute(ctx, commandFor(s, "rollback")); e == nil {
				t.Fatal("failed domain/result committed")
			}
			if !imageOf(t, s).Generation().Same(before.Generation()) {
				t.Fatal("domain mutation escaped failed dedup transaction")
			}
		})
	}
}

// An owner-provided test codec actually seals outcomes. Production owns its
// keyring; the replica package never creates a duplicate IAM secret authority.
type fixtureCodec struct{ a cipher.AEAD }

func newFixtureCodec(t *testing.T) fixtureCodec {
	t.Helper()
	b, e := aes.NewCipher(bytes.Repeat([]byte{5}, 32))
	if e != nil {
		t.Fatal(e)
	}
	a, e := cipher.NewGCM(b)
	if e != nil {
		t.Fatal(e)
	}
	return fixtureCodec{a}
}
func (c fixtureCodec) Seal(_ context.Context, o Outcome) (json.RawMessage, error) {
	b, e := json.Marshal(o)
	if e != nil {
		return nil, e
	}
	nonce := make([]byte, c.a.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return nil, e
	}
	sealed := c.a.Seal(nonce, nonce, b, nil)
	return json.Marshal(map[string]string{"sealed": base64.RawURLEncoding.EncodeToString(sealed)})
}
func (c fixtureCodec) Open(_ context.Context, raw json.RawMessage) (Outcome, error) {
	var envelope map[string]string
	if e := json.Unmarshal(raw, &envelope); e != nil {
		return Outcome{}, e
	}
	b, e := base64.RawURLEncoding.DecodeString(envelope["sealed"])
	if e != nil || len(b) < c.a.NonceSize() {
		return Outcome{}, ErrProtocol
	}
	b, e = c.a.Open(nil, b[:c.a.NonceSize()], b[c.a.NonceSize():], nil)
	if e != nil {
		return Outcome{}, e
	}
	var o Outcome
	e = json.Unmarshal(b, &o)
	return o, e
}
func TestSealedDedupSurvivesReplicaPromotionAndExpiry(t *testing.T) {
	ctx := context.Background()
	p := openStore(t, replication.Primary)
	d := &fixtureDomain{sensitive: true}
	codec := newFixtureCodec(t)
	service := serviceFor(p, d)
	service.Codec = codec
	cmd := commandFor(p, "issue-token-once")
	first, e := service.Execute(ctx, cmd)
	if e != nil {
		t.Fatal(e)
	}
	image := imageOf(t, p)
	if bytes.Contains(image.Payload.Dedup[0].Data, []byte("fixture-sensitive-token")) {
		t.Fatal("plaintext secret in replicated dedup row")
	}
	mirror := mirrorOf(t, p)
	if _, e := mirror.Promote(ctx, replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: image.Generation(), RequestID: "promote"}); e != nil {
		t.Fatal(e)
	}
	promotedDomain := &fixtureDomain{sensitive: true}
	promotedService := serviceFor(mirror, promotedDomain)
	promotedService.Codec = codec
	cmd.ExpectedTerm = mirror.Authority().Term
	second, e := promotedService.Execute(ctx, cmd)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(second.Body, first.Body) || promotedDomain.executions.Load() != 0 {
		t.Fatal("promotion reissued an inherited sensitive outcome")
	}
	promotedService.Now = func() time.Time { return cmd.NotAfter }
	if _, e := promotedService.Execute(ctx, cmd); !errors.Is(e, ErrExpired) {
		t.Fatalf("expired replay executed: %v", e)
	}
}

func TestHTTPForwardingAmbiguousTimeoutAndExplicitSameIDRetry(t *testing.T) {
	ctx := context.Background()
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	creds := credentials(t, 1)
	d := &fixtureDomain{}
	service := serviceFor(p, d)
	server := serverFor(t, p, creds, mirror.Authority().MemberID, service)
	var drop atomic.Bool
	address := tlsServer(t, creds, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if drop.CompareAndSwap(false, true) {
			record := httptest.NewRecorder()
			server.Handler().ServeHTTP(record, r)
			<-r.Context().Done()
			return
		}
		server.Handler().ServeHTTP(w, r)
	}))
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address}
	client := clientFor(t, mirror, creds, func(o *ClientOptions) { o.Timeout = 100 * time.Millisecond })
	cmd := commandFor(p, "retry-after-lost-response")
	if _, e := client.Write(ctx, peer, cmd); !errors.Is(e, ErrOutcomeUnknown) {
		t.Fatalf("ambiguous outcome falsely definite: %v", e)
	}
	if d.executions.Load() != 1 {
		t.Fatal("fixture did not commit before dropping response")
	}
	out, e := client.Write(ctx, peer, cmd)
	if e != nil || out.Status != 200 {
		t.Fatalf("explicit same-ID retry: %v", e)
	}
	if d.executions.Load() != 1 {
		t.Fatal("lost-response retry executed twice")
	}
	if _, e := client.Write(withForwarded(ctx), peer, cmd); !errors.Is(e, ErrLoop) {
		t.Fatalf("forwarded command forwarded again: %v", e)
	}
	token, e := p.WriterToken()
	if e != nil {
		t.Fatal(e)
	}
	if _, e := client.Write(sqlite.WithWriterToken(ctx, token), peer, cmd); !errors.Is(e, ErrLoop) {
		t.Fatalf("controller work forwarded: %v", e)
	}
	var got v1alpha1.DNSZone
	if e := mirror.Get(ctx, store.Key{Kind: "DNSZone", Name: "counter"}, &got); !errors.Is(e, store.ErrNotFound) {
		t.Fatal("forwarding mutated local mirror")
	}
}

func TestWireCommandsRejectInjectedRightsLoopsExpiryAndBounds(t *testing.T) {
	ctx := context.Background()
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	creds := credentials(t, 1)
	d := &fixtureDomain{}
	server := serverFor(t, p, creds, mirror.Authority().MemberID, serviceFor(p, d))
	server.opts.MaxCommandBytes = 2048
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: tlsServer(t, creds, server.Handler())}
	c := clientFor(t, mirror, creds)
	before := imageOf(t, p)
	for _, test := range []string{"builtinAdmin", "grants", "groups", "hops", "expired", "duplicate", "declared-oversize", "streamed-oversize"} {
		t.Run(test, func(t *testing.T) {
			cmd := commandFor(p, "invalid-wire-"+test)
			if test == "hops" {
				cmd.Hops = 2
			}
			if test == "expired" {
				cmd.IssuedAt = time.Now().Add(-2 * time.Minute)
				cmd.NotAfter = cmd.IssuedAt.Add(time.Minute)
			}
			b, _ := json.Marshal(cmd)
			if test == "builtinAdmin" || test == "grants" || test == "groups" {
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(b, &fields)
				var subject map[string]json.RawMessage
				_ = json.Unmarshal(fields["subject"], &subject)
				subject[test] = json.RawMessage(`true`)
				fields["subject"], _ = json.Marshal(subject)
				b, _ = json.Marshal(fields)
			}
			if test == "duplicate" {
				b = append([]byte(`{"hops":1,`), b[1:]...)
			}
			want := 400
			if test == "expired" {
				want = 409
			}
			if strings.HasSuffix(test, "oversize") {
				b = bytes.Repeat([]byte("x"), 4096)
				want = 413
			}
			address, e := c.address(peer, PathWrite)
			if e != nil {
				t.Fatal(e)
			}
			r, _ := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(b))
			r.Header.Set("Authorization", "Bearer "+creds.Token())
			r.Header.Set(HeaderMember, mirror.Authority().MemberID)
			r.Header.Set(HeaderCluster, peer.ClusterID)
			r.Header.Set("Content-Type", "application/json")
			if test == "streamed-oversize" {
				r.ContentLength = -1
			}
			response, e := c.http.Do(r)
			if e != nil {
				t.Fatal(e)
			}
			defer closeResponse(response)
			if response.StatusCode != want {
				t.Fatalf("status %d want %d", response.StatusCode, want)
			}
		})
	}
	if d.executions.Load() != 0 || !imageOf(t, p).Generation().Same(before.Generation()) {
		t.Fatal("rejected command mutated canonical state")
	}
}

func TestWriteRejectsDifferentExpectedPeerBeforeSending(t *testing.T) {
	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	creds := credentials(t, 1)
	var calls atomic.Int64
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: tlsServer(t, creds, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { calls.Add(1) }))}
	c := clientFor(t, mirror, creds)
	cmd := commandFor(p, "private-command")
	cmd.ExpectedTerm.PrimaryID = "different-primary"
	if _, err := c.Write(context.Background(), peer, cmd); !errors.Is(err, replication.ErrCAS) {
		t.Fatalf("wrong primary: %v", err)
	}
	cmd = commandFor(p, "private-command")
	cmd.ExpectedTerm.ClusterID = "different-cluster"
	if _, err := c.Write(context.Background(), peer, cmd); !errors.Is(err, replication.ErrWrongCluster) {
		t.Fatalf("wrong cluster: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("private command sent to a peer outside its expected authority")
	}
}

type invalidReplayCodec struct {
	plainCodec
	out Outcome
}

func (c invalidReplayCodec) Open(context.Context, json.RawMessage) (Outcome, error) {
	return c.out, nil
}

func TestInvalidUnsealedReplayNeverReturnedOrReexecuted(t *testing.T) {
	s := openStore(t, replication.Primary)
	d := &fixtureDomain{}
	service := serviceFor(s, d)
	cmd := commandFor(s, "retained-outcome")
	if _, err := service.Execute(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	before := imageOf(t, s)
	for _, out := range []Outcome{{Status: 0, Body: json.RawMessage(`{}`)}, {Status: 200, Body: json.RawMessage(`{"x":1,"x":2}`)}, {Status: 200, Body: json.RawMessage(`"` + strings.Repeat("x", 128) + `"`)}} {
		service.Codec = invalidReplayCodec{out: out}
		service.MaxResultBytes = 128
		if _, err := service.Execute(context.Background(), cmd); err == nil {
			t.Fatal("invalid owner-decoded replay accepted")
		}
	}
	if d.executions.Load() != 1 || !imageOf(t, s).Generation().Same(before.Generation()) {
		t.Fatal("failed replay reran command or mutated state")
	}
}

type expiringDomain struct {
	*fixtureDomain
	advance func()
}

func (d expiringDomain) Execute(ctx context.Context, tx CommandTx, c Command) (Outcome, error) {
	out, err := d.fixtureDomain.Execute(ctx, tx, c)
	d.advance()
	return out, err
}

func TestCommandExpiresWhileDomainRunsRollsBack(t *testing.T) {
	s := openStore(t, replication.Primary)
	cmd := commandFor(s, "expire-during-execute")
	before := imageOf(t, s)
	now := cmd.IssuedAt
	d := expiringDomain{fixtureDomain: &fixtureDomain{}, advance: func() { now = cmd.NotAfter }}
	service := &CommandService{Backend: SQLiteAtomic{Store: s}, Domain: d, Now: func() time.Time { return now }}
	if _, err := service.Execute(context.Background(), cmd); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired transaction: %v", err)
	}
	if !imageOf(t, s).Generation().Same(before.Generation()) {
		t.Fatal("expired domain execution committed object/dedup")
	}
}

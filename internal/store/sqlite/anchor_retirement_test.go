package sqlite

import (
	"bytes"
	"context"
	"crypto"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

func anchorTestContext(ctx context.Context, t *testing.T, s *Store) context.Context {
	t.Helper()
	original, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.Cleanup(cancel)
	writer, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	return WithWriterToken(original, writer)
}

func anchorTestBind(t *testing.T, s *Store) *AnchorSource {
	t.Helper()
	source, err := s.BindAnchorLifecycle(anchorTestContext(t.Context(), t, s))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func anchorTestDormant(t *testing.T, s *Store) accepted.Snapshot {
	t.Helper()
	next, err := accepted.NewDormant(s.Authority().Term.ClusterID, strings.Repeat("a", 32), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ctx := anchorTestContext(t.Context(), t, s)
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error { return anchorTestStage(ctx, tx, next) }); err != nil {
		t.Fatal(err)
	}
	return next
}

func anchorTestStage(ctx context.Context, tx *CommandTx, next accepted.Snapshot) error {
	rows, err := accepted.EncodeRows(next)
	if err != nil {
		return err
	}
	old, err := tx.ListSectionPrefix(ctx, accepted.Section, accepted.Prefix)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, row := range rows {
		keep[row.Key] = true
	}
	for _, row := range old {
		if !keep[row.Key] {
			if err := tx.DeleteSection(ctx, accepted.Section, row.Key); err != nil {
				return err
			}
		}
	}
	for _, row := range rows {
		unchanged := false
		for _, prior := range old {
			if prior.Key == row.Key && bytes.Equal(prior.Data, row.Data) {
				unchanged = true
			}
		}
		if !unchanged {
			if err := tx.PutSection(ctx, accepted.Section, row); err != nil {
				return err
			}
		}
	}
	return tx.RequireSectionPrefixUnchanged(ctx, accepted.Section, accepted.Prefix)
}

type anchorTestProof struct {
	next        accepted.Snapshot
	state       trustanchors.State
	observation trustanchors.Observation
	clock       *trustanchors.SessionClock
	key         *dns.DNSKEY
	signer      crypto.Signer
}

func anchorTestBootstrap(t *testing.T, s *Store, dormant accepted.Snapshot) anchorTestProof {
	t.Helper()
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
	private, err := key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	signer := private.(crypto.Signer)
	message := anchorTestSigned(t, key, signer, false)
	state, observation, err := trustanchors.Bootstrap(trustanchors.Seed{ClusterID: s.Authority().Term.ClusterID, PolicyID: "explicit-test-root", DS: []string{key.ToDS(dns.SHA256).String()}}, message, clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	next, err := accepted.ProposeManaged(dormant, state, time.Now().UTC(), observation.ValidUntil())
	if err != nil {
		t.Fatal(err)
	}
	return anchorTestProof{next: next, state: state, observation: observation, clock: clock, key: key, signer: signer}
}

func anchorTestSigned(t *testing.T, key *dns.DNSKEY, signer crypto.Signer, revoke bool) *dns.Msg {
	t.Helper()
	rr := dns.Copy(key).(*dns.DNSKEY)
	if revoke {
		rr.Flags |= dns.REVOKE
	}
	sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 3600}, Algorithm: rr.Algorithm, KeyTag: rr.KeyTag(), SignerName: ".", Inception: uint32(time.Now().Add(-time.Hour).Unix() & 0xffffffff), Expiration: uint32(time.Now().Add(time.Hour).Unix() & 0xffffffff)}
	if err := sig.Sign(signer, []dns.RR{rr}); err != nil {
		t.Fatal(err)
	}
	query := new(dns.Msg)
	query.SetQuestion(".", dns.TypeDNSKEY)
	message := new(dns.Msg)
	message.SetReply(query)
	message.Authoritative = true
	message.Answer = []dns.RR{rr, sig}
	return message
}

func anchorTestManaged(t *testing.T, s *Store, dormant accepted.Snapshot) anchorTestProof {
	t.Helper()
	ctx := anchorTestContext(t.Context(), t, s)
	update, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	proof := anchorTestBootstrap(t, s, dormant) // Actual signed acquisition AFTER capture.
	receipt, err := s.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error {
		if err := proof.observation.CheckBootstrap(proof.state, proof.clock.Sample()); err != nil {
			return err
		}
		if err := tx.RequireCommitCheck(func(context.Context) error { return proof.observation.CheckLifetime(proof.clock.Sample()) }); err != nil {
			return err
		}
		return anchorTestStage(ctx, tx, proof.next)
	})
	if err != nil || receipt.Outcome() != AnchorCommitted {
		t.Fatal("actual admitted bootstrap failed", err)
	}
	version, ok := receipt.Version()
	if !ok || version != proof.next.Version() {
		t.Fatal("missing actual committed receipt")
	}
	return proof
}

func TestAnchorLifecycleLoadsOnlyCommittedOwner(t *testing.T) {
	s := openTemp(t)
	source := anchorTestBind(t, s)
	if !source.cursor.Load().awaiting || source.cursor.Load().gate != nil {
		t.Fatal("sentinel armed from proposal")
	}
	proposed, err := accepted.NewDormant(s.Authority().Term.ClusterID, strings.Repeat("b", 32), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("fixture: startup refused")
	ctx := anchorTestContext(t.Context(), t, s)
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		if err := anchorTestStage(ctx, tx, proposed); err != nil {
			return err
		}
		return refusal
	})
	if !errors.Is(err, refusal) || !source.cursor.Load().awaiting {
		t.Fatal("rolled-back proposal armed source", err)
	}
	current := anchorTestDormant(t, s)
	if source.cursor.Load().owner.Version() != current.Version() || source.cursor.Load().gate == nil {
		t.Fatal("committed initialization not installed")
	}
	rebound, err := s.BindAnchorLifecycle(ctx)
	if err != nil || rebound != source {
		t.Fatal("binding reset committed lifecycle", err)
	}
}

func TestAnchorLifecycleOriginalWriterAndFollower(t *testing.T) {
	s := openTemp(t)
	if _, err := s.BindAnchorLifecycle(t.Context()); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("startup sampled missing original writer", err)
	}
	follower := openTemp(t, func(o *Options) { o.InitialMode = replication.Follower })
	source, err := follower.BindAnchorLifecycle(t.Context())
	if err != nil || !source.cursor.Load().awaiting {
		t.Fatal("read-only follower binding failed", err)
	}
	if _, err := follower.CaptureAnchorUpdate(t.Context(), accepted.Version{}); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("follower gained update", err)
	}
}

func TestAnchorLifecycleManagedReopenRequiresDurableDeny(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := openAt(t, path)
	source := anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	proof := anchorTestManaged(t, s, dormant)
	before := source.cursor.Load()
	if _, err := s.BindAnchorLifecycle(anchorTestContext(t.Context(), t, s)); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("managed Bind inferred absent floor as zero", err)
	}
	if source.cursor.Load() != before {
		t.Fatal("repeat Bind reset live managed cursor")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, path)
	if _, err := reopened.BindAnchorLifecycle(anchorTestContext(t.Context(), t, reopened)); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("managed reopen bypassed durable deny prerequisite", err)
	}
	rows, err := loadAnchorRows(t.Context(), reopened.r)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := accepted.DecodeRows(rows, reopened.Authority().Term.ClusterID)
	if err != nil || restored.Version() != proof.next.Version() {
		t.Fatal("refusal reset durable owner", err)
	}
	// This refusal is NOT full managed restart/follower tombstone acceptance.
	// The original persistence requirement remains pending durable-deny mounting.
}

func TestAnchorLifecycleBoundsBeforeOwnerAllocation(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < accepted.MaxRows; i++ {
		key := accepted.Prefix + "unknown/" + string(rune('A'+i))
		if _, err := s.w.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, accepted.Section, key, `{}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.BindAnchorLifecycle(anchorTestContext(t.Context(), t, s)); !errors.Is(err, replication.ErrTooLarge) {
		t.Fatal("oversized owner was not refused before allocation", err)
	}
	if s.anchors.Load() != nil {
		t.Fatal("invalid durable owner armed lifecycle")
	}
}

func TestAnchorSQLActualTermination(t *testing.T) {
	for _, kind := range []string{"rollback", "unchanged", "commit", "already-rolled-back", "commit-error"} {
		t.Run(kind, func(t *testing.T) {
			s := openTemp(t)
			tx, err := s.w.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			boundary := anchorSQL{tx: tx}
			var result error
			expected := AnchorRolledBack
			switch kind {
			case "unchanged":
				boundary.unchanged = true
				expected = AnchorUnchanged
			case "commit":
				if err := boundary.commit(); err != nil {
					t.Fatal(err)
				}
				expected = AnchorCommitted
			case "already-rolled-back":
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				expected = AnchorUnknown
			case "commit-error":
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				result = boundary.commit()
				expected = AnchorUnknown
			}
			boundary.finish(&result)
			if boundary.outcome != expected {
				t.Fatal("actual transaction outcome misclassified")
			}
			if expected == AnchorUnknown && result == nil {
				t.Fatal("ignored rollback/commit error became confirmed rollback")
			}
			if kind == "already-rolled-back" && !errors.Is(result, sql.ErrTxDone) {
				t.Fatal("lost unconfirmed rollback category")
			}
		})
	}
}

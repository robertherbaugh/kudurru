package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// These fixtures reference the parent's real schema8 fresh/Dormant/local/Bind
// helpers. No fixture migration, copied dependency or old-schema marker rescue
// belongs here; isolated schema7 execution is not parent acceptance evidence.
func anchorPollTestCapture(ctx context.Context, t *testing.T, s *Store, proof anchorTestProof) *AnchorPoll {
	t.Helper()
	p, err := s.CaptureAnchorPoll(ctx, proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal("original pre-acquisition poll capture", err)
	}
	return p
}

func anchorPollTestPack(t *testing.T, message *dns.Msg) []byte {
	t.Helper()
	raw, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func anchorPollTestReceive(t *testing.T, p *AnchorPoll, proof anchorTestProof, revoke bool) *AnchorPollReceipt {
	t.Helper()
	raw := anchorPollTestPack(t, anchorTestSigned(t, proof.key, proof.signer, revoke))
	stamp, err := p.SampleReceipt() // Raw arrival, BEFORE library unpack.
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := p.Receive(raw, stamp)
	if err != nil {
		t.Fatal("original raw authentication", err)
	}
	return receipt
}

func anchorPollTestWait(ctx context.Context, t *testing.T, until time.Time) {
	t.Helper()
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("original context ended before proof boundary", ctx.Err())
	}
}

func TestAnchorPollCaptureRetainsOneActualCutAndAllAttempts(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	ctx := anchorTestContext(t.Context(), t, s)
	p := anchorPollTestCapture(ctx, t, s, proof)
	c := p.cell.denial
	writer, ok := WriterTokenFromContext(ctx)
	if !ok || c.cursor != source.cursor.Load() || c.original != ctx || c.clock != proof.clock || !sameAnchorLocal(c.cursor.local, anchorLocalTestRead(t, s)) {
		t.Fatal("capture did not retain original managed owner/local/writer/clock")
	}
	if c.attempt == nil || p.cell.normal.attempt == nil || p.cell.second.cell.attempt == nil || c.attempt == p.cell.normal.attempt || c.attempt == p.cell.second.cell.attempt || p.cell.normal.attempt == p.cell.second.cell.attempt {
		t.Fatal("three original attempts were not separately precaptured")
	}
	for _, update := range []*anchorUpdate{p.cell.normal, p.cell.second.cell} {
		if update.cursor != c.cursor || update.source != c.source || update.original != ctx || update.writer != writer || update.authorityCut != c.authority || update.installed != &c.installed {
			t.Fatal("an update acquired a different cut")
		}
	}
	if p.cell.normal.revoke != nil || p.cell.second.cell.revoke != c {
		t.Fatal("normal and phase2 pairing confused")
	}
	if !c.installed.Same(imageOf(t, s).Generation()) || c.authority != anchorAuthority(s.Authority()) {
		t.Fatal("capture baseline was not actual committed installation")
	}
	receipt := anchorPollTestReceive(t, p, proof, false)
	result, err := s.AnchorPollCommand(receipt, func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
		return anchorLocalTestAdvance(ctx, tx, observation, proof)
	})
	if err != nil || result.Outcome() != AnchorCommitted {
		t.Fatal("actual normal command", err)
	}
	version, hasVersion := result.Version()
	if !hasVersion || version != source.cursor.Load().owner.Version() || version.Generation != proof.next.Version().Generation+1 {
		t.Fatal("actual completion did not match accepted envelope")
	}
}

func TestAnchorPollCaptureRefusesUnboundedOrChangedOriginal(t *testing.T) {
	s, _, proof := anchorLocalTestManaged(t)
	writer, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	long, stop := context.WithTimeout(t.Context(), time.Hour)
	defer stop()
	if _, err := s.CaptureAnchorPoll(WithWriterToken(long, writer), proof.next.Version(), proof.clock); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("long acquisition lifetime admitted", err)
	}
	if _, err := s.CaptureAnchorPoll(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("capture sampled a primary writer", err)
	}
	ctx := anchorTestContext(t.Context(), t, s)
	if _, err := s.CaptureAnchorPoll(ctx, proof.next.Version(), nil); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("nil clock admitted", err)
	}
	stale := proof.next.Version()
	stale.Generation++
	if _, err := s.CaptureAnchorPoll(ctx, stale, proof.clock); !errors.Is(err, accepted.ErrCAS) {
		t.Fatal("substituted accepted version admitted", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.CaptureAnchorPoll(canceled, proof.next.Version(), proof.clock); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled capture admitted", err)
	}
	if _, err := s.CaptureAnchorPoll(nil, proof.next.Version(), proof.clock); !errors.Is(err, replication.ErrInvalid) { //nolint:staticcheck // Intentionally exercise nil original context refusal.
		t.Fatal("nil original context admitted", err)
	}
	other := openTemp(t, internalOptions)
	_, dormant := anchorLocalTestDormant(t, other)
	if _, err := other.CaptureAnchorPoll(anchorTestContext(t.Context(), t, other), dormant.Version(), proof.clock); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("Dormant used managed poll instead of original Bootstrap", err)
	}
	empty := openTemp(t, internalOptions)
	anchorTestBind(t, empty)
	if _, err := empty.CaptureAnchorPoll(anchorTestContext(t.Context(), t, empty), accepted.Version{}, proof.clock); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("physical awaiting owner became poll state", err)
	}
}

func TestAnchorPollStampOwnerAndCellExcludeEqualSamples(t *testing.T) {
	s, _, proof := anchorLocalTestManaged(t)
	p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
	other := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
	raw := anchorPollTestPack(t, anchorTestSigned(t, proof.key, proof.signer, false))
	stamp, err := p.SampleReceipt()
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic identity-only case: distinct cells carry the SAME genuine
	// Sample. No fabricated clock progress or Observation constructor is used.
	wrongCell := AnchorReceiptStamp{cell: &anchorReceiptStamp{original: p.cell.denial, at: stamp.cell.at}}
	wrongOwner := AnchorReceiptStamp{cell: &anchorReceiptStamp{original: other.cell.denial, at: stamp.cell.at}}
	for _, bad := range []AnchorReceiptStamp{{}, wrongCell, wrongOwner} {
		if _, err := p.Receive(raw, bad); !errors.Is(err, replication.ErrInvalid) {
			t.Fatal("time equality substituted stamp cell/owner", err)
		}
	}
	if _, err := p.SampleReceipt(); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("issued stamp restamped", err)
	}
	copyPoll := *p
	if _, err := copyPoll.SampleReceipt(); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("copied poll issued a new receipt", err)
	}
	copiedStamp := stamp
	if _, err := copyPoll.Receive(raw, copiedStamp); err != nil {
		t.Fatal("foreign refusals consumed rightful original stamp", err)
	}
	if _, err := p.Receive(raw, stamp); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("copied stamp authenticated twice", err)
	}
	if p.cell.denial.input.Load() != anchorReceiptTerminal {
		t.Fatal("raw authentication did not terminally consume shared input")
	}
}

func TestAnchorPollIssuanceLegacyAndRawClaimsAreLinearizable(t *testing.T) {
	s, _, proof := anchorLocalTestManaged(t)
	for round := 0; round < 8; round++ {
		d, err := s.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
		if err != nil {
			t.Fatal(err)
		}
		message := anchorTestSigned(t, proof.key, proof.signer, true)
		raw := anchorPollTestPack(t, message)
		start := make(chan struct{})
		type issuance struct {
			stamp AnchorReceiptStamp
			err   error
		}
		stamps := make(chan issuance, 1)
		legacy := make(chan error, 1)
		go func() { <-start; stamp, err := d.SampleReceipt(); stamps <- issuance{stamp, err} }()
		go func() { <-start; _, err := d.Verify(message); legacy <- err }()
		close(start)
		var issued issuance
		var legacyErr error
		select {
		case issued = <-stamps:
		case <-d.cell.original.Done():
			t.Fatal("issuance race exceeded original lifetime")
		}
		select {
		case legacyErr = <-legacy:
		case <-d.cell.original.Done():
			t.Fatal("legacy race exceeded original lifetime")
		}
		if issued.err == nil {
			if !errors.Is(legacyErr, retirement.ErrUsed) {
				t.Fatal("legacy claimed an issued raw lane", legacyErr)
			}
			var winners atomic.Int32
			var invalid atomic.Int32
			var done sync.WaitGroup
			done.Add(4)
			for worker := 0; worker < 4; worker++ {
				go func() {
					defer done.Done()
					_, err := d.VerifyReceived(raw, issued.stamp)
					if err == nil {
						winners.Add(1)
					} else if !errors.Is(err, retirement.ErrUsed) {
						invalid.Add(1)
					}
				}()
			}
			finished := make(chan struct{})
			go func() { done.Wait(); close(finished) }()
			select {
			case <-finished:
			case <-d.cell.original.Done():
				t.Fatal("raw race exceeded original lifetime")
			}
			if winners.Load() != 1 || invalid.Load() != 0 {
				t.Fatal("raw receipt authenticated more/less than once", winners.Load(), invalid.Load())
			}
		} else if !errors.Is(issued.err, retirement.ErrUsed) || legacyErr != nil {
			t.Fatal("legacy/issuance had no unique winner", issued.err, legacyErr)
		}
		if d.cell.input.Load() != anchorReceiptTerminal {
			t.Fatal("shared input did not end Terminal")
		}
		if _, err := d.SampleReceipt(); !errors.Is(err, retirement.ErrUsed) {
			t.Fatal("terminal verification reissued receipt", err)
		}
	}
}

func TestAnchorPollRawBoundsAndFailedInputAreTerminal(t *testing.T) {
	for _, kind := range []string{"empty", "short", "oversized", "questions", "answers", "missing-record", "trailing", "second-frame", "compression-loop", "authority", "additional", "rdata-length"} {
		t.Run(kind, func(t *testing.T) {
			s, source, proof := anchorLocalTestManaged(t)
			p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
			raw := anchorPollTestPack(t, anchorTestSigned(t, proof.key, proof.signer, false))
			valid := slices.Clone(raw)
			switch kind {
			case "empty":
				raw = nil
			case "short":
				raw = raw[:11]
			case "oversized":
				raw = make([]byte, 65536)
			case "questions":
				binary.BigEndian.PutUint16(raw[4:6], 2)
			case "answers":
				binary.BigEndian.PutUint16(raw[6:8], 129)
			case "missing-record":
				binary.BigEndian.PutUint16(raw[6:8], 3)
			case "trailing":
				raw = append(raw, 0)
			case "second-frame":
				raw = append(raw, valid...)
			case "compression-loop":
				raw[12], raw[13] = 0xc0, 12
			case "authority":
				binary.BigEndian.PutUint16(raw[8:10], 1)
			case "additional":
				binary.BigEndian.PutUint16(raw[10:12], 2)
			case "rdata-length":
				_, questionEnd, err := dns.UnpackDomainName(raw, 12)
				if err != nil {
					t.Fatal(err)
				}
				_, recordStart, err := dns.UnpackDomainName(raw, questionEnd+4)
				if err != nil {
					t.Fatal(err)
				}
				binary.BigEndian.PutUint16(raw[recordStart+8:recordStart+10], 65535)
			}
			stamp, err := p.SampleReceipt()
			if err != nil {
				t.Fatal(err)
			}
			before := source.cursor.Load()
			if _, err := p.Receive(raw, stamp); err == nil {
				t.Fatal("malformed/bound input authenticated")
			}
			if _, err := p.Receive(valid, stamp); !errors.Is(err, retirement.ErrUsed) {
				t.Fatal("malformed first response acquired replacement", err)
			}
			if source.cursor.Load() != before || source.closed.Load() {
				t.Fatal("unauthenticated malformed bytes changed managed source")
			}
		})
	}
}

func TestAnchorPollAuthenticationAndDetachedCompressedWire(t *testing.T) {
	for _, kind := range []string{"compressed", "signature", "question", "alias", "truncated", "signer", "expiry"} {
		t.Run(kind, func(t *testing.T) {
			s, source, proof := anchorLocalTestManaged(t)
			p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
			message := anchorTestSigned(t, proof.key, proof.signer, false)
			switch kind {
			case "compressed":
				message.Compress = true
			case "signature":
				message.Answer[1].(*dns.RRSIG).Signature = "AAAA"
			case "question":
				message.Question[0].Name = "elsewhere."
			case "alias":
				message.Answer = append(message.Answer, &dns.CNAME{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 3600}, Target: "elsewhere."})
			case "truncated":
				message.Truncated = true
			case "signer":
				message.Answer[1].(*dns.RRSIG).SignerName = "elsewhere."
			case "expiry":
				message.Answer[1].(*dns.RRSIG).Expiration = uint32(time.Now().Add(-time.Hour).Unix() & 0xffffffff)
			}
			raw := anchorPollTestPack(t, message)
			stamp, err := p.SampleReceipt()
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := p.Receive(raw, stamp)
			if kind != "compressed" {
				if err == nil || receipt != nil || source.closed.Load() {
					t.Fatal("unauthenticated message became accepted evidence/denial", err)
				}
				return
			}
			if err != nil || receipt.Branch() != AnchorPollNormal {
				t.Fatal("valid compressed message refused", err)
			}
			clear(raw) // Neither parsed wire nor original TTL aliases caller bytes.
			result, err := s.AnchorPollCommand(receipt, func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
				return anchorLocalTestAdvance(ctx, tx, observation, proof)
			})
			if err != nil || result.Outcome() != AnchorCommitted {
				t.Fatal("mutated caller buffer changed retained proof", err)
			}
		})
	}
}

func TestAnchorPollRawHeldProofCannotBeRestamped(t *testing.T) {
	s, _, proof := anchorLocalTestManaged(t)
	p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
	message := anchorTestSigned(t, proof.key, proof.signer, false)
	for _, rr := range message.Answer {
		rr.Header().Ttl = 1
	}
	raw := anchorPollTestPack(t, message)
	stamp, err := p.SampleReceipt()
	if err != nil {
		t.Fatal(err)
	}
	// Establish the actual original expiry for this fixture's wait only. The
	// driver itself still authenticates exactly once using the same stamp.
	known, err := trustanchors.Verify(proof.state, message, stamp.cell.at)
	if err != nil {
		t.Fatal(err)
	}
	anchorPollTestWait(p.cell.denial.original, t, known.ValidUntil())
	if _, err := p.Receive(raw, stamp); !errors.Is(err, trustanchors.ErrExpired) {
		t.Fatal("held raw receipt renewed TTL", err)
	}
	if _, err := p.SampleReceipt(); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("expired receipt restamped", err)
	}
}

func TestAnchorPollNormalExpiryAtEntryAndFinal(t *testing.T) {
	for _, boundary := range []string{"entry", "final"} {
		t.Run(boundary, func(t *testing.T) {
			s, source, proof := anchorLocalTestManaged(t)
			p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
			message := anchorTestSigned(t, proof.key, proof.signer, false)
			for _, rr := range message.Answer {
				rr.Header().Ttl = 1
			}
			raw := anchorPollTestPack(t, message)
			stamp, err := p.SampleReceipt()
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := p.Receive(raw, stamp)
			if err != nil {
				t.Fatal(err)
			}
			before := source.cursor.Load()
			image := imageOf(t, s).Generation()
			if boundary == "entry" {
				anchorPollTestWait(p.cell.denial.original, t, receipt.cell.observation.ValidUntil())
			}
			called := false
			result, err := s.AnchorPollCommand(receipt, func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
				called = true
				if err := anchorLocalTestAdvance(ctx, tx, observation, proof); err != nil {
					return err
				}
				anchorPollTestWait(ctx, t, observation.ValidUntil())
				return nil
			})
			if !errors.Is(err, trustanchors.ErrExpired) || result.Outcome() != AnchorRolledBack || called != (boundary == "final") {
				t.Fatal("original proof expiry was bypassed", result.Outcome(), called, err)
			}
			if source.cursor.Load() != before || !sameAnchorLocal(before.local, anchorLocalTestRead(t, s)) || !image.Same(imageOf(t, s).Generation()) {
				t.Fatal("expired proposal changed accepted checkpoint/local/image")
			}
		})
	}
}

func TestAnchorPollAuthenticatedRevokeExpiryClosesWithoutDurableCredit(t *testing.T) {
	for _, boundary := range []string{"post-auth", "before-denial"} {
		t.Run(boundary, func(t *testing.T) {
			s, source, proof := anchorLocalTestManaged(t)
			p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
			message := anchorTestSigned(t, proof.key, proof.signer, true)
			ttl := uint32(0)
			if boundary == "before-denial" {
				ttl = 1
			}
			for _, rr := range message.Answer {
				rr.Header().Ttl = ttl
			}
			raw := anchorPollTestPack(t, message)
			stamp, err := p.SampleReceipt()
			if err != nil {
				t.Fatal(err)
			}
			known, err := trustanchors.Verify(proof.state, message, stamp.cell.at)
			if err != nil || len(known.Revocations()) != 1 {
				t.Fatal("fixture did not carry a real self-signature", err)
			}
			before := source.cursor.Load()
			image := imageOf(t, s).Generation()
			receipt, err := p.Receive(raw, stamp)
			if boundary == "post-auth" {
				if !errors.Is(err, trustanchors.ErrExpired) || receipt != nil {
					t.Fatal("expired authenticated revoke escaped receipt gate", err)
				}
			} else {
				if err != nil || receipt.Branch() != AnchorPollRevoke {
					t.Fatal(err)
				}
				anchorPollTestWait(p.cell.denial.original, t, known.ValidUntil())
				result, continuation, err := s.CommitAnchorPollDenial(receipt)
				if !errors.Is(err, trustanchors.ErrExpired) || continuation != nil || result.Outcome() != AnchorRolledBack {
					t.Fatal("expired revoke credited durability", err)
				}
			}
			if !source.closed.Load() || !s.anchorBlocked.Load() || source.cursor.Load() != before || !sameAnchorLocal(before.local, anchorLocalTestRead(t, s)) || !image.Same(imageOf(t, s).Generation()) {
				t.Fatal("expired known revoke reopened/changed durable accepted state")
			}
		})
	}
}

func TestAnchorPollExclusiveBranchesAndOwnConfirmedPhase2(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
	receipt := anchorPollTestReceive(t, p, proof, true)
	if receipt.Branch() != AnchorPollRevoke || (&AnchorPollReceipt{}).Branch() != AnchorPollInvalid {
		t.Fatal("invalid/self-revoke branch confused with normal")
	}
	called := false
	fn := func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
		called = true
		return anchorLocalTestAdvance(ctx, tx, observation, proof)
	}
	if _, err := s.AnchorPollCommand(receipt, fn); !errors.Is(err, replication.ErrInvalid) || called {
		t.Fatal("revoke used ordinary lane", err)
	}
	foreign := openTemp(t, internalOptions)
	if _, _, err := foreign.CommitAnchorPollDenial(receipt); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("foreign Store accepted receipt", err)
	}
	if _, err := s.AnchorCommand(p.cell.second, func(context.Context, *CommandTx) error { called = true; return nil }); !errors.Is(err, replication.ErrInvalid) || called {
		t.Fatal("paired update bypassed own effect", err)
	}
	before := source.cursor.Load()
	image := imageOf(t, s).Generation()
	phase1, continuation, err := s.CommitAnchorPollDenial(receipt)
	if err != nil || phase1.Outcome() != AnchorCommitted || continuation == nil || called {
		t.Fatal("original denial did not commit first", err)
	}
	denied := source.cursor.Load()
	if denied.owner.Version() != before.owner.Version() || !image.Same(imageOf(t, s).Generation()) || denied.local.record.RetiredThrough != proof.next.Version().Generation || len(denied.local.record.Denied) != 1 {
		t.Fatal("phase1 changed canonical history or lost inclusive denial")
	}
	if _, err := foreign.AnchorPollCommandAfterDenial(continuation, fn); !errors.Is(err, replication.ErrInvalid) || called {
		t.Fatal("foreign continuation accepted", err)
	}
	copied := *continuation
	phase2, err := s.AnchorPollCommandAfterDenial(continuation, fn)
	if err != nil || phase2.Outcome() != AnchorCommitted || !called {
		t.Fatal("paired original phase2 refused", err)
	}
	final := source.cursor.Load()
	state, err := final.owner.State()
	if err != nil || !state.Inspect().Quarantined || state.Inspect().Keys[0].Status != trustanchors.Revoked || !slices.Equal(final.local.record.Denied, denied.local.record.Denied) {
		t.Fatal("canonical tombstone/quarantine or first denial lost", err)
	}
	called = false
	if _, err := s.AnchorPollCommandAfterDenial(&copied, fn); !errors.Is(err, retirement.ErrUsed) || called {
		t.Fatal("copied continuation replayed", err)
	}
	if _, _, err := s.CommitAnchorPollDenial(receipt); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("denial receipt replayed", err)
	}
}

func TestAnchorPollNormalWrongBranchCallbackRollbackAndReuse(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
	receipt := anchorPollTestReceive(t, p, proof, false)
	if _, _, err := s.CommitAnchorPollDenial(receipt); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("normal receipt invented denial", err)
	}
	before := source.cursor.Load()
	originalError := errors.New("fixture original callback refused")
	result, err := s.AnchorPollCommand(receipt, func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
		if err := anchorLocalTestAdvance(ctx, tx, observation, proof); err != nil {
			return err
		}
		return originalError
	})
	if !errors.Is(err, originalError) || result.Outcome() != AnchorRolledBack || source.cursor.Load() != before || !sameAnchorLocal(before.local, anchorLocalTestRead(t, s)) {
		t.Fatal("callback refusal accepted staged progress", err)
	}
	if _, err := s.AnchorPollCommand(receipt, func(context.Context, *CommandTx, trustanchors.Observation) error {
		t.Error("replayed callback")
		return nil
	}); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("rolled-back receipt was reusable", err)
	}
}

func TestAnchorPollOriginalBaseRefusesInterposedEffects(t *testing.T) {
	for _, kind := range []string{"writer", "installed", "local-denial"} {
		t.Run(kind, func(t *testing.T) {
			s, source, proof := anchorLocalTestManaged(t)
			p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
			receipt := anchorPollTestReceive(t, p, proof, false)
			switch kind {
			case "writer":
				if _, err := s.EnablePrimary(t.Context(), imageOf(t, s).Generation()); err != nil {
					t.Fatal(err)
				}
			case "installed":
				ctx := anchorTestContext(t.Context(), t, s)
				if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
					return tx.PutSection(ctx, "runtime", replication.Entry{Key: "poll-fixture/independent", Data: []byte(`{"changed":true}`)})
				}); err != nil {
					t.Fatal(err)
				}
				if source.cursor.Load() != p.cell.denial.cursor {
					t.Fatal("installed-only fixture also changed source cursor")
				}
			case "local-denial":
				d, err := s.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
				if err != nil {
					t.Fatal(err)
				}
				signed := anchorTestSigned(t, proof.key, proof.signer, true)
				raw := anchorPollTestPack(t, signed)
				stamp, err := d.SampleReceipt()
				if err != nil {
					t.Fatal(err)
				}
				verified, err := d.VerifyReceived(raw, stamp)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.CommitAnchorDenial(verified); err != nil {
					t.Fatal(err)
				}
			}
			before := source.cursor.Load()
			called := false
			result, err := s.AnchorPollCommand(receipt, func(context.Context, *CommandTx, trustanchors.Observation) error { called = true; return nil })
			if err == nil || called || result.Outcome() != AnchorRolledBack || source.cursor.Load() != before {
				t.Fatal("interposed cut was adopted", err)
			}
		})
	}
}

func TestAnchorPollPhase2CannotAdoptCancellationOrAnotherDenial(t *testing.T) {
	for _, kind := range []string{"cancel", "writer", "same-denial"} {
		t.Run(kind, func(t *testing.T) {
			s, source, proof := anchorLocalTestManaged(t)
			ctx, cancel := context.WithCancel(anchorTestContext(t.Context(), t, s))
			defer cancel()
			p := anchorPollTestCapture(ctx, t, s, proof)
			receipt := anchorPollTestReceive(t, p, proof, true)
			phase1, continuation, err := s.CommitAnchorPollDenial(receipt)
			if err != nil || continuation == nil || phase1.Outcome() != AnchorCommitted {
				t.Fatal(err)
			}
			first := source.cursor.Load().local
			switch kind {
			case "cancel":
				cancel()
			case "writer":
				if _, err := s.EnablePrimary(t.Context(), imageOf(t, s).Generation()); err != nil {
					t.Fatal(err)
				}
			case "same-denial":
				d, err := s.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
				if err != nil {
					t.Fatal(err)
				}
				raw := anchorPollTestPack(t, anchorTestSigned(t, proof.key, proof.signer, true))
				stamp, err := d.SampleReceipt()
				if err != nil {
					t.Fatal(err)
				}
				verified, err := d.VerifyReceived(raw, stamp)
				if err != nil {
					t.Fatal(err)
				}
				result, _, err := s.CommitAnchorDenial(verified)
				if err != nil || result.Outcome() != AnchorUnchanged || !sameAnchorLocal(first, anchorLocalTestRead(t, s)) {
					t.Fatal("identical denial fixture did not confirm same bytes", err)
				}
			}
			before := source.cursor.Load()
			called := false
			result, err := s.AnchorPollCommandAfterDenial(continuation, func(context.Context, *CommandTx, trustanchors.Observation) error { called = true; return nil })
			if err == nil || called || result.Outcome() != AnchorRolledBack || source.cursor.Load() != before || !sameAnchorLocal(first, anchorLocalTestRead(t, s)) {
				t.Fatal("phase2 adopted replacement or lost phase1", err)
			}
		})
	}
}

func TestAnchorPollPhase1SQLFailureHasActualDispositionAndNoContinuation(t *testing.T) {
	for _, kind := range []string{"rollback", "commit-unknown"} {
		t.Run(kind, func(t *testing.T) {
			s, source, proof := anchorLocalTestManaged(t)
			p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
			receipt := anchorPollTestReceive(t, p, proof, true)
			before := source.cursor.Load()
			if kind == "rollback" {
				if _, err := s.w.ExecContext(t.Context(), `CREATE TEMP TRIGGER poll_deny_refuse BEFORE UPDATE ON main.meta WHEN old.key='trustanchors-local/v1/state' BEGIN SELECT RAISE(ABORT,'fixture poll denial refused'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, statement := range []string{
					`CREATE TEMP TABLE poll_deny_parent(id INTEGER PRIMARY KEY)`,
					`CREATE TEMP TABLE poll_deny_child(parent INTEGER REFERENCES poll_deny_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
					`CREATE TEMP TRIGGER poll_deny_unknown AFTER UPDATE ON main.meta WHEN old.key='trustanchors-local/v1/state' BEGIN INSERT INTO poll_deny_child(parent) VALUES (1); END`,
				} {
					if _, err := s.w.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, continuation, err := s.CommitAnchorPollDenial(receipt)
			var constraint interface{ Code() int }
			if !errors.As(err, &constraint) || continuation != nil || !source.closed.Load() || !s.anchorBlocked.Load() {
				t.Fatal("admission failure masqueraded as target SQL fault", err)
			}
			if kind == "rollback" {
				if constraint.Code() != 1811 || result.Outcome() != AnchorRolledBack || errors.Is(err, sql.ErrTxDone) {
					t.Fatal("target local UPDATE did not confirm rollback", err)
				}
			} else if constraint.Code() != 787 || result.Outcome() != AnchorUnknown || !errors.Is(err, sql.ErrTxDone) {
				t.Fatal("actual commit-unknown boundary not reached", err)
			}
			if source.cursor.Load() != before || !sameAnchorLocal(before.local, anchorLocalTestRead(t, s)) {
				t.Fatal("failed/unknown phase1 changed accepted local bytes")
			}
			if _, _, err := s.CommitAnchorPollDenial(receipt); !errors.Is(err, retirement.ErrUsed) {
				t.Fatal("SQL failure receipt replayed", err)
			}
		})
	}
}

func TestAnchorPollFollowerRawDenialPreservesCanonicalStateAndReopens(t *testing.T) {
	primary, _, proof := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	if err := anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, follower, primary); err != nil {
		t.Fatal(err)
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	d, err := follower.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), clock)
	if err != nil {
		t.Fatal(err)
	}
	raw := anchorPollTestPack(t, anchorTestSigned(t, proof.key, proof.signer, true))
	stamp, err := d.SampleReceipt()
	if err != nil {
		t.Fatal(err)
	}
	verified, err := d.VerifyReceived(raw, stamp)
	if err != nil {
		t.Fatal(err)
	}
	before := source.cursor.Load()
	image := imageOf(t, follower).Generation()
	authority := anchorAuthority(follower.Authority())
	result, _, err := follower.CommitAnchorDenial(verified)
	if err != nil || result.Outcome() != AnchorCommitted {
		t.Fatal("actual follower raw deny failed", err)
	}
	after := source.cursor.Load()
	oldState, _ := before.owner.State()
	newState, _ := after.owner.State()
	if before.owner.Version() != after.owner.Version() || oldState.Inspect().Checkpoint != newState.Inspect().Checkpoint || !image.Same(imageOf(t, follower).Generation()) || anchorAuthority(follower.Authority()) != authority {
		t.Fatal("follower deny invented canonical progress/authority")
	}
	if _, err := follower.WriterToken(); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower gained primary writer", err)
	}
	if after.local.record.RetiredThrough != proof.next.Version().Generation || len(after.local.record.Denied) != 1 {
		t.Fatal("inclusive follower deny missing")
	}
	path := follower.Path()
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, path, internalOptions, func(o *Options) { o.InitialMode = replication.Follower })
	restored, err := reopened.BindAnchorLifecycle(anchorLocalTestFinite(t.Context(), t))
	if err != nil || !sameAnchorLocal(after.local, restored.cursor.Load().local) || restored.cursor.Load().owner.Version() != after.owner.Version() {
		t.Fatal("actual schema8 follower reopen lost original denial/owner", err)
	}
	if _, err := reopened.WriterToken(); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower restart invented writer", err)
	}
}

func TestAnchorPollFollowerNormalReceiptCannotAdvanceOrResample(t *testing.T) {
	primary, _, proof := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	if err := anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, follower, primary); err != nil {
		t.Fatal(err)
	}
	d, err := follower.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	raw := anchorPollTestPack(t, anchorTestSigned(t, proof.key, proof.signer, false))
	stamp, err := d.SampleReceipt()
	if err != nil {
		t.Fatal(err)
	}
	before := source.cursor.Load()
	if _, err := d.VerifyReceived(raw, stamp); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("normal response became follower granting proof", err)
	}
	if _, err := d.SampleReceipt(); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("follower normal response replaced by later revoke receipt", err)
	}
	if source.cursor.Load() != before || !sameAnchorLocal(before.local, anchorLocalTestRead(t, follower)) {
		t.Fatal("normal follower receipt changed owner/local checkpoint")
	}
}

func TestAnchorPollConcurrentNormalExecutionInvokesOneCallback(t *testing.T) {
	s, _, proof := anchorLocalTestManaged(t)
	p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
	receipt := anchorPollTestReceive(t, p, proof, false)
	copied := *receipt
	var calls atomic.Int32
	type result struct {
		completion AnchorCommit
		err        error
	}
	results := make(chan result, 4)
	for worker := 0; worker < 4; worker++ {
		go func() {
			completion, err := s.AnchorPollCommand(&copied, func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
				calls.Add(1)
				return anchorLocalTestAdvance(ctx, tx, observation, proof)
			})
			results <- result{completion, err}
		}()
	}
	winners := 0
	for worker := 0; worker < 4; worker++ {
		select {
		case result := <-results:
			if result.err == nil && result.completion.Outcome() == AnchorCommitted {
				winners++
			} else if !errors.Is(result.err, retirement.ErrUsed) {
				t.Fatal("unexpected concurrent disposition", result.err)
			}
		case <-p.cell.denial.original.Done():
			t.Fatal("normal execution exceeded original lifetime")
		}
	}
	if winners != 1 || calls.Load() != 1 {
		t.Fatal("copied receipt executed more than once", winners, calls.Load())
	}
}

func TestAnchorPollNormalCancellationRetainsOriginalAndConsumes(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	ctx, cancel := context.WithCancel(anchorTestContext(t.Context(), t, s))
	defer cancel()
	p := anchorPollTestCapture(ctx, t, s, proof)
	receipt := anchorPollTestReceive(t, p, proof, false)
	before := source.cursor.Load()
	cancel()
	called := false
	result, err := s.AnchorPollCommand(receipt, func(context.Context, *CommandTx, trustanchors.Observation) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || result.Outcome() != AnchorRolledBack || called || source.cursor.Load() != before {
		t.Fatal("canceled original command was replaced", err)
	}
	if _, err := s.AnchorPollCommand(receipt, func(context.Context, *CommandTx, trustanchors.Observation) error {
		t.Error("canceled operation replayed")
		return nil
	}); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal(err)
	}
}

func TestAnchorPollConfirmedPhase1ErrorPolicyPreservesActualCompletion(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
	receipt := anchorPollTestReceive(t, p, proof, true)
	actual, continuation, err := s.CommitAnchorPollDenial(receipt)
	if err != nil || continuation == nil || actual.Outcome() != AnchorCommitted {
		t.Fatal("fixture never reached real phase1 commit", err)
	}
	after := source.cursor.Load()
	// This is the private error-policy boundary with a REAL confirmed receipt.
	// It does not claim a public SQL-return/cancellation scheduling injection.
	refused := errors.New("fixture completion handling refused after actual commit")
	recorded, next, err := finishAnchorPollDenial(p.cell, actual, continuation.cell.receipt, refused)
	if !errors.Is(err, refused) || next != nil || recorded.Outcome() != AnchorCommitted || recorded != actual {
		t.Fatal("post-effect error became rollback or a continuation", err)
	}
	if source.cursor.Load() != after || !sameAnchorLocal(after.local, anchorLocalTestRead(t, s)) || after.local.record.RetiredThrough != proof.next.Version().Generation {
		t.Fatal("completion refusal lost durable own effect")
	}
}

func anchorPollTestSignedPair(t *testing.T, first, second anchorTestProof, revoke bool) *dns.Msg {
	t.Helper()
	a := anchorTestSigned(t, first.key, first.signer, revoke)
	b := anchorTestSigned(t, second.key, second.signer, false)
	set := []dns.RR{a.Answer[0], b.Answer[0]}
	firstSignature := a.Answer[1].(*dns.RRSIG)
	secondSignature := b.Answer[1].(*dns.RRSIG)
	if err := firstSignature.Sign(first.signer, set); err != nil {
		t.Fatal(err)
	}
	if err := secondSignature.Sign(second.signer, set); err != nil {
		t.Fatal(err)
	}
	a.Answer = append(set, firstSignature, secondSignature)
	return a
}

func TestAnchorPollSelfRevokeOverridesIndependentNormalSupport(t *testing.T) {
	s := openTemp(t, internalOptions)
	source, dormant := anchorLocalTestDormant(t, s)
	ctx := anchorTestContext(t.Context(), t, s)
	bootstrap, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	first := anchorTestBootstrap(t, s, dormant)
	second := anchorTestBootstrap(t, s, dormant)
	message := anchorPollTestSignedPair(t, first, second, false)
	state, original, err := trustanchors.Bootstrap(trustanchors.Seed{ClusterID: s.Authority().Term.ClusterID, PolicyID: "explicit-test-root", DS: []string{first.key.ToDS(dns.SHA256).String(), second.key.ToDS(dns.SHA256).String()}}, message, first.clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	managed, err := accepted.ProposeManaged(dormant, state, state.Inspect().Checkpoint.Wall, original.ValidUntil())
	if err != nil {
		t.Fatal(err)
	}
	completion, err := s.AnchorCommand(bootstrap, func(ctx context.Context, tx *CommandTx) error {
		if err := original.CheckBootstrap(state, first.clock.Sample()); err != nil {
			return err
		}
		if err := tx.RequireCommitCheck(func(context.Context) error { return original.CheckBootstrap(state, first.clock.Sample()) }); err != nil {
			return err
		}
		return anchorTestStage(ctx, tx, managed)
	})
	if err != nil || completion.Outcome() != AnchorCommitted {
		t.Fatal("actual two-seed bootstrap did not commit", err)
	}
	proof := anchorTestProof{next: managed, state: state, observation: original, clock: first.clock, key: first.key, signer: first.signer}
	p := anchorPollTestCapture(anchorTestContext(t.Context(), t, s), t, s, proof)
	raw := anchorPollTestPack(t, anchorPollTestSignedPair(t, first, second, true))
	stamp, err := p.SampleReceipt()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := p.Receive(raw, stamp)
	if err != nil || receipt.Branch() != AnchorPollRevoke {
		t.Fatal("independent support concealed self-revoke", err)
	}
	if _, err := s.AnchorPollCommand(receipt, func(context.Context, *CommandTx, trustanchors.Observation) error {
		t.Error("revoke ran normal callback")
		return nil
	}); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal(err)
	}
	deny, next, err := s.CommitAnchorPollDenial(receipt)
	if err != nil || deny.Outcome() != AnchorCommitted || next == nil {
		t.Fatal(err)
	}
	advanced, err := s.AnchorPollCommandAfterDenial(next, func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
		return anchorLocalTestAdvance(ctx, tx, observation, proof)
	})
	if err != nil || advanced.Outcome() != AnchorCommitted {
		t.Fatal(err)
	}
	final, err := source.cursor.Load().owner.State()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := final.Inspect()
	revoked, valid := 0, 0
	for _, key := range snapshot.Keys {
		if key.Status == trustanchors.Revoked {
			revoked++
		}
		if key.Status == trustanchors.Valid {
			valid++
		}
	}
	if snapshot.Quarantined || revoked != 1 || valid != 1 || len(source.cursor.Load().local.record.Denied) != 1 {
		t.Fatal("independent live anchor or exact denial was lost")
	}
}

func TestAnchorPollFollowerHeldRawRevokeClosesWithoutRestamping(t *testing.T) {
	primary, _, proof := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	if err := anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, follower, primary); err != nil {
		t.Fatal(err)
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	d, err := follower.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), clock)
	if err != nil {
		t.Fatal(err)
	}
	message := anchorTestSigned(t, proof.key, proof.signer, true)
	for _, rr := range message.Answer {
		rr.Header().Ttl = 1
	}
	raw := anchorPollTestPack(t, message)
	stamp, err := d.SampleReceipt()
	if err != nil {
		t.Fatal(err)
	}
	known, err := trustanchors.Verify(proof.state, message, stamp.cell.at)
	if err != nil || len(known.Revocations()) != 1 {
		t.Fatal("fixture self-revoke unauthenticated", err)
	}
	before := source.cursor.Load()
	image := imageOf(t, follower).Generation()
	anchorPollTestWait(d.cell.original, t, known.ValidUntil())
	if verified, err := d.VerifyReceived(raw, stamp); !errors.Is(err, trustanchors.ErrExpired) || verified != nil {
		t.Fatal("queued follower receipt restamped/accepted", err)
	}
	if !source.closed.Load() || !follower.anchorBlocked.Load() || source.cursor.Load() != before || !sameAnchorLocal(before.local, anchorLocalTestRead(t, follower)) || !image.Same(imageOf(t, follower).Generation()) {
		t.Fatal("expired known follower revoke reopened or credited durability")
	}
	if _, err := d.SampleReceipt(); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("expired follower raw receipt was replaced", err)
	}
}

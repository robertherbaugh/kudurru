package sqlite

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync/atomic"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// AnchorReceiptStamp retains one original raw receipt on the exact captured
// denial. Copies share cell identity; neither time equality nor a Sample from
// another operation can establish ownership. It grants no serving authority.
type AnchorReceiptStamp struct{ cell *anchorReceiptStamp }

type anchorReceiptStamp struct {
	original *anchorDenial
	at       trustanchors.Sample
}

// Nil input is Fresh, a node with a stamp is Issued, and this immutable shared
// terminal node is Terminal. Nodes never reset or recycle within an operation.
type anchorReceiptInput struct{ stamp *anchorReceiptStamp }

var anchorReceiptTerminal = &anchorReceiptInput{}

// AnchorPoll owns mutually exclusive original normal/deny/paired-phase2 lanes.
// Capture and receipt authentication neither accept progress nor publish keys.
type AnchorPoll struct{ cell *anchorPoll }

type anchorPoll struct {
	denial *anchorDenial
	normal *anchorUpdate
	second *AnchorUpdate
}

// AnchorPollBranch describes only how an already authenticated receipt routes.
type AnchorPollBranch uint8

// A zero receipt has no branch. Self-revocation always excludes the normal lane.
const (
	AnchorPollInvalid AnchorPollBranch = iota
	AnchorPollNormal
	AnchorPollRevoke
)

// AnchorPollReceipt retains one original Observation and its immutable branch.
// Copies share execution consumption; there is no arbitrary proof constructor.
type AnchorPollReceipt struct{ cell *anchorPollReceipt }

type anchorPollReceipt struct {
	original    *anchorPoll
	observation trustanchors.Observation
	branch      AnchorPollBranch
	used        atomic.Bool
}

// AnchorPollContinuation contains only this poll's own confirmed denial effect
// and its precaptured phase2. It is neither accepted State nor a serving permit.
type AnchorPollContinuation struct{ cell *anchorPollContinuation }

type anchorPollContinuation struct {
	original *anchorPoll
	receipt  *AnchorDenialReceipt
	used     atomic.Bool
}

// CaptureAnchorPoll must precede network acquisition. One actual committed
// owner/local/authority/installed cut supplies all three original attempts. The
// caller already carries its writer and finite <=30s context and concrete clock.
func (s *Store) CaptureAnchorPoll(ctx context.Context, expected accepted.Version, clock *trustanchors.SessionClock) (*AnchorPoll, error) {
	if ctx == nil {
		return nil, replication.ErrInvalid
	}
	writer, ok := WriterTokenFromContext(ctx)
	if !ok {
		return nil, replication.ErrFenced
	}
	denial, err := s.captureAnchorDenial(ctx, expected, clock)
	if err != nil {
		return nil, err
	}
	if denial.cursor.local.record.Terminal {
		return nil, ErrAnchorUnavailable
	}
	ordinary, err := denial.cursor.gate.CaptureWrite(ctx, denial.cursor.mark)
	if err != nil {
		return nil, err
	}
	second, err := denial.cursor.gate.CaptureWrite(ctx, denial.cursor.mark)
	if err != nil {
		return nil, err
	}
	normalMode, err := captureAnchorMode(ctx, denial.source, denial.cursor, true)
	if err != nil {
		return nil, err
	}
	secondMode, err := captureAnchorMode(ctx, denial.source, denial.cursor, true)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := denial.life.check(); err != nil {
		return nil, err
	}
	if s.anchors.Load() != denial.source || denial.source.closed.Load() || s.anchorBlocked.Load() || denial.source.cursor.Load() != denial.cursor {
		return nil, accepted.ErrCAS
	}
	normal := &anchorUpdate{source: denial.source, cursor: denial.cursor, original: ctx, writer: writer, attempt: ordinary, mode: normalMode, authorityCut: denial.authority, installed: &denial.installed}
	phase2 := &AnchorUpdate{cell: &anchorUpdate{source: denial.source, cursor: denial.cursor, original: ctx, writer: writer, attempt: second, mode: secondMode, revoke: denial, authorityCut: denial.authority, installed: &denial.installed}}
	return &AnchorPoll{cell: &anchorPoll{denial: denial, normal: normal, second: phase2}}, nil
}

// SampleReceipt must run at the original raw socket receipt BEFORE unpacking,
// authentication or queueing. It issues one opaque stamp, never a replacement
// Sample for held bytes. Actual receipt placement remains the caller's contract.
func (p *AnchorPoll) SampleReceipt() (AnchorReceiptStamp, error) {
	if p == nil || p.cell == nil {
		return AnchorReceiptStamp{}, replication.ErrInvalid
	}
	return sampleAnchorReceipt(p.cell.denial)
}

// SampleReceipt issues a follower-capable deny-only original raw receipt stamp.
// No primary token, new clock, granting attempt or phase2 is obtained here.
func (d *AnchorDenial) SampleReceipt() (AnchorReceiptStamp, error) {
	if d == nil {
		return AnchorReceiptStamp{}, replication.ErrInvalid
	}
	return sampleAnchorReceipt(d.cell)
}

func sampleAnchorReceipt(c *anchorDenial) (AnchorReceiptStamp, error) {
	if c == nil || c.original == nil || c.clock == nil {
		return AnchorReceiptStamp{}, replication.ErrInvalid
	}
	if c.input.Load() != nil {
		return AnchorReceiptStamp{}, retirement.ErrUsed
	}
	if err := c.original.Err(); err != nil {
		return AnchorReceiptStamp{}, err
	}
	if err := c.life.check(); err != nil {
		return AnchorReceiptStamp{}, err
	}
	at := c.clock.Sample()
	if at == (trustanchors.Sample{}) {
		return AnchorReceiptStamp{}, trustanchors.ErrClock
	}
	stamp := &anchorReceiptStamp{original: c, at: at}
	node := &anchorReceiptInput{stamp: stamp}
	if !c.input.CompareAndSwap(nil, node) {
		return AnchorReceiptStamp{}, retirement.ErrUsed
	}
	return AnchorReceiptStamp{cell: stamp}, nil
}

// Receive consumes exactly one operation-issued raw receipt. It authenticates
// only captured State once, retains original TTL evidence, and fixes its branch.
// The caller owns framing/endpoint/request correlation and must not mutate raw
// concurrently. No raw buffer or caller-owned parsed message is retained.
func (p *AnchorPoll) Receive(raw []byte, received AnchorReceiptStamp) (*AnchorPollReceipt, error) {
	if p == nil || p.cell == nil {
		return nil, replication.ErrInvalid
	}
	observation, err := verifyAnchorReceipt(p.cell.denial, raw, received)
	if err != nil {
		return nil, err
	}
	branch := AnchorPollNormal
	if len(observation.Revocations()) != 0 {
		branch = AnchorPollRevoke
	}
	return &AnchorPollReceipt{cell: &anchorPollReceipt{original: p.cell, observation: observation, branch: branch}}, nil
}

// VerifyReceived authenticates an original raw self-revoke without sampling a
// new receipt at dequeue. It shares input consumption with legacy parsed Verify
// and cannot grant follower checkpoint/key progress from a normal observation.
func (d *AnchorDenial) VerifyReceived(raw []byte, received AnchorReceiptStamp) (*AnchorVerifiedDenial, error) {
	if d == nil {
		return nil, replication.ErrInvalid
	}
	observation, err := verifyAnchorReceipt(d.cell, raw, received)
	if err != nil {
		return nil, err
	}
	if len(observation.Revocations()) == 0 {
		return nil, replication.ErrInvalid
	}
	return &AnchorVerifiedDenial{cell: &anchorVerifiedDenial{original: d.cell, observation: observation}}, nil
}

func verifyAnchorReceipt(c *anchorDenial, raw []byte, received AnchorReceiptStamp) (trustanchors.Observation, error) {
	if c == nil || received.cell == nil || received.cell.original != c {
		return trustanchors.Observation{}, replication.ErrInvalid
	}
	node := c.input.Load()
	if node == anchorReceiptTerminal {
		return trustanchors.Observation{}, retirement.ErrUsed
	}
	if node == nil || node.stamp != received.cell {
		return trustanchors.Observation{}, replication.ErrInvalid
	}
	if !c.input.CompareAndSwap(node, anchorReceiptTerminal) {
		return trustanchors.Observation{}, retirement.ErrUsed
	}
	if err := c.original.Err(); err != nil {
		return trustanchors.Observation{}, err
	}
	if err := c.life.check(); err != nil {
		return trustanchors.Observation{}, err
	}
	message, err := unpackAnchorReceipt(raw)
	if err != nil {
		return trustanchors.Observation{}, err
	}
	state, err := c.cursor.owner.State()
	if err != nil {
		return trustanchors.Observation{}, err
	}
	observation, err := trustanchors.Verify(state, message, received.cell.at)
	if err != nil {
		return trustanchors.Observation{}, err
	}
	// Determine known denial before ANY post-authentication lifetime refusal.
	// A valid self-revoke may have expired during authentication (or TTL=0).
	revoke := len(observation.Revocations()) != 0
	err = observation.CheckLifetime(c.clock.Sample())
	if err == nil {
		err = c.original.Err()
	}
	if err == nil {
		err = c.life.check()
	}
	if err != nil {
		if revoke {
			c.source.store.closeAnchorSource(c.source)
			c.source.flushAnchorClose()
		}
		return trustanchors.Observation{}, err
	}
	return observation, nil
}

// unpackAnchorReceipt bounds actual wire bytes/counts before library unpacking
// and rejects trailing bytes. Msg.Unpack alone need not consume the whole input.
func unpackAnchorReceipt(raw []byte) (*dns.Msg, error) {
	if len(raw) > min(65535, trustanchors.MaxWireBytes) {
		return nil, trustanchors.ErrBounds
	}
	if len(raw) < 12 {
		return nil, replication.ErrInvalid
	}
	wire := bytes.Clone(raw)
	questions := binary.BigEndian.Uint16(wire[4:6])
	answers := binary.BigEndian.Uint16(wire[6:8])
	authority := binary.BigEndian.Uint16(wire[8:10])
	additional := binary.BigEndian.Uint16(wire[10:12])
	if questions != 1 || answers == 0 || answers > trustanchors.MaxLiveKeys+trustanchors.MaxSignatures || authority != 0 || additional > 1 {
		return nil, trustanchors.ErrBounds
	}
	_, offset, err := dns.UnpackDomainName(wire, 12)
	if err != nil || offset > len(wire)-4 {
		return nil, replication.ErrInvalid
	}
	offset += 4
	for i := 0; i < int(answers)+int(additional); i++ {
		_, next, err := dns.UnpackDomainName(wire, offset)
		if err != nil || next > len(wire)-10 {
			return nil, replication.ErrInvalid
		}
		length := int(binary.BigEndian.Uint16(wire[next+8 : next+10]))
		offset = next + 10
		if length > len(wire)-offset {
			return nil, replication.ErrInvalid
		}
		offset += length
	}
	if offset != len(wire) {
		return nil, replication.ErrInvalid
	}
	message := new(dns.Msg)
	if err := message.Unpack(wire); err != nil {
		return nil, replication.ErrInvalid
	}
	return message, nil
}

// Branch reports immutable dispatch metadata, not cryptographic or serving
// authority. A zero receipt never looks like a normal usable observation.
func (r *AnchorPollReceipt) Branch() AnchorPollBranch {
	if r == nil || r.cell == nil {
		return AnchorPollInvalid
	}
	return r.cell.branch
}

func (s *Store) claimAnchorPoll(receipt *AnchorPollReceipt, branch AnchorPollBranch) (*anchorPollReceipt, error) {
	if receipt == nil || receipt.cell == nil || receipt.cell.original == nil || receipt.cell.original.denial == nil || receipt.cell.original.denial.source.store != s || receipt.cell.branch != branch {
		return nil, replication.ErrInvalid
	}
	if !receipt.cell.used.CompareAndSwap(false, true) {
		return nil, retirement.ErrUsed
	}
	return receipt.cell, nil
}

// AnchorPollCommand uses only the original precaptured normal lane. Root must
// hold its original dependency pins outside R and use its retained persistence
// Original in fn. Staged values and actual completion are never serving grants.
func (s *Store) AnchorPollCommand(receipt *AnchorPollReceipt, fn func(context.Context, *CommandTx, trustanchors.Observation) error) (AnchorCommit, error) {
	r, err := s.claimAnchorPoll(receipt, AnchorPollNormal)
	if err != nil {
		return AnchorCommit{}, err
	}
	if fn == nil {
		return AnchorCommit{}, replication.ErrInvalid
	}
	c := r.original.denial
	return s.runAnchorCommand(r.original.normal, c.cursor, func(ctx context.Context, tx *CommandTx) error {
		if err := r.observation.CheckLifetime(c.clock.Sample()); err != nil {
			return err
		}
		if err := c.life.check(); err != nil {
			return err
		}
		if err := tx.RequireCommitCheck(func(context.Context) error {
			if err := r.observation.CheckLifetime(c.clock.Sample()); err != nil {
				return err
			}
			return c.life.check()
		}); err != nil {
			return err
		}
		return fn(ctx, tx, r.observation)
	})
}

// CommitAnchorPollDenial persists denial independently of root granting pins.
// Root must dispatch known self-revoke here even after cancellation. Actual
// completion and error remain separate; any error yields no continuation, even
// if SQL really committed. Such a commit is never relabeled rollback/replayed.
func (s *Store) CommitAnchorPollDenial(receipt *AnchorPollReceipt) (AnchorCommit, *AnchorPollContinuation, error) {
	r, err := s.claimAnchorPoll(receipt, AnchorPollRevoke)
	if err != nil {
		return AnchorCommit{}, nil, err
	}
	verified := &AnchorVerifiedDenial{cell: &anchorVerifiedDenial{original: r.original.denial, observation: r.observation}}
	completion, own, err := s.CommitAnchorDenial(verified)
	return finishAnchorPollDenial(r.original, completion, own, err)
}

// Preserve actual phase1 completion even when later handling refuses. Only the
// exact confirmed own-effect receipt with no error supplies this continuation.
func finishAnchorPollDenial(original *anchorPoll, completion AnchorCommit, own *AnchorDenialReceipt, err error) (AnchorCommit, *AnchorPollContinuation, error) {
	if err != nil {
		return completion, nil, err
	}
	if original == nil || own == nil || own.cell == nil || own.cell.original != original.denial || (completion.Outcome() != AnchorCommitted && completion.Outcome() != AnchorUnchanged) {
		return completion, nil, ErrAnchorUnavailable
	}
	return completion, &AnchorPollContinuation{cell: &anchorPollContinuation{original: original, receipt: own}}, nil
}

// AnchorPollCommandAfterDenial consumes only its paired confirmed own-effect
// continuation. No owner/source/clock/context/policy is recaptured. Root's
// original dependency pins must enclose this call, never be acquired under R.
func (s *Store) AnchorPollCommandAfterDenial(continuation *AnchorPollContinuation, fn func(context.Context, *CommandTx, trustanchors.Observation) error) (AnchorCommit, error) {
	if continuation == nil || continuation.cell == nil || continuation.cell.original == nil || continuation.cell.original.denial == nil || continuation.cell.original.denial.source.store != s {
		return AnchorCommit{}, replication.ErrInvalid
	}
	c := continuation.cell
	if !c.used.CompareAndSwap(false, true) {
		return AnchorCommit{}, retirement.ErrUsed
	}
	if fn == nil {
		return AnchorCommit{}, replication.ErrInvalid
	}
	return s.AnchorCommandAfterDenial(c.original.second, c.receipt, fn)
}

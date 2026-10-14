package primaryruntime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/miekg/dns"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// SemanticDigest ignores only transport authentication and the DNS message ID;
// all immutable UPDATE semantics stay bound. Root proof issuers can use this
// function without constructing a verification capability or exposing secrets.
func SemanticDigest(wire []byte) (string, error) {
	if len(wire) > 65535 {
		return "", ErrCapacity
	}
	var m dns.Msg
	if err := m.Unpack(wire); err != nil {
		return "", err
	}
	m.Id = 0
	m.Compress = false
	extra := m.Extra[:0]
	for _, rr := range m.Extra {
		if rr.Header().Rrtype != dns.TypeTSIG {
			extra = append(extra, rr)
		}
	}
	m.Extra = extra
	b, err := m.Pack()
	if err != nil {
		return "", err
	}
	return zk.Digest(b), nil
}
func validateProof(p ProofIdentity, digest string) error {
	if p.Principal.Key != canonical(p.Principal.Key) || p.Principal.Key == "." || p.SemanticDigest != digest || p.Term.Validate() != nil || p.AuthorityIncarnation == 0 {
		return ErrInvalid
	}
	if p.Algorithm != dns.HmacSHA256 && p.Algorithm != dns.HmacSHA512 {
		return ErrInvalid
	}
	if p.Transport != "udp" && p.Transport != "tcp" && p.Transport != "dot" {
		return ErrUnavailable
	}
	for _, s := range []string{p.Principal.Key, p.Principal.Version, p.ProviderVersion, p.Domain, p.Subject, p.RetryID} {
		if s == "" || len(s) > 512 {
			return ErrInvalid
		}
	}
	return nil
}
func receiptIdentity(l *zk.ZoneLifecycle, p ProofIdentity) (string, string) {
	id, _ := json.Marshal(struct {
		Zone  zk.ZoneID
		Retry string
	}{l.Spec.HeadZone(), p.RetryID})
	semantic, _ := json.Marshal(struct{ Digest, Key, Version, Provider, Domain, Subject, Transport string }{p.SemanticDigest, p.Principal.Key, p.Principal.Version, p.ProviderVersion, p.Domain, p.Subject, p.Transport})
	return zk.Digest(id), zk.Digest(semantic)
}

// Apply requires root's opaque verification capability and transactional
// admission on every attempt/response, including no-op and receipt replay.
// Forwarding is intentionally absent until root's authenticated adapter exists.
func (r *Runtime) Apply(ctx context.Context, request VerifiedUpdate) UpdateOutcome {
	if r.o.Proofs == nil || r.o.Admit == nil || r.o.Publish == nil {
		return outcome(ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	if len(request.Wire) > 65535 || int64(len(request.Wire)) > r.o.UpdateLimits.MaxUpdateBytes {
		return outcome(ErrCapacity)
	}
	wire := append([]byte(nil), request.Wire...)
	digest, err := SemanticDigest(wire)
	if err != nil {
		return outcome(err)
	}
	proof, err := r.o.Proofs.Verify(ctx, request.Proof, wire)
	if err != nil {
		return outcome(err)
	}
	if err := validateProof(proof, digest); err != nil {
		return outcome(err)
	}
	if request.Zone.ClusterID != proof.Term.ClusterID {
		return outcome(ErrInvalid)
	}
	var message dns.Msg
	if err := message.Unpack(wire); err != nil {
		return outcome(err)
	}
	tx, protocol := dynamicupdate.Parse(&message, r.o.UpdateLimits)
	if protocol != nil {
		return UpdateOutcome{Rcode: protocol.Rcode, Err: protocol}
	}
	for attempt := 0; attempt < r.o.MaxAttempts; attempt++ {
		l, err := r.ReadLifecycle(ctx, request.Zone)
		if err != nil {
			return outcome(err)
		}
		if l.Spec.Phase != zk.PhaseActive || tx.Apex() != l.Spec.Zone.Apex {
			return outcome(ErrUnavailable)
		}
		v, err := r.o.Repository.Read(ctx, l.Spec.HeadZone())
		if err != nil {
			return outcome(err)
		}
		if v.Head.Spec.Lifecycle != zk.LifecycleActive || (v.Expected.Term.ClusterID != "" && !v.Expected.Term.Same(proof.Term)) {
			return outcome(ErrUnavailable)
		}
		state, err := v.UpdateState()
		if err != nil {
			return outcome(err)
		}
		id, semantic := receiptIdentity(l, proof)
		var prior *zonestate.Receipt
		for i := range v.Aggregate.Receipts {
			if v.Aggregate.Receipts[i].ID == id {
				prior = &v.Aggregate.Receipts[i]
				break
			}
		}
		if prior != nil {
			checked, err := r.admit(ctx, Command{Lifecycle: l, Expected: v.Expected, Proof: &proof, Reason: "update-dedup"})
			if conflicts(err) {
				continue
			}
			if err != nil {
				return UpdateOutcome{Rcode: dns.RcodeServerFailure, Committed: true, Deduplicated: true, Version: version(v), Serial: prior.Serial, Err: err}
			}
			if prior.SemanticDigest != semantic || prior.Principal != proof.Principal || prior.AuthEpoch != state.AuthEpoch {
				return UpdateOutcome{Rcode: dns.RcodeRefused, Committed: true, Err: ErrInvalid}
			}
			// Do not rerun original prerequisites or semantics on replay. The
			// identical AuthEpoch binds the originally authorized grant policy.
			empty := new(dns.Msg)
			empty.SetUpdate(state.Apex)
			authTx, pe := dynamicupdate.Parse(empty, r.o.UpdateLimits)
			if pe != nil {
				return outcome(pe)
			}
			if _, pe := dynamicupdate.Plan(state, authTx, proof.Principal, r.o.Now(), r.o.UpdateLimits); pe != nil {
				return UpdateOutcome{Rcode: pe.Rcode, Committed: true, Err: pe}
			}
			out := r.publishUpdate(ctx, l, checked)
			out.Deduplicated = true
			out.Serial = prior.Serial
			return out
		}
		plan, pe := dynamicupdate.Plan(state, tx, proof.Principal, r.o.Now(), r.o.UpdateLimits)
		if pe != nil {
			_, err := r.admit(ctx, Command{Lifecycle: l, Expected: v.Expected, Proof: &proof, Reason: "update-rejected"})
			if conflicts(err) {
				continue
			}
			if err != nil {
				return outcome(err)
			}
			return UpdateOutcome{Rcode: pe.Rcode, Err: pe}
		}
		cut := r.Capture(r.o.Now())
		m, ok := cut.Member(l.Spec.Zone.Apex)
		if !ok || !m.Published || m.Incarnation != l.Spec.Incarnation || m.Sequence != l.Spec.Sequence || m.Version != version(v) {
			checked, err := r.admit(ctx, Command{Lifecycle: l, Expected: v.Expected, Proof: &proof, Reason: "update-publication-recovery"})
			if conflicts(err) {
				continue
			}
			if err != nil {
				return outcome(err)
			}
			out := r.publishUpdate(ctx, l, checked)
			if out.Err != nil {
				out.Committed = false
				return out
			}
			continue
		}
		if len(v.Aggregate.Receipts) >= r.o.Limits.MaxReceipts {
			if _, err := r.admit(ctx, Command{Lifecycle: l, Expected: v.Expected, Proof: &proof, Reason: "update-capacity"}); conflicts(err) {
				continue
			} else if err != nil {
				return outcome(err)
			}
			return outcome(fmt.Errorf("%w: retry ledger full", ErrCapacity))
		}
		a, err := zonestate.ApplyResult(v.Aggregate, plan, proof.Principal)
		if err != nil {
			return outcome(err)
		}
		a.Receipts = append(a.Receipts, zonestate.Receipt{ID: id, SemanticDigest: semantic, Principal: proof.Principal, AuthEpoch: state.AuthEpoch, CommitSequence: v.Head.Spec.CommitSequence + 1, Serial: plan.Serial, Rcode: dns.RcodeSuccess, Changed: plan.Changed})
		p, err := zonestate.Prepare(a, v.Head, r.o.Limits)
		if err != nil {
			return outcome(err)
		}
		modelizer, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: primaryzone.BuildCommitted, Limits: r.o.Limits})
		prebuilt, err := modelizer.Prepare(ctx, &zonestate.View{Head: p.Head, Aggregate: a})
		if err != nil {
			return outcome(err)
		}
		committed, err := r.admit(ctx, Command{Lifecycle: l, Expected: v.Expected, Prepared: p, Proof: &proof, Reason: "update"})
		if conflicts(err) {
			continue
		}
		if err != nil {
			return outcome(err)
		}
		if err := modelizer.Install(ctx, committed, prebuilt); err != nil {
			return UpdateOutcome{Rcode: dns.RcodeServerFailure, Committed: true, Version: version(committed), Serial: committed.Head.Spec.Serial, Err: err}
		}
		return r.publishUpdate(ctx, l, committed)
	}
	return outcome(ErrConflict)
}
func (r *Runtime) publishUpdate(ctx context.Context, l *zk.ZoneLifecycle, v *zonestate.View) UpdateOutcome {
	out := UpdateOutcome{Rcode: dns.RcodeServerFailure, Committed: true, Version: version(v), Serial: v.Head.Spec.Serial}
	e, err := r.build(ctx, l, v, nil)
	if err != nil {
		out.Err = err
		return out
	}
	if err := r.install(e); err != nil {
		out.Err = err
		return out
	}
	if err := r.publish(ctx, e); err != nil {
		out.Err = err
		return out
	}
	member, ok := r.Capture(r.o.Now()).Member(l.Spec.Zone.Apex)
	if !ok || !member.Published || member.Version != version(v) {
		out.Err = ErrUnavailable
		return out
	}
	out.Rcode = dns.RcodeSuccess
	out.Published = true
	return out
}

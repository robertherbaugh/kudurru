package main

import (
	"bytes"
	"context"
	"time"

	"github.com/google/uuid"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// primaryUpdateIngress issues only local native-listener capabilities. Current
// domain/credential/authority checks still occur in the driver's command fence.
// Follower forwarding needs a separately authenticated envelope and wire proof.
type primaryUpdateIngress struct {
	authority func() replication.Authority
	now       func() time.Time
}

type primaryUpdateProof struct {
	issuer   *primaryUpdateIngress
	request  pipeline.Request
	wire     []byte
	identity primaryruntime.ProofIdentity
}

func (p *primaryUpdateIngress) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *primaryUpdateIngress) capture(req *pipeline.Request, id zonekinds.ZoneID) (primaryruntime.VerifiedUpdate, error) {
	if p == nil || p.authority == nil || req == nil || req.Ctx == nil || req.Ctx.Err() != nil {
		return primaryruntime.VerifiedUpdate{}, primaryruntime.ErrUnavailable
	}
	proof, ok := dnsserver.VerifiedTSIGFor(req, p.clock())
	if !ok {
		return primaryruntime.VerifiedUpdate{}, primaryruntime.ErrUnavailable
	}
	authority := p.authority()
	if !authority.Writable() || authority.Term.Validate() != nil || authority.Incarnation == 0 || authority.Term.ClusterID != id.ClusterID {
		return primaryruntime.VerifiedUpdate{}, primaryruntime.ErrUnavailable
	}
	msg := proof.Message()
	tx, protocolErr := dynamicupdate.Parse(msg, dynamicupdate.Limits{})
	if protocolErr != nil {
		return primaryruntime.VerifiedUpdate{}, protocolErr
	}
	if tx.Apex() != id.Apex || id.ObjectName == "" || id.UID == "" {
		return primaryruntime.VerifiedUpdate{}, primaryruntime.ErrInvalid
	}
	msg.Compress = false
	wire, err := msg.Pack()
	if err != nil {
		return primaryruntime.VerifiedUpdate{}, primaryruntime.ErrInvalid
	}
	digest, err := primaryruntime.SemanticDigest(wire)
	if err != nil {
		return primaryruntime.VerifiedUpdate{}, err
	}
	key := proof.Identity()
	captured := *req
	captured.Msg = proof.Message()
	sealed := &primaryUpdateProof{issuer: p, request: captured, wire: bytes.Clone(wire), identity: primaryruntime.ProofIdentity{
		Principal:       dynamicupdate.Principal{Key: key.Key, Version: key.CredentialVersion},
		ProviderVersion: key.ProviderVersion, Algorithm: key.Algorithm, Transport: req.Client.Proto.String(),
		Domain: primaryUpdateDomain(id), Subject: key.Key, RetryID: uuid.NewString(), SemanticDigest: digest,
		Term: authority.Term, AuthorityIncarnation: authority.Incarnation,
	}}
	return primaryruntime.VerifiedUpdate{Zone: id, Wire: wire, Proof: sealed}, nil
}

func primaryUpdateDomain(id zonekinds.ZoneID) string { return "dns-update:" + zonekinds.HeadName(id) }

// Verify cannot construct a proof from a principal or a TSIG key string. The
// exact detached message is bound twice: to native TSIG and to this command.
func (p *primaryUpdateIngress) Verify(ctx context.Context, opaque any, wire []byte) (primaryruntime.ProofIdentity, error) {
	proof, ok := opaque.(*primaryUpdateProof)
	if p == nil || !ok || proof == nil || proof.issuer != p || ctx.Err() != nil || proof.request.Ctx == nil || proof.request.Ctx.Err() != nil || !bytes.Equal(wire, proof.wire) {
		return primaryruntime.ProofIdentity{}, primaryruntime.ErrUnavailable
	}
	if _, ok := dnsserver.VerifiedTSIGFor(&proof.request, p.clock()); !ok {
		return primaryruntime.ProofIdentity{}, primaryruntime.ErrUnavailable
	}
	return proof.identity, nil
}

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/asn1tools"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/iana/chksumtype"
	"github.com/jcmturner/gokrb5/v8/iana/flags"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/service"
	"github.com/jcmturner/gokrb5/v8/spnego"
	"github.com/jcmturner/gokrb5/v8/types"
)

// acceptor is an experimental RFC 4121 acceptor, not a production library.
// Authentication uses gokrb5's ticket, authenticator and replay verification.
// PACs are deliberately not translated into DNS authorization in this probe.
type acceptor struct {
	keytab    *keytab.Keytab
	principal types.PrincipalName
	realm     string
}

func (a *acceptor) accept(input []byte) (*securityContext, []byte, error) {
	// nsupdate (and Windows) can wrap the optimistic Kerberos token in
	// SPNEGO. Only accept Kerberos as the first choice; never negotiate NTLM.
	wrapped := false
	var negotiation spnego.SPNEGOToken
	if err := negotiation.Unmarshal(input); err == nil {
		if !negotiation.Init || len(negotiation.NegTokenInit.MechTypes) == 0 ||
			!negotiation.NegTokenInit.MechTypes[0].Equal(gssapi.OIDKRB5.OID()) ||
			len(negotiation.NegTokenInit.MechListMIC) != 0 {
			return nil, nil, errors.New("probe requires optimistic Kerberos SPNEGO without a mechanism-list MIC")
		}
		input = negotiation.NegTokenInit.MechTokenBytes
		wrapped = true
	}
	var token spnego.KRB5Token
	if err := token.Unmarshal(input); err != nil || !token.IsAPReq() {
		return nil, nil, errors.New("expected a Kerberos AP-REQ token")
	}
	r := &token.APReq
	if r.PVNO != 5 || r.Ticket.Realm != a.realm || !r.Ticket.SName.Equal(a.principal) {
		return nil, nil, errors.New("ticket is for another service or realm")
	}
	if !aes(r.Ticket.EncPart.EType) {
		return nil, nil, errors.New("probe requires AES tickets")
	}
	ok, _, err := service.VerifyAPREQ(r, service.NewSettings(a.keytab,
		service.MaxClockSkew(5*time.Minute), service.DecodePAC(false)))
	if err != nil || !ok {
		// Do not return dependency errors that may include decrypted material.
		return nil, nil, errors.New("kerberos verification failed")
	}
	if r.Authenticator.CRealm != r.Ticket.DecryptedEncPart.CRealm {
		return nil, nil, errors.New("authenticator realm does not match ticket")
	}
	if !types.IsFlagSet(&r.APOptions, flags.APOptionMutualRequired) {
		return nil, nil, errors.New("mutual authentication is required")
	}
	if err := checkChecksum(r.Authenticator.Cksum); err != nil {
		return nil, nil, err
	}
	key := r.Ticket.DecryptedEncPart.Key
	if r.Authenticator.SubKey.KeyType != 0 {
		key = r.Authenticator.SubKey
	}
	if !aes(key.KeyType) || !aes(r.Ticket.DecryptedEncPart.Key.KeyType) {
		return nil, nil, errors.New("probe requires AES context keys")
	}
	if r.Authenticator.SeqNumber < 0 {
		return nil, nil, errors.New("invalid initiator sequence")
	}
	if !time.Now().Before(r.Ticket.DecryptedEncPart.EndTime) {
		return nil, nil, errors.New("ticket expired")
	}
	ctx := &securityContext{
		key:       key,
		principal: r.Authenticator.CName.PrincipalNameString() + "@" + r.Authenticator.CRealm,
		expires:   r.Ticket.DecryptedEncPart.EndTime,
		minimum:   uint64(r.Authenticator.SeqNumber),
		ordered:   binary.LittleEndian.Uint32(r.Authenticator.Cksum.Checksum[20:24])&uint32(gssapi.ContextFlagSequence) != 0,
	}
	var sequence [4]byte
	if _, err := rand.Read(sequence[:]); err != nil {
		return nil, nil, err
	}
	initialSequence := int64(binary.BigEndian.Uint32(sequence[:])&0x3fffffff) + 1
	ctx.send = uint64(initialSequence)
	output, err := mutualReply(r, initialSequence)
	if err != nil {
		return nil, nil, err
	}
	if wrapped {
		reply := spnego.SPNEGOToken{Resp: true, NegTokenResp: spnego.NegTokenResp{
			NegState: asn1.Enumerated(spnego.NegStateAcceptCompleted), SupportedMech: gssapi.OIDKRB5.OID(), ResponseToken: output,
		}}
		output, err = reply.Marshal()
		if err != nil {
			return nil, nil, errors.New("could not encode SPNEGO reply")
		}
	}
	return ctx, output, nil
}

func aes(etype int32) bool {
	// RFC 3962 and RFC 8009. RC4/DES use another token format and are excluded.
	return etype == 17 || etype == 18 || etype == 19 || etype == 20
}

func checkChecksum(c types.Checksum) error {
	// RFC 4121 §4.1.1. No channel binding or delegated credentials in this probe.
	if c.CksumType != chksumtype.GSSAPI || len(c.Checksum) != 24 ||
		binary.LittleEndian.Uint32(c.Checksum[:4]) != 16 ||
		!bytes.Equal(c.Checksum[4:20], make([]byte, 16)) {
		return errors.New("unsupported GSS authenticator checksum")
	}
	f := binary.LittleEndian.Uint32(c.Checksum[20:24])
	required := uint32(gssapi.ContextFlagMutual | gssapi.ContextFlagReplay | gssapi.ContextFlagInteg)
	if f&required != required || f&uint32(gssapi.ContextFlagDeleg) != 0 {
		return errors.New("mutual authentication, integrity and replay protection are required; delegation is unsupported")
	}
	return nil
}

func mutualReply(r *messages.APReq, sequence int64) ([]byte, error) {
	// RFC 4120 §§5.5.2, 7.5.1 and RFC 4121 §4.1.2. An acceptor subkey is
	// optional; omit it and use the initiator subkey, or ticket session key.
	part := messages.EncAPRepPart{CTime: r.Authenticator.CTime, Cusec: r.Authenticator.Cusec, SequenceNumber: sequence}
	plain, err := asn1.Marshal(part)
	if err != nil {
		return nil, err
	}
	plain = asn1tools.AddASNAppTag(plain, 27)
	enc, err := crypto.GetEncryptedData(plain, r.Ticket.DecryptedEncPart.Key, keyusage.AP_REP_ENCPART, 0)
	if err != nil {
		return nil, errors.New("could not encrypt mutual reply")
	}
	reply, err := asn1.Marshal(messages.APRep{PVNO: 5, MsgType: 15, EncPart: enc})
	if err != nil {
		return nil, err
	}
	oid, err := asn1.Marshal(gssapi.OIDKRB5.OID())
	if err != nil {
		return nil, fmt.Errorf("encode Kerberos OID: %w", err)
	}
	reply = asn1tools.AddASNAppTag(reply, 15)
	return asn1tools.AddASNAppTag(append(append(oid, 2, 0), reply...), 0), nil
}

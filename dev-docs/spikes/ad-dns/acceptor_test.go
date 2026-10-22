package main

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/asn1tools"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/iana/chksumtype"
	"github.com/jcmturner/gokrb5/v8/iana/flags"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/spnego"
	"github.com/jcmturner/gokrb5/v8/types"
)

func testAcceptor(t *testing.T) *acceptor {
	t.Helper()
	kt := keytab.New()
	if err := kt.AddEntry("DNS/probe.example", "EXAMPLE", "ephemeral unit test fixture", time.Now(), 1, 18); err != nil {
		t.Fatal(err)
	}
	principal, realm := types.ParseSPNString("DNS/probe.example@EXAMPLE")
	return &acceptor{keytab: kt, principal: principal, realm: realm}
}

func testAPReq(t *testing.T, a *acceptor, mutate func(*types.Authenticator, *messages.Ticket)) ([]byte, types.EncryptionKey) {
	t.Helper()
	cname, _ := types.ParseSPNString("member$")
	now := time.Now().UTC().Truncate(time.Second)
	ticket, key, err := messages.NewTicket(cname, a.realm, a.principal, a.realm, types.NewKrbFlags(),
		a.keytab, 18, 1, now, now, now.Add(time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := types.NewAuthenticator(a.realm, cname)
	if err != nil {
		t.Fatal(err)
	}
	auth.Cksum = types.Checksum{CksumType: chksumtype.GSSAPI, Checksum: make([]byte, 24)}
	binary.LittleEndian.PutUint32(auth.Cksum.Checksum[:4], 16)
	binary.LittleEndian.PutUint32(auth.Cksum.Checksum[20:], uint32(gssapi.ContextFlagMutual|gssapi.ContextFlagReplay|gssapi.ContextFlagInteg))
	if mutate != nil {
		mutate(&auth, &ticket)
	}
	req, err := messages.NewAPReq(ticket, key, auth)
	if err != nil {
		t.Fatal(err)
	}
	types.SetFlag(&req.APOptions, flags.APOptionMutualRequired)
	b, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Standard initial-context token: application 0, Kerberos OID and TOK_ID.
	header := []byte{6, 9, 42, 134, 72, 134, 247, 18, 1, 2, 2, 1, 0}
	return asn1tools.AddASNAppTag(append(header, b...), 0), key
}

func TestAcceptorAndMutualReply(t *testing.T) {
	a := testAcceptor(t)
	input, key := testAPReq(t, a, nil)
	c, output, err := a.accept(input)
	if err != nil {
		t.Fatal(err)
	}
	if c.principal != "member$@EXAMPLE" {
		t.Fatal(c.principal)
	}
	var reply spnego.KRB5Token
	if err := reply.Unmarshal(output); err != nil || !reply.IsAPRep() {
		t.Fatalf("invalid mutual reply: %v", err)
	}
	plain, err := crypto.DecryptEncPart(reply.APRep.EncPart, key, keyusage.AP_REP_ENCPART)
	if err != nil {
		t.Fatal(err)
	}
	var part messages.EncAPRepPart
	if err := part.Unmarshal(plain); err != nil || part.SequenceNumber < 0 || uint64(part.SequenceNumber) != c.send {
		t.Fatalf("invalid reply sequence: %v", err)
	}
	if _, _, err := a.accept(input); err == nil {
		t.Fatal("accepted replayed AP-REQ")
	}
}

func TestAcceptorRejects(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*types.Authenticator, *messages.Ticket)
	}{
		{"wrong service", func(_ *types.Authenticator, ticket *messages.Ticket) { ticket.SName.NameString[1] = "other.example" }},
		{"wrong realm", func(_ *types.Authenticator, ticket *messages.Ticket) { ticket.Realm = "OTHER" }},
		{"authenticator realm", func(auth *types.Authenticator, _ *messages.Ticket) { auth.CRealm = "OTHER" }},
		{"short checksum", func(auth *types.Authenticator, _ *messages.Ticket) { auth.Cksum.Checksum = []byte{0} }},
		{"no integrity", func(auth *types.Authenticator, _ *messages.Ticket) { clear(auth.Cksum.Checksum[20:]) }},
		{"channel binding", func(auth *types.Authenticator, _ *messages.Ticket) { auth.Cksum.Checksum[4] = 1 }},
		{"clock skew", func(auth *types.Authenticator, _ *messages.Ticket) { auth.CTime = time.Now().Add(-time.Hour) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := testAcceptor(t)
			input, _ := testAPReq(t, a, test.mutate)
			if _, _, err := a.accept(input); err == nil {
				t.Fatal("accepted invalid AP-REQ")
			}
		})
	}
}

func FuzzAcceptor(f *testing.F) {
	f.Add([]byte{0x60, 0x00})
	a := &acceptor{keytab: keytab.New()}
	f.Fuzz(func(_ *testing.T, input []byte) { _, _, _ = a.accept(input) })
}

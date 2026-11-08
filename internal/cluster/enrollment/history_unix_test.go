//go:build unix

package enrollment

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
)

func TestAdmissionZeroValueRefusesWithoutPanic(t *testing.T) {
	for _, a := range []*Admission{nil, {}} {
		if err := a.Check("", "", Issuer{}); !errors.Is(err, ErrRefused) {
			t.Fatal("unissued admission did not refuse", err)
		}
		if a.State() != nil {
			t.Fatal("unissued admission exposed a state")
		}
	}
}

func TestMemberHistoryCapacityNeverPrunesRevocations(t *testing.T) {
	s, issuer, now := owner(t)
	for range MaxMemberHistory - 1 {
		issued, inv, err := s.Issue(issuer, "replacement", "127.0.0.2:8082", now, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		i := identity(t)
		proposal, err := inv.Propose(i)
		if err != nil {
			t.Fatal(err)
		}
		joined, err := issued.Accept(proposal, issuer, now)
		if err != nil {
			t.Fatal(err)
		}
		revoked, err := joined.Revoke(i.Trust().MemberID(), now)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		s, err = revoked.PruneInvitations(now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(s.Members()) != MaxMemberHistory {
		t.Fatal("revoked history was pruned")
	}
	issued, inv, err := s.Issue(issuer, "replacement", "127.0.0.2:8082", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := inv.Propose(identity(t))
	if err != nil {
		t.Fatal(err)
	}
	before := encoded(t, issued)
	if next, err := issued.Accept(proposal, issuer, now); !errors.Is(err, ErrCapacity) || next != nil {
		t.Fatal("historical capacity admitted a new member", err)
	}
	if !bytes.Equal(before, encoded(t, issued)) {
		t.Fatal("capacity refusal changed history")
	}
	reopened, err := Decode(before)
	if err != nil || len(reopened.Members()) != MaxMemberHistory {
		t.Fatal("history did not survive codec", err)
	}
}

func TestRetainsHistoryRejectsValidButRewrittenOwners(t *testing.T) {
	s, issuer, now := owner(t)
	issued, inv, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	i := identity(t)
	proposal, err := inv.Propose(i)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := issued.Accept(proposal, issuer, now)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := joined.Revoke(i.Trust().MemberID(), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"member", "revocation", "consumption", "secret-hash", "expiry", "early-prune"} {
		t.Run(field, func(t *testing.T) {
			candidate, err := Decode(encoded(t, revoked))
			if err != nil {
				t.Fatal(err)
			}
			candidate.doc.Revision++
			switch field {
			case "member":
				for j := range candidate.doc.Members {
					if candidate.doc.Members[j].ID == issuer.MemberID {
						candidate.doc.Members[j].Address = "127.0.0.9:8082"
					}
				}
			case "revocation":
				for j := range candidate.doc.Members {
					candidate.doc.Members[j].Revoked = false
				}
			case "consumption":
				candidate.doc.Invitations[0].UsedBy = ""
			case "secret-hash":
				candidate.doc.Invitations[0].Hash = bytes.Repeat([]byte{1}, 32)
			case "expiry":
				candidate.doc.Invitations[0].ExpiresAt = now.Add(2 * time.Minute)
			case "early-prune":
				candidate.doc.Invitations = []inviteRecord{}
			}
			// Structural validity alone is deliberately insufficient for install.
			if _, err := candidate.Encode(); err != nil {
				t.Fatal("invalid history fixture", err)
			}
			if candidate.RetainsHistory(revoked) {
				t.Fatal("rewritten historical owner accepted")
			}
		})
	}
}

func TestRevokedKeyCannotReturnUnderNewCertificateAndUUID(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	makeTrust := func() memberidentity.Trust {
		id := uuid.NewString()
		now := time.Now()
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: id + ".member.kudurru.invalid"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}
		der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		trust, err := memberidentity.ParseTrust(id, der)
		if err != nil {
			t.Fatal(err)
		}
		return trust
	}
	s, issuer, now := owner(t)
	first, replacement := makeTrust(), makeTrust()
	if first.Fingerprint() == replacement.Fingerprint() || first.KeyFingerprint() != replacement.KeyFingerprint() {
		t.Fatal("fixture did not reissue the same key")
	}
	for n, trust := range []memberidentity.Trust{first, replacement} {
		issued, inv, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		m := Member{ID: trust.MemberID(), Name: "follower", Address: "127.0.0.2:8082", Trust: trust}
		p := Proposal{invitation: inv, member: m, signature: ed25519.Sign(key, append([]byte("kudurru/member-enrollment-proof/v1\x00"), statement(inv, m)...))}
		joined, err := issued.Accept(p, issuer, now)
		if n == 1 {
			if !errors.Is(err, ErrRefused) || joined != nil {
				t.Fatal("revoked signing key returned under a new certificate", err)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		s, err = joined.Revoke(first.MemberID(), now)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestProposalSignatureBindsOriginalInvitationLifetime(t *testing.T) {
	s, issuer, now := owner(t)
	issued, inv, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	p, err := inv.Propose(identity(t))
	if err != nil {
		t.Fatal(err)
	}
	// Even an otherwise valid successor document cannot extend an old signed
	// proposal's lifetime by repeating its ID/hash and rewriting timestamps.
	changed, err := Decode(encoded(t, issued))
	if err != nil {
		t.Fatal(err)
	}
	changed.doc.Invitations[0].ExpiresAt = now.Add(2 * time.Minute)
	p.invitation.record.ExpiresAt = changed.doc.Invitations[0].ExpiresAt
	if _, err := changed.Accept(p, issuer, now); !errors.Is(err, ErrRefused) {
		t.Fatal("signature permitted lifetime substitution", err)
	}
}

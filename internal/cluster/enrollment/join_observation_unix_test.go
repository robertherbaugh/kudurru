//go:build unix

package enrollment

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
)

func TestJoinObservationRetainsExpiredAcceptanceWithoutProposalAuthority(t *testing.T) {
	s, issuer, now := owner(t)
	s, inv, err := s.Issue(issuer, "receiver", "127.0.0.2:8082", now, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	local := identity(t)
	proposal, err := inv.Propose(local)
	if err != nil {
		t.Fatal(err)
	}
	issued := s
	s, err = s.Accept(proposal, issuer, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wire, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want, err := inv.JoinFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(inv.ExpiresAt()) + time.Millisecond)
	if _, err := ParseInvitation(wire); err == nil {
		t.Fatal("expired invitation regained proposal parsing")
	}
	// Propose is a key-possession statement; the actual Accept boundary owns
	// expiry. Signing a later statement must not regain acceptance authority.
	expiredProposal, err := inv.Propose(local)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issued.Accept(expiredProposal, issuer, time.Now()); err == nil {
		t.Fatal("expired proposal regained acceptance authority")
	}
	if err := inv.CheckAccepted(s, local.Trust()); err == nil {
		t.Fatal("ordinary join lifetime extended")
	}
	o, err := ParseJoinObservation(wire)
	if err != nil {
		t.Fatal(err)
	}
	got, err := o.JoinFingerprint()
	if err != nil || got != want || !o.original.record.ExpiresAt.Equal(inv.record.ExpiresAt) {
		t.Fatal("observation replaced original identity or expiry", err)
	}
	if err := o.CheckAccepted(s, local.Trust()); err != nil {
		t.Fatal("retained historical acceptance refused", err)
	}
	raw, err := json.Marshal(o) //nolint:staticcheck // SA9005: deliberately verify generic encoding cannot expose the private invitation.
	if err != nil || string(raw) != "{}" {
		t.Fatal("observation exported private material", err)
	}
	for _, mode := range []string{"ticket", "unconsumed", "primary", "target", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			bad := *s
			bad.doc.Invitations = append([]inviteRecord(nil), s.doc.Invitations...)
			bad.doc.Members = append([]memberRecord(nil), s.doc.Members...)
			switch mode {
			case "ticket":
				bad.doc.Invitations = nil
			case "unconsumed":
				bad.doc.Invitations[0].UsedBy = ""
			default:
				for n, m := range bad.doc.Members {
					if mode == "primary" && m.ID == issuer.MemberID {
						bad.doc.Members[n].Address = "127.0.0.3:8082"
					}
					if m.ID == local.Trust().MemberID() {
						if mode == "target" {
							bad.doc.Members[n].Name = "replacement"
						}
						if mode == "revoked" {
							bad.doc.Members[n].Revoked = true
						}
					}
				}
			}
			if err := o.CheckAccepted(&bad, local.Trust()); err == nil {
				t.Fatal("missing/changed historical provenance accepted")
			}
		})
	}
	replacement, err := memberidentity.Create(filepath.Join(t.TempDir(), "replacement.json"), local.Trust().MemberID())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.CheckAccepted(s, replacement.Trust()); err == nil {
		t.Fatal("same UUID replacement CA accepted")
	}
}

//go:build unix

package enrollment

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
)

func identity(t *testing.T) *memberidentity.Identity {
	t.Helper()
	i, err := memberidentity.Create(filepath.Join(t.TempDir(), "member.json"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func owner(t *testing.T) (*State, Issuer, time.Time) {
	t.Helper()
	i := identity(t)
	now := time.Now().UTC()
	s, err := Initialize(uuid.NewString(), Member{ID: i.Trust().MemberID(), Name: "primary", Address: "127.0.0.1:8082", Trust: i.Trust()}, now)
	if err != nil {
		t.Fatal(err)
	}
	return s, Issuer{MemberID: i.Trust().MemberID(), Epoch: 1, PromotionID: uuid.NewString()}, now
}

func encoded(t *testing.T, s *State) []byte {
	t.Helper()
	raw, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestInvitationAtomicStateConsumptionAndProofBinding(t *testing.T) {
	s, issuer, now := owner(t)
	original := encoded(t, s)
	issued, invitation, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	i := identity(t)
	proposal, err := invitation.Propose(i)
	if err != nil {
		t.Fatal(err)
	}
	issuedBytes := encoded(t, issued)
	if bytes.Contains(issuedBytes, invitation.secret) || bytes.Contains(issuedBytes, []byte(base64.StdEncoding.EncodeToString(invitation.secret))) {
		t.Fatal("owner persisted the raw invitation secret")
	}
	for _, private := range []any{invitation, proposal} {
		raw, err := json.Marshal(private)
		if err != nil || string(raw) != "{}" {
			t.Fatal("generic encoding exposed private enrollment material", err)
		}
	}
	for _, change := range []string{"secret", "signature", "name", "address", "key", "issuer", "expired", "before-issue", "cluster"} {
		t.Run(change, func(t *testing.T) {
			bad := proposal
			actualIssuer, at := issuer, now
			switch change {
			case "secret":
				bad.invitation.secret = bytes.Repeat([]byte{1}, 32)
			case "signature":
				bad.signature = bytes.Repeat([]byte{1}, 64)
			case "name":
				bad.member.Name = "other"
			case "address":
				bad.member.Address = "127.0.0.3:8082"
			case "key":
				other := identity(t)
				bad.member.ID = other.Trust().MemberID()
				bad.member.Trust = other.Trust()
			case "issuer":
				actualIssuer.Epoch++
			case "expired":
				at = now.Add(time.Minute)
			case "before-issue":
				at = now.Add(-time.Nanosecond)
			case "cluster":
				bad.invitation.cluster = uuid.NewString()
			}
			if next, err := issued.Accept(bad, actualIssuer, at); !errors.Is(err, ErrRefused) || next != nil {
				t.Fatal("changed original proof admitted", err)
			}
			if !bytes.Equal(encoded(t, issued), issuedBytes) {
				t.Fatal("refusal mutated original owner")
			}
		})
	}
	accepted, err := issued.Accept(proposal, issuer, now.Add(time.Second))
	if err != nil || len(accepted.Members()) != 2 || !accepted.RetainsHistory(issued) {
		t.Fatal("valid one-time member admission failed", err)
	}
	if !bytes.Equal(original, encoded(t, s)) || !bytes.Equal(issuedBytes, encoded(t, issued)) {
		t.Fatal("transition mutated a prior immutable state")
	}
	if _, err := accepted.Accept(proposal, issuer, now.Add(2*time.Second)); !errors.Is(err, ErrRefused) {
		t.Fatal("consumed invitation replayed", err)
	}
	back, err := Decode(encoded(t, accepted))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := back.Accept(proposal, issuer, now.Add(2*time.Second)); !errors.Is(err, ErrRefused) {
		t.Fatal("serialization reset consumption", err)
	}
	members := back.Members()
	members[0].Name = "substituted"
	if bytes.Equal(encoded(t, back), nil) || !bytes.Equal(encoded(t, accepted), encoded(t, back)) {
		t.Fatal("diagnostic members changed the owner")
	}
}

func TestRevocationPreservesIdentityAndCancelsEarlierTargetInvitations(t *testing.T) {
	s, issuer, now := owner(t)
	issued, first, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	issued, second, err := issued.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	i := identity(t)
	p, err := first.Propose(i)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := issued.Accept(p, issuer, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := joined.Revoke(i.Trust().MemberID(), now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !revoked.RetainsHistory(joined) || len(revoked.Members()) != 2 {
		t.Fatal("revocation lost identity history")
	}
	replacement := identity(t)
	oldProposal, err := second.Propose(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := revoked.Accept(oldProposal, issuer, now.Add(3*time.Second)); !errors.Is(err, ErrRefused) {
		t.Fatal("old invitation rescued a revoked target with a new key", err)
	}
	issuedAgain, newInvite, err := revoked.Issue(issuer, "follower", "127.0.0.2:8082", now.Add(3*time.Second), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := newInvite.Propose(i)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuedAgain.Accept(oldKey, issuer, now.Add(4*time.Second)); !errors.Is(err, ErrRefused) {
		t.Fatal("new invitation resurrected a revoked identity", err)
	}
	newKey, err := newInvite.Propose(replacement)
	if err != nil {
		t.Fatal(err)
	}
	rejoined, err := issuedAgain.Accept(newKey, issuer, now.Add(4*time.Second))
	if err != nil || !rejoined.RetainsHistory(revoked) || len(rejoined.Members()) != 3 {
		t.Fatal("explicit new identity lost prior revocation", err)
	}
	pruned, err := rejoined.PruneInvitations(now.Add(time.Minute))
	if err != nil || !pruned.RetainsHistory(rejoined) || len(pruned.Members()) != 3 {
		t.Fatal("expiry pruning lost member history", err)
	}
	if len(pruned.doc.Invitations) != 0 {
		t.Fatal("expired invitation records were not removed")
	}
	if _, err := pruned.Accept(newKey, issuer, now.Add(5*time.Second)); err == nil {
		t.Fatal("clock rollback revived a removed token")
	}
}

// A receiver that replaces a member cannot take that member's name or internal
// address while the member is active, and nothing in the product revokes a
// member today. The replacement is therefore a new member under its own name
// and address; the member it replaces stays in the roster unchanged.
func TestReplacementReceiverNeedsItsOwnNameAndAddress(t *testing.T) {
	s, issuer, now := owner(t)
	issued, invitation, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	stuck := identity(t)
	proposal, err := invitation.Propose(stuck)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := issued.Accept(proposal, issuer, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	before := encoded(t, joined)
	for _, target := range []struct{ name, address string }{
		{"follower", "127.0.0.2:8082"}, {"follower", "127.0.0.3:8082"}, {"follower-b", "127.0.0.2:8082"}, {"primary", "127.0.0.3:8082"}, {"follower-b", "127.0.0.1:8082"},
	} {
		if next, _, err := joined.Issue(issuer, target.name, target.address, now.Add(2*time.Second), now.Add(time.Minute)); !errors.Is(err, ErrRefused) || next != nil {
			t.Fatalf("an invitation for %s at %s, which an active member holds: got %v, want %v", target.name, target.address, err, ErrRefused)
		}
	}
	if !bytes.Equal(before, encoded(t, joined)) {
		t.Fatal("a refused invitation changed the roster")
	}
	reissued, replacement, err := joined.Issue(issuer, "follower-b", "127.0.0.2:8083", now.Add(2*time.Second), now.Add(time.Minute))
	if err != nil {
		t.Fatal("a replacement under its own name and address was refused", err)
	}
	// The old identity cannot take the new place either.
	reused, err := replacement.Propose(stuck)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reissued.Accept(reused, issuer, now.Add(3*time.Second)); !errors.Is(err, ErrRefused) {
		t.Fatal("an enrolled identity joined a second time", err)
	}
	fresh, err := replacement.Propose(identity(t))
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := reissued.Accept(fresh, issuer, now.Add(3*time.Second))
	if err != nil || !replaced.RetainsHistory(joined) {
		t.Fatal("the replacement receiver was not admitted", err)
	}
	members := replaced.Members()
	if len(members) != 3 || members[1].ID != stuck.Trust().MemberID() || members[1].Name != "follower" || members[1].Revoked || members[2].Name != "follower-b" || members[2].Revoked {
		t.Fatal("the replaced member did not stay in the roster beside its replacement")
	}
}

func TestInvitationAndActiveMemberCapacityDoNotPrune(t *testing.T) {
	s, issuer, now := owner(t)
	var first Invitation
	for n := range MaxInvitations {
		next, inv, err := s.Issue(issuer, fmt.Sprintf("member-%d", n), fmt.Sprintf("127.0.0.1:%d", 9000+n), now, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			first = inv
		}
		s = next
	}
	before := encoded(t, s)
	if _, _, err := s.Issue(issuer, "overflow", "127.0.0.1:10000", now, now.Add(time.Minute)); !errors.Is(err, ErrCapacity) {
		t.Fatal("invitation capacity silently grew", err)
	}
	if !bytes.Equal(before, encoded(t, s)) {
		t.Fatal("capacity refusal pruned old invitations")
	}
	proposal, err := first.Propose(identity(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(proposal, issuer, now); err != nil {
		t.Fatal("capacity refusal damaged original invitation", err)
	}
	// Exercise the real public transitions up to the independent active cap.
	s, issuer, now = owner(t)
	for n := range MaxActiveMembers - 1 {
		issued, inv, err := s.Issue(issuer, fmt.Sprintf("member-%d", n), fmt.Sprintf("127.0.0.1:%d", 9000+n), now, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		proposal, err := inv.Propose(identity(t))
		if err != nil {
			t.Fatal(err)
		}
		s, err = issued.Accept(proposal, issuer, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	issued, inv, err := s.Issue(issuer, "overflow", "127.0.0.1:10000", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	proposal, err = inv.Propose(identity(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issued.Accept(proposal, issuer, now); !errors.Is(err, ErrCapacity) {
		t.Fatal("active member cap silently grew", err)
	}
	if len(issued.Members()) != MaxActiveMembers {
		t.Fatal("refusal changed existing members")
	}
}

func TestCompleteOwnerCodecAndHistoryRefuseCorruption(t *testing.T) {
	s, issuer, now := owner(t)
	issued, _, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	raw := encoded(t, issued)
	for _, bad := range [][]byte{nil, []byte(`{}`), append([]byte(`{"version":1,`), raw[1:]...), append(bytes.Clone(raw), ' '), append(bytes.Clone(raw), raw...)} {
		if _, err := Decode(bad); err == nil {
			t.Fatal("noncanonical or absent owner accepted")
		}
	}
	for _, edit := range []func(*document){
		func(d *document) { d.Version++ }, func(d *document) { d.Revision = 0 }, func(d *document) { d.Members = nil },
		func(d *document) { d.Members = append(d.Members, d.Members[0]) },
		func(d *document) { d.Invitations[0].UsedBy = uuid.NewString() },
		func(d *document) { d.Invitations[0].Issuer.MemberID = uuid.NewString() },
		func(d *document) { d.Invitations[0].ExpiresAt = d.Invitations[0].IssuedAt.Add(11 * time.Minute) },
	} {
		var d document
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		edit(&d)
		bad, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(bad); err == nil {
			t.Fatal("corrupted complete owner accepted")
		}
	}
	revoked, err := issued.Revoke(issuer.MemberID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !revoked.RetainsHistory(issued) {
		t.Fatal("revocation is not a valid forward transition")
	}
	if issued.RetainsHistory(revoked) {
		t.Fatal("rollback resurrected the issuer")
	}
	if _, _, err := revoked.Issue(issuer, "rescue", "127.0.0.3:8082", now.Add(2*time.Second), now.Add(time.Minute)); !errors.Is(err, ErrRefused) {
		t.Fatal("revoked issuer granted new membership", err)
	}
}

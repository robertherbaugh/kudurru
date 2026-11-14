//go:build unix

package enrollment

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestInvitationProposalWireRetainsOriginalTargetAndPossession(t *testing.T) {
	s, issuer, now := owner(t)
	issued, inv, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	packet, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseInvitation(packet)
	if err != nil {
		t.Fatal(err)
	}
	name, address := parsed.Target()
	term, endpoint, trust := parsed.IssuingPeer()
	if name != "follower" || address != "127.0.0.2:8082" || term != issuer || endpoint != "127.0.0.1:8082" || trust.Fingerprint() != inv.issuerTrust.Fingerprint() {
		t.Fatal("wire replaced original issuer or target")
	}
	proposal, err := parsed.Propose(identity(t))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proposal.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeProposal(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	admitted, err := issued.Admit(ctx, decoded, issuer)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := issued.Digest()
	after, _ := admitted.State().Digest()
	if err := admitted.Check(before, after, issuer); err != nil {
		t.Fatal(err)
	}
	// Encoding can carry a signature without verifying it. Durable admission
	// must not trust successful framing or a claimed public CA as possession.
	var w wireProposal
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	w.Signature = bytes.Repeat([]byte{1}, 64)
	// #nosec G117 -- corrupted proposal fixture in memory, never owner storage or log output
	forged, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	bad, err := DecodeProposal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issued.Admit(ctx, bad, issuer); !errors.Is(err, ErrRefused) {
		t.Fatal("framing alone granted key possession", err)
	}
}

func TestEnrollmentWireRejectsOversizeAmbiguousAndExpiredInput(t *testing.T) {
	s, issuer, now := owner(t)
	_, inv, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	packet, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(packet, invitationPrefix))
	if err != nil {
		t.Fatal(err)
	}
	var w wireInvitation
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatal(err)
	}
	w.IssuedAt = now.Add(-2 * time.Minute)
	w.ExpiresAt = now.Add(-time.Minute)
	// #nosec G117 -- expired invitation fixture in memory, never owner storage or log output
	expired, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", strings.Repeat("x", MaxInvitationBytes+1), packet + "=", invitationPrefix + base64.RawURLEncoding.EncodeToString(append([]byte(`{"version":1,`), body[1:]...)), invitationPrefix + base64.RawURLEncoding.EncodeToString(expired)} {
		if _, err := ParseInvitation(bad); err == nil {
			t.Fatal("invalid or expired invitation accepted")
		}
	}
	p, err := inv.Propose(identity(t))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{nil, bytes.Repeat([]byte("x"), MaxProposalBytes+1), append(bytes.Clone(raw), ' '), append([]byte(`{"version":1,`), raw[1:]...)} {
		if _, err := DecodeProposal(bad); err == nil {
			t.Fatal("ambiguous or oversized proposal accepted")
		}
	}
}

func TestAdmissionRetainsOriginalContextDigestsAndTerminalRefusal(t *testing.T) {
	for _, reason := range []string{"before", "after", "issuer", "canceled", "clock"} {
		t.Run(reason, func(t *testing.T) {
			s, issuer, now := owner(t)
			issued, inv, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			proposal, err := inv.Propose(identity(t))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			a, err := issued.Admit(ctx, proposal, issuer)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := issued.Digest()
			after, _ := a.State().Digest()
			badBefore, badAfter, badIssuer := before, after, issuer
			switch reason {
			case "before":
				badBefore = "changed"
			case "after":
				badAfter = "changed"
			case "issuer":
				badIssuer.Epoch++
			case "canceled":
				cancel()
			case "clock":
				a.life.last.Store(time.Now().Add(time.Second).UnixNano())
			}
			if err := a.Check(badBefore, badAfter, badIssuer); err == nil {
				t.Fatal("changed original admission survived")
			}
			if err := a.Check(before, after, issuer); !errors.Is(err, ErrRefused) {
				t.Fatal("terminal refusal was rescued by original-looking inputs", err)
			}
		})
	}
}

func TestAdmissionExpiresAtOriginalInvitationDeadline(t *testing.T) {
	s, issuer, now := owner(t)
	issued, inv, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := inv.Propose(identity(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	a, err := issued.Admit(ctx, proposal, issuer)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := issued.Digest()
	after, _ := a.State().Digest()
	time.Sleep(time.Until(inv.record.ExpiresAt) + time.Millisecond)
	if err := a.Check(before, after, issuer); !errors.Is(err, ErrRefused) {
		t.Fatal("original invitation lifetime was renewed", err)
	}
	if _, err := issued.Admit(context.Background(), proposal, issuer); !errors.Is(err, ErrRefused) {
		t.Fatal("unbounded request admitted", err)
	}
}

func TestAdmissionCopiesShareTerminalRefusal(t *testing.T) {
	s, issuer, now := owner(t)
	issued, inv, err := s.Issue(issuer, "follower", "127.0.0.2:8082", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := inv.Propose(identity(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	a, err := issued.Admit(ctx, proposal, issuer)
	if err != nil {
		t.Fatal(err)
	}
	copied := *a
	before, err := issued.Digest()
	if err != nil {
		t.Fatal(err)
	}
	after, err := a.State().Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Check("wrong original owner", after, issuer); !errors.Is(err, ErrRefused) {
		t.Fatal("changed source accepted", err)
	}
	if err := copied.Check(before, after, issuer); !errors.Is(err, ErrRefused) {
		t.Fatal("copy reopened a terminal admission", err)
	}
}

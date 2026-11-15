package resolverstate

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

func TestIdentityCheckpointChangesOwnerWithSameEffectiveTrust(t *testing.T) {
	a := fixtureKey(t, "checkpoint-identity")
	f := newOwnerFixture(t, []signingKey{a}, []signingKey{a})
	before, err := Project(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	f.step(t, []signingKey{a}, a)
	after, err := Project(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	oldVersion, newVersion := before.Identity().Owner, after.Identity().Owner
	if !slices.Equal(before.DS(), after.DS()) || before.DomainID() != after.DomainID() || before.OwnerID() == after.OwnerID() {
		t.Fatal("checkpoint-only change lost domain or reused complete owner identity")
	}
	if newVersion.Generation != oldVersion.Generation+1 || newVersion.Pure.Generation != oldVersion.Pure.Generation+1 || newVersion.Digest == oldVersion.Digest || newVersion.Pure.Digest == oldVersion.Pure.Digest || newVersion != f.owner.Version() {
		t.Fatal("complete envelope/pure successor versions were conflated")
	}
}

func TestIdentityEnvelopeOnlyChangeAndHistoryReplacement(t *testing.T) {
	a := fixtureKey(t, "envelope-identity")
	f := newOwnerFixture(t, []signingKey{a}, []signingKey{a})
	before, err := Project(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := accepted.EncodeRows(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	root := f.owner.Inspect()
	root.UpdatedAt = root.UpdatedAt.Add(time.Nanosecond)
	for i := range rows {
		if rows[i].Key == accepted.RootKey {
			rows[i].Data, err = json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	changed, err := accepted.DecodeRows(rows, root.ClusterID)
	if err != nil {
		t.Fatal("decode structural envelope-only fixture", err)
	}
	after, err := Project(changed)
	if err != nil || !slices.Equal(before.DS(), after.DS()) || before.DomainID() != after.DomainID() || before.OwnerID() == after.OwnerID() || before.Identity().Owner.Pure != after.Identity().Owner.Pure {
		t.Fatal("envelope-only change reused exact owner identity", err)
	}
	// A different history can frame the identical pure State structurally. This
	// is not an authorized runtime history replacement or a recovery operation.
	otherDormant, err := accepted.NewDormant(root.ClusterID, strings.Repeat("2", 32), f.dormant.Inspect().InitializedAt)
	if err != nil {
		t.Fatal(err)
	}
	otherOwner, err := accepted.ProposeManaged(otherDormant, f.state, f.owner.Inspect().UpdatedAt, f.observation.ValidUntil())
	if err != nil {
		t.Fatal(err)
	}
	other, err := Project(otherOwner)
	if err != nil || !slices.Equal(before.DS(), other.DS()) || before.Identity().Owner.Pure != other.Identity().Owner.Pure || before.DomainID() == other.DomainID() || before.OwnerID() == other.OwnerID() {
		t.Fatal("same pure keys under another history reused domain/owner", err)
	}
}

func TestIdentityRoundtripAndProposalAreOnlyStructural(t *testing.T) {
	a := fixtureKey(t, "structural-identity")
	f := newOwnerFixture(t, []signingKey{a}, []signingKey{a})
	proposal, err := Project(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := accepted.EncodeRows(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := accepted.DecodeRows(rows, f.owner.Inspect().ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	roundtrip, err := Project(decoded)
	if err != nil || proposal.Identity() != roundtrip.Identity() || proposal.OwnerID() != roundtrip.OwnerID() || proposal.DomainID() != roundtrip.DomainID() || !slices.Equal(proposal.DS(), roundtrip.DS()) {
		t.Fatal("unchanged structural roundtrip changed identity", err)
	}
	for i := range rows {
		rows[i].Data = bytes.Repeat([]byte("x"), len(rows[i].Data))
	}
	again, err := Project(decoded)
	if err != nil || again.OwnerID() != proposal.OwnerID() || !slices.Equal(again.DS(), proposal.DS()) {
		t.Fatal("decoded immutable history retained mutable row bytes", err)
	}
	// Exact bytes intentionally reproduce structural IDs. No Store, source
	// authentication, durable CAS, retired-generation floor, or runtime permit
	// exists here; matching hashes must never be presented as proof of those.
}

func TestIdentityAllDomainAndVersionComponentsAreBound(t *testing.T) {
	a := fixtureKey(t, "identity-components")
	f := newOwnerFixture(t, []signingKey{a}, []signingKey{a})
	p, err := Project(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	id := p.Identity()
	for _, field := range []string{"cluster", "history", "policy", "seed"} {
		t.Run(field, func(t *testing.T) {
			changed := id
			switch field {
			case "cluster":
				changed.ClusterID += "/other"
			case "history":
				changed.HistoryID = strings.Repeat("f", 32)
			case "policy":
				changed.PolicyID += "/other"
			case "seed":
				changed.SeedDigest = strings.Repeat("f", 64)
			}
			domain, owner := identities(changed, Usable)
			if domain == p.DomainID() || owner == p.OwnerID() {
				t.Fatal("domain component was not bound")
			}
		})
	}
	for _, field := range []string{"envelope-generation", "envelope-digest", "pure-generation", "pure-digest", "disposition"} {
		t.Run(field, func(t *testing.T) {
			changed, disposition := id, Usable
			switch field {
			case "envelope-generation":
				changed.Owner.Generation++
			case "envelope-digest":
				changed.Owner.Digest = strings.Repeat("f", 64)
			case "pure-generation":
				changed.Owner.Pure.Generation++
			case "pure-digest":
				changed.Owner.Pure.Digest = strings.Repeat("f", 64)
			case "disposition":
				disposition = Quarantined
			}
			// Hash-component testing does not construct a Projection from this
			// altered metadata. Project still requires complete owner validation.
			domain, owner := identities(changed, disposition)
			if domain != p.DomainID() || owner == p.OwnerID() {
				t.Fatal("exact owner component was omitted or changed the domain")
			}
		})
	}
}

func TestIdentityFramingSeparatesFieldsAndFormatDomains(t *testing.T) {
	left := framedDigest("format-a", "ab", "c")
	if left == framedDigest("format-a", "a", "bc") || left == framedDigest("format-b", "ab", "c") || left == framedDigest("format-a", "ab", "c", "") {
		t.Fatal("field boundaries or format domains were conflated")
	}
	withDelimiter := framedDigest("format-a", "a|b", "c")
	if withDelimiter == framedDigest("format-a", "a", "b|c") {
		t.Fatal("text delimiter substituted for length framing")
	}
}

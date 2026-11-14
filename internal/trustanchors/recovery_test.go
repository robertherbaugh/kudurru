package trustanchors

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func recoveryFixture(t *testing.T) (*fixture, State) {
	t.Helper()
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a)
	ra := revoke(f.a)
	lost := f.step(s, f.wire([]testKey{ra, f.b}, ra), f.point(time.Hour))
	if !lost.doc.Quarantined {
		t.Fatal("fixture did not actually lose its sole anchor")
	}
	return f, lost
}

func TestRecoveryExplicitTrustRetainsTombstonesAndOrdinaryContinuation(t *testing.T) {
	f, lost := recoveryFixture(t)
	before, err := Encode(lost)
	if err != nil {
		t.Fatal(err)
	}
	message := f.wire([]testKey{f.a, f.b, f.c}, f.b)
	next, proof, err := Recover(lost, []string{f.b.key.ToDS(dns.SHA256).String()}, message, f.point(2*time.Hour))
	if err != nil || proof.CheckRecovery(lost, next, f.point(2*time.Hour)) != nil {
		t.Fatal("explicit signed recovery refused", err)
	}
	if next.doc.Quarantined || next.doc.Generation != lost.doc.Generation+1 || next.doc.Format != RecoveryFormatVersion || len(next.doc.Recoveries) != 1 ||
		next.doc.PolicyID != lost.doc.PolicyID || next.doc.SeedDigest != lost.doc.SeedDigest || !reflect.DeepEqual(next.doc.Seeds, lost.doc.Seeds) {
		t.Fatal("recovery changed initial provenance or lost explicit version/history")
	}
	if !reflect.DeepEqual(findKey(t, next, f.a), findKey(t, lost, f.a)) || findKey(t, next, f.b).Status != Valid || findKey(t, next, f.c).Status != AddPending || findKey(t, next, f.c).Elapsed != 0 {
		t.Fatal("recovery revived a revoked key or credited pending hold-down")
	}
	encoded, err := Encode(next)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := proof.CheckRecovery(lost, restored, f.point(2*time.Hour+time.Second)); err != nil {
		t.Fatal("exact restored proposed bytes changed original proof", err)
	}
	if err := (RecoveryObservation{}).CheckRecovery(lost, restored, f.point(2*time.Hour)); !errors.Is(err, ErrProof) {
		t.Fatal("decoding alone fabricated recovery proof", err)
	}
	continued := f.step(restored, f.wire([]testKey{f.b, f.c}, f.b), f.point(2*time.Hour+time.Second))
	if !reflect.DeepEqual(continued.doc.Recoveries, next.doc.Recoveries) || !reflect.DeepEqual(findKey(t, continued, f.a), findKey(t, lost, f.a)) {
		t.Fatal("ordinary continuation lost recovery provenance or revoked history")
	}
	if err := proof.CheckRecovery(lost, continued, f.point(2*time.Hour+time.Second)); !errors.Is(err, ErrStale) {
		t.Fatal("recovery proof adopted successor", err)
	}
	if err := proof.CheckLifetime(f.point(3 * time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatal("original received TTL renewed", err)
	}
	again, err := Encode(lost)
	if err != nil || !bytes.Equal(before, again) || bytes.Contains(before, []byte("recoveries")) {
		t.Fatal("proposal changed original format-one history")
	}
	detached := next.Inspect()
	detached.Recoveries[0].Seeds[0] = "mutated"
	if next.doc.Recoveries[0].Seeds[0] == "mutated" {
		t.Fatal("diagnostic snapshot aliases permanent recovery history")
	}
}

func TestRecoveryRefusesRevokedUnusedUnauthenticatedAndLiveTrust(t *testing.T) {
	f, lost := recoveryFixture(t)
	for _, name := range []string{"revoked", "revoked-with-valid", "unused", "unsigned", "foreign-signer", "live", "rollback", "empty"} {
		t.Run(name, func(t *testing.T) {
			base, at := lost, f.point(2*time.Hour)
			seeds := []string{f.b.key.ToDS(dns.SHA256).String()}
			message := f.wire([]testKey{f.a, f.b}, f.b)
			switch name {
			case "revoked":
				seeds = []string{f.a.key.ToDS(dns.SHA256).String()}
				message = f.wire([]testKey{f.a, f.b}, f.a)
			case "revoked-with-valid":
				seeds = append(seeds, f.a.key.ToDS(dns.SHA256).String())
			case "unused":
				seeds = append(seeds, f.c.key.ToDS(dns.SHA256).String())
			case "unsigned":
				message = f.wire([]testKey{f.b})
			case "foreign-signer":
				message = f.wire([]testKey{f.a, f.b}, f.a)
			case "live":
				base = f.initial([]testKey{f.a}, f.a)
			case "rollback":
				at = f.point(0)
			case "empty":
				seeds = nil
			}
			if _, _, err := Recover(base, seeds, message, at); err == nil {
				t.Fatal("invalid operator recovery accepted")
			}
		})
	}
}

func TestRecoveryRepeatedLossKeepsEveryPriorRevocationAndReceipt(t *testing.T) {
	f, lost := recoveryFixture(t)
	b, _, err := Recover(lost, []string{f.b.key.ToDS(dns.SHA256).String()}, f.wire([]testKey{f.b}, f.b), f.point(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rb := revoke(f.b)
	secondLoss := f.step(b, f.wire([]testKey{rb}, rb), f.point(3*time.Hour))
	c, proof, err := Recover(secondLoss, []string{f.c.key.ToDS(dns.SHA256).String()}, f.wire([]testKey{f.c}, f.c), f.point(4*time.Hour))
	if err != nil || proof.CheckRecovery(secondLoss, c, f.point(4*time.Hour)) != nil || len(c.doc.Recoveries) != 2 {
		t.Fatal("second explicit recovery lost bounded history", err)
	}
	if !reflect.DeepEqual(findKey(t, c, f.a), findKey(t, lost, f.a)) || !reflect.DeepEqual(findKey(t, c, f.b), findKey(t, secondLoss, f.b)) || !reflect.DeepEqual(c.doc.Recoveries[0], b.doc.Recoveries[0]) {
		t.Fatal("second recovery erased an earlier tombstone or trust receipt")
	}
	for _, mutate := range []func(*Snapshot){
		func(d *Snapshot) { d.Format = FormatVersion },
		func(d *Snapshot) { d.Recoveries = nil },
		func(d *Snapshot) { d.Recoveries[0].PreviousDigest = "invalid" },
		func(d *Snapshot) { d.Recoveries[1].Generation = d.Recoveries[0].Generation },
		func(d *Snapshot) { d.Recoveries[1].At = d.Checkpoint.Wall.Add(time.Second) },
		func(d *Snapshot) { d.Recoveries[1].Seeds = d.Recoveries[0].Seeds },
	} {
		doc := c.Inspect()
		mutate(&doc)
		if _, err := Encode(State{doc: doc}); err == nil {
			t.Fatal("malformed permanent recovery history encoded")
		}
	}
}

func TestRecoveryPermanentHistoryCapacityRefusesWithoutPruning(t *testing.T) {
	f, state := recoveryFixture(t)
	for i := range MaxRecoveries {
		key := newFixture(t).b
		at := time.Duration(2*i+2) * time.Hour
		next, _, err := Recover(state, []string{key.key.ToDS(dns.SHA256).String()}, f.wire([]testKey{key}, key), f.point(at))
		if err != nil {
			t.Fatal("bounded recovery history refused early", i, err)
		}
		revoked := revoke(key)
		state = f.step(next, f.wire([]testKey{revoked}, revoked), f.point(at+time.Hour))
	}
	before, err := Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.doc.Recoveries) != MaxRecoveries || len(state.doc.Keys) != MaxRecoveries+1 {
		t.Fatal("bounded recovery lost permanent history before capacity")
	}
	key := newFixture(t).b
	if _, _, err := Recover(state, []string{key.key.ToDS(dns.SHA256).String()}, f.wire([]testKey{key}, key), f.point(time.Duration(2*MaxRecoveries+2)*time.Hour)); !errors.Is(err, ErrBounds) {
		t.Fatal("full recovery history pruned or exceeded its bound", err)
	}
	after, err := Encode(state)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("capacity refusal changed prior accepted history")
	}
}

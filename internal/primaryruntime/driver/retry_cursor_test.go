package driver

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
)

// A primary parent's retry cursor must move when a zone below it commits a
// lifecycle step, so a parent in backoff after a refusal (for example a
// boundary that was still moving) is retried as soon as its child progresses
// (D404). Before, only the parent's own source and lifecycle reset its
// backoff, and a recreated child waited out the parent's backoff of up to
// 32 s before the pair converged.
func TestParentRetryCursorFollowsDescendants(t *testing.T) {
	zone := func(name, apex string, typ public.ZoneType, rv string) *public.DNSZone {
		return &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), ResourceVersion: rv}, Spec: public.DNSZoneSpec{ZoneName: apex, Type: typ}}
	}
	ledger := func(name, apex, rv string, seq uint64) *zk.ZoneLifecycle {
		l := &zk.ZoneLifecycle{}
		l.ResourceVersion = rv
		l.Spec.Zone = zk.ZoneID{ObjectName: name, Apex: apex}
		l.Spec.Sequence = seq
		return l
	}
	build := func(childLedger *zk.ZoneLifecycle, child, sibling *public.DNSZone) (roster, []string) {
		all := roster{zones: map[string]*public.DNSZone{}, byApex: map[string]*public.DNSZone{}, ledgers: map[string]*zk.ZoneLifecycle{}}
		for _, z := range []*public.DNSZone{zone("parent", "example.", public.ZoneTypePrimary, "1"), child, sibling} {
			if z != nil {
				all.zones[z.Name] = z
				all.byApex[z.Spec.ZoneName] = z
			}
		}
		all.ledgers["parent"] = ledger("parent", "example.", "1", 4)
		if childLedger != nil {
			all.ledgers["child"] = childLedger
		}
		names := []string{"child", "other", "parent"}
		return all, names
	}
	cursor := func(all roster, names []string, name string) string {
		return retryCursor(all, descendantCursors(all, names), name)
	}
	child := zone("child", "child.example.", public.ZoneTypePrimary, "5")
	other := zone("other", "other.test.", public.ZoneTypePrimary, "7")
	base, names := build(ledger("child", "child.example.", "9", 3), child, other)
	want := cursor(base, names, "parent")

	// A child lifecycle commit (fence, activation, retirement, release).
	moved, _ := build(ledger("child", "child.example.", "10", 4), child, other)
	if cursor(moved, names, "parent") == want {
		t.Fatal("a child lifecycle commit did not move the parent's retry cursor")
	}
	// A child DNSZone deleted: only its historical ledger remains.
	deleted, _ := build(ledger("child", "child.example.", "9", 3), nil, other)
	if cursor(deleted, names, "parent") == want {
		t.Fatal("deleting the child did not move the parent's retry cursor")
	}
	// A zone outside the parent's apex never moves it.
	unrelated, _ := build(ledger("child", "child.example.", "9", 3), child, zone("other", "other.test.", public.ZoneTypePrimary, "8"))
	if cursor(unrelated, names, "parent") != want {
		t.Fatal("a zone outside the parent's apex moved its retry cursor")
	}
	// A child still follows its parent, and a nonprimary parent adds nothing.
	parentMoved, _ := build(ledger("child", "child.example.", "9", 3), child, other)
	parentMoved.ledgers["parent"] = ledger("parent", "example.", "2", 5)
	if cursor(parentMoved, names, "child") == cursor(base, names, "child") {
		t.Fatal("a parent lifecycle commit did not move the child's retry cursor")
	}
	secondary, _ := build(ledger("child", "child.example.", "9", 3), child, other)
	secondary.zones["parent"].Spec.Type = public.ZoneTypeSecondary
	if h := descendantCursors(secondary, names)["parent"]; h != nil {
		t.Fatal("a nonprimary parent was given a descendant cursor")
	}
}

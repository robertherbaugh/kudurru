package audit_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

func zone(name string, ns ...string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DNSZone"},
		ObjectMeta: metav1.ObjectMeta{Name: name, ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}},
		Spec:       v1alpha1.DNSZoneSpec{ZoneName: name + ".", Type: v1alpha1.ZoneTypePrimary, Nameservers: ns},
	}
}

func wrapped(t *testing.T, inner store.Store, status bool) (store.Store, *audit.Memory) {
	t.Helper()
	scheme, err := internalkinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	rec := &audit.Memory{}
	return audit.WrapStore(inner, rec, audit.WrapOptions{Node: "node-a", Scheme: scheme, Status: status}), rec
}

func TestWrapStoreRecordsWrites(t *testing.T) {
	st, rec := wrapped(t, memory.New(), false)
	ctx := store.WithActor(context.Background(), "User:alice")

	if _, err := st.Apply(ctx, zone("example.com", "ns1.example.com."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(ctx, zone("example.com", "ns2.example.com."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	// A refused write is not recorded (the API audits denials itself).
	if _, err := st.Apply(ctx, zone("example.com"), store.ApplyOptions{Manager: "file:zones.yaml"}); !store.IsManagedBy(err) {
		t.Fatalf("managed-by write: %v", err)
	}
	if err := st.UpdateStatus(ctx, zone("example.com")); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(context.Background(), store.Key{Kind: "DNSZone", Name: "example.com"}, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, store.Key{Kind: "DNSZone", Name: "missing"}, store.DeleteOptions{}); err == nil {
		t.Fatal("deleting a missing object succeeded")
	}

	evs := rec.Events()
	var actions []string
	for _, ev := range evs {
		actions = append(actions, ev.Action)
	}
	if strings.Join(actions, ",") != "dnszone.create,dnszone.update,dnszone.delete" {
		t.Fatalf("actions %v", actions)
	}
	create, update, del := evs[0], evs[1], evs[2]
	if create.Actor.String() != "User:alice" || create.Node != "node-a" || create.Manager != store.ManagerAPI ||
		create.Resource != (audit.Resource{Kind: "DNSZone", Name: "example.com"}) || create.Before != nil || create.After == nil || create.ResourceVersion == "" {
		t.Fatalf("create %+v", create)
	}
	if strings.Contains(string(create.After), "managedFields") || !strings.Contains(string(create.After), "ns1.example.com.") {
		t.Fatalf("create document %s", create.After)
	}
	if !strings.Contains(string(update.Before), "ns1.example.com.") || !strings.Contains(string(update.After), "ns2.example.com.") {
		t.Fatalf("update before %s after %s", update.Before, update.After)
	}
	// Without an actor in the context the manager acts.
	if del.Actor.String() != store.ManagerAPI || del.Actor.Kind != "" || del.After != nil || !strings.Contains(string(del.Before), "ns2.example.com.") {
		t.Fatalf("delete %+v", del)
	}
}

func TestWrapStoreStatusAndDeleteMany(t *testing.T) {
	st, rec := wrapped(t, memory.New(), true)
	ctx := context.Background()
	for _, n := range []string{"a.example", "b.example"} {
		if _, err := st.Apply(ctx, zone(n), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpdateStatus(ctx, zone("a.example")); err != nil {
		t.Fatal(err)
	}
	// The memory driver deletes in one transaction; the wrapper keeps it.
	if _, ok := st.(store.MultiDeleter); !ok {
		t.Fatal("wrapper hides MultiDeleter")
	}
	_, atomic, err := store.DeleteAll(ctx, st, []store.DeleteRequest{
		{Key: store.Key{Kind: "DNSZone", Name: "a.example"}}, {Key: store.Key{Kind: "DNSZone", Name: "gone"}}, {Key: store.Key{Kind: "DNSZone", Name: "b.example"}},
	})
	if err != nil || !atomic {
		t.Fatalf("delete all: atomic %v: %v", atomic, err)
	}
	var actions []string
	for _, ev := range rec.Events() {
		actions = append(actions, ev.Action+" "+ev.Resource.Name)
	}
	want := "dnszone.create a.example,dnszone.create b.example,dnszone.status a.example,dnszone.delete a.example,dnszone.delete b.example"
	if strings.Join(actions, ",") != want {
		t.Fatalf("actions %v", actions)
	}
}

func TestWrapStoreInternalKindsHaveNoDocuments(t *testing.T) {
	st, rec := wrapped(t, memory.New(), false)
	tok := &internalkinds.APIToken{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: internalkinds.KindAPIToken},
		ObjectMeta: metav1.ObjectMeta{Name: "tok"},
		Spec:       internalkinds.APITokenSpec{SecretHash: strings.Repeat("ab", 32)},
	}
	if _, err := st.Apply(context.Background(), tok, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	evs := rec.Events()
	if len(evs) != 1 || evs[0].Action != "apitoken.create" || evs[0].Before != nil || evs[0].After != nil {
		t.Fatalf("events %+v", evs)
	}
}

// unchanged is a driver whose Apply stores nothing new: it returns the
// stored object with its resourceVersion, like the apiserver for an
// identical server-side apply.
type unchanged struct{ *memory.Store }

func (u unchanged) Apply(ctx context.Context, obj store.Object, _ store.ApplyOptions) (store.Object, error) {
	cur := &v1alpha1.DNSZone{}
	if err := u.Get(ctx, store.KeyOf(obj), cur); err != nil {
		return nil, err
	}
	return cur, nil
}

func TestWrapStoreSkipsNoOpApply(t *testing.T) {
	mem := memory.New()
	if _, err := mem.Apply(context.Background(), zone("example.com"), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	st, rec := wrapped(t, unchanged{mem}, false)
	if _, err := st.Apply(context.Background(), zone("example.com"), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	if evs := rec.Events(); len(evs) != 0 {
		b, _ := json.Marshal(evs)
		t.Fatalf("no-op apply recorded: %s", b)
	}
}

type failing struct{}

func (failing) Record(context.Context, audit.Event) error { return context.DeadlineExceeded }

func TestWrapStoreRecorderFailureKeepsTheWrite(t *testing.T) {
	scheme, err := internalkinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	st := audit.WrapStore(memory.New(), failing{}, audit.WrapOptions{Scheme: scheme})
	if _, err := st.Apply(context.Background(), zone("example.com"), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("write failed with the recorder: %v", err)
	}
}

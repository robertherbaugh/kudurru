package audit_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
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
	usr := &internalkinds.LocalUser{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: internalkinds.KindLocalUser},
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       internalkinds.LocalUserSpec{PasswordHash: "secret-password-hash"},
	}
	if _, err := st.Apply(context.Background(), usr, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	evs := rec.Events()
	if len(evs) != 1 || evs[0].Action != "localuser.create" || evs[0].Before != nil || evs[0].After != nil {
		t.Fatalf("events %+v", evs)
	}
}

func TestTokenAuditDetailsExcludeCredentials(t *testing.T) {
	for _, driver := range []string{"wrapped", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			var st store.Store
			var events func() []audit.Event
			if driver == "wrapped" {
				var rec *audit.Memory
				st, rec = wrapped(t, memory.New(), false)
				events = rec.Events
			} else {
				st = authtest.SQLite(t)
				events = func() []audit.Event {
					rows, err := st.(*sqlite.Store).ReadAudit(context.Background(), sqlite.AuditQuery{})
					if err != nil {
						t.Fatal(err)
					}
					out := make([]audit.Event, len(rows))
					for i, row := range rows {
						out[i] = audit.FromEntry(row)
					}
					return out
				}
			}
			tok := &internalkinds.APIToken{
				ObjectMeta: metav1.ObjectMeta{Name: "abcdefgh2345", Annotations: map[string]string{"credential": "secret-metadata"}},
				Spec: internalkinds.APITokenSpec{
					Owner:       internalkinds.TokenOwner{Kind: "User", Name: "alice", ID: "local:alice", Provider: "local"},
					Description: "deployment check", SecretHash: strings.Repeat("ab", 32),
					Scopes: []string{"viewer"}, Grants: []string{"admin"},
				},
			}
			actor := audit.Actor{Kind: "User", ID: "local:alice", Name: "alice", Auth: "token"}
			req := audit.Request{IP: "127.0.0.1", UA: "deployment-script", Method: "POST", Path: "/api/v1/auth/tokens"}
			path := ""
			ctx := audit.WithRequest(store.WithActor(context.Background(), actor.String()), actor, req, func() string { return path })
			path = "binding" // the authorization decision is made after actor middleware
			out, err := st.Apply(ctx, tok, store.ApplyOptions{Manager: store.ManagerSystem})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Delete(ctx, store.KeyOf(out), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
				t.Fatal(err)
			}
			evs := events()
			if len(evs) != 2 || evs[0].Action != "apitoken.create" || evs[1].Action != "apitoken.delete" || evs[0].Before != nil || evs[1].After != nil {
				t.Fatalf("unexpected events: %+v", evs)
			}
			for _, doc := range []json.RawMessage{evs[0].After, evs[1].Before} {
				for _, want := range []string{"deployment check", "local:alice", "viewer", `"expiresAt":null`} {
					if !strings.Contains(string(doc), want) {
						t.Errorf("document lacks %q: %s", want, doc)
					}
				}
				for _, secret := range []string{tok.Spec.SecretHash, "secretHash", "secret-metadata", "annotations", "grants"} {
					if strings.Contains(string(doc), secret) {
						t.Errorf("document includes credential field %q", secret)
					}
				}
			}
			if driver == "wrapped" {
				if !reflect.DeepEqual(evs[0].Actor, actor) || evs[0].Request != req || evs[0].Authz != "binding" {
					t.Fatalf("request context lost: %+v", evs[0])
				}
			} else if err := st.(*sqlite.Store).VerifyAudit(ctx); err != nil {
				t.Fatal(err)
			}
		})
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

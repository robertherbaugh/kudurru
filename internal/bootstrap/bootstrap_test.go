package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

var (
	vOnce sync.Once
	vVal  *validator.Validator
	vErr  error
)

func testValidator(t *testing.T) *validator.Validator {
	t.Helper()
	vOnce.Do(func() { vVal, vErr = validator.New() })
	if vErr != nil {
		t.Fatal(vErr)
	}
	return vVal
}

func newStore(t *testing.T) *memory.Store {
	t.Helper()
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	return st
}

var (
	rootKey   = store.Key{Kind: "DNSZone", Name: RootZoneName}
	groupKey  = store.Key{Kind: "ClientGroup", Namespace: DefaultNamespace, Name: DefaultClientGroupName}
	policyKey = store.Key{Kind: "Policy", Namespace: DefaultNamespace, Name: DefaultPolicyName}
)

func TestSeedFirstBoot(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	res, err := Seed(ctx, st, Options{Validator: testValidator(t)})
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Key{groupKey, policyKey, rootKey}
	if !res.FirstBoot || !reflect.DeepEqual(res.Created, want) || len(res.Present) != 0 {
		t.Fatalf("result = %+v", res)
	}

	var zone v1alpha1.DNSZone
	if err := st.Get(ctx, rootKey, &zone); err != nil {
		t.Fatal(err)
	}
	if zone.Spec.ZoneName != "." || zone.Spec.Type != v1alpha1.ZoneTypeForward || zone.Spec.Forward == nil || zone.Spec.Forward.UpstreamGroup != "default" {
		t.Fatalf("root zone = %+v", zone.Spec)
	}
	if store.ManagedBy(&zone) != store.ManagerBootstrap || zone.Spec.DefaultTTL != 300 {
		t.Fatalf("root zone must be bootstrap-managed and defaulted: %q ttl=%d", store.ManagedBy(&zone), zone.Spec.DefaultTTL)
	}

	var group v1alpha1.ClientGroup
	if err := st.Get(ctx, groupKey, &group); err != nil {
		t.Fatal(err)
	}
	cidrs := []string{}
	for _, c := range group.Spec.Clients {
		cidrs = append(cidrs, c.CIDR)
	}
	if !reflect.DeepEqual(cidrs, []string{"0.0.0.0/0", "::/0"}) || store.ManagedBy(&group) != store.ManagerBootstrap {
		t.Fatalf("default group = %+v", group)
	}

	var pol v1alpha1.Policy
	if err := st.Get(ctx, policyKey, &pol); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pol.Spec.ClientGroupRefs, []string{"default"}) || len(pol.Spec.BlockListRefs) != 0 || pol.Spec.BlockTTL.Duration != 10*time.Minute {
		t.Fatalf("default policy = %+v", pol.Spec)
	}
}

func TestSeedIsIdempotentAndNeverOverridesTheUser(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, err := Seed(ctx, st, Options{}); err != nil {
		t.Fatal(err)
	}
	var zone v1alpha1.DNSZone
	if err := st.Get(ctx, rootKey, &zone); err != nil {
		t.Fatal(err)
	}

	// Second boot: nothing is written.
	res, err := Seed(ctx, st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FirstBoot || len(res.Created) != 0 {
		t.Fatalf("second boot result = %+v", res)
	}
	var again v1alpha1.DNSZone
	if err := st.Get(ctx, rootKey, &again); err != nil {
		t.Fatal(err)
	}
	if again.ResourceVersion != zone.ResourceVersion {
		t.Fatal("second boot rewrote the root zone")
	}

	// The operator edits the policy and deletes the group: both stick.
	edited := &v1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: DefaultPolicyName, Namespace: DefaultNamespace},
		Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, Priority: 7}}
	if _, err := st.Apply(ctx, edited, store.ApplyOptions{Manager: store.ManagerAPI, Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, groupKey, store.DeleteOptions{Manager: store.ManagerAPI, Force: true}); err != nil {
		t.Fatal(err)
	}
	if res, err := Seed(ctx, st, Options{}); err != nil || res.FirstBoot || len(res.Created) != 0 {
		t.Fatalf("seed after user changes = %+v, %v", res, err)
	}
	var pol v1alpha1.Policy
	if err := st.Get(ctx, policyKey, &pol); err != nil {
		t.Fatal(err)
	}
	if pol.Spec.Priority != 7 || store.ManagedBy(&pol) != store.ManagerAPI {
		t.Fatalf("user policy overwritten: %+v", pol)
	}
	if err := st.Get(ctx, groupKey, &v1alpha1.ClientGroup{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted default group recreated: %v", err)
	}
}

func TestSeedSkipsWhenAnyObjectExists(t *testing.T) {
	ctx := context.Background()
	for _, existing := range []store.Object{
		&v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypePrimary}},
		&v1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "kids", Namespace: "dns"}},
		&v1alpha1.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "lan", Namespace: "dns"}},
	} {
		t.Run(string(store.KindOf(existing)), func(t *testing.T) {
			st := newStore(t)
			if _, err := st.Apply(ctx, existing, store.ApplyOptions{Manager: store.ManagerFile("objects.d/x.yaml")}); err != nil {
				t.Fatal(err)
			}
			res, err := Seed(ctx, st, Options{})
			if err != nil || res.FirstBoot || len(res.Created) != 0 {
				t.Fatalf("seed with existing objects = %+v, %v", res, err)
			}
		})
	}
}

func TestSeedStarterBlockListHook(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	hook := func(context.Context) (*v1alpha1.BlockList, error) {
		return &v1alpha1.BlockList{
			ObjectMeta: metav1.ObjectMeta{Name: "starter"},
			Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{
				Inline: []string{"ads.example"}, Format: "domains",
			}}},
		}, nil
	}
	res, err := Seed(ctx, st, Options{Namespace: "kudurru", StarterBlockList: hook, Validator: testValidator(t)})
	if err != nil {
		t.Fatal(err)
	}
	blKey := store.Key{Kind: "BlockList", Namespace: "kudurru", Name: "starter"}
	if len(res.Created) != 4 || res.Created[0] != blKey {
		t.Fatalf("result = %+v", res)
	}
	var pol v1alpha1.Policy
	if err := st.Get(ctx, store.Key{Kind: "Policy", Namespace: "kudurru", Name: DefaultPolicyName}, &pol); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pol.Spec.BlockListRefs, []string{"starter"}) {
		t.Fatalf("default policy must reference the starter list: %+v", pol.Spec)
	}

	// The hook may return no list (a build without one) or fail.
	objs, err := Objects(ctx, Options{StarterBlockList: func(context.Context) (*v1alpha1.BlockList, error) { return nil, nil }})
	if err != nil || len(objs) != 3 {
		t.Fatalf("nil starter list: %d objects, %v", len(objs), err)
	}
	boom := errors.New("boom")
	if _, err := Seed(ctx, newStore(t), Options{StarterBlockList: func(context.Context) (*v1alpha1.BlockList, error) { return nil, boom }}); !errors.Is(err, boom) {
		t.Fatalf("hook error must surface: %v", err)
	}
	if _, err := Objects(ctx, Options{StarterBlockList: func(context.Context) (*v1alpha1.BlockList, error) { return &v1alpha1.BlockList{}, nil }}); err == nil {
		t.Fatal("unnamed starter list must be rejected")
	}
}

func TestSeedValidationAndStoreErrors(t *testing.T) {
	ctx := context.Background()
	// An invalid seed (here a starter list with an empty source) is refused
	// before it reaches the store.
	st := newStore(t)
	bad := func(context.Context) (*v1alpha1.BlockList, error) {
		return &v1alpha1.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "starter"},
			Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{}}}}, nil
	}
	if _, err := Seed(ctx, st, Options{StarterBlockList: bad, Validator: testValidator(t)}); !validator.IsInvalid(err) {
		t.Fatalf("invalid seed must be refused: %v", err)
	}
	for _, kind := range []store.Kind{"BlockList", "ClientGroup", "Policy", "DNSZone"} {
		if page, _ := st.List(ctx, kind, store.ListOptions{}); len(page.Items) != 0 {
			t.Fatalf("%s written although the seed set is invalid", kind)
		}
	}

	closed := memory.New()
	_ = closed.Close()
	if _, err := Seed(ctx, closed, Options{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("store errors must surface: %v", err)
	}
}

// TestEmbeddedStarterBlockList seeds the real embedded list through the
// CRD validator: created on first boot, referenced by Policy "default",
// never recreated after the operator deletes it.
func TestEmbeddedStarterBlockList(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	opts := Options{StarterBlockList: EmbeddedStarterBlockList, Validator: testValidator(t)}
	res, err := Seed(ctx, st, opts)
	if err != nil {
		t.Fatal(err)
	}
	blKey := store.Key{Kind: "BlockList", Namespace: DefaultNamespace, Name: "starter"}
	if !res.FirstBoot || len(res.Created) != 4 || res.Created[0] != blKey {
		t.Fatalf("result = %+v", res)
	}
	var bl v1alpha1.BlockList
	if err := st.Get(ctx, blKey, &bl); err != nil {
		t.Fatal(err)
	}
	if store.ManagedBy(&bl) != store.ManagerBootstrap || len(bl.Spec.Sources) != 1 || len(bl.Spec.Sources[0].Inline) < 2000 {
		t.Fatalf("starter list = %s, %d sources", store.ManagedBy(&bl), len(bl.Spec.Sources))
	}
	if bl.Annotations["kudurru.io/provenance"] == "" {
		t.Error("starter list carries no provenance annotation")
	}
	var pol v1alpha1.Policy
	if err := st.Get(ctx, policyKey, &pol); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pol.Spec.BlockListRefs, []string{"starter"}) {
		t.Fatalf("policy refs = %v", pol.Spec.BlockListRefs)
	}
	if err := st.Delete(ctx, blKey, store.DeleteOptions{Manager: store.ManagerBootstrap}); err != nil {
		t.Fatal(err)
	}
	if res, err := Seed(ctx, st, opts); err != nil || res.FirstBoot || len(res.Created) != 0 {
		t.Fatalf("second boot = %+v, %v", res, err)
	}
	if err := st.Get(ctx, blKey, &bl); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted starter list came back: %v", err)
	}
}

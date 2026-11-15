package objects

import (
	"context"
	"errors"
	"reflect"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

const sourceBindingYAML = `apiVersion: kudurru.io/v1alpha1
kind: RoleBinding
metadata: {name: source-operator, namespace: apps}
spec:
  role: operator
  scope: {kind: cluster}
  subjects: [{kind: User, name: "corp:alice"}]
`

// These tests own only the loader dispatch/accepted-cut contract. Production
// SQLite security history is qualified separately against the concrete adapter.
type recordingBindingEffects struct {
	st    store.Store
	plans []RoleBindingPlan
	fail  error
}

func (e *recordingBindingEffects) ApplyRoleBinding(ctx context.Context, p RoleBindingPlan) (*public.RoleBinding, error) {
	e.plans = append(e.plans, p)
	if e.fail != nil {
		return nil, e.fail
	}
	var written store.Object
	err := p.WithCurrent(ctx, func(current func() bool) error {
		if !current() {
			return ErrSourceChanged
		}
		var err error
		written, err = e.st.Apply(ctx, p.Desired(), store.ApplyOptions{Manager: p.Manager(), Force: p.Force()})
		return err
	})
	if err != nil {
		return nil, err
	}
	return written.(*public.RoleBinding), nil
}
func (e *recordingBindingEffects) DeleteRoleBinding(ctx context.Context, p RoleBindingPlan) error {
	e.plans = append(e.plans, p)
	if e.fail != nil {
		return e.fail
	}
	return p.WithCurrent(ctx, func(current func() bool) error {
		if !current() {
			return ErrSourceChanged
		}
		return e.st.Delete(ctx, p.Key(), store.DeleteOptions{Manager: p.Manager()})
	})
}
func bindingFixture(t *testing.T) (*fixture, *recordingBindingEffects) {
	t.Helper()
	e := &recordingBindingEffects{}
	f := newFixture(t, nil, func(o *Options) { o.RoleBindings = e; o.RequireRoleBindings = true })
	e.st = f.st
	return f, e
}

func TestRoleBindingRequiredCapabilityAndInstalledInline(t *testing.T) {
	st := memoryStore(t)
	if _, err := New(Options{Store: st, RequireRoleBindings: true}); err == nil {
		t.Fatal("missing required security effects accepted")
	}
	// Koanf Go ints must remain supported and caller mutations after install
	// must not silently change environment-owned input on the next Load.
	inline := []map[string]any{{"apiVersion": "kudurru.io/v1alpha1", "kind": "Policy", "metadata": map[string]any{"name": "installed"}, "spec": map[string]any{"priority": 1}}}
	f := newFixture(t, inline)
	inline[0]["spec"].(map[string]any)["priority"] = 99
	if rep := f.l.Load(ctx(t)); rep.Count(SeverityError) != 0 {
		t.Fatal("installed inline rejected")
	}
	policy := get(t, f.st, nsKey("Policy", "default", "installed"), &public.Policy{})
	if policy.Spec.Priority != 1 {
		t.Fatal("caller mutated installed inline source")
	}
	inline[0]["metadata"].(map[string]any)["name"] = "replacement"
	if rep := f.l.Load(ctx(t)); rep.Unchanged != 1 || rep.Created+rep.Deleted != 0 {
		t.Fatal("installed input followed caller maps")
	}
	// The old decoder's broken-source diagnosis is retained at Load, including
	// unencodable installed input, rather than changed to constructor failure.
	broken := newFixture(t, []map[string]any{{"invalid": make(chan int)}})
	if rep := broken.l.Load(ctx(t)); rep.Count(SeverityError) == 0 {
		t.Fatal("invalid inline lacked its load diagnosis")
	}
}

func TestRoleBindingBothNoopPathsAndRetiredCut(t *testing.T) {
	f, e := bindingFixture(t)
	f.write(t, "binding.yaml", sourceBindingYAML)
	if rep := f.l.Load(ctx(t)); rep.Created != 1 || rep.Count(SeverityError) != 0 {
		t.Fatal("initial apply")
	}
	key := nsKey("RoleBinding", "apps", "source-operator")
	original := get(t, f.st, key, &public.RoleBinding{})
	if len(e.plans) != 1 || e.plans[0].Before() != nil {
		t.Fatal("not an exact create-only plan")
	}
	// A retained receipt is unusable as soon as that pass returns.
	called := false
	if err := e.plans[0].WithCurrent(ctx(t), func(func() bool) error { called = true; return nil }); !errors.Is(err, ErrSourceChanged) || called {
		t.Fatal("retired accepted cut admitted")
	}
	// First exercise the cached agreement path, then a cold content comparison.
	for _, cold := range []bool{false, true} {
		if cold {
			f.l.rec.seen = map[store.Key]agreement{}
		}
		before := len(e.plans)
		// The deliberate memory recorder advances RV even on identical Apply.
		// Review the exact row before this Load, not the first pass's old RV.
		reviewed := get(t, f.st, key, &public.RoleBinding{})
		if rep := f.l.Load(ctx(t)); rep.Unchanged != 1 || rep.Count(SeverityError) != 0 {
			t.Fatal("guarded no-op failed")
		}
		if len(e.plans) != before+1 {
			t.Fatal("no-op bypassed security effect")
		}
		plan := e.plans[len(e.plans)-1]
		if plan.Before().UID != reviewed.UID || plan.Before().ResourceVersion != reviewed.ResourceVersion {
			t.Fatal("no-op target was not original exact row")
		}
		detached := plan.Before()
		detached.Labels = map[string]string{"changed": "caller"}
		if reflect.DeepEqual(detached, plan.Before()) {
			t.Fatal("plan getter retained caller mutation")
		}
	}
	f.remove(t, "binding.yaml")
	if rep := f.l.Load(ctx(t)); rep.Deleted != 1 || rep.Count(SeverityError) != 0 {
		t.Fatal("orphan removal bypassed effect")
	}
	last := e.plans[len(e.plans)-1]
	if !last.Deleting() || last.Desired() != nil || last.Before().UID != original.UID {
		t.Fatal("orphan plan lost target")
	}
}

func TestRoleBindingRefusedEffectNeverFallsBack(t *testing.T) {
	f, e := bindingFixture(t)
	f.write(t, "binding.yaml", sourceBindingYAML)
	e.fail = ErrSourceChanged
	if rep := f.l.Load(ctx(t)); rep.Count(SeverityError) != 1 || rep.Created != 0 {
		t.Fatal("refused apply reported success")
	}
	key := nsKey("RoleBinding", "apps", "source-operator")
	if get(t, f.st, key, &public.RoleBinding{}) != nil {
		t.Fatal("refused apply used ordinary CRUD")
	}
	e.fail = nil
	f.l.Load(ctx(t))
	e.fail = ErrSourceChanged
	// A cached agreement must also enter the refused effect.
	if rep := f.l.Load(ctx(t)); rep.Count(SeverityError) != 1 || rep.Unchanged != 0 {
		t.Fatal("no-op bypassed refusal")
	}
	f.remove(t, "binding.yaml")
	if rep := f.l.Load(ctx(t)); rep.Count(SeverityError) != 1 || rep.Deleted != 0 {
		t.Fatal("refused delete reported success")
	}
	if get(t, f.st, key, &public.RoleBinding{}) == nil {
		t.Fatal("refused delete used ordinary CRUD")
	}
}

func TestRoleBindingSourceMoveAndBrokenOwnerRetention(t *testing.T) {
	f, e := bindingFixture(t)
	f.write(t, "a.yaml", sourceBindingYAML)
	f.l.Load(ctx(t))
	key := nsKey("RoleBinding", "apps", "source-operator")
	original := get(t, f.st, key, &public.RoleBinding{})
	f.write(t, "a.yaml", "kind: [\n")
	f.write(t, "b.yaml", sourceBindingYAML)
	count := len(e.plans)
	if rep := f.l.Load(ctx(t)); rep.Updated+rep.Deleted != 0 || len(e.plans) != count {
		t.Fatal("broken original owner was stolen")
	}
	f.remove(t, "a.yaml")
	if rep := f.l.Load(ctx(t)); rep.Updated != 1 || rep.Count(SeverityError) != 0 {
		t.Fatal("source move failed")
	}
	moved := get(t, f.st, key, &public.RoleBinding{})
	p := e.plans[len(e.plans)-1]
	if !p.Force() || p.Manager() != Manager("objects.d/b.yaml") || store.ManagedBy(p.Before()) != Manager("objects.d/a.yaml") || moved.UID != original.UID {
		t.Fatal("move broadened original owner decision")
	}
	f.write(t, "b.yaml", "apiVersion: kudurru.io/v1alpha1\nkind: RoleBinding\nmetadata: {name: source-operator, namespace: apps}\nspec: {role: unknown}\n")
	count = len(e.plans)
	if rep := f.l.Load(ctx(t)); rep.Count(SeverityError) == 0 || rep.Deleted != 0 || len(e.plans) != count {
		t.Fatal("invalid declaration lost last good grant")
	}
}

func TestRoleBindingCutPinCancellationAndPrivatePlanValidation(t *testing.T) {
	f, _ := bindingFixture(t)
	src := source{name: "objects.d/binding.yaml", decoded: decodeStream("objects.d/binding.yaml", []byte(sourceBindingYAML))}
	p := &pass{r: f.l.rec, rep: Report{}, declared: map[store.Key]string{}, brokenSources: map[string]bool{}}
	it, ok := p.identify(src.docs[0])
	if !ok {
		t.Fatal("identify")
	}
	p.declared[it.key] = src.name
	original, cancel := context.WithCancel(t.Context())
	cut := p.acceptSource(original, []item{it})
	defer cut.retire()
	desired := &public.RoleBinding{}
	desired.Name, desired.Namespace = "source-operator", "apps"
	plan, err := roleBindingPlan(cut, it.key, nil, desired, Manager(src.name), false, false)
	if err != nil {
		t.Fatal("private plan")
	}
	// WithCurrent's final predicate retains original cancellation independently
	// of a later caller context; no source replacement can repair it.
	if err := plan.WithCurrent(t.Context(), func(current func() bool) error {
		if !current() {
			t.Fatal("original cut unavailable")
		}
		cancel()
		if current() {
			t.Fatal("original cancellation ignored")
		}
		return nil
	}); err != nil {
		t.Fatal("pin")
	}
	called := false
	if err := plan.WithCurrent(t.Context(), func(func() bool) error { called = true; return nil }); err == nil || called {
		t.Fatal("canceled original cut repaired")
	}
	forged := plan
	forged.manager = Manager("elsewhere.yaml")
	if err := forged.WithCurrent(t.Context(), func(func() bool) error { called = true; return nil }); !errors.Is(err, ErrSourceChanged) || called {
		t.Fatal("different manager accepted")
	}
}

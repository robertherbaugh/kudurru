package kube

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// This principal has patch, not update, permission on main resources. The
// immutable-field refusal must come from actual dry-run CRD admission, before
// any managedFields transfer; the subsequent valid Force must still work.
func TestForcePreflightLeastPrivilege(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := grant(t, fmt.Sprintf("force-preflight-%d", time.Now().UnixNano()),
		driverClusterRules("get", "list", "watch", "create", "patch", "delete"))
	ctx := t.Context()
	out, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerFile("zones.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.c.Update(ctx, out); !apierrors.IsForbidden(err) {
		t.Fatalf("test principal unexpectedly has update permission: %v", err)
	}
	before, err := s.getMeta(ctx, s.kinds.byKind["DNSZone"], store.KeyOf(out))
	if err != nil {
		t.Fatal(err)
	}
	bad := zone("home", "other.example.")
	_, err = s.Apply(ctx, bad, store.ApplyOptions{Manager: store.ManagerAPI, Force: true, ExpectedResourceVersion: out.GetResourceVersion()})
	var invalid *validator.Error
	if !errors.As(err, &invalid) || !invalid.HasCause("spec.zoneName", metav1.CauseTypeFieldValueInvalid) || !apierrors.IsInvalid(err) {
		t.Fatalf("want immutable-field admission refusal: %v", err)
	}
	after, err := s.getMeta(ctx, s.kinds.byKind["DNSZone"], store.KeyOf(out))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected Force changed durable apiserver metadata")
	}
	valid := zone("home", "home.example.")
	valid.Spec.DefaultTTL = 600
	updated, err := s.Apply(ctx, valid, store.ApplyOptions{Manager: store.ManagerAPI, Force: true, ExpectedResourceVersion: out.GetResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	if updated.(*v1alpha1.DNSZone).Spec.DefaultTTL != 600 || store.ManagedBy(updated) != store.ManagerAPI {
		t.Fatal("valid Force lost spec or ownership")
	}
	if _, err := s.Apply(ctx, valid, store.ApplyOptions{Manager: store.ManagerSystem, Force: true, ExpectedResourceVersion: out.GetResourceVersion()}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale Force did not preserve resourceVersion precondition: %v", err)
	}
}

// Change the observed object at either side of real dry-run admission. The
// actual takeover must remain guarded by the reviewed resourceVersion, and a
// refusal must not replay it against the newer object.
func TestForcePreflightResourceVersionRace(t *testing.T) {
	for _, when := range []string{"beforeAdmission", "afterAdmission"} {
		t.Run(when, func(t *testing.T) {
			requireEnv(t)
			purge(t, shared.admin)
			s := newTestStore(t, Options{})
			ctx := t.Context()
			out, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerFile("zones.yaml")})
			if err != nil {
				t.Fatal(err)
			}
			var afterExternal *metav1.PartialObjectMetadata
			hook := func() {
				var external v1alpha1.DNSZone
				if err := shared.admin.Get(ctx, client.ObjectKey{Name: "home"}, &external); err != nil {
					t.Fatal(err)
				}
				external.Status.Serial = 42
				if err := shared.admin.Status().Update(ctx, &external); err != nil {
					t.Fatal(err)
				}
				var err error
				afterExternal, err = s.getMeta(ctx, s.kinds.byKind["DNSZone"], store.KeyOf(out))
				if err != nil {
					t.Fatal(err)
				}
			}
			wrapped := &preflightClient{Client: s.c}
			if when == "beforeAdmission" {
				wrapped.beforeDryRun = hook
			} else {
				wrapped.afterDryRun = hook
			}
			s.c = wrapped
			valid := zone("home", "home.example.")
			valid.Spec.DefaultTTL = 600
			_, err = s.Apply(ctx, valid, store.ApplyOptions{Manager: store.ManagerAPI, Force: true, ExpectedResourceVersion: out.GetResourceVersion()})
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("want original RV conflict, got %v", err)
			}
			if wrapped.dryRuns != 1 {
				t.Fatalf("preflight ran %d times", wrapped.dryRuns)
			}
			after, err := s.getMeta(ctx, s.kinds.byKind["DNSZone"], store.KeyOf(out))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterExternal, after) {
				t.Fatal("Force changed ownership after concurrent external status write")
			}
		})
	}
}

type preflightClient struct {
	client.Client
	beforeDryRun, afterDryRun func()
	dryRuns                   int
}

func (c *preflightClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	options := &client.PatchOptions{}
	options.ApplyOptions(opts)
	if len(options.DryRun) == 0 {
		return c.Client.Patch(ctx, obj, patch, opts...)
	}
	c.dryRuns++
	if c.beforeDryRun != nil {
		c.beforeDryRun()
	}
	err := c.Client.Patch(ctx, obj, patch, opts...)
	if err == nil && c.afterDryRun != nil {
		c.afterDryRun()
	}
	return err
}

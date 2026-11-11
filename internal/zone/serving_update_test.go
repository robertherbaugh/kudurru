package zone_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// ServingErrors refuses a primary zone exactly when the primary runtime
// cannot compile its update mode: such a zone is never committed, so the
// SQLite node would report it not ready forever (D373). 0.1.0 stored every
// access mode in spec.policy.update, which was a placeholder there.
func TestServingErrorsRefuseUpdateModesThePrimaryRuntimeCannotCompile(t *testing.T) {
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	modes := []v1alpha1.AccessMode{"", v1alpha1.AccessDeny, v1alpha1.AccessAllow, v1alpha1.AccessAllowOnlyPrivate,
		v1alpha1.AccessAllowOnlyZoneNameServers, v1alpha1.AccessUseNetworkACL, v1alpha1.AccessZoneNameServersAndACL}
	for _, mode := range modes {
		dz := &v1alpha1.DNSZone{
			ObjectMeta: metav1.ObjectMeta{Name: "upd", UID: "upd-uid", ResourceVersion: "1"},
			Spec: v1alpha1.DNSZoneSpec{ZoneName: "upd.example.", Type: v1alpha1.ZoneTypePrimary, Nameservers: []string{"ns1.upd.example."},
				Policy: v1alpha1.ZonePolicy{Update: mode}},
		}
		errs := zone.ServingErrors(dz)
		_, compileErr := currentguard.CompileUpdatePolicy(context.Background(), keys, dz, 0)
		if (len(errs) == 0) != (compileErr == nil) {
			t.Errorf("update %q: ServingErrors %v, but the primary runtime says %v", mode, errs, compileErr)
		}
		if len(errs) > 0 && errs[0].Field != "spec.policy.update" {
			t.Errorf("update %q: refusal names %v", mode, errs)
		}
		// A forward zone has no update policy and is not judged on it.
		dz.Spec.Type, dz.Spec.Forward = v1alpha1.ZoneTypeForward, &v1alpha1.ForwardSpec{UpstreamGroup: "default"}
		if errs := zone.ServingErrors(dz); len(errs) != 0 {
			t.Errorf("forward zone with update %q: %v", mode, errs)
		}
	}
}

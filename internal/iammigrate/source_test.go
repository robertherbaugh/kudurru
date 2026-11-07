package iammigrate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func unitMigrator(t *testing.T) *Migrator {
	t.Helper()
	opts := Options{Namespace: "system"}
	if err := opts.Limits.defaults(); err != nil {
		t.Fatal(err)
	}
	return &Migrator{opts: opts, files: map[string]bool{"/operator/oidc": true}}
}
func unitSecret(kind, name, ns, raw string) *corev1.Secret {
	labels := map[string]string{kindLabel: kind}
	if ns != "" {
		labels[namespaceLabel] = ns
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName(kind, ns, name), Namespace: "system", UID: types.UID("source-" + name), ResourceVersion: "opaque-rv", CreationTimestamp: metav1.NewTime(time.Unix(100, 0)), Labels: labels, Annotations: map[string]string{nameAnnotation: name}}, Type: internalType, Data: map[string][]byte{"object": []byte(raw)}}
}

func TestSourcePrivateExtractionDoesNotHideBadJSON(t *testing.T) {
	m := unitMigrator(t)
	for _, spec := range []string{`{"builtinAdmin":true,"builtinAdmin":false}`, `{"builtinAdmin":null}`, `{"builtinAdmin":"true"}`, `{"builtinAdmin":true,"unknownSecret":"do-not-print"}`} {
		sec := unitSecret("ServiceAccount", "ci", "apps", `{"metadata":{"name":"ci","namespace":"apps"},"spec":`+spec+`}`)
		_, err := m.decodeSource(sec)
		requireProblem(t, err, "invalid-source")
		if strings.Contains(err.Error(), "do-not-print") {
			t.Fatal("sensitive decoder value leaked")
		}
	}
	for _, raw := range []string{`{} {}`, strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65), `{"metadata":{"name":"ci","namespace":"apps"},"spec":{"disabled":false,"disabled":true}}`} {
		if _, err := m.decodeSource(unitSecret("ServiceAccount", "ci", "apps", raw)); err == nil {
			t.Fatal("ambiguous JSON accepted")
		}
	}
	sec := unitSecret("ServiceAccount", "ci", "apps", `{"metadata":{"name":"ci","namespace":"apps","uid":"forged","resourceVersion":"forged","creationTimestamp":"2026-01-01T00:00:00Z"},"spec":{"builtinAdmin":true}}`)
	s, err := m.decodeSource(sec)
	if err != nil {
		t.Fatal(err)
	}
	if s.obj.GetUID() != sec.UID || s.obj.GetResourceVersion() != sec.ResourceVersion || !s.obj.GetCreationTimestamp().Time.Equal(sec.CreationTimestamp.Time) || s.admin == nil || !*s.admin {
		t.Fatal("private extraction or Secret identity lost")
	}
	raw, _ := json.Marshal(s.obj)
	if strings.Contains(string(raw), "builtinAdmin") {
		t.Fatal("private authority leaked into public payload")
	}
}

func TestSourceCapabilitiesAndMetadataBoundaries(t *testing.T) {
	m := unitMigrator(t)
	for _, field := range []string{`"clientSecretFile":"/operator/oidc"`, `"clientSecretRef":{"name":"sso","key":"client"}`} {
		raw := `{"metadata":{"name":"sso"},"spec":{"issuer":"https://sso.example","clientID":"ci",` + field + `}}`
		s, err := m.decodeSource(unitSecret("IdentityProvider", "sso", "", raw))
		if err != nil {
			t.Fatal(err)
		}
		if s.obj.(*publicv1.IdentityProvider).Spec.ClientSecretRef == nil {
			t.Fatal("missing capability")
		}
	}
	_, err := m.decodeSource(unitSecret("IdentityProvider", "sso", "", `{"metadata":{"name":"sso"},"spec":{"issuer":"https://sso.example","clientID":"ci","clientSecretFile":"/untrusted/file"}}`))
	requireProblem(t, err, "credentials-denied")
	for _, metadata := range []string{`"ownerReferences":[{"apiVersion":"v1","kind":"Secret","name":"parent","uid":"uid"}]`, `"finalizers":["example.org/preserve"]`, `"annotations":{"passwordHash":"private"}`} {
		sec := unitSecret("ServiceAccount", "ci", "apps", `{"metadata":{"name":"ci","namespace":"apps",`+metadata+`},"spec":{}}`)
		if _, err := m.decodeSource(sec); err == nil {
			t.Fatal("unsafe lifecycle/private metadata accepted")
		}
	}
	sec := unitSecret("ServiceAccount", "ci", "apps", `{"metadata":{"name":"different","namespace":"apps"},"spec":{}}`)
	_, err = m.decodeSource(sec)
	requireProblem(t, err, "invalid-source")
	m.opts.Limits.ObjectBytes = 32
	_, err = m.decodeSource(unitSecret("ServiceAccount", "ci", "apps", `{"metadata":{"name":"ci","namespace":"apps"},"spec":{}}`))
	requireProblem(t, err, "invalid-source")
}

func TestOriginalIssuanceFloorAndImmutableBindings(t *testing.T) {
	m := unitMigrator(t)
	sa, err := m.decodeSource(unitSecret("ServiceAccount", "ci", "apps", `{"metadata":{"name":"ci","namespace":"apps"},"spec":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, uid string
		issued    int64
		bound     bool
	}{{"earlier", "", 99, false}, {"equal-unbound", "", 100, false}, {"later", "", 101, true}, {"equal-bound", "source-ci", 100, true}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"metadata":{"name":"aaaaaaaaaaaa"},"spec":{"owner":{"kind":"ServiceAccount","name":"ci","namespace":"apps","id":"sa:apps/ci","uid":"` + tc.uid + `"},"secretHash":"` + strings.Repeat("0", 64) + `"}}`
			sec := unitSecret("APIToken", "aaaaaaaaaaaa", "", raw)
			sec.CreationTimestamp = metav1.NewTime(time.Unix(tc.issued, 0))
			token, err := m.decodeSource(sec)
			if err != nil {
				t.Fatal(err)
			}
			p, err := m.makePlan("cluster", "release", []*source{token, sa})
			if err != nil {
				t.Fatal(err)
			}
			records, bound, unbound, err := p.credentials(map[key]types.UID{{"ServiceAccount", "apps", "ci"}: "new-uid"})
			if err != nil {
				t.Fatal(err)
			}
			if (bound == 1) != tc.bound || bound+unbound != 1 {
				t.Fatal("issuance proof widened", bound, unbound)
			}
			if !tc.bound {
				return
			}
			b := newBindings(records)
			current := sa.obj.(*publicv1.ServiceAccount).DeepCopy()
			current.UID = "new-uid"
			tok := token.obj.(*internalv1.APIToken)
			r, ok := b.ResolveToken(tok, current)
			if !ok || r.OriginalAccountCreatedAt.Unix() != 100 {
				t.Fatal("lost original floor")
			}
			r.Owner.UID = "mutated"
			if r2, ok := b.ResolveToken(tok, current); !ok || r2.Owner.UID != "new-uid" {
				t.Fatal("returned binding aliases owner state")
			}
			tok.UID = "recreated-token"
			if _, ok := b.ResolveToken(tok, current); ok {
				t.Fatal("recreated token inherited proof")
			}
		})
	}
}

func TestPlanCapsAndSafeFormatting(t *testing.T) {
	m := unitMigrator(t)
	s, err := m.decodeSource(unitSecret("ServiceAccount", "ci", "apps", `{"metadata":{"name":"ci","namespace":"apps"},"spec":{"builtinAdmin":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	m.opts.Limits.JournalBytes = 100
	_, err = m.makePlan("cluster", "release", []*source{s})
	requireProblem(t, err, "bounds-exceeded")
	m.opts.Limits.JournalBytes = 512 << 10
	p, err := m.makePlan("cluster", "release", []*source{s})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{fmt.Sprintf("%v", p), fmt.Sprintf("%#v", p), fmt.Sprintf("%#v", newBindings(nil))} {
		if strings.Contains(value, "source-ci") || strings.Contains(value, "builtinAdmin") || strings.Contains(value, "Sources:") {
			t.Fatal("private formatted state leaked")
		}
	}
	data, err := json.Marshal(p)
	if err != nil || strings.Contains(string(data), "source-ci") {
		t.Fatal("private JSON state leaked")
	}
	for _, limits := range []Limits{{Objects: 1025}, {JournalBytes: 513 << 10}, {PageSize: -1}, {RequestTimeout: time.Minute}} {
		if err = limits.defaults(); err == nil {
			t.Fatal("limits raised beyond hard cap")
		}
	}
}

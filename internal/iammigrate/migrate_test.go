package iammigrate

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEnvtestFullCutoverAndPrivateBindings(t *testing.T) {
	e := newTestEnv(t, nil)
	sa := e.seed(t, "ServiceAccount", "ci", e.ns, `{"description":"build","builtinAdmin":true,"token":{"secretName":"ci-wire"}}`, `{"tokenID":"aaaaaaaaaaaa","lastUsed":"2026-10-13T00:00:00Z"}`)
	owner := accountOwner(key{"ServiceAccount", e.ns, "ci"}, sa.UID)
	originalState := &internalv1.PrincipalState{ObjectMeta: metav1.ObjectMeta{Name: internalv1.PrincipalStateName(owner)}, Spec: internalv1.PrincipalStateSpec{Owner: owner, BuiltinAdmin: true}}
	originalRaw, _ := json.Marshal(originalState)
	sourceState := e.seedRaw(t, "PrincipalState", originalState.Name, "", originalRaw)
	user := e.seed(t, "LocalUser", "alice", "", `{"groups":["operators"],"disabled":false,"builtinAdmin":true}`, `{"sessionsValidAfter":"2026-10-12T00:00:00Z"}`)
	e.seed(t, "RoleBinding", "build", e.ns, `{"role":"viewer","scope":{"kind":""},"subjects":[{"kind":"ServiceAccount","name":"ci","namespace":"`+e.ns+`"}]}`, "")
	idpName := e.ns + "-sso"
	e.seed(t, "IdentityProvider", idpName, "", `{"issuer":"https://sso.example","clientID":"ci","clientSecretFile":"/operator/oidc"}`, `{"conditions":[{"type":"Ready","status":"False","observedGeneration":7,"reason":"Offline","message":"waiting","lastTransitionTime":"2026-10-13T00:00:00Z"}]}`)
	// Explicit source UID binds equal-second timestamps without guessing ordering.
	token := e.seed(t, "APIToken", "aaaaaaaaaaaa", "", `{"owner":{"kind":"ServiceAccount","name":"ci","namespace":"`+e.ns+`","id":"sa:`+e.ns+`/ci","uid":"`+string(sa.UID)+`"},"secretHash":"`+strings.Repeat("0", 64)+`","scopes":["viewer"]}`, `{"lastUsedIP":"192.0.2.1"}`)
	localToken := e.seed(t, "APIToken", "bbbbbbbbbbbb", "", `{"owner":{"kind":"User","name":"alice","provider":"local","id":"local:alice","uid":"`+string(user.UID)+`"},"secretHash":"`+strings.Repeat("1", 64)+`"}`, "")
	state, err := e.m.Check(e.ctx)
	if err != nil || state.State != Required || state.RemoteAllowed {
		t.Fatalf("upgrade gate: %+v %v", state, err)
	}
	plan := e.plan(t)
	if plan.Counts().BoundTokens != 2 || plan.Counts().Authorities != 1 {
		t.Fatal(plan.Counts())
	}
	result, err := e.m.Begin(e.ctx, plan, OfflineAcknowledgement{true})
	if err != nil || !result.RemoteAllowed || result.State != Complete {
		t.Fatalf("cutover: %+v %v", result, err)
	}
	for _, original := range []*corev1.Secret{sa, user, token, localToken, sourceState} {
		current, err := e.core.CoreV1().Secrets(e.ns).Get(e.ctx, original.Name, metav1.GetOptions{})
		if err != nil || !reflect.DeepEqual(current, original) {
			t.Fatal("original private/source Secret changed", err)
		}
	}
	u, err := e.m.getPublic(e.ctx, key{"ServiceAccount", e.ns, "ci"})
	if err != nil {
		t.Fatal(err)
	}
	current, err := publicObject(u, key{"ServiceAccount", e.ns, "ci"})
	if err != nil {
		t.Fatal(err)
	}
	account := current.(*publicv1.ServiceAccount)
	if account.UID == sa.UID || account.Status.TokenID != "aaaaaaaaaaaa" || account.Annotations[publicv1.ManagedByAnnotation] != "file:iam.yaml" {
		t.Fatal("identity/status/ownership not migrated")
	}
	raw, _ := json.Marshal(account)
	if strings.Contains(string(raw), "builtinAdmin") || strings.Contains(string(raw), "private-old-snapshot") {
		t.Fatal("private privilege metadata leaked")
	}
	rb, _ := e.m.getPublic(e.ctx, key{"RoleBinding", e.ns, "build"})
	obj, err := publicObject(rb, key{"RoleBinding", e.ns, "build"})
	if err != nil || obj.(*publicv1.RoleBinding).Spec.Scope.Kind != "" {
		t.Fatal("scope changed", err)
	}
	ip, _ := e.m.getPublic(e.ctx, key{"IdentityProvider", "", idpName})
	obj, err = publicObject(ip, key{"IdentityProvider", "", idpName})
	if err != nil {
		t.Fatal(err)
	}
	idp := obj.(*publicv1.IdentityProvider)
	if idp.Spec.ClientSecretRef.File != "/operator/oidc" || idp.Status.Conditions[0].ObservedGeneration != 0 {
		t.Fatal("IdP conversion changed capability or observation")
	}
	b, err := e.m.Bindings(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := e.m.decodeSource(token)
	if err != nil {
		t.Fatal(err)
	}
	tok := decoded.obj.(*internalv1.APIToken)
	localSource, localErr := e.m.decodeSource(localToken)
	userSource, userErr := e.m.decodeSource(user)
	if localErr != nil || userErr != nil {
		t.Fatal(localErr, userErr)
	}
	if localBinding, ok := b.ResolveToken(localSource.obj.(*internalv1.APIToken), userSource.obj); !ok || localBinding.Owner.UID != string(user.UID) || localBinding.OriginalAccountUID != user.UID {
		t.Fatal("local identity changed")
	}
	// Returned bindings are provenance only; source authority stays private.
	binding, ok := b.ResolveToken(tok, account)
	if !ok || binding.Owner.UID != string(account.UID) || binding.OriginalAccountUID != sa.UID || !binding.OriginalAccountCreatedAt.Equal(sa.CreationTimestamp.Time) {
		t.Fatal("missing exact UID/issuance proof")
	}
	principal := internalv1.PrincipalStateName(binding.Owner)
	private, err := e.core.CoreV1().Secrets(e.ns).Get(e.ctx, secretName("PrincipalState", "", principal), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := e.m.decodeSource(private)
	if err != nil || !proof.obj.(*internalv1.PrincipalState).Spec.BuiltinAdmin || proof.obj.(*internalv1.PrincipalState).Spec.Owner.UID != string(account.UID) {
		t.Fatal("private authority not bound", err)
	}
	// Mutable use status is permitted, credential mutations and UID reuse aren't.
	tok.Status.LastUsedIP = "198.51.100.9"
	if _, ok = b.ResolveToken(tok, account); !ok {
		t.Fatal("usage invalidated credential proof")
	}
	copyToken := tok.DeepCopy()
	copyToken.Spec.Scopes = nil
	if _, ok = b.ResolveToken(copyToken, account); ok {
		t.Fatal("changed scope inherited migration proof")
	}
	replacement := account.DeepCopy()
	replacement.UID = "replacement"
	if _, ok = b.ResolveToken(tok, replacement); ok {
		t.Fatal("recreated account inherited credential")
	}
	account.Spec.Disabled = true
	if _, ok = b.ResolveToken(tok, account); ok {
		t.Fatal("disabled account accepted")
	}
	account.Spec.Disabled = false
	// Deleting credentials/public accounts after cutover must not rerun migration.
	if err = e.core.CoreV1().Secrets(e.ns).Delete(e.ctx, token.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = e.m.resource(key{"ServiceAccount", e.ns, "ci"}).Delete(e.ctx, "ci", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	check, err := e.m.Check(e.ctx)
	if err != nil || !check.RemoteAllowed {
		t.Fatalf("restart after ordinary revocation: %+v %v", check, err)
	}
}

func TestEnvtestUncertainCreateCannotAdoptForgedProvenance(t *testing.T) {
	var fault *faultTransport
	armed := false
	e := newTestEnv(t, func(base http.RoundTripper) http.RoundTripper {
		f := &faultTransport{base: base}
		fault = f
		f.after = func(r *http.Request, response *http.Response) bool {
			if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/apis/kudurru.io/") && response.StatusCode == 201 {
				armed = true
			}
			return false
		}
		f.before = func(r *http.Request) bool { return armed && journalPut(r) }
		return f
	})
	e.seed(t, "ServiceAccount", "ci", e.ns, `{"builtinAdmin":true}`, "")
	p := e.plan(t)
	result, err := e.m.Begin(e.ctx, p, OfflineAcknowledgement{true})
	requireProblem(t, err, "uncertain-create")
	if result.RemoteAllowed {
		t.Fatal("unknown receipt opened IAM")
	}
	fault.mu.Lock()
	fault.before = nil
	fault.after = nil
	fault.mu.Unlock()
	check, err := e.m.Check(e.ctx)
	requireProblem(t, err, "uncertain-create")
	if check.RemoteAllowed || check.State != RecoveryRequired {
		t.Fatal(check)
	}
	_, j, err := e.m.loadJournal(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	k := key{"ServiceAccount", e.ns, "ci"}
	obj, _ := e.m.getPublic(e.ctx, k)
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["kudurru.io/migration-id"] = j.ID
	obj.SetAnnotations(annotations)
	if _, err = e.m.resource(k).Update(e.ctx, obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	result, err = e.m.Resume(e.ctx, check.JournalResourceVersion, OfflineAcknowledgement{true})
	requireProblem(t, err, "uncertain-create")
	if result.RemoteAllowed {
		t.Fatal("forged annotations adopted")
	}
	// Explicit UID-preconditioned operator cleanup, then a new known Create.
	obj, _ = e.m.getPublic(e.ctx, k)
	uid := obj.GetUID()
	if err = e.m.resource(k).Delete(e.ctx, k.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
		t.Fatal(err)
	}
	check, _ = e.m.Check(e.ctx)
	result, err = e.m.Resume(e.ctx, check.JournalResourceVersion, OfflineAcknowledgement{true})
	if err != nil || !result.RemoteAllowed {
		t.Fatalf("offline recovery: %+v %v", result, err)
	}
}

func TestEnvtestLostCreateResponseAndCompletionResponse(t *testing.T) {
	for _, boundary := range []string{"create", "completion"} {
		t.Run(boundary, func(t *testing.T) {
			var fault *faultTransport
			e := newTestEnv(t, func(base http.RoundTripper) http.RoundTripper {
				fault = &faultTransport{base: base}
				fault.after = func(r *http.Request, response *http.Response) bool {
					if response.StatusCode < 200 || response.StatusCode >= 300 {
						return false
					}
					if boundary == "create" {
						return r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/apis/kudurru.io/")
					}
					return journalPut(r) && journalState(r) == Complete
				}
				return fault
			})
			e.seed(t, "ServiceAccount", "ci", e.ns, `{}`, "")
			result, err := e.m.Begin(e.ctx, e.plan(t), OfflineAcknowledgement{true})
			if err == nil || result.RemoteAllowed {
				t.Fatal("unknown response accepted")
			}
			fault.mu.Lock()
			fault.after = nil
			fault.mu.Unlock()
			check, err := e.m.Check(e.ctx)
			if boundary == "create" {
				requireProblem(t, err, "uncertain-create")
				if check.RemoteAllowed {
					t.Fatal(check)
				}
			} else if err != nil || !check.RemoteAllowed {
				t.Fatalf("committed completion not recovered: %+v %v", check, err)
			}
		})
	}
}

func TestEnvtestUIDLessLegacyTokenStrictFloor(t *testing.T) {
	e := newTestEnv(t, nil)
	account := e.seed(t, "ServiceAccount", "ci", e.ns, `{}`, "")
	// Wait for the real apiserver's whole-second timestamp to advance.
	deadline := account.CreationTimestamp.Add(1100 * time.Millisecond)
	if delay := time.Until(deadline); delay > 0 {
		time.Sleep(delay)
	}
	token := e.seed(t, "APIToken", "aaaaaaaaaaaa", "", `{"owner":{"kind":"ServiceAccount","name":"ci","namespace":"`+e.ns+`","id":"sa:`+e.ns+`/ci"},"secretHash":"`+strings.Repeat("0", 64)+`"}`, "")
	p := e.plan(t)
	if p.Counts().BoundTokens != 1 {
		t.Fatal("strictly later credential not proved", p.Counts())
	}
	result, err := e.m.Begin(e.ctx, p, OfflineAcknowledgement{true})
	if err != nil || !result.RemoteAllowed {
		t.Fatal(err)
	}
	b, err := e.m.Bindings(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := e.m.decodeSource(token)
	u, _ := e.m.getPublic(e.ctx, key{"ServiceAccount", e.ns, "ci"})
	obj, _ := publicObject(u, key{"ServiceAccount", e.ns, "ci"})
	if _, ok := b.ResolveToken(s.obj.(*internalv1.APIToken), obj); !ok {
		t.Fatal("UID-less token mapping unavailable")
	}
}

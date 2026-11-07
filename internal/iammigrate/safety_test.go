package iammigrate

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestEnvtestCaptureOneStableJournalCut(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{true: "native", false: "complete"}[native], func(t *testing.T) {
			var fault *faultTransport
			e := newTestEnv(t, func(base http.RoundTripper) http.RoundTripper { fault = &faultTransport{base: base}; return fault })
			fresh := newTestEnv(t, nil)
			fresh.m.opts.Namespace = e.ns
			if !native {
				e.seed(t, "ServiceAccount", "ci", e.ns, `{}`, "")
				if _, err := e.m.Begin(e.ctx, e.plan(t), OfflineAcknowledgement{true}); err != nil {
					t.Fatal(err)
				}
			}
			gets := 0
			fault.before = func(r *http.Request) bool {
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/secrets/"+JournalName) {
					return false
				}
				gets++
				if gets == 2 {
					if native {
						if _, err := fresh.m.Begin(e.ctx, fresh.plan(t), OfflineAcknowledgement{true}); err != nil {
							t.Fatal(err)
						}
					} else {
						sec, j, err := fresh.m.loadJournal(e.ctx)
						if err != nil {
							t.Fatal(err)
						}
						j.Owner = "22222222-2222-2222-2222-222222222222"
						if _, err = fresh.m.save(e.ctx, sec, j); err != nil {
							t.Fatal(err)
						}
					}
				}
				return false
			}
			s, b, err := e.m.Capture(e.ctx)
			requireProblem(t, err, "CAS-conflict")
			if s.RemoteAllowed || b != nil || gets != 2 {
				t.Fatal("journal drift admitted", s, gets)
			}
			fault.mu.Lock()
			fault.before = nil
			fault.mu.Unlock()
			s, b, err = e.m.Capture(e.ctx)
			if err != nil || !s.RemoteAllowed || b == nil {
				t.Fatal("stable complete cut rejected", s, err)
			}
		})
	}
}

func TestEnvtestSingleLostReceiptCannotBeRepairedByFailureHandler(t *testing.T) {
	var fault *faultTransport
	e := newTestEnv(t, func(base http.RoundTripper) http.RoundTripper { fault = &faultTransport{base: base}; return fault })
	e.seed(t, "ServiceAccount", "ci", e.ns, `{"builtinAdmin":true}`, "")
	created, failedOnce := false, false
	fault.after = func(r *http.Request, resp *http.Response) bool {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/apis/kudurru.io/") && resp.StatusCode == 201 {
			created = true
		}
		return false
	}
	fault.before = func(r *http.Request) bool {
		if created && !failedOnce && journalPut(r) {
			failedOnce = true
			return true
		}
		return false
	}
	s, err := e.m.Begin(e.ctx, e.plan(t), OfflineAcknowledgement{true})
	requireProblem(t, err, "uncertain-create")
	if s.RemoteAllowed {
		t.Fatal("lost receipt opened admission")
	}
	fault.mu.Lock()
	fault.before = nil
	fault.after = nil
	fault.mu.Unlock()
	_, j, err := e.m.loadJournal(e.ctx)
	if err != nil || len(j.Public) != 1 || j.Public[0].UID != "" {
		t.Fatal("failure handler repaired lost UID receipt", err)
	}
	s, b, err := e.m.Capture(e.ctx)
	requireProblem(t, err, "uncertain-create")
	if s.RemoteAllowed || b != nil {
		t.Fatal("unproved target adopted")
	}
}

func TestEnvtestNativeExplicitIncarnationProof(t *testing.T) {
	e := newTestEnv(t, nil)
	user := e.seed(t, "LocalUser", "alice", "", `{}`, "")
	token := e.seed(t, "APIToken", "aaaaaaaaaaaa", "", `{"owner":{"kind":"User","name":"alice","provider":"local","id":"local:alice","uid":"`+string(user.UID)+`"},"secretHash":"`+strings.Repeat("0", 64)+`"}`, "")
	s, b, err := e.m.Capture(e.ctx)
	if err != nil || s.State != Native || !s.RemoteAllowed || b != nil {
		t.Fatal(s, err)
	}
	var doc map[string]any
	if err = json.Unmarshal(token.Data["object"], &doc); err != nil {
		t.Fatal(err)
	}
	doc["spec"].(map[string]any)["owner"].(map[string]any)["uid"] = "unproved-incarnation"
	token.Data["object"], _ = json.Marshal(doc)
	if _, err = e.core.CoreV1().Secrets(e.ns).Update(e.ctx, token, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	s, b, err = e.m.Capture(e.ctx)
	requireProblem(t, err, "private-conflict")
	if s.RemoteAllowed || b != nil {
		t.Fatal("native mismatched UID admitted")
	}
}

func TestEnvtestRealPermissionDenialAndRequestDeadline(t *testing.T) {
	e := newTestEnv(t, nil)
	cfg := rest.CopyConfig(testConfig)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: "iam-migration-unprivileged"}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	denied, err := New(core, dc, Options{Namespace: e.ns})
	if err != nil {
		t.Fatal(err)
	}
	s, b, err := denied.Capture(e.ctx)
	if err == nil || s.RemoteAllowed || b != nil {
		t.Fatal("permission denial admitted", s, err)
	}
	// A hanging transport obeys the request context; it cannot outlive the cap.
	cfg = rest.CopyConfig(testConfig)
	cfg.Wrap(func(http.RoundTripper) http.RoundTripper { return deadlineTransport{} })
	core, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := New(core, dc, Options{Namespace: e.ns, Limits: Limits{RequestTimeout: 5 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s, b, err = bounded.Capture(e.ctx)
	if err == nil || s.RemoteAllowed || b != nil || time.Since(start) > time.Second {
		t.Fatal("deadline did not bound admission read", s, err)
	}
}

type deadlineTransport struct{}

func (deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestEnvtestPaginationAndPreflightCaps(t *testing.T) {
	for _, bound := range []string{"pagination", "objects", "total", "journal", "object"} {
		t.Run(bound, func(t *testing.T) {
			e := newTestEnv(t, nil)
			for _, name := range []string{"a", "b", "c"} {
				e.seed(t, "ServiceAccount", name, e.ns, `{}`, "")
			}
			e.m.opts.Limits.PageSize = 1
			switch bound {
			case "objects":
				e.m.opts.Limits.Objects = 2
			case "total":
				e.m.opts.Limits.TotalBytes = 1
			case "journal":
				e.m.opts.Limits.JournalBytes = 100
			case "object":
				e.m.opts.Limits.ObjectBytes = 1
			}
			p, err := e.m.Plan(e.ctx)
			switch bound {
			case "pagination":
				if err != nil || p.Counts().Sources != 3 {
					t.Fatal("server cursor lost sources", err)
				}
			case "object":
				requireProblem(t, err, "invalid-source")
			default:
				requireProblem(t, err, "bounds-exceeded")
			}
			if sec, _, loadErr := e.m.loadJournal(e.ctx); loadErr != nil || sec != nil {
				t.Fatal("preflight wrote journal", loadErr)
			}
		})
	}
}

func TestEnvtestIssuedTokenUsesVerifiedBindingAndCurrentRevocation(t *testing.T) {
	e := newTestEnv(t, nil)
	original := e.seed(t, "ServiceAccount", "ci", e.ns, `{}`, "")
	// Force a genuine old issuance instant before the new public account time.
	if wait := time.Until(original.CreationTimestamp.Add(1100 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	wire, id := tokens.Generate()
	tokenSecret := e.seed(t, "APIToken", id, "", `{"owner":{"kind":"ServiceAccount","name":"ci","namespace":"`+e.ns+`","id":"sa:`+e.ns+`/ci","uid":"`+string(original.UID)+`"},"secretHash":"`+tokens.Hash(wire)+`","scopes":["viewer"]}`, "")
	if wait := time.Until(tokenSecret.CreationTimestamp.Add(1100 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	if _, err := e.m.Begin(e.ctx, e.plan(t), OfflineAcknowledgement{true}); err != nil {
		t.Fatal(err)
	}
	s, b, err := e.m.Capture(e.ctx)
	if err != nil || !s.RemoteAllowed {
		t.Fatal(s, err)
	}
	tokSource, err := e.m.decodeSource(tokenSecret)
	if err != nil {
		t.Fatal(err)
	}
	tok := tokSource.obj.(*internalv1.APIToken)
	u, err := e.m.getPublic(e.ctx, key{"ServiceAccount", e.ns, "ci"})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := publicObject(u, key{"ServiceAccount", e.ns, "ci"})
	if err != nil {
		t.Fatal(err)
	}
	account := obj.(*publicv1.ServiceAccount)
	if !account.CreationTimestamp.After(tok.CreationTimestamp.Time) {
		t.Fatal("fixture does not exercise original issuance floor")
	}
	st := memory.New()
	defer func() { _ = st.Close() }()
	for _, object := range []store.Object{tok, account} {
		if _, err = st.Apply(e.ctx, object, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
	}
	verifier, err := tokens.New(tokens.Options{Store: st, Migration: func(_ context.Context, current *internalv1.APIToken, owner store.Object) (tokens.MigratedIdentity, bool) {
		binding, ok := b.ResolveToken(current, owner)
		return tokens.MigratedIdentity{AccountUID: binding.Owner.UID, TokenUID: string(binding.TokenUID), OriginalCreatedAt: binding.OriginalAccountCreatedAt}, ok
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = verifier.Prime(e.ctx); err != nil {
		t.Fatal(err)
	}
	principal, err := verifier.Verify(e.ctx, wire, "")
	if err != nil || principal.AccountUID != string(account.UID) || principal.BuiltinAdmin || len(principal.Scopes) != 1 || principal.Scopes[0] != "viewer" {
		t.Fatal("issued credential failed or widened rights", err)
	}
	if _, err = verifier.Verify(e.ctx, wire+"wrong", ""); err == nil {
		t.Fatal("wrong wire secret accepted")
	}
	account.Spec.Disabled = true
	verifier.ServiceAccounts().Upsert(account)
	if _, err = verifier.Verify(e.ctx, wire, ""); err == nil {
		t.Fatal("current disabled account accepted")
	}
	account.Spec.Disabled = false
	account.UID = "recreated"
	verifier.ServiceAccounts().Upsert(account)
	if _, err = verifier.Verify(e.ctx, wire, ""); err == nil {
		t.Fatal("current recreated account accepted")
	}
	// The live private Secret retains its original verifier and owner bytes.
	now, err := e.core.CoreV1().Secrets(e.ns).Get(e.ctx, tokenSecret.Name, metav1.GetOptions{})
	if err != nil || string(now.Data["object"]) != string(tokenSecret.Data["object"]) {
		t.Fatal("credential Secret changed", err)
	}
}

func TestEnvtestMalformedIncompleteReceiptRejectedBeforeResumeWrites(t *testing.T) {
	for _, mutation := range []string{"unknown-key", "conversion-digest", "counts", "authority"} {
		t.Run(mutation, func(t *testing.T) {
			var fault *faultTransport
			e := newTestEnv(t, func(base http.RoundTripper) http.RoundTripper { fault = &faultTransport{base: base}; return fault })
			e.seed(t, "ServiceAccount", "ci", e.ns, `{}`, "")
			fault.before = func(r *http.Request) bool {
				return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/secrets")
			}
			_, err := e.m.Begin(e.ctx, e.plan(t), OfflineAcknowledgement{true})
			if err == nil {
				t.Fatal("journal interruption did not fire")
			}
			fault.mu.Lock()
			fault.before = nil
			fault.mu.Unlock()
			// Create the pinned initial journal to simulate a corrupt interrupted run.
			p := e.plan(t)
			j := p.journal("33333333-3333-3333-3333-333333333333", "44444444-4444-4444-4444-444444444444")
			switch mutation {
			case "unknown-key":
				j.Public[0].Key.Name = "unlisted-target"
			case "conversion-digest":
				j.Public[0].Digest = strings.Repeat("0", 64)
			case "counts":
				j.Counts.Public++
			case "authority":
				j.Public[0].UID = "unverified-target"
				j.Public[0].Intent = true
				j.Private = []receipt{{Key: key{Kind: "PrincipalState", Name: "unplanned"}, Digest: strings.Repeat("0", 64), Intent: true}}
			}
			raw, _ := json.Marshal(j)
			sec, err := e.core.CoreV1().Secrets(e.ns).Create(e.ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: JournalName}, Type: journalType, Data: map[string][]byte{"journal": raw}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = e.m.Resume(e.ctx, sec.ResourceVersion, OfflineAcknowledgement{true})
			requireProblem(t, err, "invalid-journal")
			now, err := e.core.CoreV1().Secrets(e.ns).Get(e.ctx, JournalName, metav1.GetOptions{})
			if err != nil || now.ResourceVersion != sec.ResourceVersion {
				t.Fatal("corrupt resume wrote journal", err)
			}
			target, err := e.m.getPublic(e.ctx, key{"ServiceAccount", e.ns, "ci"})
			if err != nil || target != nil {
				t.Fatal("corrupt resume wrote public target", err)
			}
		})
	}
}

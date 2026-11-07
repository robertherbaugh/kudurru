package iammigrate

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEnvtestNativeGateAndOfflineAcknowledgement(t *testing.T) {
	e := newTestEnv(t, nil)
	s, err := e.m.Check(e.ctx)
	if err != nil || s.State != Native || !s.RemoteAllowed {
		t.Fatal(s, err)
	}
	_, err = e.m.Begin(e.ctx, e.plan(t), OfflineAcknowledgement{})
	requireProblem(t, err, "offline-required")
	if sec, _, err := e.m.loadJournal(e.ctx); err != nil || sec != nil {
		t.Fatal("unacknowledged migration wrote a journal")
	}
	e.seed(t, "LocalUser", "alice", "", `{}`, "")
	s, err = e.m.Check(e.ctx)
	if err != nil || !s.RemoteAllowed {
		t.Fatal("native private users misclassified", s, err)
	}
	e.seed(t, "APIToken", "aaaaaaaaaaaa", "", `{"owner":{"kind":"User","name":"alice","provider":"local","id":"local:alice"},"secretHash":"`+strings.Repeat("0", 64)+`"}`, "")
	s, err = e.m.Check(e.ctx)
	if err != nil || s.State != Required || s.RemoteAllowed {
		t.Fatal("UID-less native credential admitted", s, err)
	}
}

func TestEnvtestPlanConflictAndSourceDrift(t *testing.T) {
	for _, boundary := range []string{"existing-target", "changed-source", "bounds"} {
		t.Run(boundary, func(t *testing.T) {
			e := newTestEnv(t, nil)
			source := e.seed(t, "ServiceAccount", "ci", e.ns, `{}`, "")
			p := e.plan(t)
			switch boundary {
			case "existing-target":
				s := p.sources[0].obj.DeepCopyObject().(store.Object)
				s.SetUID("")
				s.SetResourceVersion("")
				s.SetCreationTimestamp(metav1.Time{})
				u, _ := toUnstructured(s)
				u.SetGeneration(0)
				if _, err := e.m.resource(sourceKey(p.sources[0])).Create(e.ctx, u, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				_, err := e.m.Plan(e.ctx)
				requireProblem(t, err, "target-conflict")
			case "changed-source":
				source.Data["object"] = []byte(`{"metadata":{"name":"ci","namespace":"` + e.ns + `"},"spec":{"disabled":true}}`)
				if _, err := e.core.CoreV1().Secrets(e.ns).Update(e.ctx, source, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				_, err := e.m.Begin(e.ctx, p, OfflineAcknowledgement{true})
				requireProblem(t, err, "source-changed")
			case "bounds":
				e.m.opts.Limits.JournalBytes = 256
				_, err := e.m.Plan(e.ctx)
				requireProblem(t, err, "bounds-exceeded")
			}
			sec, _, err := e.m.loadJournal(e.ctx)
			if err != nil || sec != nil {
				t.Fatal("invalid preflight wrote journal")
			}
		})
	}
}

func TestEnvtestJournalAndArchiveCorruptionFailClosed(t *testing.T) {
	for _, what := range []string{"version", "duplicate", "cluster", "namespace-uid", "binding", "archive", "source"} {
		t.Run(what, func(t *testing.T) {
			e := newTestEnv(t, nil)
			source := e.seed(t, "ServiceAccount", "ci", e.ns, `{}`, "")
			_, err := e.m.Begin(e.ctx, e.plan(t), OfflineAcknowledgement{true})
			if err != nil {
				t.Fatal(err)
			}
			sec, j, err := e.m.loadJournal(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch what {
			case "version":
				j.Version++
			case "cluster":
				j.Cluster = "different-cluster"
			case "namespace-uid":
				j.NamespaceUID = "different-release-namespace"
			case "binding":
				j.Counts.UnboundTokens++
				j.Completion = completion(j)
			case "archive":
				if err = e.core.CoreV1().Secrets(e.ns).Delete(e.ctx, j.Sources[0].Archive, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			case "source":
				source.Annotations["drift"] = "changed"
				if _, err = e.core.CoreV1().Secrets(e.ns).Update(e.ctx, source, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if what != "archive" && what != "source" {
				raw, _ := json.Marshal(j)
				if what == "duplicate" {
					raw = []byte(`{"version":1,"version":1}`)
				}
				sec.Data["journal"] = raw
				if _, err = e.core.CoreV1().Secrets(e.ns).Update(e.ctx, sec, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			s, err := e.m.Check(e.ctx)
			if err == nil || s.RemoteAllowed {
				t.Fatal("corruption admitted", s, err)
			}
			if _, err = e.m.Bindings(e.ctx); err == nil {
				t.Fatal("corrupt private provenance accepted")
			}
		})
	}
}

func TestEnvtestStaleJournalCASAndPrivateConflict(t *testing.T) {
	e := newTestEnv(t, nil)
	e.seed(t, "ServiceAccount", "ci", e.ns, `{}`, "")
	_, err := e.m.Begin(e.ctx, e.plan(t), OfflineAcknowledgement{true})
	if err != nil {
		t.Fatal(err)
	}
	sec, j, err := e.m.loadJournal(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale := sec.DeepCopy()
	j.Owner = "11111111-1111-1111-1111-111111111111"
	if _, err = e.m.save(e.ctx, sec, j); err != nil {
		t.Fatal(err)
	}
	_, err = e.m.save(e.ctx, stale, j)
	requireProblem(t, err, "CAS-conflict")
	// Old private authority bound to another incarnation cannot be transferred.
	e2 := newTestEnv(t, nil)
	sa := e2.seed(t, "ServiceAccount", "ci", e2.ns, `{"builtinAdmin":true}`, "")
	owner := accountOwner(key{"ServiceAccount", e2.ns, "ci"}, sa.UID)
	owner.UID = "old-replaced-uid"
	name := internalv1.PrincipalStateName(owner)
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{"owner": owner, "builtinAdmin": true}})
	e2.seedRaw(t, "PrincipalState", name, "", raw)
	_, err = e2.m.Plan(e2.ctx)
	requireProblem(t, err, "private-conflict")
}

func TestEnvtestAPIFailureAndCancelledContextCloseAdmission(t *testing.T) {
	var fault *faultTransport
	e := newTestEnv(t, func(base http.RoundTripper) http.RoundTripper { fault = &faultTransport{base: base}; return fault })
	fault.mu.Lock()
	fault.before = func(*http.Request) bool { return true }
	fault.mu.Unlock()
	s, err := e.m.Check(e.ctx)
	if err == nil || s.RemoteAllowed {
		t.Fatal("API outage admitted")
	}
	fault.mu.Lock()
	fault.before = nil
	fault.mu.Unlock()
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	s, err = e.m.Check(ctx)
	if err == nil || s.RemoteAllowed {
		t.Fatal("cancelled read admitted")
	}
}

package iammigrate

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func migrationWrite(r *http.Request) bool {
	return r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete
}

func crashFixture(t *testing.T, e *testEnv) []*corev1.Secret {
	t.Helper()
	return []*corev1.Secret{
		e.seed(t, "ServiceAccount", "ci", e.ns, `{"builtinAdmin":true,"description":"build"}`, `{"tokenID":"aaaaaaaaaaaa"}`),
		e.seed(t, "RoleBinding", "ci", e.ns, `{"role":"viewer","scope":{"kind":""},"subjects":[{"kind":"ServiceAccount","name":"ci","namespace":"`+e.ns+`"}]}`, ""),
		e.seed(t, "IdentityProvider", e.ns+"-sso", "", `{"issuer":"https://sso.example","clientID":"ci","clientSecretRef":{"name":"sso","key":"client"}}`, `{"conditions":[{"type":"Ready","status":"False","reason":"Offline","message":"waiting","lastTransitionTime":"2026-10-13T00:00:00Z"}]}`),
		e.seed(t, "LocalUser", "alice", "", `{}`, ""),
	}
}

// Discover all writes in the real algorithm, then interrupt before and after
// every archive, intent, Create, receipt, status and completion write. This
// follows new boundaries automatically instead of asserting a fixed count.
func TestEnvtestEveryWriteCrashAndRollbackArtifacts(t *testing.T) {
	var baseline *faultTransport
	e := newTestEnv(t, func(base http.RoundTripper) http.RoundTripper {
		baseline = &faultTransport{base: base}
		return baseline
	})
	crashFixture(t, e)
	writes := 0
	baseline.before = func(r *http.Request) bool {
		if migrationWrite(r) {
			writes++
		}
		return false
	}
	if s, err := e.m.Begin(e.ctx, e.plan(t), OfflineAcknowledgement{true}); err != nil || !s.RemoteAllowed {
		t.Fatal(s, err)
	}
	if writes < 12 {
		t.Fatal("expected all migration write boundaries", writes)
	}
	for _, timing := range []string{"before", "after"} {
		for boundary := 1; boundary <= writes; boundary++ {
			t.Run(fmt.Sprintf("%s-%02d", timing, boundary), func(t *testing.T) {
				var fault *faultTransport
				e := newTestEnv(t, func(base http.RoundTripper) http.RoundTripper { fault = &faultTransport{base: base}; return fault })
				originals := crashFixture(t, e)
				plan := e.plan(t)
				count, stopped := 0, false
				fault.before = func(r *http.Request) bool {
					if !migrationWrite(r) {
						return false
					}
					if stopped {
						return true
					}
					count++
					if timing == "before" && count == boundary {
						stopped = true
						return true
					}
					return false
				}
				fault.after = func(r *http.Request, resp *http.Response) bool {
					if timing == "after" && migrationWrite(r) && count == boundary && resp.StatusCode >= 200 && resp.StatusCode < 300 {
						stopped = true
						return true
					}
					return false
				}
				s, err := e.m.Begin(e.ctx, plan, OfflineAcknowledgement{true})
				if !stopped || err == nil || s.RemoteAllowed {
					t.Fatal("interrupted write allowed admission", s, err, count)
				}
				fault.mu.Lock()
				fault.before = nil
				fault.after = nil
				fault.mu.Unlock()
				s, b, err := e.m.Capture(e.ctx)
				if err != nil {
					requireProblem(t, err, "uncertain-create")
					if s.RemoteAllowed || b != nil {
						t.Fatal("unknown UID admitted")
					}
					// Only an explicit operator-selected UID deletion permits a new Create.
					_, j, loadErr := e.m.loadJournal(e.ctx)
					if loadErr != nil {
						t.Fatal(loadErr)
					}
					for _, r := range j.Public {
						if !r.Intent || r.UID != "" {
							continue
						}
						u, getErr := e.m.getPublic(e.ctx, r.Key)
						if getErr != nil {
							t.Fatal(getErr)
						}
						if u != nil {
							uid := u.GetUID()
							if deleteErr := e.m.resource(r.Key).Delete(e.ctx, r.Key.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); deleteErr != nil {
								t.Fatal(deleteErr)
							}
						}
					}
					for _, r := range j.Private {
						if !r.Intent || r.UID != "" {
							continue
						}
						sec, getErr := e.core.CoreV1().Secrets(e.ns).Get(e.ctx, secretName(r.Key.Kind, "", r.Key.Name), metav1.GetOptions{})
						if getErr != nil {
							t.Fatal(getErr)
						}
						uid := sec.UID
						if deleteErr := e.core.CoreV1().Secrets(e.ns).Delete(e.ctx, sec.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); deleteErr != nil {
							t.Fatal(deleteErr)
						}
					}
					s, _, err = e.m.Capture(e.ctx)
					if err != nil {
						t.Fatal(err)
					}
				}
				if s.State == Required || s.State == Native {
					s, err = e.m.Begin(e.ctx, plan, OfflineAcknowledgement{true})
				} else if s.State != Complete {
					if s.RemoteAllowed || b != nil {
						t.Fatal("incomplete snapshot admitted")
					}
					s, err = e.m.Resume(e.ctx, s.JournalResourceVersion, OfflineAcknowledgement{true})
				}
				if err != nil || !s.RemoteAllowed {
					t.Fatal("offline recovery failed", s, err)
				}
				for _, original := range originals {
					now, getErr := e.core.CoreV1().Secrets(e.ns).Get(e.ctx, original.Name, metav1.GetOptions{})
					if getErr != nil || !reflect.DeepEqual(original, now) {
						t.Fatal("rollback artifact changed", getErr)
					}
				}
			})
		}
	}
}

type heldCreateTransport struct {
	base             http.RoundTripper
	once             sync.Once
	entered, release chan struct{}
}

func (h *heldCreateTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/apis/kudurru.io/") {
		h.once.Do(func() {
			close(h.entered)
			select {
			case <-h.release:
			case <-r.Context().Done():
			}
		})
	}
	return h.base.RoundTrip(r)
}

func TestEnvtestStaleRunnerLateCreateCannotRecordOrComplete(t *testing.T) {
	var held *heldCreateTransport
	e := newTestEnv(t, func(base http.RoundTripper) http.RoundTripper {
		held = &heldCreateTransport{base: base, entered: make(chan struct{}), release: make(chan struct{})}
		return held
	})
	e.seed(t, "ServiceAccount", "ci", e.ns, `{"builtinAdmin":true}`, "")
	plan := e.plan(t)
	type outcome struct {
		s   Status
		err error
	}
	done := make(chan outcome, 1)
	go func() { s, err := e.m.Begin(e.ctx, plan, OfflineAcknowledgement{true}); done <- outcome{s, err} }()
	select {
	case <-held.entered:
	case <-e.ctx.Done():
		t.Fatal("old runner did not reach Create")
	}
	defer close(held.release)
	// A separate uncached client claims the exact inspected RV while the old
	// request is still in flight. No test client predicts target UIDs.
	fresh := newTestEnv(t, nil)
	fresh.m.opts.Namespace = e.ns
	status, err := fresh.m.Check(e.ctx)
	if err != nil || status.RemoteAllowed || status.State != Running {
		t.Fatal(status, err)
	}
	winner, err := fresh.m.Resume(e.ctx, status.JournalResourceVersion, OfflineAcknowledgement{true})
	if err != nil || !winner.RemoteAllowed {
		t.Fatal(winner, err)
	}
	_, err = fresh.m.Resume(e.ctx, status.JournalResourceVersion, OfflineAcknowledgement{true})
	requireProblem(t, err, "CAS-conflict")
	held.release <- struct{}{}
	select {
	case loser := <-done:
		if loser.err == nil || loser.s.RemoteAllowed {
			t.Fatal("stale runner completed", loser)
		}
	case <-e.ctx.Done():
		t.Fatal("stale runner did not terminate")
	}
	cut, b, err := fresh.m.Capture(e.ctx)
	if err != nil || !cut.RemoteAllowed || cut.JournalResourceVersion != winner.JournalResourceVersion || b == nil {
		t.Fatal("stale runner overwrote winner", cut, err)
	}
}

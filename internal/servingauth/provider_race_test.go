package servingauth

import (
	"context"
	"sync"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestExternalHeldViewConcurrentDetachedAuthentication(t *testing.T) {
	f := newExternalFixture(t)
	cookie := f.enroll(t, false)
	v := f.view(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 16 {
				p, err := v.Authenticate(t.Context(), Credential{Kind: OIDCJWT, Value: f.raw}, f.now)
				if err != nil {
					t.Error(err)
					return
				}
				p.Groups[0] = "modified"
				p.Grants[0] = "admin"
				p.External.Evidence = "modified"
				s, err := v.CookieSession(cookie, f.now)
				if err != nil {
					t.Error(err)
					return
				}
				s.Principal.External.SessionID = "modified"
				s.Principal.Groups[0] = "modified"
				s.RolesSnapshot[0] = "admin"
			}
		})
	}
	wg.Wait()
}
func TestExternalConcurrentCurrentEnrollmentSeesSingleExactIdentity(t *testing.T) {
	f := newExternalFixture(t)
	f.enroll(t, false)
	proof := f.proof(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				d, err := ResolveExternalSessionCurrent(ctx, tx, f.deps, proof, f.now)
				if err != nil {
					return err
				}
				if d.Existing == nil || d.Existing.UID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
					t.Error("incarnation changed")
				}
				d.Existing.Groups[0] = "modified"
				d.Grants[0] = "admin"
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	state := f.state(t)
	if len(state.ExternalIdentities) != 1 || state.ExternalIdentities[0].Groups[0] == "modified" {
		t.Fatal("concurrent resolver mutated enrollment")
	}
}

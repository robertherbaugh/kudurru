package providerentitlement_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	pe "github.com/robertherbaugh/kudurru/internal/auth/providerentitlement"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestPendingRequiresOriginalOpaqueTargetAndConcurrentStringsGrantNothing(t *testing.T) {
	f := newLicenseFixture(t)
	p := profile(t, f.manager(t, f.claims(), "test"))
	s := newStore(t, f.at)
	ctx, target := writer(t, s)
	for name, setup := range map[string]func() (context.Context, serving.TermActivation, string){
		"nil":     func() (context.Context, serving.TermActivation, string) { return nil, target, p },
		"missing": func() (context.Context, serving.TermActivation, string) { return t.Context(), target, p },
		"zero-token": func() (context.Context, serving.TermActivation, string) {
			return sqlite.WithWriterToken(t.Context(), sqlite.WriterToken{}), target, p
		},
		"canceled": func() (context.Context, serving.TermActivation, string) {
			c, cancel := context.WithCancel(ctx)
			cancel()
			return c, target, p
		},
		"wrong-incarnation": func() (context.Context, serving.TermActivation, string) {
			wrong := target
			wrong.Incarnation++
			return ctx, wrong, p
		},
		"wrong-term": func() (context.Context, serving.TermActivation, string) {
			wrong := target
			wrong.Term.Epoch++
			return ctx, wrong, p
		},
		"uppercase-profile": func() (context.Context, serving.TermActivation, string) { return ctx, target, strings.ToUpper(p) },
		"profile-cap": func() (context.Context, serving.TermActivation, string) {
			return ctx, target, strings.Repeat("a", 1<<20)
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, requested, value := setup()
			if e, err := pe.NewPending(c, requested, value); e != "" || !errors.Is(err, pe.ErrContinuity) {
				t.Fatal("pending constructor accepted invalid opaque authority shape", err)
			}
		})
	}
	before := image(t, s)
	results := make(chan string, 64)
	var wg sync.WaitGroup
	for range cap(results) {
		wg.Go(func() {
			e, err := pe.NewPending(ctx, target, p)
			if err != nil {
				t.Error(err)
			}
			results <- e
		})
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for e := range results {
		if e == "" || seen[e] {
			t.Fatal("concurrent pending activation nonce reused")
		}
		seen[e] = true
		if _, err := pe.Parse(e, target.Term.ClusterID, p); err != nil {
			t.Fatal(err)
		}
	}
	if !before.Generation().Same(image(t, s).Generation()) {
		t.Fatal("pending constructor performed a command")
	}
	if _, err := pe.Installed(registry(t, s), target.Term.ClusterID, p); !errors.Is(err, pe.ErrContinuity) {
		t.Fatal("pending values silently enrolled authority")
	}
	if _, err := s.EnablePrimary(t.Context(), before.Generation()); err != nil {
		t.Fatal(err)
	}
	// The pure constructor cannot check authority liveness. Stale carried input
	// can form a string, but actual SQLite proves it cannot admit a command.
	if _, err := pe.NewPending(ctx, target, p); err != nil {
		t.Fatal("constructor unexpectedly read/sampled live writer authority", err)
	}
	if err := s.Command(ctx, target.Term, func(context.Context, *sqlite.CommandTx) error { return nil }); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("stale pending context admitted actual no-op command", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pe.NewPending(ctx, target, p); err != nil {
		t.Fatal("pure constructor reads closed Store", err)
	}
}

func TestParseExactVersionBoundariesAndExpectedClusterProfile(t *testing.T) {
	f := newLicenseFixture(t)
	p := profile(t, f.manager(t, f.claims(), "test"))
	s := newStore(t, f.at)
	e := pending(t, s, p)
	cluster := s.Authority().Term.ClusterID
	parts := strings.Split(e, "/")
	for name, value := range map[string]string{
		"missing":         "",
		"unknown-version": strings.Replace(e, "/v1/", "/v2/", 1),
		"uppercase-hash":  strings.Join([]string{parts[0], parts[1], strings.Repeat("A", 64), parts[3], parts[4]}, "/"),
		"uppercase-nonce": strings.Join([]string{parts[0], parts[1], parts[2], parts[3], strings.Repeat("A", 32)}, "/"),
		"bad-nonce":       strings.Join([]string{parts[0], parts[1], parts[2], parts[3], strings.Repeat("z", 32)}, "/"),
		"extra-component": e + "/more",
		"short":           e[:len(e)-1],
		"long":            e + "0",
		"whitespace":      " " + e,
		"cap":             strings.Repeat("x", 1<<20),
	} {
		t.Run(name, func(t *testing.T) {
			if parsed, err := pe.Parse(value, cluster, p); parsed != "" || !errors.Is(err, pe.ErrContinuity) {
				t.Fatal("invalid encoding accepted", err)
			}
		})
	}
	for _, expected := range [][2]string{{"other-cluster", p}, {"", p}, {cluster, strings.Repeat("b", 64)}, {strings.Repeat("a", 257), p}, {cluster, strings.ToUpper(p)}} {
		if _, err := pe.Parse(e, expected[0], expected[1]); !errors.Is(err, pe.ErrContinuity) {
			t.Fatal("unexpected cluster/profile accepted", err)
		}
	}
	if _, err := pe.Installed(nil, cluster, p); !errors.Is(err, pe.ErrContinuity) {
		t.Fatal("nil registry adopted pending string")
	}
}

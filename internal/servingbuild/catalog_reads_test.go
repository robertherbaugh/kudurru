package servingbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apihttp "github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

var catalogReadPaths = []string{"catalogue", "catalogue/services", "catalogue/categories", "catalogue/safesearch"}

type heldCatalogReads struct {
	*CatalogueReads
	remaining atomic.Int64
	entered   chan context.Context
	resume    chan struct{}
}

func (h *heldCatalogReads) read(ctx context.Context) (*catalogue.Catalogue, error) {
	if h.remaining.Add(-1) >= 0 {
		h.entered <- ctx
		select {
		case <-h.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h.Catalogue(ctx)
}

func catalogReadHTTP(t *testing.T, f *fixture, publisher *serving.Publisher, read func(context.Context) (*catalogue.Catalogue, error)) *httptest.Server {
	t.Helper()
	cop := http.NewCrossOriginProtection()
	auth, err := servinghttp.New(servinghttp.Options{Publisher: publisher, Cookie: session.CookieConfig{Secure: session.SecureAlways}, CrossOriginProtection: cop})
	if err != nil {
		t.Fatal(err)
	}
	api, err := apihttp.New(apihttp.Options{Store: f.store, Validator: f.builder.opts.Schema.validator, Config: f.deps.Host,
		Authenticate: auth.Middleware, Authorizer: auth, CrossOrigin: cop, Catalogue: apihttp.CatalogueSource{
			Read: read, Holder: catalogue.NewHolder(catalogDocument(t, "competing", "https://live.example/competing")),
			Status:       func() catalogue.Status { panic("captured catalog consulted live refresh status") },
			ImportStatus: func() catalogue.Status { panic("captured catalog consulted live import status") },
		}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	return server
}

func catalogReadDocument(t *testing.T, id, origin string) *catalogue.Catalogue {
	t.Helper()
	data := fmt.Appendf(nil, `{"schemaVersion":2,"version":"%s","license":"Apache-2.0",
		"categories":[{"id":"video","name":"%s category","order":1}],
		"services":[{"id":"%s","name":"%s service","category":"video","rules":["||%s.example^"],"source":"test"}],
		"safeSearch":[{"id":"search","name":"%s search","addresses":["192.0.2.9"],"domains":["search.example"],"source":"%s"}]}`, id, id, id, id, id, id, id)
	c, err := catalogue.Parse(data, origin)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func catalogReadWait(t *testing.T, p *serving.Publisher, c *catalogue.Catalogue) serving.Identity {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		changed := p.Subscribe()
		lease, err := p.CaptureHTTP(t.Context())
		if err == nil {
			v, ok := lease.Bundle().Component("blocking")
			view, typed := v.(*blockingstage.View)
			id := lease.Bundle().Identity()
			matched := ok && typed && view.Catalogue() != nil && view.Catalogue().Fingerprint() == c.Fingerprint() && view.Catalogue().Origin() == c.Origin()
			lease.Release()
			if matched {
				return id
			}
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("catalog did not publish: %+v", p.Status())
		}
	}
}

func catalogReadStartPublisher(t *testing.T, source serving.Source, prepare serving.PrepareFunc) *serving.Publisher {
	t.Helper()
	p, err := serving.New(serving.Options{Source: source, Prepare: prepare, Required: serving.CompleteManifest(), RetryInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
		p.Close()
	})
	select {
	case <-p.Initialized():
	case <-time.After(10 * time.Second):
		t.Fatalf("catalog publisher did not initialize: %+v", p.Status())
	}
	return p
}

func TestCapturedCatalogActualSQLiteHeldRefreshAndSourceReplacement(t *testing.T) {
	for _, change := range []string{"same-image-refresh", "source-replacement"} {
		t.Run(change, func(t *testing.T) {
			f := newCatalogFixture(t)
			a := catalogReadDocument(t, "alpha", f.url)
			f.holder.Set(a)
			originalID := catalogReadWait(t, f.publisher, a)
			h := &heldCatalogReads{CatalogueReads: NewCatalogueReads(), entered: make(chan context.Context, len(catalogReadPaths)), resume: make(chan struct{})}
			h.remaining.Store(int64(len(catalogReadPaths)))
			server := catalogReadHTTP(t, f.fixture, f.publisher, h.read)
			// Join handlers before server cleanup even on setup/assertion failure.
			t.Cleanup(func() {
				select {
				case <-h.resume:
				default:
					close(h.resume)
				}
			})
			results := make([]chan objectReadResponse, len(catalogReadPaths))
			for i, path := range catalogReadPaths {
				results[i] = make(chan objectReadResponse, 1)
				go func() { results[i] <- objectReadGET(t.Context(), server, f.token, path) }()
			}
			contexts := make([]context.Context, len(catalogReadPaths))
			for i := range contexts {
				select {
				case contexts[i] = <-h.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("catalog read did not reach original authenticated lease")
				}
				if c, err := h.Catalogue(contexts[i]); err != nil || c.Digest() != a.Digest() {
					t.Fatal("fixture did not hold catalog A", err)
				}
			}
			freshServer := server
			var successorID serving.Identity
			b := catalogReadDocument(t, "beta", f.url)
			if change == "same-image-refresh" {
				f.holder.Set(b)
				successorID = catalogReadWait(t, f.publisher, b)
				if !originalID.Base.Same(successorID.Base) || originalID == successorID {
					t.Fatal("same-image refresh did not replace dependency installation", originalID, successorID)
				}
			} else {
				// The configured source is restart-required. Build a successor
				// assembly from the new original source, never relabel owner A.
				source := f.owner.source
				source.URL = "https://successor.example/catalog"
				source.AdGuardServicesURL = "https://successor.example/import"
				setCatalogSettings(t, f.fixture, source)
				b = catalogReadDocument(t, "beta", source.URL)
				holder := catalogue.NewHolder(b)
				imp, err := catalogue.ParseAdGuard([]byte(`{"blocked_services":[{"id":"imported_beta","name":"Imported Beta","group":"streaming","rules":["||imported.example^"]}]}`), source.AdGuardServicesURL)
				if err != nil {
					t.Fatal(err)
				}
				holder.SetImport(imp)
				b = holder.Current()
				owner, err := NewCatalogRuntime(holder, source)
				if err != nil {
					t.Fatal(err)
				}
				opts := f.builder.opts
				capture := opts.Capture
				opts.Capture, err = owner.CaptureDependencies(func(ctx context.Context, image *replication.Image) (Dependencies, error) {
					d, err := capture(ctx, image)
					if err == nil {
						host := *d.Host
						host.Blocking.Catalogue = source
						d.Host = &host
					}
					return d, err
				})
				if err != nil {
					t.Fatal(err)
				}
				build, err := New(opts)
				if err != nil {
					t.Fatal(err)
				}
				guard, err := owner.GuardSource(f.store)
				if err != nil {
					t.Fatal(err)
				}
				publisher := catalogReadStartPublisher(t, guard, build.Prepare)
				successorID = catalogReadWait(t, publisher, b)
				if originalID.Base.Same(successorID.Base) {
					t.Fatal("source edit did not change canonical image")
				}
				freshServer = catalogReadHTTP(t, f.fixture, publisher, NewCatalogueReads().Catalogue)
				// The old assembly cannot serve fresh reads with a new source.
				diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "catalogue").diagnosticResponse, http.StatusServiceUnavailable)
			}
			close(h.resume)
			for i, path := range catalogReadPaths {
				body := diagnosticWant(t, (<-results[i]).diagnosticResponse, http.StatusOK)
				if !strings.Contains(body, "alpha") || strings.Contains(body, "beta") || strings.Contains(body, "competing") || strings.Contains(body, "refresh") {
					t.Fatalf("held %s mixed later catalog or live diagnostics: %s", path, body)
				}
				fresh := diagnosticWant(t, objectReadGET(t.Context(), freshServer, f.token, path).diagnosticResponse, http.StatusOK)
				if !strings.Contains(fresh, "beta") || strings.Contains(fresh, "alpha") || strings.Contains(fresh, "competing") || strings.Contains(fresh, "refresh") {
					t.Fatalf("fresh %s did not use catalog B: %s", path, fresh)
				}
				if path == "catalogue" {
					var info apihttp.CatalogueInfo
					wantSources := 1
					if imp, _ := b.Imported(); imp != nil {
						wantSources++
					}
					if err := json.Unmarshal([]byte(fresh), &info); err != nil || info.Origin != b.Origin() || info.Digest != b.Digest() || len(info.Sources) != wantSources || info.Services != len(b.Services) || info.Sources[0].Services != 1 {
						t.Fatal("actual captured acquisition receipt lost", info, err)
					}
					if imp, _ := b.Imported(); imp != nil && (info.Sources[1].Origin != imp.Origin() || info.Sources[1].Digest != imp.Digest() || info.Sources[1].Services != 1 || !info.Sources[1].Loaded) {
						t.Fatal("actual captured import receipt lost", info.Sources)
					}
				}
			}
			for _, ctx := range contexts {
				if _, err := h.Catalogue(ctx); err == nil {
					t.Fatal("completed handler retained a usable released catalog lease")
				}
			}
		})
	}
}

func TestCapturedCatalogActualSQLiteMissingReleasedAndCanceled(t *testing.T) {
	f := newFixture(t)
	r := NewCatalogueReads()
	var nilReader *CatalogueReads
	for _, read := range []func(context.Context) (*catalogue.Catalogue, error){
		nilReader.Catalogue,
		func(context.Context) (*catalogue.Catalogue, error) { return r.Catalogue(nil) }, //nolint:staticcheck // Deliberately exercise a missing request context.
		func(context.Context) (*catalogue.Catalogue, error) { return r.Catalogue(context.Background()) },
		func(ctx context.Context) (*catalogue.Catalogue, error) {
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			return r.Catalogue(canceled)
		},
	} {
		server := catalogReadHTTP(t, f, f.pub, read)
		for _, path := range catalogReadPaths {
			diagnosticWant(t, objectReadGET(t.Context(), server, f.token, path).diagnosticResponse, http.StatusServiceUnavailable)
		}
	}
	originals := make(chan context.Context, 1)
	server := catalogReadHTTP(t, f, f.pub, func(ctx context.Context) (*catalogue.Catalogue, error) { originals <- ctx; return r.Catalogue(ctx) })
	diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "catalogue").diagnosticResponse, http.StatusOK)
	original := <-originals
	// Sending a new authenticated request cannot repair the saved old context.
	stale := catalogReadHTTP(t, f, f.pub, func(context.Context) (*catalogue.Catalogue, error) { return r.Catalogue(original) })
	for _, path := range catalogReadPaths {
		diagnosticWant(t, objectReadGET(t.Context(), stale, f.token, path).diagnosticResponse, http.StatusServiceUnavailable)
	}
}

func TestCapturedCatalogActualSQLiteMissingBlockingOwner(t *testing.T) {
	f := newFixture(t)
	for _, value := range []any{struct{}{}, &blockingstage.View{}} {
		// Explicit test-only incomplete owner; production builder always
		// supplies the correctly typed blocking view in the complete bundle.
		p := catalogReadStartPublisher(t, f.store, func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
			prepared, err := f.builder.Prepare(ctx, c)
			if err == nil {
				for i := range prepared.Components {
					if prepared.Components[i].Name == "blocking" {
						prepared.Components[i].Value = value
					}
				}
			}
			return prepared, err
		})
		server := catalogReadHTTP(t, f, p, NewCatalogueReads().Catalogue)
		for _, path := range catalogReadPaths {
			diagnosticWant(t, objectReadGET(t.Context(), server, f.token, path).diagnosticResponse, http.StatusServiceUnavailable)
		}
	}
}

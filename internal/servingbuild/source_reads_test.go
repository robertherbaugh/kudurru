package servingbuild

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	apihttp "github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sourceReadHTTP(t *testing.T, f *fixture, objects apihttp.ObjectReadSource, bodies apihttp.SourceBodyReadSource) *httptest.Server {
	t.Helper()
	cop := http.NewCrossOriginProtection()
	auth, err := servinghttp.New(servinghttp.Options{Publisher: f.pub, Cookie: session.CookieConfig{Secure: session.SecureAlways}, CrossOriginProtection: cop})
	if err != nil {
		t.Fatal(err)
	}
	api, err := apihttp.New(apihttp.Options{Store: f.store, Validator: f.builder.opts.Schema.validator, Config: f.deps.Host, Authenticate: auth.Middleware, Authorizer: auth, ObjectReads: objects, SourceBodyReads: bodies, CrossOrigin: cop})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	return server
}

func sourceReadPage(t *testing.T, result objectReadResponse) apihttp.SourceEntries {
	t.Helper()
	body := diagnosticWant(t, result.diagnosticResponse, http.StatusOK)
	var page apihttp.SourceEntries
	if err := json.Unmarshal([]byte(body), &page); err != nil || page.Items == nil || page.StaleExcludes == nil || page.Fetched != nil || page.Stale {
		t.Fatal("source page was incomplete or fabricated local fetch metadata", body, err)
	}
	return page
}

type heldSourceBodyReads struct {
	*SourceBodyReads
	after     bool
	remaining atomic.Int64
	entered   chan context.Context
	resume    chan struct{}
}

func (h *heldSourceBodyReads) OpenSourceBody(ctx context.Context, ns, name, source string) (io.ReadCloser, string, int64, error) {
	if h.remaining.Add(-1) < 0 {
		return h.SourceBodyReads.OpenSourceBody(ctx, ns, name, source)
	}
	wait := func() error {
		h.entered <- ctx
		select {
		case <-h.resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !h.after {
		if err := wait(); err != nil {
			return nil, "", 0, err
		}
		return h.SourceBodyReads.OpenSourceBody(ctx, ns, name, source)
	}
	body, digest, size, err := h.SourceBodyReads.OpenSourceBody(ctx, ns, name, source)
	if err != nil {
		return nil, "", 0, err
	}
	if err := wait(); err != nil {
		_ = body.Close()
		return nil, "", 0, err
	}
	return body, digest, size, nil
}

func sourceReadPut(t *testing.T, f *fixture, list *public.BlockList, body string) *public.BlockList {
	t.Helper()
	entry, _, err := f.builder.opts.Lists.Put(listfetcher.SourceKey(list.Namespace, list.Spec.Sources[0]), strings.NewReader(body), listfetcher.PutOptions{Fetched: f.now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	var stored public.BlockList
	if err := f.store.Get(t.Context(), store.KeyOf(list), &stored); err != nil {
		t.Fatal(err)
	}
	stored.Status.Sources = []public.SourceStatus{{Name: "original", Digest: entry.Digest, LastFetched: &metav1.Time{Time: f.now.Add(-time.Hour)}}}
	if err := f.store.UpdateStatus(t.Context(), &stored); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Get(t.Context(), store.KeyOf(list), &stored); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	return &stored
}

const sourceReadPath = "blocklists/default/browser/sources/original/entries"

func TestSourceBodyReadsActualSQLiteHeldURLConfigMapReplacement(t *testing.T) {
	for _, kind := range []string{"url", "configMap"} {
		for _, after := range []bool{false, true} {
			t.Run(kind+"/opened="+map[bool]string{false: "before", true: "after"}[after], func(t *testing.T) {
				f := newFixture(t)
				src := public.ListSource{Name: "original", Format: "domains", Exclude: []string{"beta.example", "gone.example"}}
				if kind == "url" {
					src.URL = "https://lists.example.test/original"
				} else {
					src.ConfigMapRef = &public.ObjectRef{Name: "original", Namespace: "default"}
				}
				list := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "browser", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{src}}}
				original := sourceReadPut(t, f, list, "alpha.example\nbeta.example\n")
				reader, err := NewSourceBodyReads(f.builder.opts.Lists)
				if err != nil {
					t.Fatal(err)
				}
				h := &heldSourceBodyReads{SourceBodyReads: reader, after: after, entered: make(chan context.Context, 1), resume: make(chan struct{})}
				h.remaining.Store(1)
				server := sourceReadHTTP(t, f, NewObjectReads(), h)
				t.Cleanup(func() {
					select {
					case <-h.resume:
					default:
						close(h.resume)
					}
				})
				done := make(chan objectReadResponse, 1)
				go func() { done <- objectReadGET(t.Context(), server, f.token, sourceReadPath+"?limit=1") }()
				var held context.Context
				select {
				case held = <-h.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("source request did not hold original authenticated lease")
				}
				list.Spec.Sources[0].Exclude = nil
				current := sourceReadPut(t, f, list, "fresh.example\n")
				if original.UID != current.UID || original.ResourceVersion == current.ResourceVersion {
					t.Fatal("fixture did not replace same-UID source receipt")
				}
				close(h.resume)
				result := <-done
				if after {
					old := sourceReadPage(t, result)
					if result.etag != original.ResourceVersion || old.Digest != original.Status.Sources[0].Digest || old.Total != 2 || old.Excluded != 1 || len(old.Items) != 1 || old.Items[0].Value != "alpha.example" || len(old.StaleExcludes) != 1 || old.StaleExcludes[0] != "gone.example" || old.Continue == "" {
						t.Fatal("held descriptor borrowed current receipt/exclusions", old, result.etag)
					}
					diagnosticWant(t, objectReadGET(t.Context(), server, f.token, sourceReadPath+"?limit=1&continue="+old.Continue).diagnosticResponse, http.StatusConflict)
				} else {
					diagnosticWant(t, result.diagnosticResponse, http.StatusServiceUnavailable)
				}
				fresh := sourceReadPage(t, objectReadGET(t.Context(), server, f.token, sourceReadPath))
				if fresh.Digest != current.Status.Sources[0].Digest || fresh.Total != 1 || fresh.Excluded != 0 || len(fresh.Items) != 1 || fresh.Items[0].Value != "fresh.example" || len(fresh.StaleExcludes) != 0 {
					t.Fatal("fresh response not exact successor", fresh)
				}
				if body, _, _, err := reader.OpenSourceBody(held, "default", "browser", "original"); err == nil {
					_ = body.Close()
					t.Fatal("released original lease remained usable")
				}
			})
		}
	}
}

func TestSourceBodyReadsActualSQLiteInlineSourceAndUIDReplacement(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		t.Run(map[bool]string{false: "source", true: "uid"}[recreate], func(t *testing.T) {
			f := newFixture(t)
			list := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "browser", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "original", Inline: []string{"old.example", "second.example"}}}}}
			if _, err := f.store.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				t.Fatal(err)
			}
			f.waitCurrent(t)
			reader, err := NewSourceBodyReads(f.builder.opts.Lists)
			if err != nil {
				t.Fatal(err)
			}
			h := &heldObjectReads{ObjectReads: NewObjectReads(), entered: make(chan context.Context, 1), resume: make(chan struct{})}
			h.remaining.Store(1)
			server := sourceReadHTTP(t, f, h, reader)
			cleanupHeldObjectReads(t, h)
			done := make(chan objectReadResponse, 1)
			go func() { done <- objectReadGET(t.Context(), server, f.token, sourceReadPath+"?limit=1") }()
			held := objectReadWait(t, h)
			objects, _, err := h.ObjectReads.ObjectSnapshot(held)
			if err != nil {
				t.Fatal(err)
			}
			old, _ := objects.Get(store.KeyOf(list))
			if recreate {
				if err := f.store.Delete(t.Context(), store.KeyOf(list), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
					t.Fatal(err)
				}
			} else {
				list.Spec.Sources[0].Name = "replacement"
			}
			list.Spec.Sources[0].Inline = []string{"new.example"}
			if _, err := f.store.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				t.Fatal(err)
			}
			f.waitCurrent(t)
			close(h.resume)
			result := <-done
			page := sourceReadPage(t, result)
			if result.etag != old.GetResourceVersion() || len(page.Items) != 1 || page.Items[0].Value != "old.example" || page.Continue == "" {
				t.Fatal("held inline data mixed source/UID replacement", page)
			}
			if recreate {
				diagnosticWant(t, objectReadGET(t.Context(), server, f.token, sourceReadPath+"?limit=1&continue="+page.Continue).diagnosticResponse, http.StatusConflict)
				fresh := sourceReadPage(t, objectReadGET(t.Context(), server, f.token, sourceReadPath))
				if fresh.Items[0].Value != "new.example" {
					t.Fatal(fresh)
				}
			} else {
				diagnosticWant(t, objectReadGET(t.Context(), server, f.token, sourceReadPath).diagnosticResponse, http.StatusNotFound)
			}
		})
	}
}

type sourceReadProbe struct {
	reader *SourceBodyReads
	check  func(context.Context)
}

func (p sourceReadProbe) OpenSourceBody(ctx context.Context, ns, name, source string) (io.ReadCloser, string, int64, error) {
	p.check(ctx)
	return p.reader.OpenSourceBody(ctx, ns, name, source)
}

func TestSourceBodyReadsActualSQLiteDescriptorVerificationAndCancellation(t *testing.T) {
	f := newFixture(t)
	list := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "browser", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "original", Format: "domains", URL: "https://lists.example.test/body"}}}}
	sourceReadPut(t, f, list, "alpha.example\n")
	reader, err := NewSourceBodyReads(f.builder.opts.Lists)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := f.builder.opts.Lists.Path(list.Spec.Sources[0].URL)
	captured := make(chan context.Context, 8)
	server := sourceReadHTTP(t, f, NewObjectReads(), sourceReadProbe{reader, func(ctx context.Context) { captured <- ctx }})
	// Populate the parsed digest index before corrupting the real body. Cached
	// derived data must never stand in for original descriptor verification.
	if page := sourceReadPage(t, objectReadGET(t.Context(), server, f.token, sourceReadPath)); page.Total != 1 {
		t.Fatal("valid original body did not populate the source index")
	}
	for _, fault := range []string{"hash", "size", "directory", "too-large", "missing"} {
		t.Run(fault, func(t *testing.T) {
			// Deliberately corrupt actual cache files AFTER full publication,
			// keeping the original metadata and public receipt unchanged.
			if err := os.RemoveAll(cachePath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cachePath, []byte("alpha.example\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "hash":
				if err := os.WriteFile(cachePath, []byte("wrong.example\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "size":
				if err := os.Truncate(cachePath, 2); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(cachePath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(cachePath, 0o700); err != nil {
					t.Fatal(err)
				}
			case "too-large":
				if err := os.Truncate(cachePath, maxSourceReadBytes+1); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Remove(cachePath); err != nil {
					t.Fatal(err)
				}
			}
			diagnosticWant(t, objectReadGET(t.Context(), server, f.token, sourceReadPath).diagnosticResponse, http.StatusServiceUnavailable)
		})
	}
	if _, err := NewSourceBodyReads(nil); err == nil {
		t.Fatal("nil cache accepted")
	}
	if _, _, _, err := reader.OpenSourceBody(context.Background(), "default", "browser", "original"); err == nil {
		t.Fatal("missing original capture accepted")
	}
	ctx, cancel := context.WithCancel(<-captured)
	cancel()
	if _, _, _, err := reader.OpenSourceBody(ctx, "default", "browser", "original"); err == nil {
		t.Fatal("canceled capture accepted")
	}
	if _, _, _, err := (*SourceBodyReads)(nil).OpenSourceBody(ctx, "default", "browser", "original"); err == nil {
		t.Fatal("nil reader accepted")
	}
	if _, _, _, err := reader.OpenSourceBody(nil, "default", "browser", "original"); err == nil { //nolint:staticcheck // SA1012: exercise rejection of a nil request context.
		t.Fatal("nil context accepted")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("fixture cancellation did not persist")
	}
	if err := os.WriteFile(cachePath, []byte("alpha.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	live := sourceReadHTTP(t, f, NewObjectReads(), sourceReadProbe{reader, func(ctx context.Context) {
		derived, cancel := context.WithCancel(ctx)
		defer cancel()
		body, _, _, err := reader.OpenSourceBody(derived, "default", "browser", "original")
		if err != nil {
			t.Error("live original descriptor did not verify", err)
			return
		}
		defer func() { _ = body.Close() }()
		cancel()
		if _, err := body.Read(make([]byte, 1)); err == nil {
			t.Error("verified descriptor ignored later cancellation")
		}
		if _, _, _, err := reader.OpenSourceBody(derived, "default", "browser", "original"); err == nil {
			t.Error("canceled live original capture opened a descriptor")
		}
		for _, source := range []string{"0", "missing"} {
			if rc, _, _, err := reader.OpenSourceBody(ctx, "default", "browser", source); err == nil {
				_ = rc.Close()
				t.Error("opener accepted noncanonical/absent source", source)
			}
		}
	}})
	if page := sourceReadPage(t, objectReadGET(t.Context(), live, f.token, sourceReadPath)); page.Total != 1 {
		t.Fatal("valid original body did not recover", page)
	}
}

func TestSourceBodyReadsActualSQLiteOversizeReceiptAndEmptyBody(t *testing.T) {
	f := newFixture(t)
	off := false
	list := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "browser", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "original", Enabled: &off, Format: "domains", URL: "https://lists.example.test/large"}}}}
	// Disabled sources may retain a real large receipt without making complete
	// DNS compilation parse it. The browser still enforces its own 64MiB cap.
	file, err := os.CreateTemp(t.TempDir(), "large")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if err := file.Truncate(maxSourceReadBytes + 1); err != nil {
		t.Fatal(err)
	}
	entry, _, err := f.builder.opts.Lists.Put(list.Spec.Sources[0].URL, file, listfetcher.PutOptions{})
	if err != nil || entry.Size != maxSourceReadBytes+1 {
		t.Fatal("actual oversize cache entry missing", entry, err)
	}
	if _, err := f.store.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	var stored public.BlockList
	if err := f.store.Get(t.Context(), store.KeyOf(list), &stored); err != nil {
		t.Fatal(err)
	}
	stored.Status.Sources = []public.SourceStatus{{Name: "original", Digest: entry.Digest}}
	if err := f.store.UpdateStatus(t.Context(), &stored); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	reader, err := NewSourceBodyReads(f.builder.opts.Lists)
	if err != nil {
		t.Fatal(err)
	}
	server := sourceReadHTTP(t, f, NewObjectReads(), reader)
	diagnosticWant(t, objectReadGET(t.Context(), server, f.token, sourceReadPath).diagnosticResponse, http.StatusServiceUnavailable)
	list.Spec.Sources[0].Enabled = nil
	storedPtr := sourceReadPut(t, f, list, "")
	page := sourceReadPage(t, objectReadGET(t.Context(), server, f.token, sourceReadPath))
	if page.Total != 0 || len(page.Items) != 0 || page.Digest != storedPtr.Status.Sources[0].Digest {
		t.Fatal("empty verified body was confused with missing body", page)
	}
}

func TestSourceBodyReadsActualSQLiteCursorDoesNotPromiseBodyRetention(t *testing.T) {
	f := newFixture(t)
	list := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "browser", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "original", Format: "domains", URL: "https://lists.example.test/retention"}}}}
	sourceReadPut(t, f, list, "alpha.example\nbeta.example\n")
	reader, err := NewSourceBodyReads(f.builder.opts.Lists)
	if err != nil {
		t.Fatal(err)
	}
	server := sourceReadHTTP(t, f, NewObjectReads(), reader)
	first := sourceReadPage(t, objectReadGET(t.Context(), server, f.token, sourceReadPath+"?limit=1"))
	if first.Continue == "" {
		t.Fatal("fixture did not return a body cursor")
	}
	lease, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	base := lease.Bundle().Identity().Base
	lease.Release()
	f.builder.opts.Lists.Remove(list.Spec.Sources[0].URL)
	// The whole image/compiled rules remain held, but no descriptor/body is
	// retained by a browser cursor. Do not return newer bytes or empty success.
	diagnosticWant(t, objectReadGET(t.Context(), server, f.token, sourceReadPath+"?limit=1&continue="+first.Continue).diagnosticResponse, http.StatusServiceUnavailable)
	lease, err = f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Bundle().Identity().Base.Same(base) {
		t.Fatal("fixture unexpectedly changed the captured public generation")
	}
	lease.Release()
	if _, _, err := f.builder.opts.Lists.Put(list.Spec.Sources[0].URL, strings.NewReader("alpha.example\nbeta.example\n"), listfetcher.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	second := sourceReadPage(t, objectReadGET(t.Context(), server, f.token, sourceReadPath+"?limit=1&continue="+first.Continue))
	if len(second.Items) != 1 || second.Items[0].Value != "beta.example" || second.Continue != "" {
		t.Fatal("exact receipt recovery did not preserve pagination", second)
	}
}

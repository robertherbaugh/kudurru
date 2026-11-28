package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/auth/requestlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func requestAdmissionOnly(before, after *replication.Image) bool {
	if before == nil || after == nil || after.Revision != before.Revision+1 || after.ObjectResourceVersion != before.ObjectResourceVersion || !after.Term.Same(before.Term) {
		return false
	}
	a, b := before.Payload, after.Payload
	strip := func(rows []replication.Entry) ([]replication.Entry, []byte) {
		out := []replication.Entry{}
		var counter []byte
		for _, row := range rows {
			if row.Key == requestlimit.Key {
				counter = row.Data
			} else {
				out = append(out, row)
			}
		}
		return out, counter
	}
	var old, next []byte
	a.Runtime, old = strip(a.Runtime)
	b.Runtime, next = strip(b.Runtime)
	return len(next) > 0 && !bytes.Equal(old, next) && reflect.DeepEqual(a, b)
}

func TestServingHTTPRequestBudgetsAcrossAdaptersAndReplicaRestart(t *testing.T) {
	f := newServingBrowserFixture(t)
	adapter := func() http.Handler {
		h, err := newServingHTTP(servingHTTPOptions{Runtime: f.s, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
		if err != nil {
			t.Fatal(err)
		}
		h.opts.ProviderHandler = h.CapturedBrowserHandler()
		return h.Middleware(h.Handler())
	}
	servers := []http.Handler{f.server, adapter()}
	call := func(i int, method, path string, want int) {
		t.Helper()
		r := httptest.NewRequest(method, "http://kudurru.test/api/v1"+path, strings.NewReader(`{}`))
		r.RemoteAddr = "192.0.2.41:1234"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		servers[i].ServeHTTP(w, r)
		expectServingHTTPStatus(t, w, want)
		if want == 429 && (w.Header().Get("Retry-After") == "" || w.Header().Get("Location") != "") {
			t.Fatal("locked request escaped admission")
		}
	}
	for i := range 5 {
		call(i%2, "GET", "/auth/oidc/corp/start", 302)
	}
	servers[1] = adapter()
	call(1, "GET", "/auth/oidc/corp/start", 429)
	// Invalid callbacks are charged before cookie interpretation or provider I/O.
	before := f.op.requests.Load()
	for i := range 10 {
		call(i%2, "GET", "/auth/oidc/corp/callback", 401)
	}
	servers[1] = adapter()
	call(1, "GET", "/auth/oidc/corp/callback", 429)
	for i := range 5 {
		call(i%2, "POST", "/auth/cli/corp/prepare", 400)
	}
	servers[1] = adapter()
	call(1, "POST", "/auth/cli/corp/prepare", 429)
	if f.op.requests.Load() != before {
		t.Fatal("invalid requests reached token endpoint")
	}
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	find := func(image *replication.Image) []byte {
		for _, row := range image.Payload.Runtime {
			if row.Key == requestlimit.Key {
				return row.Data
			}
		}
		return nil
	}
	if len(find(image)) == 0 || bytes.Contains(find(image), []byte("192.0.2.41")) || requestlimit.ValidateImage(image) != nil {
		t.Fatal("private replicated budget missing or malformed")
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "follower.db")
	open := func() *sqlite.Store {
		st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: f.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "request-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	follower := open()
	t.Cleanup(func() { _ = follower.Close() })
	if err := follower.Enroll(t.Context(), f.s.opts.Store.Authority().Term); err != nil {
		t.Fatal(err)
	}
	install := captureServingReplica(t, follower)
	source, err := f.s.opts.Store.ReadGeneration(install)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(install, *source)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := follower.InstallReplica(install, validated, replication.InstallOptions{ExpectedTerm: source.Term})
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	follower = open()
	after, err := follower.ReadGeneration(t.Context())
	if err != nil || !after.Generation().Same(installed) || !bytes.Equal(find(source), find(after)) {
		t.Fatal("follower restart lost request budget", err)
	}
	entered := false
	err = follower.Command(t.Context(), follower.Authority().Term, func(context.Context, *sqlite.CommandTx) error { entered = true; return nil })
	if err == nil || entered {
		t.Fatal("follower admitted local request charge")
	}
}

func TestServingHTTPRequestAdmissionPrecedesFirstCaptureAndRetainsWriter(t *testing.T) {
	for _, mode := range []string{"cancel", "same-fence-replacement", "success"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingHTTPFixture(t, false)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			r := httptest.NewRequest("GET", "http://kudurru.test/api/v1/auth/oidc/corp/start", nil).WithContext(ctx)
			r.RemoteAddr = "192.0.2.42:1"
			w := httptest.NewRecorder()
			entered := false
			f.h.requestAdmissionMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				entered = true
				if _, err := servinghttp.Identity(r.Context()); err == nil {
					t.Fatal("charge already held an authentication lease")
				}
				deadline, _ := r.Context().Deadline()
				original, _ := ctx.Deadline()
				if !deadline.Equal(original) {
					t.Fatal("admission extended original deadline")
				}
				writer := f.s.primary.writer.active.Load()
				switch mode {
				case "cancel":
					cancel()
				case "same-fence-replacement":
					replacement := &primaryWriterTerm{ctx: writer.ctx, token: writer.token, fence: writer.fence}
					f.s.primary.writer.active.Store(replacement)
					defer f.s.primary.writer.active.Store(writer)
					if _, release, err := f.s.primary.writer.bind(r.Context(), writer.fence); err == nil {
						release()
						t.Fatal("adopted same-fence replacement")
					}
				}
				err := f.h.checkRequestAdmission(r, requestlimit.Start)
				if (err == nil) != (mode == "success") {
					t.Fatal("original admission lifetime/writer not retained", err)
				}
				if mode == "success" && f.h.checkRequestAdmission(r, requestlimit.Start) == nil {
					t.Fatal("request admission reused")
				}
			})).ServeHTTP(w, r)
			if !entered {
				t.Fatal("actual counter command not completed", w.Code)
			}
			image, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range image.Payload.Runtime {
				found = found || row.Key == requestlimit.Key
			}
			if !found {
				t.Fatal("postcommit cancellation refunded charge")
			}
		})
	}
}

func TestServingHTTPRequestAdmissionMissingWriterRefuses(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.h.opts.Runtime = &servingRuntime{opts: f.s.opts, publisher: f.s.publisher}
	entered := false
	h := f.h.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { entered = true }))
	r := httptest.NewRequest("GET", "http://kudurru.test/api/v1/auth/oidc/corp/start", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if entered || w.Code != 503 {
		t.Fatal("missing writer reached authentication capture", w.Code)
	}
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("missing writer changed request budget", err)
	}
}

// TestServingHTTPRequestAdmissionWaitsForItsOwnPublication holds the node's
// publication of a sign-in request's own lane charge for longer than the
// adapter's capture wait (#275). The charge is a commit of the request itself,
// made before the adapter captures, so no publication served before it is
// eligible for the request. Such a request was refused with 503
// generation_pending after servingHTTPCaptureWait, which no sign-in route
// declares, and on a node whose preparation outlasts that wait every resend
// charged and committed again and was refused the same way. The request must
// instead wait for the publication of its charge and then answer as the route
// does.
func TestServingHTTPRequestAdmissionWaitsForItsOwnPublication(t *testing.T) {
	for _, c := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"start", "GET", "/auth/oidc/corp/start", "", 302},
		{"callback", "GET", "/auth/oidc/corp/callback", "", 401},
		{"cli", "POST", "/auth/cli/corp/prepare", "{}", 400},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newServingBrowserFixture(t)
			awaitServingWakesPublished(t, f.s)
			before := f.s.opts.Store.Authority().Installed.Revision
			// The barrier lets the node prepare the charge's generation but
			// not publish it until it is released.
			_, unpin, err := f.s.publisher.Pin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			pinned := true
			defer func() {
				if pinned {
					unpin()
				}
			}()
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() { answered <- f.call(t, c.method, c.path, c.body, "", nil, "") }()
			awaitServingRuntime(t, f.s, func() bool { return f.s.opts.Store.Authority().Installed.Revision > before })
			select {
			case w := <-answered:
				t.Fatalf("answered %d %s while the publication of its own charge was held", w.Code, strings.TrimSpace(w.Body.String()))
			case <-time.After(servingHTTPCaptureWait + time.Second):
			}
			unpin()
			pinned = false
			select {
			case w := <-answered:
				expectServingHTTPStatus(t, w, c.want)
			case <-time.After(10 * time.Second):
				t.Fatal("no answer after the publication of the charge was released")
			}
		})
	}
}

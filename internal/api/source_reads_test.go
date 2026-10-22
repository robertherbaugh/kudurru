package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"k8s.io/apimachinery/pkg/types"
)

type sourceBodyFixture struct {
	open   func(context.Context, string, string, string) (io.ReadCloser, string, int64, error)
	calls  int
	closed int
}

func (f *sourceBodyFixture) OpenSourceBody(ctx context.Context, ns, name, source string) (io.ReadCloser, string, int64, error) {
	f.calls++
	return f.open(ctx, ns, name, source)
}

type sourceFixtureBody struct {
	io.Reader
	close func()
}

func (b sourceFixtureBody) Close() error { b.close(); return nil }

func sourceTestDigest(body string) string {
	hash := sha256.Sum256([]byte(body))
	return hex.EncodeToString(hash[:])
}

func sourceTestList() *public.BlockList {
	return &public.BlockList{ObjectMeta: objectMeta("default", "captured"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "original", Format: "domains", Inline: []string{"alpha.example", "beta.example", "gamma.example"}, Exclude: []string{"beta.example", "gone.example"}}}}}
}

func sourceTestPrepare(t *testing.T, objects *objectReadFixture, bl *public.BlockList) {
	t.Helper()
	snapshot, err := zonecache.Prepare(zonecache.PrepareOptions{}, []store.Object{bl}, nil)
	if err != nil {
		t.Fatal(err)
	}
	objects.objects = snapshot.Objects
}

func newSourceReadFixture(t *testing.T, bl *public.BlockList, body string) (*harness, *objectReadFixture, *sourceBodyFixture) {
	t.Helper()
	bl.UID, bl.ResourceVersion = types.UID("original-uid"), "original-opaque-rv"
	h, objects := newObjectReadFixture(t, bl)
	fixture := &sourceBodyFixture{}
	fixture.open = func(_ context.Context, ns, name, source string) (io.ReadCloser, string, int64, error) {
		if ns != "default" || name != "captured" || source != "original" {
			t.Fatal("opener received an uncaptured key/source", ns, name, source)
		}
		return sourceFixtureBody{strings.NewReader(body), func() { fixture.closed++ }}, sourceTestDigest(body), int64(len(body)), nil
	}
	h.srv.opts.SourceBodyReads = fixture
	return h, objects, fixture
}

const sourceTestPath = "/api/v1/blocklists/default/captured/sources/original/entries"

func TestCapturedSourceInlineStructuredAndBoundCursor(t *testing.T) {
	bl := sourceTestList()
	h, objects, bodies := newSourceReadFixture(t, bl, "")
	var first, second SourceEntries
	r := h.do(http.MethodGet, sourceTestPath+"?limit=1&q=EXAMPLE", "viewer", nil).want(http.StatusOK)
	r.json(&first)
	if r.Header().Get("ETag") != bl.ResourceVersion || len(first.Items) != 1 || first.Items[0].Value != "alpha.example" || first.Continue == "" || first.Fetched != nil || first.Stale || first.Digest != "" || !reflect.DeepEqual(first.StaleExcludes, []string{"gone.example"}) {
		t.Fatal("incorrect original inline page", first, r.Header())
	}
	path := "/api/v1/blocklists/default/captured/sources/0/entries?limit=1&q=" + url.QueryEscape(" example ") + "&continue=" + first.Continue
	h.do(http.MethodGet, path, "viewer", nil).want(http.StatusOK).json(&second)
	if len(second.Items) != 1 || second.Items[0].Value != "beta.example" || second.Items[0].Enabled || second.Total != 3 || second.Matched != 3 || second.Excluded != 1 || second.Continue == "" {
		t.Fatal("canonical name/query or excluded page differs", second)
	}
	for _, query := range []string{"limit=2&q=example", "limit=1&q=alpha", "limit=1&q=example&enabled=false"} {
		h.do(http.MethodGet, sourceTestPath+"?"+query+"&continue="+first.Continue, "viewer", nil).want(http.StatusConflict)
	}
	for _, mutate := range []func(*public.BlockList){
		func(b *public.BlockList) { b.ResourceVersion = "new-rv" },
		func(b *public.BlockList) { b.UID = "recreated" },
		func(b *public.BlockList) { b.Spec.Sources[0].Exclude = nil },
		func(b *public.BlockList) { b.Spec.Sources[0].Subdomains = true },
		func(b *public.BlockList) { b.Spec.Sources[0].Inline[0] = "changed.example" },
	} {
		changed := bl.DeepCopy()
		mutate(changed)
		sourceTestPrepare(t, objects, changed)
		h.do(http.MethodGet, path, "viewer", nil).want(http.StatusConflict)
	}
	sourceTestPrepare(t, objects, bl)
	objects.base.Revision++
	h.do(http.MethodGet, path, "viewer", nil).want(http.StatusConflict)
	objects.base.Revision--
	for _, raw := range []string{"not-token", encodeCursor("1"), strings.Repeat("a", maxObjectReadCursor+1), base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"v":1}`))} {
		h.do(http.MethodGet, sourceTestPath+"?continue="+raw, "viewer", nil).want(http.StatusBadRequest)
	}
	if bodies.calls != 0 {
		t.Fatal("inline read opened disk")
	}
	off := false
	bl.Spec.Sources[0] = public.ListSource{Name: "original", Entries: []public.ListEntry{{Type: public.EntryWildcard, Value: "example"}, {Type: public.EntryRegex, Value: "ads.*", Enabled: &off}}}
	sourceTestPrepare(t, objects, bl)
	var entries SourceEntries
	h.do(http.MethodGet, sourceTestPath+"?enabled=false", "viewer", nil).want(http.StatusOK).json(&entries)
	if entries.Kind != "entries" || entries.Total != 2 || entries.Excluded != 1 || len(entries.Items) != 1 || entries.Items[0].Value != "/ads.*/" || entries.Items[0].Enabled {
		t.Fatal("structured switches/rules differ", entries)
	}
}

func TestCapturedSourceExternalReceiptCanonicalIdentityAndClose(t *testing.T) {
	body := "alpha.example\nbeta.example\ngamma.example\n"
	bl := sourceTestList()
	bl.Spec.Sources[0].Inline = nil
	bl.Spec.Sources[0].URL = "https://example.test/list"
	bl.Status.Sources = []public.SourceStatus{{Name: "original", Digest: sourceTestDigest(body)}}
	h, objects, bodies := newSourceReadFixture(t, bl, body)
	var out SourceEntries
	h.do(http.MethodGet, "/api/v1/blocklists/default/captured/sources/0/entries?limit=1", "viewer", nil).want(http.StatusOK).json(&out)
	if out.Digest != sourceTestDigest(body) || out.Stale || out.Fetched != nil || out.Kind != "url" || out.Total != 3 || out.Excluded != 1 || bodies.closed != 1 {
		t.Fatal("external metadata was fabricated or descriptor leaked", out, bodies)
	}
	changed := bl.DeepCopy()
	changed.Status.Sources[0].Digest = strings.Repeat("c", 64)
	sourceTestPrepare(t, objects, changed)
	before := bodies.calls
	h.do(http.MethodGet, sourceTestPath+"?limit=1&continue="+out.Continue, "viewer", nil).want(http.StatusConflict)
	if bodies.calls != before {
		t.Fatal("mismatched cursor opened body")
	}
	sourceTestPrepare(t, objects, bl)
	for _, fault := range []string{"error", "wrong-digest", "oversize", "nil", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			request := h.req(http.MethodGet, sourceTestPath, "viewer", nil)
			ctx, cancel := context.WithCancel(request.Context())
			defer cancel()
			bodies.open = func(context.Context, string, string, string) (io.ReadCloser, string, int64, error) {
				rc := sourceFixtureBody{strings.NewReader(body), func() { bodies.closed++ }}
				switch fault {
				case "error":
					return rc, "", 0, errors.New("unavailable")
				case "wrong-digest":
					return rc, strings.Repeat("c", 64), int64(len(body)), nil
				case "oversize":
					return rc, sourceTestDigest(body), maxCapturedSourceBytes + 1, nil
				case "nil":
					return nil, sourceTestDigest(body), int64(len(body)), nil
				default:
					cancel()
					return rc, sourceTestDigest(body), int64(len(body)), nil
				}
			}
			closed := bodies.closed
			h.serve(request.WithContext(ctx)).want(http.StatusServiceUnavailable)
			if fault != "nil" && bodies.closed != closed+1 {
				t.Fatal("failure leaked body")
			}
		})
	}
}

func TestCapturedSourceAuthorizationMissingAmbiguousAndLegacy(t *testing.T) {
	bl := sourceTestList()
	h, objects, bodies := newSourceReadFixture(t, bl, "")
	h.seed(store.ManagerAPI, &public.BlockList{ObjectMeta: objectMeta("default", "live-only"), Spec: bl.Spec})
	h.do(http.MethodGet, sourceTestPath+"?continue=malformed", "zoneadmin", nil).want(http.StatusForbidden)
	h.do(http.MethodGet, "/api/v1/blocklists/default/live-only/sources/0/entries", "viewer", nil).want(http.StatusNotFound)
	h.do(http.MethodGet, "/api/v1/blocklists/default/captured/sources/missing/entries?continue=malformed", "viewer", nil).want(http.StatusNotFound)
	for _, fault := range []string{"duplicate-source", "duplicate-status", "missing-receipt", "bad-receipt", "mixed", "oversize", "rpz"} {
		changed := bl.DeepCopy()
		code := http.StatusServiceUnavailable
		switch fault {
		case "duplicate-source":
			changed.Spec.Sources = append(changed.Spec.Sources, changed.Spec.Sources[0])
		case "duplicate-status":
			changed.Status.Sources = []public.SourceStatus{{Name: "original"}, {Name: "original"}}
		case "missing-receipt", "bad-receipt":
			changed.Spec.Sources[0].Inline = nil
			changed.Spec.Sources[0].URL = "https://example.test/list"
			if fault == "bad-receipt" {
				changed.Status.Sources = []public.SourceStatus{{Name: "original", Digest: strings.Repeat("A", 64)}}
			}
		case "mixed":
			changed.Spec.Sources[0].URL = "https://example.test/list"
		case "oversize":
			changed.Spec.Sources[0].Inline = []string{strings.Repeat("a", (1<<20)+1)}
		default:
			changed.Spec.Sources[0].Format = "rpz"
			code = http.StatusUnprocessableEntity
		}
		sourceTestPrepare(t, objects, changed)
		h.do(http.MethodGet, sourceTestPath, "viewer", nil).want(code)
	}
	if bodies.calls != 0 {
		t.Fatal("unauthorized/malformed/inline source accessed disk")
	}
	sourceTestPrepare(t, objects, bl)
	for _, err := range []error{serving.ErrReleased, serving.ErrComponents, context.Canceled} {
		objects.err = err
		h.do(http.MethodGet, sourceTestPath, "viewer", nil).want(http.StatusServiceUnavailable)
	}
	objects.err = nil
	h.srv.opts.SourceBodyReads = nil
	h.do(http.MethodGet, sourceTestPath, "viewer", nil).want(http.StatusServiceUnavailable)
	h.srv.opts.ObjectReads = nil
	h.srv.opts.SourceBodyReads = bodies
	h.do(http.MethodGet, sourceTestPath, "viewer", nil).want(http.StatusServiceUnavailable)
	h.srv.opts.SourceBodyReads = nil
	h.do(http.MethodGet, "/api/v1/blocklists/default/live-only/sources/0/entries", "viewer", nil).want(http.StatusOK)
}

func TestCapturedSourceScanAdmissionPreservesRetryAfter(t *testing.T) {
	h, _, bodies := newSourceReadFixture(t, sourceTestList(), "")
	for range cap(h.srv.bodyScans) {
		h.srv.bodyScans <- struct{}{}
	}
	defer func() {
		for len(h.srv.bodyScans) > 0 {
			<-h.srv.bodyScans
		}
	}()
	r := h.do(http.MethodGet, sourceTestPath, "viewer", nil).want(http.StatusServiceUnavailable)
	if r.Header().Get("Retry-After") != "5" || bodies.calls != 0 {
		t.Fatal("scan admission lost retry header or opened body", r.Header(), bodies.calls)
	}
}

package auditlog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/audit/audittest"
	"github.com/robertherbaugh/kudurru/internal/audit/spool"
)

var env struct {
	cfg    *rest.Config
	client client.Client
	skip   string
}

func TestMain(m *testing.M) {
	ctrllog.SetLogger(logr.Discard())
	klog.SetLogger(logr.Discard())
	os.Exit(run(m))
}

func run(m *testing.M) int {
	assets := assetsDir()
	if assets == "" {
		env.skip = "envtest binaries not found: run `make envtest` or set KUBEBUILDER_ASSETS"
		return m.Run()
	}
	_, file, _, _ := goruntime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	b, err := os.ReadFile(filepath.Join(root, "config", "crd", "audit", "audit.kudurru.io_auditlogs.yaml")) //nolint:gosec // the repository's CRD
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(b, &crd); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	e := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{&crd}, BinaryAssetsDirectory: assets}
	args := e.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := e.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "auditlog: envtest:", err)
		return 1
	}
	defer func() { _ = e.Stop() }()
	c, err := client.New(cfg, client.Options{Scheme: testScheme()})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	env.cfg, env.client = cfg, c
	return m.Run()
}

// assetsDir is $KUBEBUILDER_ASSETS or the newest bin/k8s/<version> that
// `make envtest` installed.
func assetsDir() string {
	if d := os.Getenv("KUBEBUILDER_ASSETS"); d != "" {
		return d
	}
	_, file, _, _ := goruntime.Caller(0)
	dirs, _ := filepath.Glob(filepath.Join(filepath.Dir(file), "..", "..", "..", "bin", "k8s", "*-"+goruntime.GOOS+"-"+goruntime.GOARCH))
	sort.Strings(dirs)
	if len(dirs) == 0 {
		return ""
	}
	return dirs[len(dirs)-1]
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = auditv1.AddToScheme(s)
	return s
}

var nsSeq atomic.Int64

// namespace creates a fresh namespace for one test.
func namespace(t *testing.T) string {
	t.Helper()
	if env.skip != "" {
		t.Skip(env.skip)
	}
	name := fmt.Sprintf("audit-%d", nsSeq.Add(1))
	if err := env.client.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		t.Fatal(err)
	}
	return name
}

func openSpool(t *testing.T, node string) *spool.Spool {
	t.Helper()
	s, err := spool.Open(context.Background(), spool.Options{Path: filepath.Join(t.TempDir(), spool.FileName), Node: node})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ship sends everything src holds that the target does not.
func ship(t *testing.T, tgt *Target, src *spool.Spool) {
	t.Helper()
	ctx := context.Background()
	after, err := tgt.Cursor(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := src.ReadAfter(ctx, after, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) > 0 {
		if err := tgt.Append(ctx, src, evs); err != nil {
			t.Fatal(err)
		}
	}
}

// store is spool → target → reader as one audittest.Store: Read ships
// first.
type store struct {
	t   *testing.T
	sp  *spool.Spool
	tgt *Target
	rd  *Reader
}

func (s *store) Record(ctx context.Context, ev audit.Event) error { return s.sp.Record(ctx, ev) }
func (s *store) Read(ctx context.Context, q audit.Query) ([]audit.Event, error) {
	ship(s.t, s.tgt, s.sp)
	return s.rd.Read(ctx, q)
}

func TestConformance(t *testing.T) {
	audittest.Run(t, func(t *testing.T) audittest.Store {
		ns := namespace(t)
		// Small batches, so pages cross batch boundaries.
		return &store{t: t, sp: openSpool(t, "node-a"), tgt: &Target{Client: env.client, Namespace: ns, MaxBytes: 2048},
			rd: &Reader{Client: env.client, Namespace: ns, ListTTL: time.Nanosecond}}
	})
}

func event(ts time.Time, actor, kind, name string) audit.Event {
	return audit.Event{Time: ts, Actor: audit.Actor{Kind: "User", Name: actor}, Action: strings.ToLower(kind) + ".update",
		Resource: audit.Resource{Kind: kind, Name: name}, Result: audit.ResultSuccess, After: []byte(`{"spec":{"x":1}}`),
		Request: audit.Request{ID: "r1", IP: "10.0.0.1"}}
}

// TestNodesMerge: every replica reads every node's batches in one time
// order, pages across them with the opaque cursor, and names each event
// uniquely.
func TestNodesMerge(t *testing.T) {
	ns := namespace(t)
	ctx := context.Background()
	key, _ := SealKey([]byte("0123456789abcdef0123456789abcdef"))
	tgt := &Target{Client: env.client, Namespace: ns, SealKey: key, MaxBytes: 4096}
	base := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	a, b := openSpool(t, "kudurru-0"), openSpool(t, "kudurru-1")
	for i := range 20 {
		sp := a
		if i%2 == 1 {
			sp = b
		}
		if err := sp.Record(ctx, event(base.Add(time.Duration(i)*time.Second), "alice", "DNSZone", fmt.Sprintf("z%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	ship(t, tgt, a)
	ship(t, tgt, b)
	ship(t, tgt, a) // nothing new: idempotent

	var list auditv1.AuditLogList
	if err := env.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) < 2 {
		t.Fatalf("%d batches", len(list.Items))
	}
	for _, rd := range []*Reader{{Client: env.client, Namespace: ns}, {Client: env.client, Namespace: ns, BodyCache: 1}} {
		for _, desc := range []bool{false, true} {
			q := audit.Query{Limit: 3, Desc: desc}
			var got []audit.Event
			uids := map[string]bool{}
			for {
				page, err := rd.Read(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				for _, ev := range page {
					if uids[ev.UID] || ev.UID == "" {
						t.Fatalf("uid %q twice or empty", ev.UID)
					}
					uids[ev.UID] = true
				}
				got = append(got, page...)
				if len(page) < q.Limit {
					break
				}
				q.After(page[len(page)-1])
			}
			if len(got) != 20 {
				t.Fatalf("desc=%v: %d events", desc, len(got))
			}
			for i, ev := range got {
				want := i
				if desc {
					want = 19 - i
				}
				if ev.Resource.Name != fmt.Sprintf("z%d", want) {
					t.Fatalf("desc=%v: event %d is %s", desc, i, ev.Resource.Name)
				}
				wantNode := "kudurru-0"
				if want%2 == 1 {
					wantNode = "kudurru-1"
				}
				if ev.Node != wantNode || ev.Request.IP != "10.0.0.1" || string(ev.After) != `{"spec":{"x":1}}` {
					t.Fatalf("event %+v", ev)
				}
			}
		}
	}
	if err := Verify(ctx, env.client, ns, [][]byte{key}); err != nil {
		t.Fatal(err)
	}
	// An id cursor of a node store is not a cursor of this store.
	if _, err := (&Reader{Client: env.client, Namespace: ns}).Read(ctx, audit.Query{AfterID: 3}); err != audit.ErrBadCursor {
		t.Fatalf("id cursor: %v", err)
	}
}

// TestPendingShowsAtOnce: the serving node's unshipped events are read
// with the batches, once.
func TestPendingShowsAtOnce(t *testing.T) {
	ns := namespace(t)
	ctx := context.Background()
	sp := openSpool(t, "kudurru-0")
	tgt := &Target{Client: env.client, Namespace: ns}
	now := time.Now().UTC()
	for i := range 3 {
		if err := sp.Record(ctx, event(now.Add(time.Duration(i)*time.Millisecond), "alice", "Policy", "p")); err != nil {
			t.Fatal(err)
		}
	}
	ship(t, tgt, sp)
	if err := sp.Record(ctx, event(now.Add(time.Second), "bob", "Policy", "p")); err != nil {
		t.Fatal(err)
	}
	// What audit.Shipper.Pending returns once Run has read the cursor.
	pending := func(ctx context.Context, n int) ([]audit.Event, error) {
		after, err := tgt.Cursor(ctx, sp)
		if err != nil {
			return nil, err
		}
		return sp.ReadAfter(ctx, after, n)
	}
	rd := &Reader{Client: env.client, Namespace: ns, Pending: pending, PendingSource: sp.SourceID()}
	evs, err := rd.Read(ctx, audit.Query{Desc: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 || evs[0].Actor.Name != "bob" {
		t.Fatalf("events %+v", evs)
	}
	// Shipped and still pending (a race between the two): read once.
	all, _ := sp.ReadAfter(ctx, 0, 10)
	rd.Pending = func(context.Context, int) ([]audit.Event, error) { return all, nil }
	ship(t, tgt, sp)
	rd.ListTTL = time.Nanosecond
	if evs, err = rd.Read(ctx, audit.Query{}); err != nil || len(evs) != 4 {
		t.Fatalf("%d events: %v", len(evs), err)
	}
}

// TestBudgetStubs: beyond the hourly budget events keep their link but
// lose documents, reason and request.
func TestBudgetStubs(t *testing.T) {
	ns := namespace(t)
	ctx := context.Background()
	sp := openSpool(t, "kudurru-0")
	tgt := &Target{Client: env.client, Namespace: ns, HourlyBudget: 2}
	for i := range 4 {
		if err := sp.Record(ctx, event(time.Now().Add(time.Duration(i)*time.Millisecond), "mallory", "blocking", "")); err != nil {
			t.Fatal(err)
		}
	}
	ship(t, tgt, sp)
	evs, err := (&Reader{Client: env.client, Namespace: ns}).Read(ctx, audit.Query{})
	if err != nil || len(evs) != 4 {
		t.Fatalf("%d events: %v", len(evs), err)
	}
	if evs[1].After == nil || evs[2].After != nil || evs[3].Request.IP != "" || evs[3].Actor.Name != "mallory" {
		t.Fatalf("stubs %+v", evs)
	}
	if err := Verify(ctx, env.client, ns, nil); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyDetectsChanges: an edit by someone without the encryption key
// breaks the seal; without a seal, the event hash; a missing batch breaks
// the link.
func TestVerifyDetectsChanges(t *testing.T) {
	ns := namespace(t)
	ctx := context.Background()
	key, _ := SealKey([]byte("0123456789abcdef0123456789abcdef"))
	sp := openSpool(t, "kudurru-0")
	tgt := &Target{Client: env.client, Namespace: ns, SealKey: key, MaxBytes: 600}
	for i := range 6 {
		if err := sp.Record(ctx, event(time.Now().Add(time.Duration(i)*time.Millisecond), "alice", "DNSZone", fmt.Sprintf("z%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	ship(t, tgt, sp)
	if err := Verify(ctx, env.client, ns, [][]byte{key}); err != nil {
		t.Fatal(err)
	}
	var list auditv1.AuditLogList
	if err := env.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Spec.FirstID < list.Items[j].Spec.FirstID })
	if len(list.Items) < 3 {
		t.Fatalf("%d batches; want at least 3", len(list.Items))
	}
	mid := list.Items[1]
	mid.Spec.Events[0].Reason = "nothing to see"
	if err := env.client.Update(ctx, &mid); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, env.client, ns, [][]byte{key}); err == nil || !strings.Contains(err.Error(), "seal") {
		t.Fatalf("sealed edit: %v", err)
	}
	if err := Verify(ctx, env.client, ns, nil); err == nil || !strings.Contains(err.Error(), "was changed") {
		t.Fatalf("unsealed edit: %v", err)
	}
	if err := env.client.Delete(ctx, &mid); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, env.client, ns, [][]byte{key}); err == nil || !strings.Contains(err.Error(), "does not follow") {
		t.Fatalf("missing batch: %v", err)
	}
}

func TestPrune(t *testing.T) {
	ns := namespace(t)
	ctx := context.Background()
	sp := openSpool(t, "kudurru-0")
	tgt := &Target{Client: env.client, Namespace: ns, MaxBytes: 600}
	old := time.Now().Add(-500 * 24 * time.Hour)
	for i := range 3 {
		if err := sp.Record(ctx, event(old.Add(time.Duration(i)*time.Second), "alice", "DNSZone", "old")); err != nil {
			t.Fatal(err)
		}
	}
	ship(t, tgt, sp)
	if err := sp.Record(ctx, event(time.Now(), "alice", "DNSZone", "new")); err != nil {
		t.Fatal(err)
	}
	ship(t, tgt, sp)
	rd := &Reader{Client: env.client, Namespace: ns}
	n, err := rd.Prune(ctx, time.Now().Add(-400*24*time.Hour), env.client)
	if err != nil || n == 0 {
		t.Fatalf("pruned %d: %v", n, err)
	}
	evs, err := rd.Read(ctx, audit.Query{})
	if err != nil || len(evs) != 1 || evs[0].Resource.Name != "new" {
		t.Fatalf("after prune %+v: %v", evs, err)
	}
	// The cursor of a source survives the prune of its older batches.
	if c, err := tgt.Cursor(ctx, sp); err != nil || c != 4 {
		t.Fatalf("cursor %d: %v", c, err)
	}
	if err := Verify(ctx, env.client, ns, nil); err != nil {
		t.Fatalf("verify after prune: %v", err)
	}
}

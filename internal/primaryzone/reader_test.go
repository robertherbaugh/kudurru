package primaryzone

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type readStore struct {
	store.Store
	get  func(context.Context, store.Key, store.Object) error
	list func(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error)
}

type commandReadStore struct {
	*sqlite.Store
	entered, writerStarted chan struct{}
}

func (s commandReadStore) Command(ctx context.Context, term replication.Term, fn func(context.Context, *sqlite.CommandTx) error) error {
	return s.Store.Command(ctx, term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		close(s.entered)
		select {
		case <-s.writerStarted:
		case <-ctx.Done():
			return ctx.Err()
		}
		return fn(ctx, tx)
	})
}

func TestSQLiteReaderPagedCutAgainstConcurrentReplacement(t *testing.T) {
	st := openSQLite(t, filepath.Join(t.TempDir(), "state.db"))
	z := desiredZone()
	z.UID, z.ResourceVersion, z.CreationTimestamp = "", "", metav1.Time{}
	z = apply(t, st, z, "api").(*public.DNSZone)
	id := zonekinds.ZoneID{ClusterID: st.Info().ClusterID, ObjectName: z.Name, UID: string(z.UID), Apex: "example."}
	for i := 0; i < 5; i++ {
		rs := desiredSet(fmt.Sprintf("source-%d", i), endpoint(fmt.Sprintf("host-%d", i), "192.0.2.1"))
		rs.UID, rs.ResourceVersion, rs.CreationTimestamp = "", "", metav1.Time{}
		apply(t, st, rs, "api")
	}
	reader := NewReader(st, ReaderOptions{PageSize: 1, MaxSources: 5})
	before := st.Authority()
	first, err := reader.ReadDesired(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, st.Authority()) {
		t.Fatal("read-only command advanced durable authority/revision")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wrapped := commandReadStore{Store: st, entered: make(chan struct{}), writerStarted: make(chan struct{})}
	written := make(chan error, 1)
	go func() {
		select {
		case <-wrapped.entered:
		case <-ctx.Done():
			written <- ctx.Err()
			return
		}
		close(wrapped.writerStarted)
		written <- st.Command(ctx, before.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			nextZone := z.DeepCopy()
			nextZone.Spec.DefaultTTL++
			if _, err := tx.Apply(ctx, nextZone, store.ApplyOptions{Manager: "api", ExpectedResourceVersion: z.ResourceVersion}); err != nil {
				return err
			}
			for _, rs := range first.RecordSets {
				next := rs.DeepCopy()
				next.Spec.Endpoints[0].Targets = []string{"192.0.2.2"}
				if _, err := tx.Apply(ctx, next, store.ApplyOptions{Manager: "api", ExpectedResourceVersion: rs.ResourceVersion}); err != nil {
					return err
				}
			}
			return nil
		})
	}()
	old, err := NewReader(wrapped, ReaderOptions{PageSize: 1, MaxSources: 5}).ReadDesired(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	latest, err := reader.ReadDesired(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []struct {
		in   *zonestate.DesiredInputs
		addr string
		ttl  uint32
	}{{old, "192.0.2.1", z.Spec.DefaultTTL}, {latest, "192.0.2.2", z.Spec.DefaultTTL + 1}} {
		if !cut.in.Complete || len(cut.in.RecordSets) != 5 || cut.in.Zone.Spec.DefaultTTL != cut.ttl {
			t.Fatal("paginated read mixed zone/source generations")
		}
		for _, rs := range cut.in.RecordSets {
			if rs.Spec.Endpoints[0].Targets[0] != cut.addr {
				t.Fatal("paginated read mixed source generations")
			}
		}
	}
}

func (s readStore) Get(ctx context.Context, key store.Key, into store.Object) error {
	if s.get != nil {
		return s.get(ctx, key, into)
	}
	return s.Store.Get(ctx, key, into)
}
func (s readStore) List(ctx context.Context, kind store.Kind, o store.ListOptions) (store.ObjectList, error) {
	if s.list != nil {
		return s.list(ctx, kind, o)
	}
	return s.Store.List(ctx, kind, o)
}
func (s readStore) Info() store.Info { info := s.Store.Info(); info.Driver = "kube"; return info }
func TestReaderIncompleteAndChangedIntentNeverMeansPrune(t *testing.T) {
	st := openSQLite(t, filepath.Join(t.TempDir(), "state.db"))
	z := desiredZone()
	z.UID = ""
	z.ResourceVersion = ""
	z.CreationTimestamp = metav1.Time{}
	z = apply(t, st, z, "api").(*public.DNSZone)
	id := zonekinds.ZoneID{ClusterID: st.Info().ClusterID, ObjectName: z.Name, UID: string(z.UID), Apex: "example."}
	fault := errors.New("interrupted list")
	for _, tc := range []struct {
		name    string
		wrapper func() readStore
		want    error
	}{
		{"pagination interrupted", func() readStore {
			calls := 0
			return readStore{Store: st, list: func(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error) {
				calls++
				if calls == 1 {
					return store.ObjectList{Continue: "snapshot-token"}, nil
				}
				return store.ObjectList{}, fault
			}}
		}, fault},
		{"repeated token", func() readStore {
			return readStore{Store: st, list: func(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error) {
				return store.ObjectList{Continue: "repeated"}, nil
			}}
		}, zonestate.ErrCapacity},
		{"zone intent changed", func() readStore {
			calls := 0
			return readStore{Store: st, get: func(ctx context.Context, key store.Key, into store.Object) error {
				if err := st.Get(ctx, key, into); err != nil {
					return err
				}
				calls++
				if calls == 2 {
					into.(*public.DNSZone).Spec.DefaultTTL++
				}
				return nil
			}}
		}, zonestate.ErrConflict},
		{"zone UID recreated", func() readStore {
			calls := 0
			return readStore{Store: st, get: func(ctx context.Context, key store.Key, into store.Object) error {
				if err := st.Get(ctx, key, into); err != nil {
					return err
				}
				calls++
				if calls == 2 {
					into.(*public.DNSZone).UID = "new-uid"
				}
				return nil
			}}
		}, zonestate.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if in, err := NewReader(tc.wrapper(), ReaderOptions{}).ReadDesired(context.Background(), id); !errors.Is(err, tc.want) || in != nil {
				t.Fatalf("incomplete/changed read %+v %v", in, err)
			}
		})
	}
	// Status RVs are equality tokens; a different post-list RV with identical
	// intent must not substitute for the actually captured pre-list revision.
	wrapper := readStore{Store: st, get: func(ctx context.Context, key store.Key, into store.Object) error {
		if err := st.Get(ctx, key, into); err != nil {
			return err
		}
		into.SetResourceVersion("different-status-rv")
		return nil
	}}
	in, err := NewReader(wrapper, ReaderOptions{}).ReadDesired(context.Background(), id)
	if err != nil || !in.Complete {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewReader(st, ReaderOptions{}).ReadDesired(canceled, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation %v", err)
	}
}

func TestReaderPagesBoundBeforeCopyAndEmptyContinuationLoop(t *testing.T) {
	st := openSQLite(t, filepath.Join(t.TempDir(), "state.db"))
	z := desiredZone()
	z.UID = ""
	z.ResourceVersion = ""
	z.CreationTimestamp = metav1.Time{}
	z = apply(t, st, z, "api").(*public.DNSZone)
	id := zonekinds.ZoneID{ClusterID: st.Info().ClusterID, ObjectName: z.Name, UID: string(z.UID), Apex: "example."}
	calls := 0
	wrapped := readStore{Store: st, list: func(_ context.Context, _ store.Kind, o store.ListOptions) (store.ObjectList, error) {
		calls++
		if o.Limit > 32 {
			t.Fatalf("page exceeds fixed bound %d", o.Limit)
		}
		return store.ObjectList{Continue: fmt.Sprintf("empty-%d", calls)}, nil
	}}
	if _, err := NewReader(wrapped, ReaderOptions{MaxSources: 2}).ReadDesired(context.Background(), id); !errors.Is(err, zonestate.ErrCapacity) || calls > 3 {
		t.Fatalf("empty pages unbounded %d %v", calls, err)
	}
	rs := desiredSet("oversize", endpoint("host", "192.0.2.1"))
	rs.Annotations = map[string]string{"padding": strings.Repeat("x", 1<<20)}
	calls = 0
	wrapped.list = func(_ context.Context, _ store.Kind, o store.ListOptions) (store.ObjectList, error) {
		calls++
		if o.Limit != 1 {
			t.Fatalf("remaining-byte page cap %d", o.Limit)
		}
		return store.ObjectList{Items: []store.Object{rs}}, nil
	}
	if in, err := NewReader(wrapped, ReaderOptions{MaxBytes: 2048}).ReadDesired(context.Background(), id); !errors.Is(err, zonestate.ErrCapacity) || in != nil || calls != 1 {
		t.Fatalf("byte cap %v %v %d", in, err, calls)
	}
}

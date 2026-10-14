// Package primaryzone adapts actual desired sources into one complete committed
// primary generation. It is never invoked on the DNS query path.
package primaryzone

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReaderOptions bounds the complete read. Policy captures root-verified active
// credential versions and dependencies, outside the query path.
type ReaderOptions struct {
	MaxSources int
	MaxBytes   int64
	PageSize   int
	Timeout    time.Duration
	Policy     func(context.Context, *public.DNSZone) (*zonestate.Policy, error)
	Targets    func(zonekinds.ZoneID) []zonestate.DesiredTarget
}

// Reader reads actual store objects; watch events only schedule its callers.
type Reader struct {
	store   store.Store
	options ReaderOptions
}

// NewReader applies finite defaults. SQLite pages within one fenced transaction;
// Kubernetes continuation tokens preserve one consistent paginated list snapshot.
func NewReader(st store.Store, o ReaderOptions) *Reader {
	if o.MaxSources <= 0 {
		o.MaxSources = 100000
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 64 << 20
	}
	if o.PageSize <= 0 {
		o.PageSize = 1000
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	return &Reader{store: st, options: o}
}

// ReadDesired validates the UID and relevant zone intent around the complete list.
func (r *Reader) ReadDesired(ctx context.Context, id zonekinds.ZoneID) (*zonestate.DesiredInputs, error) {
	ctx, cancel := context.WithTimeout(ctx, r.options.Timeout)
	defer cancel()
	if r.options.MaxSources > 1_000_000 {
		return nil, zonestate.ErrCapacity
	}
	if r.store == nil {
		return nil, zonestate.ErrInvalid
	}
	var targets []zonestate.DesiredTarget
	if r.options.Targets != nil {
		targets = r.options.Targets(id)
		if len(targets) > 4096 {
			return nil, zonestate.ErrCapacity
		}
	}
	targets = append([]zonestate.DesiredTarget(nil), targets...)
	var out *zonestate.DesiredInputs
	var err error
	if handled, result, readErr := r.readSQLite(ctx, id, targets); handled {
		out, err = result, readErr
	} else {
		out, err = r.readCut(ctx, id, targets, r.store, r.store.Info().Driver == "kube")
	}
	if err != nil {
		return nil, err
	}
	// Keep the actually captured pre-list revision, never substitute a newer RV.
	if r.options.Policy != nil {
		out.Policy, err = r.options.Policy(ctx, out.Zone.DeepCopy())
		if err != nil {
			return nil, err
		}
		if out.Policy == nil {
			return nil, zonestate.ErrInvalid
		}
	}
	out.Complete = true
	return out, nil
}

// desiredReadStore is deliberately narrower than Store: a transaction-bound
// adapter exposes only Get/List and cannot accidentally reenter driver writes.
type desiredReadStore interface {
	Get(context.Context, store.Key, store.Object) error
	List(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error)
}

func (r *Reader) readCut(ctx context.Context, id zonekinds.ZoneID, targets []zonestate.DesiredTarget, st desiredReadStore, paginated bool) (*zonestate.DesiredInputs, error) {
	var before public.DNSZone
	key := store.Key{Kind: "DNSZone", Name: id.ObjectName}
	if err := st.Get(ctx, key, &before); err != nil {
		return nil, err
	}
	if err := matchesZone(&before, id); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(&before)
	if err != nil {
		return nil, err
	}
	left := r.options.MaxBytes
	if int64(len(raw)) > left {
		return nil, zonestate.ErrCapacity
	}
	left -= int64(len(raw))
	digest, err := zoneReadDigest(&before)
	if err != nil {
		return nil, err
	}
	out := &zonestate.DesiredInputs{Zone: before.DeepCopy(), Targets: targets}
	seenTokens := map[string]bool{}
	seenObjects := map[store.Key]bool{}
	next := ""
	count := 0
	pages := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pages++
		if pages > r.options.MaxSources+1 || count >= r.options.MaxSources {
			return nil, zonestate.ErrCapacity
		}
		// One oversized object can exceed the remaining byte budget, but no page
		// ever allocates MaxSources objects before the accumulator is checked.
		bytePage := max(int64(1), left/int64(store.MaxObjectBytes))
		limit := min(r.options.PageSize, 32, r.options.MaxSources-count)
		if int64(limit) > bytePage {
			limit = int(bytePage) //nolint:gosec // G115: 1 <= bytePage < limit <= 32 in this branch.
		}
		page, err := st.List(ctx, "DNSRecordSet", store.ListOptions{Limit: limit, Continue: next})
		if err != nil {
			return nil, err
		}
		if len(page.Items) > limit {
			return nil, zonestate.ErrCapacity
		}
		for _, obj := range page.Items {
			count++
			rs, ok := obj.(*public.DNSRecordSet)
			if !ok || rs == nil || seenObjects[store.KeyOf(rs)] {
				return nil, zonestate.ErrInvalid
			}
			seenObjects[store.KeyOf(rs)] = true
			raw, err := json.Marshal(rs)
			if err != nil {
				return nil, err
			}
			if int64(len(raw)) > left {
				return nil, zonestate.ErrCapacity
			}
			left -= int64(len(raw))
			if rs.Spec.ZoneRef.Name == id.ObjectName {
				out.RecordSets = append(out.RecordSets, rs.DeepCopy())
			}
		}
		if page.Continue == "" {
			break
		}
		if !paginated || len(page.Continue) > 4096 || seenTokens[page.Continue] {
			return nil, zonestate.ErrCapacity
		}
		seenTokens[page.Continue] = true
		next = page.Continue
	}
	var after public.DNSZone
	if err := st.Get(ctx, key, &after); err != nil {
		return nil, err
	}
	current, err := zoneReadDigest(&after)
	if err != nil {
		return nil, err
	}
	if matchesZone(&after, id) != nil || current != digest {
		return nil, zonestate.ErrConflict
	}
	return out, nil
}

func matchesZone(z *public.DNSZone, id zonekinds.ZoneID) error {
	apex, err := dynamicupdate.CanonicalName(z.Spec.ZoneName)
	if err != nil || z.Name != id.ObjectName || string(z.UID) != id.UID || z.Spec.Type != public.ZoneTypePrimary || apex != id.Apex || z.ResourceVersion == "" {
		return zonestate.ErrInvalid
	}
	return nil
}

// IntentDigest excludes status/RV churn, but includes UID, selector labels,
// ownership and creation time used for selection. Derived status is not intent.
func IntentDigest(obj store.Object) (string, error) {
	b, err := intentObject(obj)
	if err != nil {
		return "", err
	}
	return zonekinds.Digest(b), nil
}
func intentObject(obj store.Object) ([]byte, error) {
	if obj == nil {
		return nil, zonestate.ErrInvalid
	}
	metadata := struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace,omitempty"`
		UID       string            `json:"uid"`
		Created   metav1.Time       `json:"creationTimestamp"`
		Labels    map[string]string `json:"labels,omitempty"`
		Manager   string            `json:"manager"`
	}{obj.GetName(), obj.GetNamespace(), string(obj.GetUID()), obj.GetCreationTimestamp(), obj.GetLabels(), store.ManagedBy(obj)}
	var spec any
	switch o := obj.(type) {
	case *public.DNSZone:
		spec = o.Spec
	case *public.DNSRecordSet:
		spec = o.Spec
	default:
		return nil, fmt.Errorf("unsupported desired source %T", obj)
	}
	return json.Marshal(struct {
		Metadata any `json:"metadata"`
		Spec     any `json:"spec"`
	}{metadata, spec})
}

// NS address status is a frozen serving dependency, not desired intent. A
// changing dependency invalidates this read without claiming API supersession.
func zoneReadDigest(z *public.DNSZone) (string, error) {
	intent, err := IntentDigest(z)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Intent    string
		Addresses []string
	}{intent, z.Status.NameserverAddresses})
	return zonekinds.Digest(raw), err
}

// SourceToken captures a persisted desired object's equality token and relevant digest.
func SourceToken(obj store.Object) (zonestate.SourceToken, error) {
	digest, err := IntentDigest(obj)
	if err != nil {
		return zonestate.SourceToken{}, err
	}
	return zonestate.SourceToken{ID: store.KeyOf(obj).String(), UID: string(obj.GetUID()), ResourceVersion: obj.GetResourceVersion(), IntentDigest: digest}, nil
}

package externaldns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	zonemodel "github.com/robertherbaugh/kudurru/internal/zone"
)

var endpointResource = schema.GroupVersionResource{Group: "externaldns.k8s.io", Version: "v1alpha1", Resource: "dnsendpoints"}

const (
	crdOwner      = "externaldns.kudurru.io/source-uid"
	crdSource     = "externaldns.kudurru.io/source-name"
	crdVersion    = "externaldns.kudurru.io/source-version"
	crdManager    = "external-dns-crd-"
	crdTimeout    = 30 * time.Second
	crdMaxObjects = 1024
	crdMaxBytes   = 16 << 20
)

// CRDOptions configures the optional Kubernetes compatibility source. Source
// CRs are read-only; only derived DNSRecordSets in their namespace are managed.
type CRDOptions struct {
	Store        store.Store
	Validator    *validator.Validator
	Namespace    string
	ZoneSelector map[string]string
	// Admit rechecks the original term and current integration entitlement.
	Admit  func(context.Context) error
	Logger *slog.Logger
}

// CRDController reconciles actual DNSEndpoint observations through the ordinary
// Store ownership/CAS path. Run belongs to an elected term, never the query path.
// Kubernetes offers no transaction across a source, zone and destination: the
// checks below bound stale work but do not claim linearizable cross-object writes.
type CRDController struct {
	opts   CRDOptions
	client dynamic.Interface
}

// NewCRDController requires the actual kube driver's guarded RESTConfig.
func NewCRDController(cfg *rest.Config, opts CRDOptions) (*CRDController, error) {
	if cfg == nil || opts.Store == nil || opts.Validator == nil || opts.Admit == nil {
		return nil, errors.New("externaldns: incomplete CRD source options")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	opts.ZoneSelector = maps.Clone(opts.ZoneSelector)
	config := rest.CopyConfig(cfg)
	// Bound a single list page/watch response before decoding it. The whole
	// retained source list has its own aggregate byte/object limits below.
	config.Wrap(func(rt http.RoundTripper) http.RoundTripper { return crdTransport{rt} })
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &CRDController{opts: opts, client: client}, nil
}

type crdTransport struct{ http.RoundTripper }
type crdBody struct {
	io.Reader
	io.Closer
}

func (t crdTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.RoundTripper.RoundTrip(req)
	if err == nil && res != nil && res.Body != nil {
		res.Body = crdBody{Reader: io.LimitReader(res.Body, 2<<20), Closer: res.Body}
	}
	return res, err
}

func (c *CRDController) sources() dynamic.ResourceInterface {
	return c.client.Resource(endpointResource).Namespace(c.opts.Namespace)
}

// Run uses a complete initial list, watch invalidations and periodic relists.
// Events only wake a fresh bounded reconciliation; their payload grants no write.
func (c *CRDController) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		rctx, cancel := context.WithTimeout(ctx, crdTimeout)
		rv, err := c.reconcile(rctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.opts.Logger.Warn("DNSEndpoint reconciliation refused", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
		wctx, stop := context.WithTimeout(ctx, 10*time.Second)
		seconds := int64(10)
		w, err := c.sources().Watch(wctx, metav1.ListOptions{ResourceVersion: rv, TimeoutSeconds: &seconds})
		if err == nil {
			select {
			case <-wctx.Done():
			case <-w.ResultChan():
			}
			w.Stop()
		} else {
			select {
			case <-wctx.Done():
			case <-time.After(time.Second):
			}
		}
		stop()
	}
	return nil
}

func (c *CRDController) list(ctx context.Context) ([]unstructured.Unstructured, string, error) {
	var all []unstructured.Unstructured
	var rv, cont string
	bytes := 0
	for {
		page, err := c.sources().List(ctx, metav1.ListOptions{Limit: 64, Continue: cont})
		if err != nil {
			return nil, "", err
		}
		if page.GetResourceVersion() == "" || (rv != "" && rv != page.GetResourceVersion()) {
			return nil, "", store.ErrConflict
		}
		rv = page.GetResourceVersion()
		for _, item := range page.Items {
			data, err := item.MarshalJSON()
			if err != nil {
				return nil, "", err
			}
			bytes += len(data)
			if bytes > crdMaxBytes || len(all) == crdMaxObjects {
				return nil, "", store.ErrTooLarge
			}
			all = append(all, item)
		}
		cont = page.GetContinue()
		if cont == "" {
			return all, rv, nil
		}
	}
}

func (c *CRDController) records(ctx context.Context) ([]*v1alpha1.DNSRecordSet, error) {
	var out []*v1alpha1.DNSRecordSet
	cont := ""
	for {
		page, err := c.opts.Store.List(ctx, "DNSRecordSet", store.ListOptions{Namespace: c.opts.Namespace, Limit: 128, Continue: cont, LabelSelector: map[string]string{"externaldns.kudurru.io/source": "crd"}})
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Items {
			rs, ok := obj.(*v1alpha1.DNSRecordSet)
			if !ok || len(out) == crdMaxObjects*8 {
				return nil, store.ErrTooLarge
			}
			out = append(out, rs)
		}
		cont = page.Continue
		if cont == "" {
			return out, nil
		}
	}
}

func (c *CRDController) zones(ctx context.Context) ([]*v1alpha1.DNSZone, error) {
	var out []*v1alpha1.DNSZone
	cont, count, size := "", 0, 0
	for {
		page, err := c.opts.Store.List(ctx, "DNSZone", store.ListOptions{Limit: 128, Continue: cont})
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Items {
			count++
			data, err := json.Marshal(obj)
			if err != nil {
				return nil, err
			}
			size += len(data)
			if count > crdMaxObjects || size > crdMaxBytes {
				return nil, store.ErrTooLarge
			}
			z, ok := obj.(*v1alpha1.DNSZone)
			if !ok {
				return nil, errors.New("externaldns: invalid zone observation")
			}
			if z.DeletionTimestamp == nil && z.Spec.Type == v1alpha1.ZoneTypePrimary && matchLabels(z.Labels, c.opts.ZoneSelector) {
				out = append(out, z)
			}
		}
		cont = page.Continue
		if cont == "" {
			return out, nil
		}
	}
}

func (c *CRDController) reconcile(ctx context.Context) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		return "", errors.New("externaldns: reconciliation must be bounded")
	}
	if err := c.opts.Admit(ctx); err != nil {
		return "", err
	}
	items, rv, err := c.list(ctx)
	if err != nil {
		return "", err
	}
	zones, err := c.zones(ctx)
	if err != nil {
		return "", err
	}
	current, err := c.records(ctx)
	if err != nil {
		return "", err
	}
	bySource := map[string][]*v1alpha1.DNSRecordSet{}
	for _, rs := range current {
		if ownedCRD(rs) {
			bySource[rs.Annotations[crdOwner]] = append(bySource[rs.Annotations[crdOwner]], rs)
		}
	}
	var firstError error
	seen := map[string]bool{}
	for i := range items {
		source := &items[i]
		uid := string(source.GetUID())
		if uid == "" || source.GetResourceVersion() == "" {
			return rv, errors.New("externaldns: source identity missing")
		}
		seen[uid] = true
		desired, err := c.project(ctx, source, zones)
		if err != nil {
			// Invalid current data withdraws only this source's previous rows.
			// It cannot preserve stale addresses by remaining invalid forever.
			c.opts.Logger.Warn("invalid DNSEndpoint omitted", "namespace", source.GetNamespace(), "name", source.GetName(), "error", err)
			desired = nil
		}
		if err := c.apply(ctx, source, desired, bySource[uid], zones); err != nil {
			// One conflicting source cannot starve unrelated source owners.
			// The refused source operation ends; it is not replayed here.
			if firstError == nil {
				firstError = err
			}
		}
	}
	for uid, rows := range bySource {
		if seen[uid] {
			continue
		}
		for _, rs := range rows {
			// A complete list alone is not a delete capability. Confirm absence
			// or replacement of the exact source UID immediately before cleanup.
			source, err := c.client.Resource(endpointResource).Namespace(rs.Namespace).Get(ctx, rs.Annotations[crdSource], metav1.GetOptions{})
			if !apierrors.IsNotFound(err) && (err != nil || string(source.GetUID()) == uid) {
				if err != nil {
					return rv, err
				}
				return rv, store.ErrConflict
			}
			if err := c.remove(ctx, rs, uid); err != nil {
				return rv, err
			}
		}
	}
	return rv, firstError
}

func sourceManager(uid string) string { return store.ManagerIntegration(crdManager + uid) }

func ownedCRD(rs *v1alpha1.DNSRecordSet) bool {
	uid, name := rs.Annotations[crdOwner], rs.Annotations[crdSource]
	if uid == "" || name == "" || store.ManagedBy(rs) != sourceManager(uid) || len(rs.OwnerReferences) != 1 {
		return false
	}
	owner := rs.OwnerReferences[0]
	if string(owner.UID) != uid || owner.Name != name || owner.Kind != "DNSEndpoint" || owner.APIVersion != endpointResource.GroupVersion().String() {
		return false
	}
	sum := sha256.Sum256([]byte(uid + "/" + rs.Spec.ZoneRef.Name))
	return rs.Name == "externaldns-"+hex.EncodeToString(sum[:20])
}

func (c *CRDController) project(ctx context.Context, source *unstructured.Unstructured, zones []*v1alpha1.DNSZone) (map[string]*v1alpha1.DNSRecordSet, error) {
	out := map[string]*v1alpha1.DNSRecordSet{}
	if source.GetDeletionTimestamp() != nil {
		return out, nil
	}
	raw, found, err := unstructured.NestedSlice(source.Object, "spec", "endpoints")
	if err != nil || !found || len(raw) > 1000 {
		return nil, errors.New("invalid endpoint count or shape")
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var endpoints []*Endpoint
	if err := json.Unmarshal(data, &endpoints); err != nil {
		return nil, err
	}
	for _, endpoint := range endpoints {
		if endpoint == nil {
			return nil, errors.New("null endpoint")
		}
		ep := toEndpoint(endpoint)
		if err := zonemodel.ValidateEndpoint(ep); err != nil {
			return nil, err
		}
		z := zoneFor(ep.DNSName, zones)
		if z == nil {
			continue
		}
		// An ambiguous apex never chooses one owner by list ordering.
		for _, other := range zones {
			if other.Name != z.Name && normName(other.Spec.ZoneName) == normName(z.Spec.ZoneName) {
				return nil, errors.New("ambiguous zone apex")
			}
		}
		key := string(source.GetUID()) + "/" + z.Name
		sum := sha256.Sum256([]byte(key))
		name := "externaldns-" + hex.EncodeToString(sum[:20])
		rs := out[name]
		if rs == nil {
			rs = &v1alpha1.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: source.GetNamespace(), Labels: maps.Clone(source.GetLabels()),
				Annotations:     map[string]string{crdOwner: string(source.GetUID()), crdSource: source.GetName(), crdVersion: source.GetResourceVersion()},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: endpointResource.GroupVersion().String(), Kind: "DNSEndpoint", Name: source.GetName(), UID: source.GetUID()}},
			}, Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: z.Name}}}
			if rs.Labels == nil {
				rs.Labels = map[string]string{}
			}
			rs.Labels["externaldns.kudurru.io/source"] = "crd"
			store.SetManagedBy(rs, sourceManager(string(source.GetUID())))
			selector, err := zonemodel.NewRecordSetSelector(z)
			if err != nil {
				return nil, err
			}
			if !selector.Admits(rs) {
				continue
			}
			out[name] = rs
		}
		rs.Spec.Endpoints = append(rs.Spec.Endpoints, ep)
	}
	if len(out) > 8 {
		return nil, store.ErrTooLarge
	}
	for _, rs := range out {
		if !zonemodel.RecordSetValid(rs) {
			return nil, errors.New("invalid or duplicated endpoint")
		}
		if err := c.opts.Validator.Validate(ctx, rs, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c *CRDController) currentSource(ctx context.Context, original *unstructured.Unstructured) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now, err := c.client.Resource(endpointResource).Namespace(original.GetNamespace()).Get(ctx, original.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	if now.GetUID() != original.GetUID() || now.GetResourceVersion() != original.GetResourceVersion() {
		return store.ErrConflict
	}
	return c.opts.Admit(ctx)
}

func (c *CRDController) apply(ctx context.Context, source *unstructured.Unstructured, desired map[string]*v1alpha1.DNSRecordSet, current []*v1alpha1.DNSRecordSet, zones []*v1alpha1.DNSZone) error {
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rs := desired[name]
		var old v1alpha1.DNSRecordSet
		err := c.opts.Store.Get(ctx, store.KeyOf(rs), &old)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		absent := errors.Is(err, store.ErrNotFound)
		manager := sourceManager(string(source.GetUID()))
		if !absent && (!ownedCRD(&old) || store.ManagedBy(&old) != manager || old.Annotations[crdOwner] != string(source.GetUID())) {
			return &store.ErrManagedBy{Key: store.KeyOf(rs), Owner: store.ManagedBy(&old)}
		}
		if !absent && reflect.DeepEqual(old.Spec, rs.Spec) && reflect.DeepEqual(old.Labels, rs.Labels) && old.Annotations[crdVersion] == rs.Annotations[crdVersion] {
			continue
		}
		for _, original := range zones {
			if original.Name != rs.Spec.ZoneRef.Name {
				continue
			}
			var now v1alpha1.DNSZone
			if err := c.opts.Store.Get(ctx, store.KeyOf(original), &now); err != nil {
				return err
			}
			if now.UID != original.UID || now.ResourceVersion != original.ResourceVersion {
				return store.ErrConflict
			}
		}
		if err := c.currentSource(ctx, source); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		opts := store.ApplyOptions{Manager: manager, CreateOnly: absent}
		if !absent {
			opts.ExpectedResourceVersion = old.ResourceVersion
		}
		if _, err := c.opts.Store.Apply(ctx, rs, opts); err != nil {
			return err
		}
	}
	for _, rs := range current {
		if desired[rs.Name] != nil {
			continue
		}
		if err := c.currentSource(ctx, source); err != nil {
			return err
		}
		if err := c.remove(ctx, rs, string(source.GetUID())); err != nil {
			return err
		}
	}
	return nil
}

func (c *CRDController) remove(ctx context.Context, rs *v1alpha1.DNSRecordSet, uid string) error {
	manager := sourceManager(uid)
	if !ownedCRD(rs) || store.ManagedBy(rs) != manager || rs.Annotations[crdOwner] != uid {
		return fmt.Errorf("externaldns: unowned cleanup candidate")
	}
	if err := c.opts.Admit(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := c.opts.Store.Delete(ctx, store.KeyOf(rs), store.DeleteOptions{Manager: manager, ExpectedUID: string(rs.UID), ExpectedResourceVersion: rs.ResourceVersion})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

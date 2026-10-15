// Package bootstrap seeds a fresh store on first boot (docs/architecture.md
// §6.4): the root zone "." forwarding to the upstream group "default"
// (DNSZone "root"), ClientGroup "default" matching every client and Policy
// "default" applying to it, all written with Manager "bootstrap".
//
// Seeding is idempotent and never overrides the operator: it runs only
// when the store holds no object of the seeded kinds at all (a genuinely
// first boot), and even then it only creates objects that are absent. Once
// anything else exists (objects from files, the API, a restore or a
// previous boot) Seed does nothing, so a deleted default stays deleted.
// File-managed objects take over a seeded object by declaring it (see
// internal/objects); the API may do the same.
//
// The build-time starter blocklist (§6.4) plugs in through
// Options.StarterBlockList; EmbeddedStarterBlockList provides the list
// embedded by internal/listdata (Steven Black's ad-hoc list, MIT), which
// is created on first boot only, like every other seed.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/listdata"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// Names of the seeded objects.
const (
	RootZoneName           = "root"
	DefaultClientGroupName = "default"
	DefaultPolicyName      = "default"
	// DefaultUpstreamGroup is the upstream group the root zone forwards to;
	// config.Validate requires upstreams.groups.default to exist.
	DefaultUpstreamGroup = "default"
	// DefaultNamespace holds the namespaced seeds (ClientGroup, Policy,
	// BlockList) unless Options.Namespace says otherwise.
	DefaultNamespace = "default"
)

// StarterBlockListFunc returns the build-time starter blocklist (an inline
// or embedded-source BlockList), or nil when the build carries none. When
// it returns a list, Seed creates it and Policy "default" references it.
type StarterBlockListFunc func(ctx context.Context) (*v1alpha1.BlockList, error)

// EmbeddedStarterBlockList is the StarterBlockListFunc for the list
// embedded in the binary: BlockList "starter" with one inline domains
// source built from internal/listdata (provenance and licence in
// internal/listdata/stevenblack). Pass it as Options.StarterBlockList.
func EmbeddedStarterBlockList(ctx context.Context) (*v1alpha1.BlockList, error) {
	return listdata.StarterBlockList(ctx)
}

// Options configures Seed.
type Options struct {
	// ContinueFirstBoot resumes a call that already returned FirstBoot=true
	// but failed during writes. Only retain this flag for that startup
	// attempt; later restarts must still respect operator deletions.
	ContinueFirstBoot bool
	// Namespace of the namespaced seeds; default "default". In Kubernetes
	// pass the release namespace.
	Namespace string
	// UpstreamGroup the root zone forwards to; default "default".
	UpstreamGroup string
	// Validator, when set, validates (and defaults) every seed before it is
	// written, exactly like any other write.
	Validator *validator.Validator
	// StarterBlockList is the seam for the embedded starter list; nil
	// seeds no list.
	StarterBlockList StarterBlockListFunc
	// Logger receives one line per created object; default slog.Default().
	Logger *slog.Logger
}

// ResolveNamespace uses the release namespace for new Kubernetes installs.
// Keep an existing default/default policy (including operator edits) as the
// fallback until a default policy exists in the release namespace. This
// preserves legacy installs without moving or overwriting their objects.
func ResolveNamespace(ctx context.Context, st store.Store, namespace string) (string, error) {
	if namespace == DefaultNamespace {
		return namespace, nil
	}
	for _, ns := range []string{namespace, DefaultNamespace} {
		key := store.Key{Kind: "Policy", Namespace: ns, Name: DefaultPolicyName}
		var pol v1alpha1.Policy
		switch err := st.Get(ctx, key, &pol); {
		case err == nil:
			return ns, nil
		case !errors.Is(err, store.ErrNotFound):
			return "", fmt.Errorf("bootstrap: resolve namespace: get %s: %w", key, err)
		}
	}
	return namespace, nil
}

// NamespaceFromObjects applies ResolveNamespace's precedence to a local.db
// object set without requiring an apiserver read.
func NamespaceFromObjects(objects []store.Object, namespace string) string {
	for _, ns := range []string{namespace, DefaultNamespace} {
		for _, obj := range objects {
			if p, ok := obj.(*v1alpha1.Policy); ok && p.Namespace == ns && p.Name == DefaultPolicyName {
				return ns
			}
		}
	}
	return namespace
}

func (o *Options) setDefaults() {
	if o.Namespace == "" {
		o.Namespace = DefaultNamespace
	}
	if o.UpstreamGroup == "" {
		o.UpstreamGroup = DefaultUpstreamGroup
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Result reports what Seed did.
type Result struct {
	// FirstBoot is true when the store held no object of the seeded kinds.
	FirstBoot bool
	// Created lists the seeds written by this call.
	Created []store.Key
	// Present lists seeds that already existed (a concurrent replica or an
	// interrupted earlier first boot) and were left untouched.
	Present []store.Key
}

// Objects returns the seed set in creation order: the starter BlockList
// (when the hook provides one), ClientGroup "default", Policy "default"
// and DNSZone "root".
func Objects(ctx context.Context, opts Options) ([]store.Object, error) {
	opts.setDefaults()
	var objs []store.Object
	policy := &v1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultPolicyName, Namespace: opts.Namespace},
		Spec:       v1alpha1.PolicySpec{ClientGroupRefs: []string{DefaultClientGroupName}},
	}
	if opts.StarterBlockList != nil {
		bl, err := opts.StarterBlockList(ctx)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: starter blocklist: %w", err)
		}
		if bl != nil {
			bl = bl.DeepCopy()
			if bl.Name == "" {
				return nil, errors.New("bootstrap: starter blocklist has no name")
			}
			if bl.Namespace == "" {
				bl.Namespace = opts.Namespace
			}
			objs = append(objs, bl)
			policy.Spec.BlockListRefs = []string{bl.Name}
		}
	}
	objs = append(objs,
		&v1alpha1.ClientGroup{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultClientGroupName, Namespace: opts.Namespace},
			Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{
				{CIDR: "0.0.0.0/0", Name: "All IPv4 clients"},
				{CIDR: "::/0", Name: "All IPv6 clients"},
			}},
		},
		policy,
		&v1alpha1.DNSZone{
			ObjectMeta: metav1.ObjectMeta{Name: RootZoneName},
			Spec: v1alpha1.DNSZoneSpec{
				ZoneName: ".",
				Type:     v1alpha1.ZoneTypeForward,
				Forward:  &v1alpha1.ForwardSpec{UpstreamGroup: opts.UpstreamGroup},
			},
		},
	)
	return objs, nil
}

// Seed creates the first-boot objects when the store holds no object of
// their kinds. It is safe to call on every start and from several replicas
// at once (identical writes by the same manager).
func Seed(ctx context.Context, st store.Store, opts Options) (Result, error) {
	opts.setDefaults()
	objs, err := Objects(ctx, opts)
	if err != nil {
		return Result{}, err
	}
	if !opts.ContinueFirstBoot {
		seen := map[store.Kind]bool{}
		for _, o := range objs {
			kind := store.KindOf(o)
			if seen[kind] {
				continue
			}
			seen[kind] = true
			page, err := st.List(ctx, kind, store.ListOptions{Limit: 1})
			if err != nil {
				return Result{}, fmt.Errorf("bootstrap: list %s: %w", kind, err)
			}
			if len(page.Items) > 0 {
				return Result{}, nil
			}
		}
	}

	// Validate the whole set before the first write, so a bad seed cannot
	// leave a partial first boot behind.
	if opts.Validator != nil {
		for _, o := range objs {
			if err := opts.Validator.Validate(ctx, o, nil); err != nil {
				return Result{}, fmt.Errorf("bootstrap: %s: %w", store.KeyOf(o), err)
			}
		}
	}

	res := Result{FirstBoot: true}
	for _, o := range objs {
		key := store.KeyOf(o)
		probe := o.DeepCopyObject().(store.Object)
		switch err := st.Get(ctx, key, probe); {
		case err == nil:
			res.Present = append(res.Present, key)
			continue
		case !errors.Is(err, store.ErrNotFound):
			return res, fmt.Errorf("bootstrap: get %s: %w", key, err)
		}
		if _, err := st.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerBootstrap}); err != nil {
			if store.IsManagedBy(err) {
				// Created by another writer between Get and Apply: theirs wins.
				res.Present = append(res.Present, key)
				continue
			}
			return res, fmt.Errorf("bootstrap: apply %s: %w", key, err)
		}
		res.Created = append(res.Created, key)
		opts.Logger.Info("bootstrap: created first-boot default", "object", key.String())
	}
	return res, nil
}

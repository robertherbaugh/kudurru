package main

import (
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// kubeRPZ selects transferred RPZ feeds for the Kubernetes (legacy policy)
// serving path (D390). The SQLite path binds feeds once per complete serving
// bundle; here the policy Index comes from the zone cache, so each query binds
// the BlockList objects of its own policy decision to the node-local feed
// snapshot and the TSIG ring it was acquired with. Every input is an in-memory
// atomic read: the DNS path never reads the store, waits for a leader or does
// network work. A required feed that is missing, rotated, unbound or expired
// stays in the selection as an unavailable sentinel, so it fails closed.
type kubeRPZ struct {
	config *zonecache.Cache
	feeds  *rpz.Manager
	// keys is the published stored-key ring; nil while it is pending or
	// withdrawn, when only unsigned feeds can bind.
	keys     func() *dnsserver.StaticKeyring
	unsigned *dnsserver.StaticKeyring
	bound    atomic.Pointer[kubeRPZBinding]
}

// kubeRPZBinding is one immutable binding of the configured feeds to an exact
// configuration snapshot, feed snapshot and ring. It is rebuilt only when one
// of the three installations changes, never per query.
type kubeRPZBinding struct {
	config    *zonecache.Snapshot
	feeds     *rpz.Snapshot
	keys      *dnsserver.StaticKeyring
	bound     *rpz.BoundSnapshot
	available bool
}

func newKubeRPZ(config *zonecache.Cache, feeds *rpz.Manager, keys func() *dnsserver.StaticKeyring) (*kubeRPZ, error) {
	unsigned, err := dnsserver.NewStaticKeyring()
	if err != nil {
		return nil, err
	}
	return &kubeRPZ{config: config, feeds: feeds, keys: keys, unsigned: unsigned}, nil
}

// capture implements blockingstage.Options.RPZ. The stage pins the result once
// per request and inherits it for synthesized targets.
func (k *kubeRPZ) capture(req *pipeline.Request) (*rpz.Selection, error) {
	d := policy.FromRequest(req)
	if d == nil || d.Selection == nil {
		return nil, nil
	}
	var lists []*v1alpha1.BlockList
	for _, name := range d.Selection.BlockLists() {
		if list := d.List(name).BlockList(); hasRPZSource(list) {
			lists = append(lists, list)
		}
	}
	if len(lists) == 0 {
		return nil, nil
	}
	b := k.binding()
	return b.bound.Select(lists, b.available), nil
}

func (k *kubeRPZ) binding() *kubeRPZBinding {
	config := k.config.Snapshot()
	var feeds *rpz.Snapshot
	if k.feeds != nil {
		feeds = k.feeds.Snapshot()
	}
	var keys *dnsserver.StaticKeyring
	if k.keys != nil {
		keys = k.keys()
	}
	if keys == nil {
		keys = k.unsigned
	}
	if b := k.bound.Load(); b != nil && b.config == config && b.feeds == feeds && b.keys == keys {
		return b
	}
	b := &kubeRPZBinding{config: config, feeds: feeds, keys: keys}
	if config != nil {
		// A stale or unsynced configuration keeps every selected feed required
		// and unavailable, as the feed status reports.
		b.available = config.Authoritative()
		// A binding error leaves bound nil: every selected feed is a sentinel.
		b.bound, _ = feeds.Bind(config.Objects.BlockLists(), keys)
	}
	k.bound.Store(b)
	return b
}

// hasRPZSource reports whether the list has any RPZ-shaped source. It is a
// cheap prefilter only; rpz Select applies the exact enablement rules.
func hasRPZSource(list *v1alpha1.BlockList) bool {
	if list == nil {
		return false
	}
	for i := range list.Spec.Sources {
		if list.Spec.Sources[i].RPZ != nil || list.Spec.Sources[i].Format == "rpz" {
			return true
		}
	}
	return false
}

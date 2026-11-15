package main

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// kubeUpdateAttempts bounds re-evaluation after a lost CAS race. A losing
// attempt never committed, so it is re-planned against fresh data rather
// than replayed; RFC 2136 section 3.7 requires exactly this serialization.
const kubeUpdateAttempts = 8

// kubeUpdateRecordSetPrefix names the one UPDATE-owned DNSRecordSet per zone.
const kubeUpdateRecordSetPrefix = "rfc2136-"

// kubeUpdateStore is the Kubernetes store surface the handler uses.
type kubeUpdateStore interface {
	Get(context.Context, store.Key, store.Object) error
	List(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error)
	Apply(context.Context, store.Object, store.ApplyOptions) (store.Object, error)
	Delete(context.Context, store.Key, store.DeleteOptions) error
}

// kubeUpdate implements RFC 2136 on the Kubernetes driver (F12a, D366).
//
// Every UPDATE reads the zone and all of its record sets from the store, plans
// with the shared dynamicupdate engine (prerequisites, key x owner x type grants
// and ownership reservations for every record another source owns), and commits
// the complete UPDATE-owned record set of the zone in ONE DNSRecordSet with an
// exact resourceVersion CAS (create-only, versioned update or versioned delete).
// Concurrent UPDATEs therefore serialize on that object: a transaction never
// observes another's partial result, and a prerequisite failure writes nothing.
// API, file and integration writers own their own objects; UPDATE never changes
// their records.
type kubeUpdate struct {
	// zones serializes this node's UPDATEs per zone so local concurrency
	// queues instead of racing; CAS still serializes across nodes.
	zones     sync.Map // apex -> *sync.Mutex
	store     kubeUpdateStore
	keys      func() *dnsserver.StaticKeyring
	namespace string
	limits    dynamicupdate.Limits
	now       func() time.Time
	log       *slog.Logger
}

func (u *kubeUpdate) reply(req *pipeline.Request, rcode int) (*pipeline.Response, error) {
	r := pipeline.Reply(req, rcode, pipeline.StatusAuthoritative)
	if r.Msg != nil {
		r.Msg.Authoritative = true
	}
	return r, nil
}

// Handle answers one UPDATE message (pipeline.Next form).
func (u *kubeUpdate) Handle(req *pipeline.Request) (*pipeline.Response, error) {
	if req == nil || req.Msg == nil || req.Ctx == nil {
		return nil, pipeline.ErrServingCapture
	}
	tx, perr := dynamicupdate.Parse(req.Msg, u.limits)
	if perr != nil {
		return u.reply(req, perr.Rcode)
	}
	keys := u.keys()
	if keys == nil {
		return u.reply(req, dns.RcodeServerFailure)
	}
	principal := dynamicupdate.Principal{}
	if verified, ok := dnsserver.VerifiedTSIGFor(req, u.now()); ok {
		identity := verified.Identity()
		principal = dynamicupdate.Principal{Key: identity.Key, Version: identity.CredentialVersion}
	}
	ctx, cancel := context.WithTimeout(req.Ctx, 10*time.Second)
	defer cancel()
	lock, _ := u.zones.LoadOrStore(tx.Apex(), &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	for attempt := 0; attempt < kubeUpdateAttempts; attempt++ {
		if attempt > 0 {
			// Another node won the CAS; back off briefly before re-planning.
			select {
			case <-ctx.Done():
				return u.reply(req, dns.RcodeServerFailure)
			case <-time.After(time.Duration(attempt*25) * time.Millisecond):
			}
		}
		rcode, retry, err := u.attempt(ctx, keys, tx, principal)
		if err != nil {
			u.log.Warn("dynamic update failed", "zone", tx.Apex(), "error", err)
			return u.reply(req, dns.RcodeServerFailure)
		}
		if !retry {
			return u.reply(req, rcode)
		}
	}
	u.log.Warn("dynamic update contended; refusing without a write", "zone", tx.Apex())
	return u.reply(req, dns.RcodeServerFailure)
}

func (u *kubeUpdate) zoneFor(ctx context.Context, apex string) (*public.DNSZone, error) {
	list, err := u.store.List(ctx, "DNSZone", store.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, obj := range list.Items {
		dz, ok := obj.(*public.DNSZone)
		if !ok || dz.Spec.Type != public.ZoneTypePrimary || public.IsDisabled(dz) || !dz.DeletionTimestamp.IsZero() {
			continue
		}
		if name, err := dynamicupdate.CanonicalName(dz.Spec.ZoneName); err == nil && name == apex {
			return dz, nil
		}
	}
	return nil, nil
}

// attempt plans and commits once; retry reports a lost CAS race.
func (u *kubeUpdate) attempt(ctx context.Context, keys *dnsserver.StaticKeyring, tx *dynamicupdate.Transaction, principal dynamicupdate.Principal) (int, bool, error) {
	dz, err := u.zoneFor(ctx, tx.Apex())
	if err != nil {
		return 0, false, err
	}
	if dz == nil {
		return dns.RcodeNotAuth, false, nil
	}
	if dz.Spec.Policy.Update == "" || dz.Spec.Policy.Update == public.AccessDeny {
		return dns.RcodeRefused, false, nil
	}
	policy, err := currentguard.CompileUpdatePolicy(ctx, keys, dz, 0)
	if err != nil {
		return dns.RcodeRefused, false, nil
	}
	list, err := u.store.List(ctx, "DNSRecordSet", store.ListOptions{})
	if err != nil {
		return 0, false, err
	}
	ownedName := kubeUpdateRecordSetPrefix + dz.Name
	var sets []*public.DNSRecordSet
	var owned *public.DNSRecordSet
	for _, obj := range list.Items {
		rs, ok := obj.(*public.DNSRecordSet)
		if !ok || rs.Spec.ZoneRef.Name != dz.Name {
			continue
		}
		sets = append(sets, rs)
		if rs.Name == ownedName && rs.Namespace == u.namespaceFor(dz) {
			owned = rs
		}
	}
	if owned != nil && store.ManagedBy(owned) != store.ManagerDynamicUpdate {
		// A foreign object occupies the UPDATE-owned name; never take it over.
		return dns.RcodeRefused, false, nil
	}
	served := zone.ServedRecordSets(dz, sets)
	built, _ := zone.Build(dz, served, zone.BuildOptions{Now: u.now()})
	if built == nil {
		return dns.RcodeServerFailure, false, nil
	}
	ownedIDs := map[string]bool{}
	if owned != nil {
		for _, rr := range kubeUpdateRRs(dz, owned) {
			if id, err := dynamicupdate.Identity(rr); err == nil {
				ownedIDs[id] = true
			}
		}
	}
	state := dynamicupdate.State{Apex: tx.Apex(), UpdateEnabled: policy.Enabled, AuthEpoch: policy.AuthEpoch, Credentials: policy.Credentials, Grants: policy.Grants}
	reserved := map[[2]any]bool{}
	built.RRsets(func(set *zone.RRset) bool {
		foreign := set.Type == dns.TypeSOA || set.Type == dns.TypeNS && set.Name == tx.Apex()
		for _, rr := range set.RRs {
			state.Records = append(state.Records, dns.Copy(rr))
			if id, err := dynamicupdate.Identity(rr); err != nil || !ownedIDs[id] {
				foreign = true
			}
		}
		if foreign {
			reserved[[2]any{set.Name, set.Type}] = true
			state.Reservations = append(state.Reservations, dynamicupdate.Reservation{Name: set.Name, Type: set.Type, Subtree: set.Type == dns.TypeDNAME, Source: "kubernetes"})
		}
		return true
	})
	result, perr := dynamicupdate.Plan(state, tx, principal, u.now(), u.limits)
	if perr != nil {
		return perr.Rcode, false, nil
	}
	if !result.Changed {
		return dns.RcodeSuccess, false, nil
	}
	endpoints, ok := kubeUpdateEndpoints(result.Records, reserved)
	if !ok {
		return dns.RcodeRefused, false, nil
	}
	err = u.commit(ctx, dz, owned, endpoints)
	if errors.Is(err, store.ErrConflict) {
		return 0, true, nil
	}
	if errors.Is(err, errKubeUpdateSelector) {
		return dns.RcodeRefused, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return dns.RcodeSuccess, false, nil
}

var errKubeUpdateSelector = errors.New("zone recordSetSelector does not admit the UPDATE-owned record set")

// namespaceFor places the UPDATE-owned set where the zone's selector admits it.
func (u *kubeUpdate) namespaceFor(dz *public.DNSZone) string {
	if sel := dz.Spec.RecordSetSelector; sel != nil && len(sel.Namespaces) > 0 && !slices.Contains(sel.Namespaces, u.namespace) {
		return sel.Namespaces[0]
	}
	return u.namespace
}

func (u *kubeUpdate) commit(ctx context.Context, dz *public.DNSZone, owned *public.DNSRecordSet, endpoints []public.Endpoint) error {
	key := store.Key{Kind: "DNSRecordSet", Namespace: u.namespaceFor(dz), Name: kubeUpdateRecordSetPrefix + dz.Name}
	if len(endpoints) == 0 {
		if owned == nil {
			return nil
		}
		return u.store.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerDynamicUpdate, ExpectedUID: string(owned.UID), ExpectedResourceVersion: owned.ResourceVersion})
	}
	next := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: dz.Name}, Endpoints: endpoints}}
	if sel := dz.Spec.RecordSetSelector; sel != nil && sel.LabelSelector != nil && len(sel.LabelSelector.MatchLabels) > 0 {
		next.Labels = maps.Clone(sel.LabelSelector.MatchLabels)
	}
	if s, err := zone.NewRecordSetSelector(dz); err != nil || !s.Admits(next) {
		return errKubeUpdateSelector
	}
	opts := store.ApplyOptions{Manager: store.ManagerDynamicUpdate, CreateOnly: owned == nil}
	if owned != nil {
		next.ResourceVersion = owned.ResourceVersion
		opts.ExpectedResourceVersion = owned.ResourceVersion
	}
	_, err := u.store.Apply(ctx, next, opts)
	return err
}

// kubeUpdateRRs parses an UPDATE-owned set's endpoints into RRs.
func kubeUpdateRRs(dz *public.DNSZone, rs *public.DNSRecordSet) []dns.RR {
	var out []dns.RR
	for _, ep := range rs.Spec.Endpoints {
		owner, err := zone.OwnerName(dz.Spec.ZoneName, ep.DNSName)
		if err != nil {
			continue
		}
		typ, ok := dns.StringToType[strings.ToUpper(ep.RecordType)]
		if !ok {
			continue
		}
		ttl := uint32(ep.RecordTTL) //nolint:gosec // validated non-negative CRD field; bounded by the API schema
		for _, target := range ep.Targets {
			if rr, err := zone.ParseTarget(owner, typ, ttl, target); err == nil {
				out = append(out, rr)
			}
		}
	}
	return out
}

// kubeUpdateEndpoints groups the UPDATE-owned part of a planned zone (every
// RRset not reserved for another source) into one endpoint per name and type.
func kubeUpdateEndpoints(records []dns.RR, reserved map[[2]any]bool) ([]public.Endpoint, bool) {
	type group struct {
		name    string
		typ     uint16
		ttl     uint32
		targets []string
	}
	groups := map[[2]any]*group{}
	var order [][2]any
	for _, rr := range records {
		h := rr.Header()
		k := [2]any{strings.ToLower(h.Name), h.Rrtype}
		if reserved[k] || h.Rrtype == dns.TypeSOA {
			continue
		}
		g, ok := groups[k]
		if !ok {
			g = &group{name: strings.ToLower(h.Name), typ: h.Rrtype, ttl: h.Ttl}
			groups[k] = g
			order = append(order, k)
		}
		target := strings.TrimSpace(strings.TrimPrefix(rr.String(), h.String()))
		if target == "" {
			return nil, false
		}
		// Every target must survive the same parser the zone builder uses.
		if back, err := zone.ParseTarget(g.name, g.typ, g.ttl, target); err != nil || !dns.IsDuplicate(back, rr) {
			return nil, false
		}
		g.targets = append(g.targets, target)
	}
	out := make([]public.Endpoint, 0, len(order))
	for _, k := range order {
		g := groups[k]
		out = append(out, public.Endpoint{DNSName: g.name, RecordType: dns.TypeToString[g.typ], RecordTTL: int64(g.ttl), Targets: g.targets})
	}
	slices.SortFunc(out, func(a, b public.Endpoint) int {
		if c := strings.Compare(a.DNSName, b.DNSName); c != 0 {
			return c
		}
		return strings.Compare(a.RecordType, b.RecordType)
	})
	return out, true
}

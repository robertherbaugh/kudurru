package unblock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Kind is the store kind of the requests.
const Kind store.Kind = v1alpha1.KindUnblockRequest

// Manager is the store manager of the requests the block page writes.
const Manager = store.ManagerSystem

// Defaults are the unblockRequests.* defaults, used without Settings.
var Defaults = config.UnblockRequests{
	Enabled: true, RateLimit: 5, MaxPending: 1000,
	PendingExpiry: config.Duration(14 * 24 * time.Hour), Retention: config.Duration(30 * 24 * time.Hour),
}

// Errors of Submit and the decisions.
var (
	// ErrPaused: unblockRequests.maxPending requests are pending.
	ErrPaused = errors.New("unblock: too many pending requests")
	// ErrState: the request is not in a state the operation applies to.
	ErrState = errors.New("unblock: request state")
	// ErrInvalid: the decision is not valid for the request.
	ErrInvalid = errors.New("unblock: invalid decision")
)

// Options configures a Service.
type Options struct {
	Store store.Store
	// Namespace receives the requests ("default").
	Namespace string
	// Node is this node's name, recorded on submissions.
	Node string
	// Settings returns the current unblockRequests.* keys; nil uses
	// Defaults.
	Settings func() config.UnblockRequests
	// Privacy returns querylog.privacy and whether querylog.anonymize is
	// pseudonym; nil records clients in full.
	Privacy func() (querylog.Mode, bool)
	// Now is the clock; nil is time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Service is the request book. Build it with New and Run it on every
// node; RunJanitor on the leader.
type Service struct {
	opts Options

	mu     sync.RWMutex
	items  map[store.Key]*v1alpha1.UnblockRequest
	synced chan struct{}
	once   sync.Once
}

// New returns a Service.
func New(o Options) *Service {
	if o.Namespace == "" {
		o.Namespace = "default"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	o.Logger = o.Logger.With("component", "unblock")
	return &Service{opts: o, items: map[store.Key]*v1alpha1.UnblockRequest{}, synced: make(chan struct{})}
}

// Namespace is where the requests are created.
func (s *Service) Namespace() string { return s.opts.Namespace }

// Settings are the current unblockRequests.* keys.
func (s *Service) Settings() config.UnblockRequests {
	if s.opts.Settings == nil {
		return Defaults
	}
	return s.opts.Settings()
}

func (s *Service) now() time.Time { return s.opts.Now() }

// Synced is closed once the first list of the watch has been read.
func (s *Service) Synced() <-chan struct{} { return s.synced }

// maxWatchBackoff bounds the pause between failed watches.
const maxWatchBackoff = 30 * time.Second

// Run keeps the in-memory index of the requests until ctx ends.
func (s *Service) Run(ctx context.Context) error {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		w, err := s.opts.Store.Watch(ctx, Kind, store.WatchOptions{})
		if err != nil {
			s.opts.Logger.Warn("watch failed; retrying", "err", err, "after", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, maxWatchBackoff)
			continue
		}
		backoff = 100 * time.Millisecond
		s.session(ctx, w)
		w.Stop()
	}
	return nil
}

// session applies one watch: a full list up to the bookmark, then changes.
func (s *Service) session(ctx context.Context, w store.Watcher) {
	listing, seen := true, map[store.Key]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.ResultChan():
			if !ok {
				return
			}
			switch ev.Type {
			case store.Bookmark:
				if listing {
					listing = false
					s.mu.Lock()
					for k := range s.items {
						if !seen[k] {
							delete(s.items, k)
						}
					}
					s.mu.Unlock()
					seen = nil
					s.once.Do(func() { close(s.synced) })
				}
			case store.Added, store.Modified:
				r, ok := ev.Object.(*v1alpha1.UnblockRequest)
				if !ok {
					continue
				}
				if listing {
					seen[store.KeyOf(r)] = true
				}
				s.upsert(r)
			case store.Deleted:
				if ev.Object != nil {
					s.mu.Lock()
					delete(s.items, keyOf(ev.Object.GetNamespace(), ev.Object.GetName()))
					s.mu.Unlock()
				}
			}
		}
	}
}

func keyOf(ns, name string) store.Key { return store.Key{Kind: Kind, Namespace: ns, Name: name} }

// upsert stores r unless the index already has a newer copy (a watch event
// can trail the write that Submit or a decision recorded directly).
func (s *Service) upsert(r *v1alpha1.UnblockRequest) {
	k := keyOf(r.Namespace, r.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.items[k]; ok && newerRV(cur.ResourceVersion, r.ResourceVersion) {
		return
	}
	s.items[k] = r.DeepCopy()
}

// newerRV reports whether a is a later resourceVersion than b. Both
// drivers hand out increasing integers; anything else is not compared.
func newerRV(a, b string) bool {
	var x, y uint64
	if _, err := fmt.Sscan(a, &x); err != nil {
		return false
	}
	if _, err := fmt.Sscan(b, &y); err != nil {
		return false
	}
	return x > y
}

// Get returns the indexed request ns/name, nil when there is none.
func (s *Service) Get(ns, name string) *v1alpha1.UnblockRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.items[keyOf(ns, name)]; ok {
		return r.DeepCopy()
	}
	return nil
}

// Counts are the indexed requests by state.
type Counts struct {
	Pending, Approved, Denied, Expired int
}

// Counts counts the indexed requests by state.
func (s *Service) Counts() Counts {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var c Counts
	for _, r := range s.items {
		switch r.EffectiveState() {
		case v1alpha1.UnblockPending:
			c.Pending++
		case v1alpha1.UnblockApproved:
			c.Approved++
		case v1alpha1.UnblockDenied:
			c.Denied++
		case v1alpha1.UnblockExpired:
			c.Expired++
		}
	}
	return c
}

// List returns the indexed requests in state (all when empty), newest
// first (latest submission, then name).
func (s *Service) List(state v1alpha1.UnblockRequestState) []*v1alpha1.UnblockRequest {
	s.mu.RLock()
	out := make([]*v1alpha1.UnblockRequest, 0, len(s.items))
	for _, r := range s.items {
		if state == "" || r.EffectiveState() == state {
			out = append(out, r.DeepCopy())
		}
	}
	s.mu.RUnlock()
	SortNewestFirst(out)
	return out
}

// SortNewestFirst orders requests by their latest submission, newest
// first, then by namespace and name.
func SortNewestFirst(rs []*v1alpha1.UnblockRequest) {
	sort.SliceStable(rs, func(i, j int) bool {
		ti, tj := RequestedAt(rs[i]), RequestedAt(rs[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		if rs[i].Namespace != rs[j].Namespace {
			return rs[i].Namespace < rs[j].Namespace
		}
		return rs[i].Name < rs[j].Name
	})
}

// RequestedAt is the latest submission of r (its creation without one).
func RequestedAt(r *v1alpha1.UnblockRequest) time.Time {
	if r.Spec.RequestedAt != nil {
		return r.Spec.RequestedAt.Time
	}
	return r.CreationTimestamp.Time
}

// ClientForm is how a client address is recorded under the query-log
// privacy mode: the address under full and hide_domains, its /24 or /48
// network address under anonymize_client with prefix (as the query log
// records it), nothing under the pseudonym and stricter modes.
func ClientForm(client netip.Addr, mode querylog.Mode, pseudonym bool) (string, v1alpha1.UnblockClientPrivacy) {
	if !client.IsValid() {
		return "", v1alpha1.UnblockClientHidden
	}
	client = client.Unmap()
	switch mode {
	case querylog.PrivacyFull, querylog.PrivacyHideDomains:
		return client.String(), v1alpha1.UnblockClientFull
	case querylog.PrivacyAnonymizeClient:
		if pseudonym {
			return "", v1alpha1.UnblockClientHidden
		}
		bits := 24
		if client.Is6() {
			bits = 48
		}
		return netip.PrefixFrom(client, bits).Masked().Addr().String(), v1alpha1.UnblockClientPrefix
	}
	return "", v1alpha1.UnblockClientHidden
}

func (s *Service) clientForm(client netip.Addr) (string, v1alpha1.UnblockClientPrivacy) {
	mode, pseudonym := querylog.PrivacyFull, false
	if s.opts.Privacy != nil {
		mode, pseudonym = s.opts.Privacy()
	}
	return ClientForm(client, mode, pseudonym)
}

// Name is the object name of the request of client (as recorded) for
// domain: one object per pair, so repeated requests update it.
func Name(client, domain string) string {
	sum := sha256.Sum256([]byte(client + "\x00" + domain))
	return "ur-" + hex.EncodeToString(sum[:8])
}

// Lookup returns the indexed request of client for domain, nil when
// there is none.
func (s *Service) Lookup(client netip.Addr, domain string) *v1alpha1.UnblockRequest {
	c, _ := s.clientForm(client)
	return s.Get(s.opts.Namespace, Name(c, domain))
}

// Block is why the name was blocked, as the block page found it.
type Block struct {
	Kind, List, Rule, Policy string
}

// Submission is one request from the block page.
type Submission struct {
	// Client is the requesting address (recorded as querylog.privacy
	// allows), Domain the blocked name from the Host header or SNI.
	Client netip.Addr
	Domain string
	// Name, Contact and Reason are the user's plain-text fields, already
	// length-checked.
	Name, Contact, Reason string
	// Groups and Policies are the client's at request time.
	Groups, Policies []string
	// Block is the reason the block page found; nil when unknown.
	Block *Block
}

// maxHistory bounds status.history.
const maxHistory = 20

// Submit creates the request of sub, or updates the client's pending one
// for the domain. A decided or expired request asked again returns to
// Pending with its decision moved to status.history; an approval still in
// force is returned unchanged. It answers ErrPaused when a new pending
// request would exceed unblockRequests.maxPending.
func (s *Service) Submit(ctx context.Context, sub Submission) (*v1alpha1.UnblockRequest, error) {
	client, privacy := s.clientForm(sub.Client)
	name := Name(client, sub.Domain)
	key := keyOf(s.opts.Namespace, name)
	existing := &v1alpha1.UnblockRequest{}
	if err := s.opts.Store.Get(ctx, key, existing); errors.Is(err, store.ErrNotFound) {
		existing = nil
	} else if err != nil {
		return nil, err
	}
	now := s.now()
	if existing != nil && Active(existing, now) {
		return existing, nil
	}
	reopen := existing == nil || existing.EffectiveState() != v1alpha1.UnblockPending
	if reopen && s.Counts().Pending >= s.Settings().MaxPending {
		return nil, ErrPaused
	}
	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	obj := &v1alpha1.UnblockRequest{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: v1alpha1.KindUnblockRequest},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.opts.Namespace},
		Spec: v1alpha1.UnblockRequestSpec{
			Domain: sub.Domain, Client: client, ClientPrivacy: privacy,
			Groups: capList(sub.Groups), Policies: capList(sub.Policies), Node: s.opts.Node,
			RequesterName: sub.Name, Contact: sub.Contact, Reason: sub.Reason,
			RequestedAt: &at, Submissions: 1,
		},
	}
	if b := sub.Block; b != nil {
		obj.Spec.Block = &v1alpha1.UnblockBlock{Kind: b.Kind, List: b.List, Rule: truncate(b.Rule, 1024), Policy: b.Policy}
	}
	opts := store.ApplyOptions{Manager: Manager}
	if existing != nil {
		obj.Labels, obj.Annotations = existing.Labels, existing.Annotations
		obj.Spec.Submissions = existing.Spec.Submissions + 1
		opts.ExpectedResourceVersion = existing.ResourceVersion
		opts.Force = store.ManagedBy(existing) != Manager
	}
	written, err := s.opts.Store.Apply(ctx, obj, opts)
	if err != nil {
		return nil, err
	}
	out, ok := written.(*v1alpha1.UnblockRequest)
	if !ok {
		return nil, fmt.Errorf("unblock: store returned %T", written)
	}
	if reopen {
		st := v1alpha1.UnblockRequestStatus{State: v1alpha1.UnblockPending}
		if existing != nil {
			st.History = pushHistory(existing.Status)
		}
		out = out.DeepCopy()
		out.Status = st
		if err := s.opts.Store.UpdateStatus(ctx, out); err != nil {
			return nil, err
		}
		if fresh := s.reread(ctx, key); fresh != nil {
			out = fresh
		}
	}
	s.upsert(out)
	return out, nil
}

// reread returns the stored request, nil on error.
func (s *Service) reread(ctx context.Context, key store.Key) *v1alpha1.UnblockRequest {
	r := &v1alpha1.UnblockRequest{}
	if err := s.opts.Store.Get(ctx, key, r); err != nil {
		return nil
	}
	return r
}

// pushHistory returns st's history with st's own decision appended, when
// it has one, oldest dropped beyond maxHistory.
func pushHistory(st v1alpha1.UnblockRequestStatus) []v1alpha1.UnblockDecision {
	h := slices.Clone(st.History)
	if st.State != "" && st.State != v1alpha1.UnblockPending {
		h = append(h, v1alpha1.UnblockDecision{State: st.State, DecidedBy: st.DecidedBy, DecidedAt: st.DecidedAt, Note: st.Note, Allow: st.Allow})
	}
	if len(h) > maxHistory {
		h = h[len(h)-maxHistory:]
	}
	return h
}

// Active reports whether r is an approval whose allow entry is in force
// at now.
func Active(r *v1alpha1.UnblockRequest, now time.Time) bool {
	a := r.Status.Allow
	return r.EffectiveState() == v1alpha1.UnblockApproved && a != nil && a.Removed == nil &&
		(a.ExpiresAt == nil || a.ExpiresAt.After(now))
}

// capList keeps the first 16 entries (the CRD bound).
func capList(in []string) []string {
	if len(in) > 16 {
		in = in[:16]
	}
	return slices.Clone(in)
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

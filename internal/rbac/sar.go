package rbac

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
)

// KindKubernetesServiceAccount is the Principal.Kind the SAR authorizer acts
// on (auth.KindKubernetesServiceAccount).
const KindKubernetesServiceAccount = "KubernetesServiceAccount"

// SARGroup is the API group of the real and virtual resources checked by
// SubjectAccessReview (§5.5).
const SARGroup = "kudurru.io"

// SARResources maps kudurru RBAC resources to the Kubernetes resource names
// SubjectAccessReview asks about. Real CRDs keep their plural names; the
// rest are virtual resources that exist only as strings in RBAC rules
// (querylogs, stats, caches, blocking, audits, clusters, settings, ...), so
// Kubernetes RBAC is the policy for Kubernetes identities without extra
// CRDs. The chart's aggregated ClusterRoles use the same names.
var SARResources = map[string]string{
	"zones":             "dnszones",
	"records":           "dnsrecordsets",
	"blocklists":        "blocklists",
	"policies":          "policies",
	"clientgroups":      "clientgroups",
	"clients":           "clients",
	"querylog":          "querylogs",
	"stats":             "stats",
	"cache":             "caches",
	"blocking":          "blocking",
	"settings":          "settings",
	"node":              "nodes",
	"users":             "users",
	"groups":            "groups",
	"serviceaccounts":   "serviceaccounts",
	"tokens":            "tokens",
	"identityproviders": "identityproviders",
	"rolebindings":      "rolebindings",
	"integrations":      "integrations",
	"cluster":           "clusters",
	"edge":              "edges",
	"audit":             "audits",
	"unblockrequests":   "unblockrequests",
}

// SARVerb maps a kudurru verb to the Kubernetes verb: read is get for a
// named object and list otherwise, write is update for a named object and
// create otherwise, delete is delete, and the extra verbs (toggle, flush,
// refresh, export, purge, transfer, ...) pass through as custom verbs.
func SARVerb(verb string, named bool) string {
	switch verb {
	case "read":
		if named {
			return "get"
		}
		return "list"
	case "write":
		if named {
			return "update"
		}
		return "create"
	}
	return verb
}

// SAR cache defaults: allowed verdicts are kept for 2 minutes (§5.5:
// "cached ≤ 2 min"), denials for 30 seconds so a freshly granted
// permission takes effect quickly.
const (
	DefaultSARAllowTTL = 2 * time.Minute
	DefaultSARDenyTTL  = 30 * time.Second
	maxSAREntries      = 10_000
)

// SAROptions configures a SARAuthorizer.
type SAROptions struct {
	// Client creates SubjectAccessReviews (the kudurru ServiceAccount
	// needs system:auth-delegator). Required.
	Client authorizationv1client.SubjectAccessReviewsGetter
	// AllowTTL and DenyTTL bound the verdict cache; defaults 2m and 30s,
	// capped at 2m.
	AllowTTL, DenyTTL time.Duration
	// Logger receives SAR failures; default discards.
	Logger *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// SARAuthorizer authorizes Kubernetes ServiceAccount principals with
// SubjectAccessReview against kudurru.io resources (§5.5, D21, D55). Other
// principals are left to the next authorizer of the Chain. Verdicts are
// cached per (identity, attributes). A failing apiserver is a denial with a
// reason, never an error, so binding-authorized requests keep working
// during an apiserver outage.
type SARAuthorizer struct {
	opts SAROptions

	mu    sync.Mutex
	cache map[string]sarEntry
}

type sarEntry struct {
	d   Decision
	exp time.Time
}

// NewSARAuthorizer validates opts.
func NewSARAuthorizer(opts SAROptions) (*SARAuthorizer, error) {
	if opts.Client == nil {
		return nil, errors.New("rbac: SAR authorizer needs a SubjectAccessReview client")
	}
	if opts.AllowTTL <= 0 || opts.AllowTTL > DefaultSARAllowTTL {
		opts.AllowTTL = DefaultSARAllowTTL
	}
	if opts.DenyTTL <= 0 || opts.DenyTTL > DefaultSARAllowTTL {
		opts.DenyTTL = DefaultSARDenyTTL
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &SARAuthorizer{opts: opts, cache: map[string]sarEntry{}}, nil
}

// ResourceAttributes returns the SubjectAccessReview attributes for a.
// A zone-scoped request names the zone when it names no object itself.
func ResourceAttributes(a Attributes) *authorizationv1.ResourceAttributes {
	res, ok := SARResources[a.Resource]
	if !ok {
		res = a.Resource
	}
	name := a.Name
	if name == "" && a.Scope.Kind == ScopeZone {
		name = a.Scope.Name
	}
	return &authorizationv1.ResourceAttributes{
		Group:     SARGroup,
		Resource:  res,
		Verb:      SARVerb(a.Verb, name != ""),
		Namespace: a.Namespace,
		Name:      name,
	}
}

// Authorize implements Authorizer. Path is "sar".
func (s *SARAuthorizer) Authorize(ctx context.Context, p Principal, a Attributes) (Decision, error) {
	if p.Kind != KindKubernetesServiceAccount || p.Name == "" {
		return Decision{Allowed: false, Path: PathSAR, Reason: "not a Kubernetes identity"}, nil
	}
	ra := ResourceAttributes(a)
	key := sarKey(p, ra)
	now := s.opts.Now()
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && now.Before(e.exp) {
		s.mu.Unlock()
		return withMissing(e.d, a), nil
	}
	s.mu.Unlock()

	review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		ResourceAttributes: ra,
		User:               p.Name,
		Groups:             slices.Clone(p.Groups),
		UID:                p.UID,
	}}
	res, err := s.opts.Client.SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		s.opts.Logger.WarnContext(ctx, "rbac: SubjectAccessReview failed", "user", p.Name, "resource", ra.Resource, "verb", ra.Verb, "err", err)
		return withMissing(Decision{Allowed: false, Path: PathSAR, Reason: "SubjectAccessReview unavailable"}, a), nil
	}
	d := Decision{Allowed: res.Status.Allowed && !res.Status.Denied, Path: PathSAR}
	switch {
	case d.Allowed:
		d.Reason = "kubernetes rbac: " + strings.TrimSpace(res.Status.Reason)
	case res.Status.EvaluationError != "":
		d.Reason = "kubernetes rbac: " + res.Status.EvaluationError
	default:
		d.Reason = "kubernetes rbac denies " + ra.Verb + " " + ra.Resource + "." + ra.Group
	}
	ttl := s.opts.DenyTTL
	if d.Allowed {
		ttl = s.opts.AllowTTL
	}
	s.mu.Lock()
	if len(s.cache) >= maxSAREntries {
		for k, e := range s.cache {
			if !now.Before(e.exp) {
				delete(s.cache, k)
			}
		}
		if len(s.cache) >= maxSAREntries {
			clear(s.cache)
		}
	}
	s.cache[key] = sarEntry{d: d, exp: now.Add(ttl)}
	s.mu.Unlock()
	return withMissing(d, a), nil
}

func withMissing(d Decision, a Attributes) Decision {
	if !d.Allowed {
		missing := a
		d.Missing = &missing
	}
	return d
}

func sarKey(p Principal, ra *authorizationv1.ResourceAttributes) string {
	h := sha256.New()
	for _, s := range []string{p.Name, p.UID, strings.Join(p.Groups, "\x00"), ra.Group, ra.Resource, ra.Verb, ra.Namespace, ra.Name} {
		h.Write([]byte(s))
		h.Write([]byte{0xff})
	}
	return hex.EncodeToString(h.Sum(nil))
}

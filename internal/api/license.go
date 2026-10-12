package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// KeyLicense is the 402 of a write the license does not cover.
const KeyLicense = "license_required"

// LicenseSource is the node's license (*license.Manager).
type LicenseSource interface {
	Status() license.Status
	CheckNodes(n int) error
	CheckPolicies(n int) error
}

// LicenseInfo is GET /license: the effective license of this node and how
// many nodes the cluster has against its limit.
type LicenseInfo struct {
	license.Status
	// NodesInUse is how many nodes the cluster has now.
	NodesInUse int `json:"nodesInUse"`
	// OverLimit is true while the cluster is larger than the license
	// allows; configuration writes through the API answer 402 meanwhile.
	// DNS service is never affected.
	OverLimit bool `json:"overLimit"`
	Took
}

type licenseOutput struct{ Body *LicenseInfo }

func (s *Server) registerLicense() {
	register(s, operation(http.MethodGet, "/license", "getLicense", "License state, node limit and licensed features of this node", "cluster"),
		perm{resource: "settings", verb: "read"}, s.getLicense)
}

func (s *Server) getLicense(ctx context.Context, _ *struct{}) (*licenseOutput, error) {
	var st license.Status
	if s.opts.License != nil {
		st = s.opts.License.Status()
	} else {
		st = license.Unrestricted().Status()
	}
	n := s.nodeCount(ctx)
	return &licenseOutput{Body: &LicenseInfo{Status: st, NodesInUse: n, OverLimit: n > st.Nodes}}, nil
}

// nodeCountTTL is how long the node count is reused by the license gate.
const nodeCountTTL = 10 * time.Second

type nodeCounter struct {
	mu   sync.Mutex
	at   time.Time
	n    int
	have bool
}

// nodeCount returns the number of cluster nodes, 0 when membership fails
// (the gate fails open: a membership outage never blocks an operator).
// requireFeature answers 402 license_required when f is not licensed. A
// server without Options.License does not gate.
func (s *Server) requireFeature(f license.Feature, what string) error {
	if s.opts.License == nil || s.opts.License.Status().Allows(f) {
		return nil
	}
	return newError(http.StatusPaymentRequired, KeyLicense, what+" needs a license key",
		"add a license key (a free personal key covers 3 nodes; see LICENSING.md); DNS keeps working meanwhile")
}

func (s *Server) nodeCount(ctx context.Context) int {
	c := &s.nodes
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && s.opts.Now().Sub(c.at) < nodeCountTTL {
		return c.n
	}
	peers, err := s.opts.Membership.Peers(ctx)
	if err != nil {
		return 0
	}
	c.n, c.at, c.have = len(peers), s.opts.Now(), true
	return c.n
}

// licenseExempt are the write routes that stay open when the cluster is
// over its node limit: signing in, asking a question, and the operational
// controls (pause blocking, flush the cache, act on a logged query).
var licenseExempt = []string{"/auth/", "/dns/query", "/dns/blocking", "/cache/flush", "/queries/"}

// licenseMW answers 402 to configuration writes while the cluster is larger
// than the license allows. It only narrows the management plane: reads, the
// login, the operational controls and, above all, DNS answering are never
// blocked. It does nothing without Options.License.
func (s *Server) licenseMW(next http.Handler) http.Handler {
	if s.opts.License == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		rel, ok := strings.CutPrefix(r.URL.Path, BasePath)
		if !ok || exempt(rel) {
			next.ServeHTTP(w, r)
			return
		}
		if err := s.opts.License.CheckNodes(s.nodeCount(r.Context())); err != nil {
			writeError(w, r, newError(http.StatusPaymentRequired, KeyLicense, err.Error(),
				"remove nodes, or add a license key (a free personal key covers 3 nodes; see LICENSING.md); DNS keeps working meanwhile"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkPolicyLimit refuses to create a Policy beyond the license's limit
// (two without a key). Existing policies are never touched: a lapsed key
// only stops new ones.
func (s *Server) checkPolicyLimit(ctx context.Context) error {
	if s.opts.License == nil || s.opts.License.Status().PolicyLimit == 0 {
		return nil
	}
	n := 0
	for cont := ""; ; {
		page, err := s.opts.Store.List(ctx, store.Kind("Policy"), store.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			return storeError(err)
		}
		n += len(page.Items)
		if cont = page.Continue; cont == "" {
			break
		}
	}
	if err := s.opts.License.CheckPolicies(n); err != nil {
		return newError(http.StatusPaymentRequired, KeyLicense, err.Error(),
			"delete a policy, or add a license key (a free personal key lifts the limit; see LICENSING.md); existing policies keep working")
	}
	return nil
}

func exempt(rel string) bool {
	for _, p := range licenseExempt {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

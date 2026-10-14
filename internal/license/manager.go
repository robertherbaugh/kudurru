package license

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// State is where a node stands against its key.
type State string

// The states. Community, expired and invalid all mean the free limits:
// one node, one policy and no licensed features.
const (
	// StateCommunity is no key at all.
	StateCommunity State = "community"
	// StateActive is a valid key inside its term.
	StateActive State = "active"
	// StateGrace is a key that expired less than Grace ago; it still works.
	StateGrace State = "grace"
	// StateExpired is a key that expired more than Grace ago.
	StateExpired State = "expired"
	// StateInvalid is a key that did not verify, or is not valid yet.
	StateInvalid State = "invalid"
)

// Status is the effective license of a node at one moment.
type Status struct {
	State State `json:"state"`
	// Plan, Licensee and ID describe the key; empty without one.
	Plan     Plan   `json:"plan,omitempty"`
	Licensee string `json:"licensee,omitempty"`
	ID       string `json:"id,omitempty"`
	// Source names where the key came from (flag, env, file path).
	Source string `json:"source,omitempty"`
	// Nodes is the effective node limit and Features the effective licensed
	// features, after the state is applied.
	Nodes    int       `json:"nodes"`
	Features []Feature `json:"features" nullable:"false"`
	// PolicyLimit is how many Policy objects may exist; 0 means no limit.
	PolicyLimit int `json:"policyLimit" doc:"How many Policy objects may exist; 0 means no limit."`
	// ExpiresAt and GraceEndsAt bound the key's term and its grace.
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	GraceEndsAt *time.Time `json:"graceEndsAt,omitempty"`
	// Error says why a key is invalid.
	Error string `json:"error,omitempty"`
}

// Allows reports whether the status grants f.
func (s Status) Allows(f Feature) bool { return slices.Contains(s.Features, f) }

// Manager holds the node's key and evaluates it against a clock. The zero
// value and a nil *Manager are the community license.
type Manager struct {
	ring Keyring
	now  func() time.Time

	mu      sync.RWMutex
	claims  *Claims
	err     error
	source  string
	current atomic.Pointer[installation]
	changed chan struct{}
}

// Options configures New.
type Options struct {
	// Keyring defaults to the embedded one.
	Keyring Keyring
	// Now is the clock (tests).
	Now func() time.Time
}

// New returns a Manager with no key (the community license).
func New(opts Options) (*Manager, error) {
	if opts.Keyring == nil {
		ring, err := EmbeddedKeyring()
		if err != nil {
			return nil, err
		}
		opts.Keyring = ring
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	m := &Manager{ring: opts.Keyring, now: opts.Now}
	m.refreshLocked()
	return m, nil
}

// Set verifies raw and makes it the node's key. An empty raw removes the key.
// On error the node keeps no key and Status reports state invalid, so a typo
// never silently leaves the previous key in force.
func (m *Manager) Set(raw, source string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.refreshLocked()
	if strings.TrimSpace(raw) == "" {
		m.claims, m.err, m.source = nil, nil, ""
		return nil
	}
	c, err := Verify(raw, m.ring)
	m.source = source
	if err != nil {
		m.claims, m.err = nil, err
		return err
	}
	m.claims, m.err = &c, nil
	return nil
}

// Status evaluates the key at the manager's clock.
func (m *Manager) Status() Status {
	if m == nil {
		return community()
	}
	m.mu.RLock()
	claims, err, source := m.claims, m.err, m.source
	m.mu.RUnlock()
	return statusAt(claims, err, source, m.clock())
}

func statusAt(claims *Claims, err error, source string, now time.Time) Status {
	if err != nil {
		s := community()
		s.State, s.Source, s.Error = StateInvalid, source, err.Error()
		return s
	}
	if claims == nil {
		return community()
	}
	exp := claims.Expires()
	grace := exp.Add(Grace)
	s := Status{
		Plan: claims.Plan, Licensee: claims.Licensee, ID: claims.ID, Source: source,
		ExpiresAt: &exp, GraceEndsAt: &grace,
	}
	switch {
	case claims.NotBefore > now.Unix():
		c := community()
		c.State, c.Plan, c.Licensee, c.ID, c.Source = StateInvalid, s.Plan, s.Licensee, s.ID, source
		c.Error = "the key is not valid before " + time.Unix(claims.NotBefore, 0).UTC().Format(time.RFC3339)
		return c
	case now.Before(exp):
		s.State = StateActive
	case now.Before(grace):
		s.State = StateGrace
	default:
		c := community()
		c.State, c.Plan, c.Licensee, c.ID, c.Source = StateExpired, s.Plan, s.Licensee, s.ID, source
		c.ExpiresAt, c.GraceEndsAt = s.ExpiresAt, s.GraceEndsAt
		return c
	}
	s.Nodes = claims.Nodes
	s.PolicyLimit = 0
	s.Features = []Feature{}
	for _, f := range Features() {
		if claims.Allows(f) {
			s.Features = append(s.Features, f)
		}
	}
	return s
}

func community() Status {
	return Status{State: StateCommunity, Nodes: CommunityNodes, Features: []Feature{}, PolicyLimit: CommunityPolicies}
}

// Allows reports whether this node may use f now.
func (m *Manager) Allows(f Feature) bool { return m.Status().Allows(f) }

// ErrFeature is wrapped by Require when f is not licensed.
var ErrFeature = errors.New("feature needs a license key")

// Require returns nil when f is licensed, else an error saying what to do.
func (m *Manager) Require(f Feature) error {
	s := m.Status()
	if s.Allows(f) {
		return nil
	}
	return fmt.Errorf("%w: %s needs a kudurru license key (state %s); a free personal key or a commercial one unlocks it (https://kudurru.io/license)",
		ErrFeature, f, s.State)
}

// RequireStart is the check of a process that must not stop answering for want
// of a key: the dnsdist and keepalived supervisors, which sit in the data path.
// It returns nil when f is licensed; nil with a warning when the key is in its
// grace period or has expired but covered f (the deployment was licensed, so a
// restart after a lapse must never become an outage: the lapse is a billing
// matter, not a reason to stop answering); and an error only when there is no
// verifiable key that ever covered f, which is a first start that has nothing
// to lose. The warning says what to do.
func (m *Manager) RequireStart(f Feature) (warning string, err error) {
	s := m.Status()
	switch {
	case s.Allows(f) && s.State == StateGrace:
		return fmt.Sprintf("the license key expired on %s; it keeps working until %s", s.ExpiresAt.Format(time.DateOnly), s.GraceEndsAt.Format(time.DateOnly)), nil
	case s.Allows(f):
		return "", nil
	case s.State == StateExpired && m.covered(f):
		return fmt.Sprintf("the license key expired on %s, more than %d days ago; %s keeps running because it was licensed. Renew the key",
			s.ExpiresAt.Format(time.DateOnly), int(Grace/(24*time.Hour)), f), nil
	}
	return "", m.Require(f)
}

// covered reports whether the key (expired or not) grants f.
func (m *Manager) covered(f Feature) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.claims != nil && m.claims.Allows(f)
}

// ErrNodeLimit is wrapped by CheckNodes when the cluster is larger than the
// license allows.
var ErrNodeLimit = errors.New("node limit exceeded")

// CheckNodes returns an ErrNodeLimit when n nodes exceed the license.
func (m *Manager) CheckNodes(n int) error {
	s := m.Status()
	if n <= s.Nodes {
		return nil
	}
	return fmt.Errorf("%w: the cluster has %d nodes and the license (state %s) allows %d", ErrNodeLimit, n, s.State, s.Nodes)
}

// ErrPolicyLimit is wrapped by CheckPolicies when no more Policy objects may
// be created.
var ErrPolicyLimit = errors.New("policy limit reached")

// CheckPolicies returns an ErrPolicyLimit when n existing Policy objects
// leave no room for another.
func (m *Manager) CheckPolicies(n int) error {
	s := m.Status()
	if s.PolicyLimit == 0 || n < s.PolicyLimit {
		return nil
	}
	exist := fmt.Sprintf("%d policies exist", n)
	if n == 1 {
		exist = "1 policy exists"
	}
	return fmt.Errorf("%w: %s and the license (state %s) allows %d", ErrPolicyLimit, exist, s.State, s.PolicyLimit)
}

// CheckPolicyCreate returns an ErrPolicyLimit when the API may not create
// the Policy namespace/name while n Policy objects exist. With a policy limit
// (no valid key) only the default policy (CommunityPolicyName in
// defaultNamespace, the namespace of the server's seeded defaults) may be
// created, and only while fewer than the limit exist; with a key anything goes.
func (m *Manager) CheckPolicyCreate(defaultNamespace, namespace, name string, n int) error {
	s := m.Status()
	if s.PolicyLimit == 0 {
		return nil
	}
	if namespace != defaultNamespace || name != CommunityPolicyName {
		return fmt.Errorf("%w: without a valid license key (state %s) only the policy %s/%s can be created",
			ErrPolicyLimit, s.State, defaultNamespace, CommunityPolicyName)
	}
	return m.CheckPolicies(n)
}

// Unrestricted returns a Manager that allows every feature and any number
// of nodes, for tests of code that sits behind a license check.
func Unrestricted() *Manager {
	m := &Manager{now: time.Now}
	far := time.Now().AddDate(100, 0, 0)
	m.claims = &Claims{
		ID: "unrestricted", Issuer: Issuer, Licensee: "tests", Plan: PlanEnterprise,
		Nodes: math.MaxInt32, IssuedAt: time.Now().Unix(), ExpiresAt: far.Unix(),
	}
	m.source = "test"
	m.refreshLocked()
	return m
}

// Source is a place a key can come from.
type Source struct {
	// Name is what Status.Source reports ("--license-key", "env KUDURRU_LICENSE_KEY", a path).
	Name string
	// Read returns the key, "" when the source holds none.
	Read func() (string, error)
}

// Literal is a source that holds value.
func Literal(name, value string) Source {
	return Source{Name: name, Read: func() (string, error) { return value, nil }}
}

// File is a source that reads path; a missing file is no key.
func File(name, path string) Source {
	return Source{Name: name, Read: func() (string, error) {
		b, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the file
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return string(b), err
	}}
}

// Load applies the first source that holds a key, in order. A source that
// cannot be read, or whose key does not verify, makes the status invalid; it
// does not fall through to a later source, because the operator named it.
func (m *Manager) Load(sources []Source) error {
	for _, src := range sources {
		raw, err := src.Read()
		if err != nil {
			m.mu.Lock()
			m.claims, m.err, m.source = nil, fmt.Errorf("read %s: %w", src.Name, err), src.Name
			m.refreshLocked()
			m.mu.Unlock()
			return fmt.Errorf("read %s: %w", src.Name, err)
		}
		if strings.TrimSpace(raw) == "" {
			continue
		}
		return m.Set(raw, src.Name)
	}
	return m.Set("", "")
}

// Run loads the sources, then reloads them every interval so a key written
// to a mounted file or a renewed Secret takes effect without a restart, and
// logs every change of state. It returns when ctx ends.
func (m *Manager) Run(ctx context.Context, sources []Source, interval time.Duration, log *slog.Logger) error {
	last := Status{State: "unset"}
	report := func() {
		if err := m.Load(sources); err != nil {
			log.Warn("license key", "error", err)
		}
		s := m.Status()
		if s.State == last.State && s.ID == last.ID && s.Source == last.Source {
			return
		}
		last = s
		attrs := []any{"state", s.State, "nodes", s.Nodes, "features", s.Features}
		if s.ID != "" {
			attrs = append(attrs, "plan", s.Plan, "licensee", s.Licensee, "id", s.ID, "source", s.Source)
		}
		if s.ExpiresAt != nil {
			attrs = append(attrs, "expires", s.ExpiresAt.Format(time.DateOnly))
		}
		switch s.State {
		case StateInvalid:
			log.Warn("license key is invalid; running with the community license", append(attrs, "error", s.Error)...)
		case StateGrace:
			log.Warn("license key expired; it keeps working until the end of the grace period", append(attrs, "graceEnds", s.GraceEndsAt.Format(time.DateOnly))...)
		case StateExpired:
			log.Warn("license key expired; running with the community license", attrs...)
		default:
			log.Info("license", attrs...)
		}
	}
	report()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			report()
		}
	}
}

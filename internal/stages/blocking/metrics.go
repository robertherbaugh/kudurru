package blocking

import (
	"slices"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// maxLabelValues bounds the group and list label values (§11.1); later
// values are reported as "_other".
const maxLabelValues = 64

const otherLabel = "_other"

// outcome is one (action, source) pair of kudurru_block_decisions_total.
type outcome uint8

// Outcomes.
const (
	blockDeny outcome = iota
	blockResponse
	blockAllowOnly
	allowList
	allowException
	passNone
	passDisabled
	numOutcomes
)

// outcomeLabels are the action and source label values per outcome.
var outcomeLabels = [numOutcomes][2]string{
	blockDeny:      {"block", "deny"},
	blockResponse:  {"block", "response"},
	blockAllowOnly: {"block", "allow_only"},
	allowList:      {"allow", "allowlist"},
	allowException: {"allow", "exception"},
	passNone:       {"pass", "none"},
	passDisabled:   {"pass", "disabled"},
}

// metrics are the blocking instruments (docs/architecture.md §11.1). All
// methods are safe on a nil *metrics.
type metrics struct {
	decisions *prometheus.CounterVec // action, source, group
	listHits  *prometheus.CounterVec // list

	groups, lists labelLimiter
	// byGroup is a copy-on-write map of resolved counters, so the per-query
	// cost is one atomic load and one string-keyed map read.
	byGroup atomic.Pointer[map[string]*[numOutcomes]prometheus.Counter]
	mu      sync.Mutex
	byList  sync.Map // string -> prometheus.Counter
}

// labelLimiter admits the first maxLabelValues distinct values.
type labelLimiter struct {
	seen sync.Map // string -> struct{}
	n    atomic.Int64
}

func (l *labelLimiter) admit(v string) string {
	if _, ok := l.seen.Load(v); ok {
		return v
	}
	if l.n.Add(1) > maxLabelValues {
		l.n.Add(-1)
		return otherLabel
	}
	if _, loaded := l.seen.LoadOrStore(v, struct{}{}); loaded {
		l.n.Add(-1)
	}
	return v
}

func newMetrics(reg prometheus.Registerer, enabled prometheus.Collector) (*metrics, error) {
	m := &metrics{
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_block_decisions_total",
			Help: "Blocking decisions for recursive queries of clients with a filtering policy, by action (block, allow, pass), source (allowlist, allow_only, exception, deny, response, disabled, none) and client group.",
		}, []string{"action", "source", "group"}),
		listHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_block_list_hits_total",
			Help: "Blocked queries by the list whose rule decided.",
		}, []string{"list"}),
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{m.decisions, m.listHits, enabled} {
			if err := reg.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}

func (m *metrics) decision(o outcome, group string) {
	if m == nil {
		return
	}
	if cs := m.byGroup.Load(); cs != nil {
		if c, ok := (*cs)[group]; ok {
			c[o].Inc()
			return
		}
	}
	m.counters(group)[o].Inc()
}

// counters resolves every outcome's counter of group once.
func (m *metrics) counters(group string) *[numOutcomes]prometheus.Counter {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.byGroup.Load()
	if old != nil {
		if c, ok := (*old)[group]; ok {
			return c
		}
	}
	label := m.groups.admit(group)
	c := new([numOutcomes]prometheus.Counter)
	for o, l := range outcomeLabels {
		c[o] = m.decisions.WithLabelValues(l[0], l[1], label)
	}
	next := make(map[string]*[numOutcomes]prometheus.Counter, 1)
	if old != nil {
		for k, v := range *old {
			next[k] = v
		}
	}
	next[group] = c
	m.byGroup.Store(&next)
	return c
}

func (m *metrics) listHit(list string) {
	if m == nil || list == "" {
		return
	}
	if c, ok := m.byList.Load(list); ok {
		c.(prometheus.Counter).Inc()
		return
	}
	c := m.listHits.WithLabelValues(m.lists.admit(list))
	m.byList.Store(list, c)
	c.Inc()
}

// enabledCollector reports kudurru_blocking_enabled{group} at scrape time,
// so an expiring temporary disable needs no timer.
type enabledCollector struct {
	desc  *prometheus.Desc
	state func() map[string]bool
}

func newEnabledCollector(state func() map[string]bool) *enabledCollector {
	return &enabledCollector{
		desc: prometheus.NewDesc("kudurru_blocking_enabled",
			"1 when blocking applies to the client group right now (blocking.enabled, not temporarily disabled, and the group's current policy names lists or is allow-only).",
			[]string{"group"}, nil),
		state: state,
	}
}

// Describe implements prometheus.Collector.
func (c *enabledCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implements prometheus.Collector. Groups are sorted; beyond
// maxLabelValues the rest are folded into one "_other" series that is 1
// when blocking applies to any of them.
func (c *enabledCollector) Collect(ch chan<- prometheus.Metric) {
	state := c.state()
	groups := make([]string, 0, len(state))
	for g := range state {
		groups = append(groups, g)
	}
	slices.Sort(groups)
	other, folded := 0.0, false
	for i, g := range groups {
		v := 0.0
		if state[g] {
			v = 1
		}
		if i >= maxLabelValues {
			other, folded = max(other, v), true
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v, g)
	}
	if folded {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, other, otherLabel)
	}
}

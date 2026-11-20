package k8s

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

var errTruncatedPage = errors.New("list page exceeds the 4 MiB response bound")

type listResult string

const (
	listOK           listResult = "ok"
	listForbidden    listResult = "forbidden"
	listTooLarge     listResult = "tooLarge"
	listTruncated    listResult = "truncated"
	listNotInstalled listResult = "notInstalled"
	listError        listResult = "error"
)

func accessDenied(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err)
}

func classifyList(err error) listResult {
	switch {
	case err == nil:
		return listOK
	case errors.Is(err, errTruncatedPage):
		return listTruncated
	case accessDenied(err):
		return listForbidden
	case errors.Is(err, store.ErrTooLarge):
		return listTooLarge
	case apierrors.IsNotFound(err):
		return listNotInstalled
	default:
		return listError
	}
}

type sourceHealth struct {
	mu          sync.Mutex
	lists       map[*sourceKind]listResult
	watchDenied map[*sourceKind]bool
	changed     chan struct{}
}

func (h *sourceHealth) signalLocked() {
	if h.changed != nil {
		close(h.changed)
		h.changed = nil
	}
}

func (h *sourceHealth) replaceLists(lists map[*sourceKind]listResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if maps.Equal(h.lists, lists) {
		return
	}
	h.lists = lists
	for kind := range h.watchDenied {
		if _, active := lists[kind]; !active {
			delete(h.watchDenied, kind)
		}
	}
	h.signalLocked()
}

func (h *sourceHealth) setWatchDenied(kind *sourceKind, denied bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.watchDenied[kind] == denied {
		return
	}
	if denied {
		if h.watchDenied == nil {
			h.watchDenied = make(map[*sourceKind]bool)
		}
		h.watchDenied[kind] = true
	} else {
		delete(h.watchDenied, kind)
	}
	h.signalLocked()
}

var healthKinds = slices.Concat([]*sourceKind{gatewayKind}, sourceKinds)

func (c *Controller) publishListHealth(lists map[*sourceKind]listResult) {
	if c.metrics != nil {
		for _, kind := range healthKinds {
			if _, selected := lists[kind]; !selected {
				c.metrics.objects.DeleteLabelValues(kind.label)
			}
		}
	}
	c.health.replaceLists(lists)
}

// Problems reports list failures and watch denials for the kinds the zone
// requests, and whether all of those kinds have been checked this term.
func (c *Controller) Problems(spec *v1alpha1.ExternalSpec) ([]string, bool) {
	c.health.mu.Lock()
	defer c.health.mu.Unlock()
	var problems []string
	checked := true
	for _, kind := range healthKinds {
		if !enabled(spec, kind.family) {
			continue
		}
		var reason string
		switch c.health.lists[kind] {
		case listOK:
			if c.health.watchDenied[kind] {
				reason = "watch denied; allow watch on " + kind.gvr.GroupResource().String() + " (using periodic lists)"
			}
		case listForbidden:
			reason = "list denied; allow list on " + kind.gvr.GroupResource().String()
		case listTooLarge:
			reason = "list exceeds the 4,096-object or 32 MiB per-kind cap"
		case listTruncated:
			reason = errTruncatedPage.Error()
		case listNotInstalled:
			reason = "not served by the Kubernetes API"
		case listError:
			reason = "list failed; see the elected replica's logs"
		default:
			checked = false
		}
		if reason != "" {
			problems = append(problems, kind.kind+": "+reason)
		}
	}
	return problems, checked
}

// HealthChanged returns the current close-and-replace notification channel.
func (c *Controller) HealthChanged() <-chan struct{} {
	c.health.mu.Lock()
	defer c.health.mu.Unlock()
	if c.health.changed == nil {
		c.health.changed = make(chan struct{})
	}
	return c.health.changed
}

type sourceMetrics struct {
	lists   *prometheus.CounterVec
	objects *prometheus.GaugeVec
}

func registerSourceMetrics(reg prometheus.Registerer) (*sourceMetrics, error) {
	if reg == nil {
		return nil, nil
	}
	m := &sourceMetrics{
		lists: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_external_zone_source_list_total",
			Help: "External-zone source list passes by Kubernetes kind and result.",
		}, []string{"kind", "result"}),
		objects: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_external_zone_source_objects",
			Help: "Objects inspected in the latest attempted list, including a cap-triggering object but not an undecodable page.",
		}, []string{"kind"}),
	}
	for _, metric := range []prometheus.Collector{m.lists, m.objects} {
		if err := reg.Register(metric); err != nil {
			return nil, fmt.Errorf("external zones: register metrics: %w", err)
		}
	}
	return m, nil
}

func (m *sourceMetrics) recordList(kind *sourceKind, result listResult, observed int) {
	if m != nil {
		m.lists.WithLabelValues(kind.label, string(result)).Inc()
		m.objects.WithLabelValues(kind.label).Set(float64(observed))
	}
}

package auditlog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/audit/auditkeys"
)

// Target defaults.
const (
	// DefaultMaxBytes caps the encoded events of one batch, far below the
	// apiserver's 1.5 MiB object limit.
	DefaultMaxBytes = 512 << 10
	// DefaultHourlyBudget is how many denied or failed events per hour a
	// node stores in full; the rest are stored as stubs (no documents,
	// reason or request), so a flood of audited requests cannot fill etcd.
	// Successful actions are always stored in full: they are bounded by
	// real changes, and a flood of denials must not blank them.
	DefaultHourlyBudget = 2000
	// maxEvents is the CRD's limit on events per batch.
	maxEvents = 1000
	// maxFilterAnnotation caps the kinds and actors annotations.
	maxFilterAnnotation = 2048
)

// Target writes AuditLog batches; it implements audit.Target.
type Target struct {
	// Client creates and lists AuditLogs (a scheme with audit.kudurru.io).
	Client client.Client
	// Namespace is the release namespace.
	Namespace string
	// Keys supplies independent operator-provisioned trust in production.
	Keys *auditkeys.Source
	// SealKey is an explicit legacy test/import seam.
	// SealKey seals every batch (SealKey of the encryption key); nil
	// leaves batches unsealed.
	SealKey []byte
	// MaxBytes and HourlyBudget default to DefaultMaxBytes and
	// DefaultHourlyBudget.
	MaxBytes, HourlyBudget int
	// Now is the clock; default time.Now.
	Now func() time.Time

	mu         sync.Mutex
	hour       time.Time
	usedInHour int
	// charged is the last event id charged per source, so a batch retried
	// after a failed create is not charged again; stubFrom is the first id
	// of a source stored as a stub this hour, so a retry gets the same
	// answer.
	charged  map[string]int64
	stubFrom map[string]int64
}

var _ audit.Target = (*Target)(nil)

// Cursor implements audit.Target: the last id of the source's batches.
func (t *Target) Cursor(ctx context.Context, src audit.Source) (int64, error) {
	var list metav1.PartialObjectMetadataList
	list.SetGroupVersionKind(auditv1.GroupVersion.WithKind("AuditLogList"))
	if err := t.Client.List(ctx, &list, client.InNamespace(t.Namespace), client.MatchingLabels{auditv1.LabelSource: labelValue(src.SourceID())}); err != nil {
		return 0, fmt.Errorf("auditlog: list batches of %s: %w", src.SourceID(), err)
	}
	var last int64
	for i := range list.Items {
		if id, err := strconv.ParseInt(list.Items[i].Annotations[auditv1.AnnotationLastID], 10, 64); err == nil && id > last {
			last = id
		}
	}
	return last, nil
}

// Append implements audit.Target: one or more batches of at most
// MaxBytes of events each.
func (t *Target) Append(ctx context.Context, src audit.Source, evs []audit.Event) error {
	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	node := src.Node()
	var batch []auditv1.AuditLogEvent
	size, stubs := 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := t.create(ctx, src, batch, stubs)
		batch, size, stubs = nil, 0, 0
		return err
	}
	for _, ev := range evs {
		e := fromEvent(node, ev, !t.keep(src.SourceID(), ev))
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if len(batch) > 0 && (size+len(b) > maxBytes || len(batch) == maxEvents) {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, e)
		size += len(b)
		if e.Stub {
			stubs++
		}
	}
	return flush()
}

// keep reports whether ev is stored in full. A successful action always
// is; a denied or failed one takes one event from the hour's budget,
// once (a retried event gets its earlier answer), and is stored as a stub
// when the budget is spent.
func (t *Target) keep(source string, ev audit.Event) bool {
	if ev.Result == audit.ResultSuccess {
		return true
	}
	budget := t.HourlyBudget
	if budget <= 0 {
		budget = DefaultHourlyBudget
	}
	now := time.Now
	if t.Now != nil {
		now = t.Now
	}
	hour := now().Truncate(time.Hour)
	t.mu.Lock()
	defer t.mu.Unlock()
	if !hour.Equal(t.hour) || t.charged == nil {
		t.hour, t.usedInHour, t.stubFrom = hour, 0, map[string]int64{}
	}
	if t.charged == nil {
		t.charged = map[string]int64{}
	}
	if ev.ID <= t.charged[source] {
		from := t.stubFrom[source]
		return from == 0 || ev.ID < from
	}
	t.charged[source] = ev.ID
	if t.usedInHour >= budget {
		if t.stubFrom[source] == 0 {
			t.stubFrom[source] = ev.ID
		}
		return false
	}
	t.usedInHour++
	return true
}

func (t *Target) create(ctx context.Context, src audit.Source, events []auditv1.AuditLogEvent, stubs int) error {
	first, last := events[0], events[len(events)-1]
	spec := auditv1.AuditLogSpec{
		Node: src.Node(), Source: src.SourceID(), FirstID: first.ID, LastID: last.ID, Count: len(events),
		From: first.Time, Until: last.Time, PrevHash: first.PrevHash, Hash: last.Hash, Stubs: stubs, Events: events,
	}
	for _, e := range events {
		if e.Time.Before(&spec.From) {
			spec.From = e.Time
		}
		if spec.Until.Before(&e.Time) {
			spec.Until = e.Time
		}
	}
	if t.Keys != nil {
		ring, _, err := t.Keys.Load(ctx)
		if err != nil {
			return err
		}
		spec.Seal, err = ring.Sign(spec)
		if err != nil {
			return err
		}
	} else if len(t.SealKey) > 0 {
		s, err := seal(t.SealKey, spec)
		if err != nil {
			return err
		}
		spec.Seal = s
	}
	obj := &auditv1.AuditLog{
		ObjectMeta: metav1.ObjectMeta{
			Name:      batchName(src.Node(), src.SourceID(), first.ID),
			Namespace: t.Namespace,
			Labels:    map[string]string{auditv1.LabelSource: labelValue(src.SourceID()), auditv1.LabelNode: labelValue(src.Node())},
			Annotations: map[string]string{
				auditv1.AnnotationFrom:   spec.From.UTC().Format(time.RFC3339Nano),
				auditv1.AnnotationUntil:  spec.Until.UTC().Format(time.RFC3339Nano),
				auditv1.AnnotationLastID: strconv.FormatInt(last.ID, 10),
			},
		},
		Spec: spec,
	}
	obj.SetGroupVersionKind(auditv1.GroupVersion.WithKind("AuditLog"))
	kinds, actors := map[string]bool{}, map[string]bool{}
	for _, e := range events {
		kinds[e.Resource.Kind] = true
		actors[toEvent(&spec, &e).Actor.String()] = true
	}
	if v := joinSet(kinds); len(v) <= maxFilterAnnotation {
		obj.Annotations[auditv1.AnnotationKinds] = v
	}
	if v := joinSet(actors); len(v) <= maxFilterAnnotation {
		obj.Annotations[auditv1.AnnotationActors] = v
	}
	if err := t.Client.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("auditlog: create %s: %w", obj.Name, err)
	}
	return nil
}

// batchName is "<node>-<source[:8]>-<first id>": deterministic, so a
// retried create finds the batch it already wrote.
func batchName(node, source string, first int64) string {
	n := strings.ToLower(node)
	if len(n) > 200 || len(validation.IsDNS1123Subdomain(n)) > 0 {
		n = "node-" + shortHash(node)
	}
	return fmt.Sprintf("%s-%s-%d", n, short(source), first)
}

// labelValue is v when it is a valid label value, else a hash of it.
func labelValue(v string) string {
	if len(validation.IsValidLabelValue(v)) == 0 {
		return v
	}
	return "h-" + shortHash(v)
}

func short(source string) string {
	s := strings.ToLower(strings.ReplaceAll(source, "-", ""))
	if len(s) > 8 {
		s = s[:8]
	}
	return s
}

func shortHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:8])
}

func joinSet(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

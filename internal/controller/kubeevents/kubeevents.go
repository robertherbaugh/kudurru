// Package kubeevents records the controller's operational transitions as
// Kubernetes core/v1 Events on the kudurru objects (dev-docs/design-record.md
// §5.1), so `kubectl describe zone home` shows SerialBumped, Duplicated,
// OutsideZone and Invalid.
//
// The serve command constructs a Sink only when the store is the kube
// driver. Events for cluster-scoped kinds (DNSZone) land in the "default"
// namespace, as they do with client-go's recorder.
package kubeevents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/record/util"
	"k8s.io/client-go/tools/reference"
	"k8s.io/utils/clock"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/controller"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// DefaultComponent is the Event source component (reportingController).
const DefaultComponent = "kudurru-controller"

const (
	queueCapacity = 128
	writeTimeout  = 5 * time.Second
)

// Options configures a Sink.
type Options struct {
	// Component is the event source; default DefaultComponent.
	Component string
	// Host is the reporting instance; default the hostname (pod name).
	Host string
	// Scheme resolves object kinds for involvedObject; default a scheme
	// with kudurru.io/v1alpha1.
	Scheme     *runtime.Scheme
	Registerer prometheus.Registerer
	Logger     *slog.Logger
}

// Sink uses one bounded queue and one API writer. The writer uses client-go's
// event correlator for aggregation and spam filtering without a broadcaster
// that could silently discard items from another queue.
type Sink struct {
	scheme *runtime.Scheme
	source corev1.EventSource
	queue  chan corev1.Event
	drops  *prometheus.CounterVec
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

var _ controller.EventSink = (*Sink)(nil)

// New starts a worker that writes Events through events.
func New(events typedcorev1.EventsGetter, opts Options) (*Sink, error) {
	if events == nil {
		return nil, errors.New("kubeevents: nil events client")
	}
	if opts.Component == "" {
		opts.Component = DefaultComponent
	}
	if opts.Host == "" {
		opts.Host, _ = os.Hostname()
	}
	if opts.Scheme == nil {
		opts.Scheme = runtime.NewScheme()
		if err := v1alpha1.AddToScheme(opts.Scheme); err != nil {
			return nil, err
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	drops := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kudurru_controller_events_dropped_total",
		Help: "Kubernetes controller Events dropped due to queue overflow, delivery errors, or invalid input.",
	}, []string{"reason"})
	if opts.Registerer != nil {
		if err := opts.Registerer.Register(drops); err != nil {
			return nil, fmt.Errorf("kubeevents: register dropped events: %w", err)
		}
	}
	for _, reason := range []string{"queue_full", "write_failed", "invalid_event"} {
		drops.WithLabelValues(reason)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink := &Sink{
		scheme: opts.Scheme, source: corev1.EventSource{Component: opts.Component, Host: opts.Host},
		queue: make(chan corev1.Event, queueCapacity), drops: drops, logger: opts.Logger,
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	go sink.run(events)
	return sink, nil
}

// NewForConfig is New with a core/v1 client built from cfg (the same
// rest.Config the kube store driver uses).
func NewForConfig(cfg *rest.Config, opts Options) (*Sink, error) {
	c, err := typedcorev1.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubeevents: client: %w", err)
	}
	return New(c, opts)
}

// Event implements controller.EventSink.
func (s *Sink) Event(obj store.Object, eventType, reason, message string) {
	if s.ctx.Err() != nil {
		return
	}
	ref, err := reference.GetReference(s.scheme, obj)
	if err != nil || !util.ValidateEventType(eventType) {
		s.drops.WithLabelValues("invalid_event").Inc()
		return
	}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	now := metav1.Now()
	event := corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: namespace, Name: util.GenerateEventName(ref.Name, now.UnixNano())},
		InvolvedObject: *ref, Type: eventType, Reason: reason, Message: message,
		Source: s.source, ReportingController: s.source.Component, ReportingInstance: s.source.Host,
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	select {
	case s.queue <- event:
	default:
		s.drops.WithLabelValues("queue_full").Inc()
	}
}

// Shutdown cancels the writer and waits for it to exit without flushing the queue.
func (s *Sink) Shutdown() {
	s.cancel()
	<-s.done
}

func (s *Sink) run(events typedcorev1.EventsGetter) {
	defer close(s.done)
	correlator := record.NewEventCorrelator(clock.RealClock{})
	for {
		select {
		case <-s.ctx.Done():
			return
		case event := <-s.queue:
			if s.ctx.Err() != nil {
				return
			}
			if err := s.deliver(events, correlator, &event); err != nil && s.ctx.Err() == nil {
				s.drops.WithLabelValues("write_failed").Inc()
				s.logger.Warn("Kubernetes controller Event dropped", "namespace", event.Namespace, "name", event.Name, "reason", event.Reason, "err", err)
			}
		}
	}
}

func (s *Sink) deliver(events typedcorev1.EventsGetter, correlator *record.EventCorrelator, event *corev1.Event) error {
	result, err := correlator.EventCorrelate(event)
	if err != nil || result.Skip {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, writeTimeout)
	defer cancel()
	client := events.Events(result.Event.Namespace)
	var stored *corev1.Event
	if result.Event.Count > 1 {
		stored, err = client.Patch(ctx, result.Event.Name, types.StrategicMergePatchType, result.Patch, metav1.PatchOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if stored == nil {
		result.Event.ResourceVersion = ""
		stored, err = client.Create(ctx, result.Event, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}
	correlator.UpdateState(stored)
	return nil
}

// Package kubeevents records the controller's operational transitions as
// Kubernetes core/v1 Events on the kudurru objects (dev-docs/design-record.md
// §5.1), so `kubectl describe zone home` shows SerialBumped, Duplicated,
// OutsideZone and Invalid.
//
// It is the only part of the controller that imports client-go's event
// machinery: internal/controller depends on its EventSink interface alone,
// and the serve command constructs a Sink only when the store is the kube
// driver. Events for cluster-scoped kinds (DNSZone) land in the "default"
// namespace, as client-go's recorder does for every cluster-scoped object.
package kubeevents

import (
	"errors"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/controller"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// DefaultComponent is the Event source component (reportingController).
const DefaultComponent = "kudurru-controller"

// Options configures a Sink.
type Options struct {
	// Component is the event source; default DefaultComponent.
	Component string
	// Host is the reporting instance; default the hostname (pod name).
	Host string
	// Scheme resolves object kinds for involvedObject; default a scheme
	// with kudurru.io/v1alpha1.
	Scheme *runtime.Scheme
}

// Sink is a controller.EventSink backed by a client-go event recorder. Its
// broadcaster aggregates and rate-limits repeated events and drops (never
// blocks) when the apiserver is slow, so reconcilers are never held up.
type Sink struct {
	broadcaster record.EventBroadcaster
	recorder    record.EventRecorder
}

var _ controller.EventSink = (*Sink)(nil)

// New starts a broadcaster that writes Events through events.
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
	b := record.NewBroadcaster()
	b.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: events.Events("")})
	return &Sink{
		broadcaster: b,
		recorder:    b.NewRecorder(opts.Scheme, corev1.EventSource{Component: opts.Component, Host: opts.Host}),
	}, nil
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
	s.recorder.Event(obj, eventType, reason, message)
}

// Shutdown stops the broadcaster; queued events may be dropped.
func (s *Sink) Shutdown() { s.broadcaster.Shutdown() }

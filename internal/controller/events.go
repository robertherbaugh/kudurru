package controller

import (
	"context"
	"log/slog"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// Event types, as in Kubernetes core/v1 Events.
const (
	EventNormal  = "Normal"
	EventWarning = "Warning"
)

// Event reasons the reconcilers emit (docs/architecture.md §3.4, §5.1).
const (
	EventSerialBumped = "SerialBumped"
	EventDuplicated   = "Duplicated"
	EventOutsideZone  = "OutsideZone"
	EventInvalid      = "Invalid"
	EventOccluded     = "Occluded"
)

// EventSink receives operational transitions. Implementations must be safe
// for concurrent use and must not block: reconcilers call Event inline.
// The kube driver's sink (internal/controller/kubeevents) records
// Kubernetes Events; LogSink is the default everywhere else.
type EventSink interface {
	Event(obj store.Object, eventType, reason, message string)
}

// LogSink writes events as structured log lines.
type LogSink struct {
	Logger *slog.Logger
}

// Event implements EventSink.
func (s LogSink) Event(obj store.Object, eventType, reason, message string) {
	l := s.Logger
	if l == nil {
		l = slog.Default()
	}
	level := slog.LevelInfo
	if eventType == EventWarning {
		level = slog.LevelWarn
	}
	l.Log(context.Background(), level, "event", "object", store.KeyOf(obj).String(), "type", eventType, "reason", reason, "message", message)
}

// RecordedEvent is one event captured by a MemorySink.
type RecordedEvent struct {
	Key     store.Key
	Type    string
	Reason  string
	Message string
}

// MemorySink records events in memory; tests and the measurement harness
// use it.
type MemorySink struct {
	mu     sync.Mutex
	events []RecordedEvent
}

// Event implements EventSink.
func (s *MemorySink) Event(obj store.Object, eventType, reason, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, RecordedEvent{Key: store.KeyOf(obj), Type: eventType, Reason: reason, Message: message})
}

// Events returns a copy of the recorded events.
func (s *MemorySink) Events() []RecordedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RecordedEvent(nil), s.events...)
}

// multiSink fans out to several sinks.
type multiSink []EventSink

func (m multiSink) Event(obj store.Object, eventType, reason, message string) {
	for _, s := range m {
		s.Event(obj, eventType, reason, message)
	}
}

// MultiSink returns a sink that forwards every event to each of sinks, e.g.
// a kubeevents.Sink and a LogSink.
func MultiSink(sinks ...EventSink) EventSink { return multiSink(sinks) }

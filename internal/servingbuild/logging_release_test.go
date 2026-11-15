package servingbuild

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// idleSink is an additional query-log sink that must never be opened: no
// event reaches a candidate that is not published.
type idleSink struct{ opens atomic.Int64 }

func (*idleSink) Name() string { return "idle" }
func (s *idleSink) Open(context.Context, json.RawMessage, prometheus.Registerer) error {
	s.opens.Add(1)
	return nil
}
func (*idleSink) Write([]*querylog.Event) error { return nil }
func (*idleSink) Flush(context.Context) error   { return nil }
func (*idleSink) Close() error                  { return nil }

// TestCandidateReturnsItsQueryLogSinkReferences: the logging route of a
// candidate holds one reference on each additional query-log sink (D371).
// A candidate that is prepared and then refused must return them itself:
// nothing else knows the route, and a sink that keeps a reference keeps its
// queue, its metric series and, once started, its goroutine and connection
// until the process ends, even after it was removed from the settings. A
// candidate that is accepted hands the reference to its bundle, which
// returns it through the logging component's release hook.
func TestCandidateReturnsItsQueryLogSinkReferences(t *testing.T) {
	f := newFixture(t)
	f.stop()
	image, token, err := f.store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	sink := &idleSink{}
	collector := querylog.NewAsync(&idleSink{}, 16, 1, time.Second, logger)
	host := querylog.NewSinkHost(querylog.SinkHostOptions{Collector: collector, Logger: logger, Factories: map[string]querylog.SinkFactory{
		"test": func(querylog.SinkSpec) (querylog.Sink, error) { return sink, nil },
	}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = collector.Close(ctx)
		_ = host.Close(ctx)
	})
	var routes atomic.Int64
	prepareLogging := func(context.Context, config.Querylog) (*querylog.Captured, error) {
		set, err := host.Prepare([]querylog.SinkSpec{{Name: "export", Type: "test"}})
		if err != nil {
			return nil, err
		}
		routes.Add(1)
		return collector.PrepareCapturedSinks(&idleSink{}, nil, true, 1, nil, set), nil
	}
	referenced := func() bool {
		st := host.Status()
		return len(st) == 1 && st[0].Name == "export"
	}
	// A sink without references retires behind the collector's queue, so
	// the host forgets it a moment after the release.
	forgotten := func() bool {
		deadline := time.Now().Add(5 * time.Second)
		for len(host.Status()) != 0 {
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(time.Millisecond)
		}
		return true
	}

	for name, capture := range map[string]func(ctx context.Context, image *replication.Image) (Dependencies, error){
		// Builder.Prepare refuses after the semantic owners are complete.
		"dependency changed after its capture": func(ctx context.Context, image *replication.Image) (Dependencies, error) {
			deps, err := f.builder.opts.Capture(ctx, image)
			f.epoch.Add(1)
			return deps, err
		},
		// prepareSemantic refuses right after it prepared the route.
		"catalog resource does not match the settings": func(ctx context.Context, image *replication.Image) (Dependencies, error) {
			deps, err := f.builder.opts.Capture(ctx, image)
			deps.Catalog = CatalogResource{}
			return deps, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := f.builder.opts
			opts.PrepareLogging, opts.Capture = prepareLogging, capture
			b, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			before := routes.Load()
			prepared, err := b.Prepare(t.Context(), serving.Candidate{Image: image, Token: token})
			if !errors.Is(err, serving.ErrDependency) || len(prepared.Components) != 0 {
				t.Fatalf("the candidate was not refused: %v", err)
			}
			if routes.Load() != before+1 {
				t.Fatal("the refusal came before the logging route was prepared: this case proves nothing")
			}
			if !forgotten() {
				t.Fatalf("a refused candidate kept its additional-sink references: %+v", host.Status())
			}
		})
	}

	opts := f.builder.opts
	opts.PrepareLogging = prepareLogging
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := b.Prepare(t.Context(), serving.Candidate{Image: image, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	if !referenced() {
		t.Fatalf("an accepted candidate does not hold its sink: %+v", host.Status())
	}
	released := false
	for _, component := range prepared.Components {
		if component.Name == "logging" && component.Release != nil {
			route, ok := component.Value.(*querylog.Captured)
			if !ok || !route.Enabled() {
				t.Fatalf("logging component is not the prepared route: %T", component.Value)
			}
			component.Release()
			released = forgotten()
			continue
		}
		if component.Release != nil {
			component.Release()
		}
	}
	if !released {
		t.Fatalf("the logging component's release hook did not return the sink references: %+v", host.Status())
	}
	if got := sink.opens.Load(); got != 0 {
		t.Fatalf("a sink that received no event was opened %d times", got)
	}
}

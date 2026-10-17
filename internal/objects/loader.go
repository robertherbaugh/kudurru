package objects

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// Defaults of Options.
const (
	DefaultInlineSource = "kudurru.yaml"
	DefaultNamespace    = "default"
	DefaultDebounce     = 500 * time.Millisecond
	DefaultPollInterval = 60 * time.Second
)

// maxFileBytes bounds one object file.
const maxFileBytes = 64 << 20

// ErrStaticReadOnly is returned by Loader.CheckAPIWrite in the ha-static
// shape (dev-docs/design-record.md §4.4): every node carries identical files,
// so the API and UI are read-only for all kudurru.io objects, not only the
// file-managed ones, and twins cannot diverge through node-local writes.
var ErrStaticReadOnly = errors.New("objects: cluster.role is static; kudurru.io objects are read-only through the API (edit the object files on every node)")

// Options configures a Loader.
type Options struct {
	// Store receives the objects. Required.
	Store store.Store
	// Validator checks every document (ValidateUnstructured, strict unknown
	// fields). Default: one shared validator built from the embedded CRDs on
	// first use.
	Validator *validator.Validator
	// Inline are the objects embedded in kudurru.yaml (config.Config.Objects).
	Inline []map[string]any
	// InlineSource names the inline objects' source; their manager is
	// "file:<InlineSource>". Default "kudurru.yaml".
	InlineSource string
	// Dir is the objects directory (config.Config.ObjectsDir); "" disables
	// it. Every *.yaml file directly inside is a source named
	// "<base(Dir)>/<file>", e.g. "objects.d/zones.yaml"; sub-directories,
	// dot files and other extensions are ignored. A missing directory is an
	// empty one.
	Dir string
	// DefaultNamespace is used for namespaced objects that do not name one,
	// as kubectl does with its current namespace. Default "default".
	DefaultNamespace string
	// Debounce coalesces bursts of file events. Default 500 ms.
	Debounce time.Duration
	// PollInterval re-runs the load even without file events (missed
	// events, network file systems, store drift). Default 60 s; negative
	// disables polling.
	PollInterval time.Duration
	// Registerer receives the kudurru_objects_* metrics; nil skips
	// registration.
	Registerer prometheus.Registerer
	// Logger receives load summaries; default slog.Default().
	Logger *slog.Logger
	// Static is cluster.role: static (ha-static, §4.4): the API must refuse
	// writes to every kudurru.io object. The loader itself is unaffected.
	Static bool
}

// Status is the loader state for /healthz and the UI.
type Status struct {
	Loaded       bool          `json:"loaded"`
	Watching     bool          `json:"watching"`
	Dir          string        `json:"dir,omitempty"`
	Sources      []string      `json:"sources"`
	Files        int           `json:"files"`
	Objects      int           `json:"objects"`
	LastLoad     time.Time     `json:"lastLoad,omitzero"`
	LastDuration time.Duration `json:"lastDurationNs,omitempty"`
	Errors       int           `json:"errors"`
	Warnings     int           `json:"warnings"`
	APIReadOnly  bool          `json:"apiReadOnly"`
	// Diagnoses of the last load (errors, warnings and ownership changes).
	Diagnoses []Diagnosis `json:"diagnoses,omitempty"`
}

// Loader reconciles file-managed objects (kudurru.yaml objects: and
// objectsDir/*.yaml) into the store and keeps them in sync with the files.
// It is safe for concurrent use; loads are serialised.
type Loader struct {
	opts     Options
	dirLabel string
	rec      *reconciler
	m        *metrics
	log      *slog.Logger
	trigger  chan struct{}

	loadMu sync.Mutex // serialises Load

	mu        sync.RWMutex
	status    Status
	lastDiags []string
}

// New builds a Loader. It does not touch the store or the files; call Load
// (or Run) for that.
func New(opts Options) (*Loader, error) {
	if opts.Store == nil {
		return nil, errors.New("objects: Options.Store is required")
	}
	if opts.Validator == nil {
		v, err := sharedValidator()
		if err != nil {
			return nil, err
		}
		opts.Validator = v
	}
	if opts.InlineSource == "" {
		opts.InlineSource = DefaultInlineSource
	}
	if opts.DefaultNamespace == "" {
		opts.DefaultNamespace = DefaultNamespace
	}
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	l := &Loader{
		opts:    opts,
		m:       newMetrics(opts.Registerer),
		log:     opts.Logger,
		trigger: make(chan struct{}, 1),
	}
	if opts.Dir != "" {
		opts.Dir = filepath.Clean(opts.Dir)
		l.opts.Dir = opts.Dir
		l.dirLabel = filepath.Base(opts.Dir)
		if l.dirLabel == opts.InlineSource {
			return nil, fmt.Errorf("objects: objects directory %q collides with the inline source name", opts.Dir)
		}
	}
	l.rec = &reconciler{
		st:        opts.Store,
		v:         opts.Validator,
		defaultNS: opts.DefaultNamespace,
		manager:   store.ManagerFile,
		fileMode:  true,
		log:       opts.Logger,
		seen:      map[store.Key]agreement{},
	}
	l.status = Status{Dir: opts.Dir, APIReadOnly: opts.Static}
	return l, nil
}

// Manager returns the managedBy value of objects declared in a source
// name as reported in Diagnosis.Source ("objects.d/zones.yaml").
func Manager(source string) string { return store.ManagerFile(source) }

// Load reads every source once and reconciles the store: each declared
// object is applied with Manager file:<source>, objects owned by a file
// that no longer declares them are deleted, and every problem becomes a
// Diagnosis instead of aborting the load. Sources that could not be read
// or parsed completely keep the objects they own.
func (l *Loader) Load(ctx context.Context) Report {
	l.loadMu.Lock()
	defer l.loadMu.Unlock()

	srcs, brokenPrefixes, readDiags := l.readSources()
	rep := l.rec.run(ctx, srcs, brokenPrefixes, readDiags)
	l.m.observe(rep, len(srcs))

	l.mu.Lock()
	l.status.Loaded = true
	l.status.Sources = rep.Sources
	l.status.Files = len(srcs)
	l.status.Objects = rep.Objects
	l.status.LastLoad = rep.Started.Add(rep.Duration)
	l.status.LastDuration = rep.Duration
	l.status.Errors = rep.Count(SeverityError)
	l.status.Warnings = rep.Count(SeverityWarning)
	l.status.Diagnoses = rep.Diagnoses
	changed := l.diagsChanged(rep.Diagnoses)
	l.mu.Unlock()

	l.logReport(rep, changed)
	return rep
}

// diagsChanged reports whether the diagnoses differ from the previous
// load, so a 60 s poll does not repeat the same warnings. Callers hold mu.
func (l *Loader) diagsChanged(diags []Diagnosis) bool {
	cur := make([]string, len(diags))
	for i, d := range diags {
		cur[i] = string(d.Severity) + " " + d.String()
	}
	changed := !slices.Equal(cur, l.lastDiags)
	l.lastDiags = cur
	return changed
}

func (l *Loader) logReport(rep Report, diagsChanged bool) {
	if diagsChanged {
		for _, d := range rep.Diagnoses {
			attrs := []any{"reason", string(d.Reason), "source", d.Source}
			if d.Document > 0 {
				attrs = append(attrs, "document", d.Document)
			}
			if d.Object != "" {
				attrs = append(attrs, "object", d.Object)
			}
			if d.Owner != "" {
				attrs = append(attrs, "owner", d.Owner)
			}
			if d.Hint != "" {
				attrs = append(attrs, "hint", d.Hint)
			}
			switch d.Severity {
			case SeverityError, SeverityWarning:
				l.log.Warn("objects: "+d.Message, attrs...)
			default:
				l.log.Info("objects: "+d.Message, attrs...)
			}
		}
	}
	if rep.Created+rep.Updated+rep.Deleted > 0 || diagsChanged {
		l.log.Info("objects: loaded", "sources", len(rep.Sources), "objects", rep.Objects,
			"created", rep.Created, "updated", rep.Updated, "unchanged", rep.Unchanged,
			"deleted", rep.Deleted, "skipped", rep.Skipped,
			"errors", rep.Count(SeverityError), "duration", rep.Duration)
	}
}

// readSources reads the inline objects and the directory, in apply order.
func (l *Loader) readSources() (srcs []source, brokenPrefixes []string, diags []Diagnosis) {
	if len(l.opts.Inline) > 0 {
		srcs = append(srcs, source{name: l.opts.InlineSource, decoded: decodeInline(l.opts.InlineSource, l.opts.Inline)})
	}
	if l.opts.Dir == "" {
		return srcs, nil, nil
	}
	entries, err := os.ReadDir(l.opts.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return srcs, nil, nil
	}
	if err != nil {
		diags = append(diags, Diagnosis{
			Severity: SeverityWarning, Reason: ReasonReadFailed, Source: l.dirLabel,
			Message: "cannot read the objects directory: " + err.Error(),
			Hint:    "objects already applied from it are kept until it can be read",
		})
		return srcs, []string{l.dirLabel + "/"}, diags
	}
	for _, e := range entries { // os.ReadDir sorts by name
		name := e.Name()
		if strings.HasPrefix(name, ".") || filepath.Ext(name) != ".yaml" {
			continue
		}
		src := path.Join(l.dirLabel, name)
		full := filepath.Join(l.opts.Dir, name)
		data, skip, err := readFile(full) // follows symlinks (ConfigMap mounts)
		if skip {
			continue
		}
		if err != nil {
			srcs = append(srcs, source{name: src, decoded: decoded{broken: true, diags: []Diagnosis{{
				Severity: SeverityError, Reason: ReasonReadFailed, Source: src,
				Message: err.Error(),
				Hint:    "objects already applied from this file are kept until it can be read",
			}}}})
			continue
		}
		srcs = append(srcs, source{name: src, decoded: decodeStream(src, data)})
	}
	return srcs, nil, diags
}

// readFile reads a regular file (or a symlink to one). skip is set for
// directories and other non-regular files.
func readFile(name string) (data []byte, skip bool, err error) {
	fi, err := os.Stat(name)
	if err != nil {
		return nil, false, err
	}
	if !fi.Mode().IsRegular() {
		return nil, true, nil
	}
	if fi.Size() > maxFileBytes {
		return nil, false, fmt.Errorf("file is larger than %d bytes", maxFileBytes)
	}
	data, err = os.ReadFile(name) //nolint:gosec // the operator chooses objectsDir (hostOnly)
	return data, false, err
}

// Run loads once and then watches the files until ctx is done.
func (l *Loader) Run(ctx context.Context) error {
	l.Load(ctx)
	return l.Watch(ctx)
}

// Trigger requests a reload without waiting for a file event (SIGHUP, an
// admin "reload objects" action). It never blocks.
func (l *Loader) Trigger() {
	select {
	case l.trigger <- struct{}{}:
	default:
	}
}

// Watch re-runs Load on file changes in Dir (fsnotify, debounced) and every
// PollInterval, until ctx is done. A directory that does not exist yet, or
// disappears, is picked up again on the next poll. Watch does not perform
// an initial load; serve calls Load first so the objects are in the store
// before zonecache starts.
func (l *Loader) Watch(ctx context.Context) error {
	var (
		events <-chan fsnotify.Event
		errs   <-chan error
		w      *fsnotify.Watcher
	)
	if l.opts.Dir != "" {
		var err error
		if w, err = fsnotify.NewWatcher(); err != nil {
			l.log.Warn("objects: file watching unavailable; polling only", "error", err, "interval", l.opts.PollInterval)
		} else {
			defer func() { _ = w.Close() }()
			events, errs = w.Events, w.Errors
		}
	}
	watching := l.addWatch(w)

	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	var poll <-chan time.Time
	if l.opts.PollInterval > 0 {
		t := time.NewTicker(l.opts.PollInterval)
		defer t.Stop()
		poll = t.C
	}
	for {
		select {
		case <-ctx.Done():
			l.setWatching(false)
			return nil
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if filepath.Clean(ev.Name) == l.opts.Dir && ev.Has(fsnotify.Remove|fsnotify.Rename) {
				watching = false
				l.setWatching(false)
			}
			debounce.Reset(l.opts.Debounce)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			l.log.Warn("objects: file watch error", "dir", l.opts.Dir, "error", err)
		case <-l.trigger:
			l.Load(ctx)
		case <-debounce.C:
			l.Load(ctx)
		case <-poll:
			if !watching {
				watching = l.addWatch(w)
			}
			l.Load(ctx)
		}
	}
}

// addWatch starts watching Dir; false when there is nothing to watch or
// the directory cannot be watched (yet).
func (l *Loader) addWatch(w *fsnotify.Watcher) bool {
	if w == nil {
		return false
	}
	if err := w.Add(l.opts.Dir); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			l.log.Warn("objects: cannot watch the objects directory; polling", "dir", l.opts.Dir, "error", err)
		}
		l.setWatching(false)
		return false
	}
	l.setWatching(true)
	return true
}

func (l *Loader) setWatching(v bool) {
	l.mu.Lock()
	l.status.Watching = v
	l.mu.Unlock()
}

// Status returns a copy of the loader state.
func (l *Loader) Status() Status {
	l.mu.RLock()
	defer l.mu.RUnlock()
	st := l.status
	st.Sources = slices.Clone(st.Sources)
	st.Diagnoses = slices.Clone(st.Diagnoses)
	return st
}

// Diagnoses returns the diagnoses of the last load.
func (l *Loader) Diagnoses() []Diagnosis { return l.Status().Diagnoses }

// Ready is a health.ReadyFunc: unready until the first load finished.
// Problems in object files never make the node unready (the last good
// objects keep serving); they are summarised in the message that /healthz
// shows for the component.
func (l *Loader) Ready() (bool, []string) {
	st := l.Status()
	if !st.Loaded {
		return false, []string{"initial load pending"}
	}
	var msgs []string
	for _, d := range st.Diagnoses {
		if d.Severity == SeverityInfo {
			continue
		}
		if len(msgs) == 3 {
			msgs = append(msgs, fmt.Sprintf("and %d more", st.Errors+st.Warnings-3))
			break
		}
		msgs = append(msgs, d.String())
	}
	return true, msgs
}

// APIReadOnly reports whether the API must refuse writes to every
// kudurru.io object (ha-static, §4.4).
func (l *Loader) APIReadOnly() bool { return l.opts.Static }

// CheckAPIWrite returns ErrStaticReadOnly when the API must refuse an
// object write (ha-static); the API maps it to 403/409 with the message.
func (l *Loader) CheckAPIWrite() error {
	if l.opts.Static {
		return ErrStaticReadOnly
	}
	return nil
}

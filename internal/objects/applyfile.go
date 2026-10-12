package objects

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

var (
	sharedOnce sync.Once
	sharedV    *validator.Validator
	sharedErr  error
)

// sharedValidator builds the process-wide validator once; compiling the
// CEL programs of every CRD is the expensive part.
func sharedValidator() (*validator.Validator, error) {
	sharedOnce.Do(func() { sharedV, sharedErr = validator.New() })
	return sharedV, sharedErr
}

// ApplyOption tunes ApplyFile and ApplyBytes.
type ApplyOption func(*applyConfig)

type applyConfig struct {
	manager   string
	v         *validator.Validator
	defaultNS string
	log       *slog.Logger
}

// WithManager sets the ApplyOptions.Manager (default store.ManagerAPI, the
// manager `kudurru apply -f` uses). file: managers are reserved for the
// Loader, which deletes file-managed objects no source declares.
func WithManager(m string) ApplyOption { return func(c *applyConfig) { c.manager = m } }

// WithValidator sets the validator (default: the shared one).
func WithValidator(v *validator.Validator) ApplyOption {
	return func(c *applyConfig) { c.v = v }
}

// WithDefaultNamespace sets the namespace for namespaced objects that do
// not name one (default "default").
func WithDefaultNamespace(ns string) ApplyOption {
	return func(c *applyConfig) { c.defaultNS = ns }
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) ApplyOption { return func(c *applyConfig) { c.log = l } }

// ApplyFile applies every object of the multi-document YAML file at path to
// st, once: the in-process equivalent of `kudurru apply -f` until the M3
// CLI talks to the API. Documents are validated like the Loader's; invalid
// documents, conflicts with other managers and store failures become
// Diagnoses in the report while the rest is applied. Nothing is deleted.
// Objects managed by "bootstrap" are taken over; objects of any other
// manager are left alone. The error is non-nil only when the file cannot
// be read or the options are unusable.
func ApplyFile(ctx context.Context, st store.Store, path string, opts ...ApplyOption) (Report, error) {
	data, skip, err := readFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("objects: %w", err)
	}
	if skip {
		return Report{}, fmt.Errorf("objects: %s is not a regular file", path)
	}
	return ApplyBytes(ctx, st, path, data, opts...)
}

// ApplyBytes is ApplyFile for data already in memory; name identifies it in
// diagnoses.
func ApplyBytes(ctx context.Context, st store.Store, name string, data []byte, opts ...ApplyOption) (Report, error) {
	cfg := applyConfig{manager: store.ManagerAPI, defaultNS: DefaultNamespace}
	for _, o := range opts {
		o(&cfg)
	}
	if st == nil {
		return Report{}, fmt.Errorf("objects: nil store")
	}
	if cfg.manager == "" {
		return Report{}, fmt.Errorf("objects: empty manager")
	}
	if cfg.v == nil {
		v, err := sharedValidator()
		if err != nil {
			return Report{}, err
		}
		cfg.v = v
	}
	if cfg.log == nil {
		cfg.log = slog.Default()
	}
	rec := &reconciler{
		st:        st,
		v:         cfg.v,
		defaultNS: cfg.defaultNS,
		manager:   func(string) string { return cfg.manager },
		log:       cfg.log,
	}
	return rec.run(ctx, []source{{name: name, decoded: decodeStream(name, data)}}, nil, nil), nil
}

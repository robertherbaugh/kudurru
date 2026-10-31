package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// CachedBackend retains the last confirmed shared document for offline startup.
// Regular reads and all writes still require the authoritative backend.
type CachedBackend struct {
	Backend Backend
	Path    string
	// AllowOffline optionally narrows which backend errors permit fallback.
	// In particular, an HTTP URL wrapper must not turn an identity refusal
	// into an availability failure merely by implementing net.Error.
	AllowOffline func(error) bool
}

func (b CachedBackend) Read(ctx context.Context) (Document, string, error) {
	d, rev, err := b.Backend.Read(ctx)
	if err != nil {
		return d, rev, err
	}
	if err := d.Validate(); err != nil {
		return d, rev, err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return d, rev, err
	}
	previous, readErr := os.ReadFile(b.Path)
	if readErr == nil && bytes.Equal(previous, raw) {
		return d, rev, nil
	}
	if err := os.MkdirAll(filepath.Dir(b.Path), 0o750); err != nil {
		return d, rev, err
	}
	file, err := os.CreateTemp(filepath.Dir(b.Path), ".ui-settings-*")
	if err != nil {
		return d, rev, err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.Write(raw); err != nil {
		_ = file.Close()
		return d, rev, err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return d, rev, err
	}
	if err = file.Close(); err != nil {
		return d, rev, err
	}
	if err = os.Rename(file.Name(), b.Path); err != nil {
		return d, rev, err
	}
	return d, rev, nil
}

// Save requires the authoritative backend; cached state never authorizes writes.
func (b CachedBackend) Save(ctx context.Context, d Document, rev string) (string, error) {
	return b.Backend.Save(ctx, d, rev)
}

// ReadStartup falls back only on transport or apiserver availability failures.
// Malformed state, authorization failures, and damaged local files fail closed.
func (b CachedBackend) ReadStartup(ctx context.Context) (Document, string, error) {
	d, rev, err := b.Read(ctx)
	if err == nil {
		return d, rev, nil
	}
	if b.AllowOffline != nil && !b.AllowOffline(err) {
		return d, rev, err
	}
	var network net.Error
	if !errors.As(err, &network) && !errors.Is(err, context.DeadlineExceeded) && !apierrors.IsServiceUnavailable(err) && !apierrors.IsTimeout(err) && !apierrors.IsServerTimeout(err) && !apierrors.IsTooManyRequests(err) {
		return d, rev, err
	}
	raw, readErr := os.ReadFile(b.Path)
	if errors.Is(readErr, os.ErrNotExist) {
		return Document{}, "", nil
	}
	if readErr != nil {
		return Document{}, "", readErr
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, "", fmt.Errorf("cached UI settings: %w", err)
	}
	return d, "offline", d.Validate()
}

// ReadStartup uses the local last-good document only when the backend supports it.
func ReadStartup(ctx context.Context, b Backend) (Document, string, error) {
	if cached, ok := b.(interface {
		ReadStartup(context.Context) (Document, string, error)
	}); ok {
		return cached.ReadStartup(ctx)
	}
	return b.Read(ctx)
}

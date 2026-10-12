// Package authtest provides the stores the identity-layer tests run
// against: the in-memory reference driver and a temporary SQLite state.db
// serving the internal kinds, so every behaviour is checked on both.
package authtest

import (
	"path/filepath"
	"sync"
	"testing"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

var (
	valOnce sync.Once
	val     *validator.Validator
	valErr  error
)

// Validator returns a process-wide CRD validator (compiling it is the
// slow part of opening a sqlite store).
func Validator(t testing.TB) *validator.Validator {
	t.Helper()
	valOnce.Do(func() { val, valErr = validator.New() })
	if valErr != nil {
		t.Fatal(valErr)
	}
	return val
}

// SQLite opens a temporary state.db with the internal kinds registered.
func SQLite(t testing.TB) store.Store {
	t.Helper()
	scheme, err := internalv1.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]store.Kind, 0, len(internalv1.Kinds))
	for _, k := range internalv1.Kinds {
		kinds = append(kinds, store.Kind(k))
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), sqlite.FileName), sqlite.Options{
		Validator: Validator(t), Scheme: scheme, InternalKinds: kinds, Identity: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// Memory returns an in-memory store.
func Memory(t testing.TB) store.Store {
	t.Helper()
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// Drivers runs fn once per driver as a subtest.
func Drivers(t *testing.T, fn func(t *testing.T, st store.Store)) {
	t.Helper()
	for _, d := range []struct {
		name string
		open func(testing.TB) store.Store
	}{{"memory", Memory}, {"sqlite", SQLite}} {
		t.Run(d.name, func(t *testing.T) { fn(t, d.open(t)) })
	}
}

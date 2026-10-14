package stack

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"syscall"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type unavailableIdentityStore struct {
	store.Store
	err error
}

func (s *unavailableIdentityStore) List(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error) {
	return store.ObjectList{}, s.err
}

func TestDeferredIdentityInitializationPreservesErrors(t *testing.T) {
	opts := Options{Config: loadConfig(t, testConfig), Store: &unavailableIdentityStore{Store: authtest.Memory(t), err: syscall.ECONNREFUSED}, Keys: keys.Keys{Current: make([]byte, 32)}, Out: io.Discard, Logger: slog.New(slog.DiscardHandler), RetryUnavailable: func(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }}
	opts.Config.Auth.Session.Secure = "invalid"
	if _, err := New(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "secure") {
		t.Fatalf("unavailable API must not hide invalid session config: %v", err)
	}
	opts.Config.Auth.Session.Secure = "never"
	s, err := New(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Users().List()) != 0 {
		t.Fatal("unavailable API must not manufacture an admin")
	}
	permanent := errors.New("sqlite: database disk image is malformed")
	opts.Store.(*unavailableIdentityStore).err = permanent
	if err := s.Run(t.Context()); !errors.Is(err, permanent) {
		t.Fatalf("deferred initialization hid a permanent store error: %v", err)
	}
	if _, err := New(t.Context(), opts); !errors.Is(err, permanent) {
		t.Fatalf("initialization hid a permanent store error: %v", err)
	}
}

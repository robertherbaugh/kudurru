package runtimeowners

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/requestlimit"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestRequestAdmissionActualCommandRollbackAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), sqlite.FileName)
	s := openOwnerStore(t, path)
	initializeAnchorOwners(t, s)
	before := anchorOwnerImage(t, s)
	fault := errors.New("test: request admission final refusal")
	for _, mode := range []string{"callback", "final", "clock", "cancel", "poison"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), requestlimit.Lifetime)
			defer cancel()
			now := time.Now().UTC()
			entered := false
			err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := requestlimit.Charge(ctx, tx, requestlimit.Start, "192.0.2.3", func() time.Time { return now }); err != nil {
					return err
				}
				if raw, err := tx.GetSection(ctx, requestlimit.Section, requestlimit.Key); err != nil || len(raw) == 0 {
					t.Fatal("charge target not staged", err)
				}
				entered = true
				switch mode {
				case "callback":
					return fault
				case "final":
					return tx.RequireCommitCheck(func(context.Context) error { return fault })
				case "clock":
					now = now.Add(-time.Hour)
				case "cancel":
					cancel()
				case "poison":
					_ = tx.PutSection(ctx, "invalid-section", replication.Entry{Key: "bad", Data: []byte(`{}`)})
				}
				return nil
			})
			want := fault
			switch mode {
			case "clock":
				want = requestlimit.ErrState
			case "cancel":
				want = context.Canceled
			case "poison":
				want = replication.ErrInvalid
			}
			if !entered || !errors.Is(err, want) {
				t.Fatal("staged target or exact refusal missing", err)
			}
			requireAnchorOwnerUnchanged(t, s, before)
		})
	}
	for range 5 {
		ctx, cancel := context.WithTimeout(t.Context(), requestlimit.Lifetime)
		err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			return requestlimit.Charge(ctx, tx, requestlimit.Start, "192.0.2.3", nil)
		})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	locked := anchorOwnerImage(t, s)
	if locked.Revision != before.Revision+5 || ValidateImage(locked) != nil {
		t.Fatal("charges did not commit canonical complete row")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openOwnerStore(t, path)
	ctx, cancel := context.WithTimeout(t.Context(), requestlimit.Lifetime)
	defer cancel()
	err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return requestlimit.Charge(ctx, tx, requestlimit.Start, "192.0.2.3", nil)
	})
	var refusal *loginlimit.Locked
	if !errors.As(err, &refusal) {
		t.Fatal("restart reset charged requests", err)
	}
	requireAnchorOwnerUnchanged(t, s, locked)
	// Invalid/unknown state cannot be staged or installed as a complete owner.
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, requestlimit.Section, replication.Entry{Key: requestlimit.Key, Data: []byte(`{"version":2}`)})
	})
	if !errors.Is(err, requestlimit.ErrState) {
		t.Fatal("unknown history accepted", err)
	}
	requireAnchorOwnerUnchanged(t, s, locked)
	bad := *locked
	bad.Payload = locked.Payload
	bad.Payload.Runtime = append(append([]replication.Entry{}, locked.Payload.Runtime...), replication.Entry{Key: requestlimit.Prefix + "foreign", Data: []byte(`{}`)})
	if ValidateImage(&bad) == nil {
		t.Fatal("foreign request budget prefix admitted")
	}
}

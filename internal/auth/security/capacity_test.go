package security

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func otherSecurityRows(ctx context.Context, tx *sqlite.CommandTx, count int, payload json.RawMessage) error {
	for n := range count {
		if err := tx.PutSection(ctx, "security", replication.Entry{Key: fmt.Sprintf("other-owner/%04d", n), Data: payload}); err != nil {
			return err
		}
	}
	return nil
}

func TestSecurityCapacityIncludesOtherOwnersAndRollsBack(t *testing.T) {
	for _, scenario := range []string{"initialize rows", "loaded rows", "staged rows", "staged bytes", "noop staged rows"} {
		t.Run(scenario, func(t *testing.T) {
			st := openSecurityStore(t, filepath.Join(t.TempDir(), "state.db"))
			if scenario != "initialize rows" {
				command(t, st, Initialize)
			}
			if scenario == "loaded rows" {
				command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
					return otherSecurityRows(ctx, tx, servingauth.MaxRows-1, json.RawMessage(`{}`))
				})
			}
			before, err := st.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if scenario == "initialize rows" {
					if err := otherSecurityRows(ctx, tx, servingauth.MaxRows, json.RawMessage(`{}`)); err != nil {
						return err
					}
					return Initialize(ctx, tx)
				}
				s, err := Load(ctx, tx)
				if err != nil {
					return err
				}
				if scenario != "noop staged rows" {
					if err := s.InvalidateGrant("viewer", time.Now()); err != nil {
						return err
					}
				}
				switch scenario {
				case "staged rows", "noop staged rows":
					if err := otherSecurityRows(ctx, tx, servingauth.MaxRows, json.RawMessage(`{}`)); err != nil {
						return err
					}
				case "staged bytes":
					payload, _ := json.Marshal(strings.Repeat("x", servingauth.MaxRowBytes-2))
					if err := otherSecurityRows(ctx, tx, servingauth.MaxSecurityBytes/len(payload)+1, payload); err != nil {
						return err
					}
				}
				return s.Commit(ctx)
			})
			if !errors.Is(err, servingauth.ErrBounds) {
				t.Fatal("combined section exceeded auth bounds", err)
			}
			after, err := st.ReadGeneration(t.Context())
			if err != nil || !after.Generation().Same(before.Generation()) {
				t.Fatal("capacity failure did not roll back entire command", err)
			}
		})
	}
}

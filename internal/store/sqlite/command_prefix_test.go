package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestSectionPrefixStagedReadsDetachedRowsAndOneCommit(t *testing.T) {
	s := openTemp(t)
	before := imageOf(t, s).Generation()
	var held *CommandTx
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		held = tx
		if err := tx.CheckSectionPrefixBounds(ctx, "runtime", "inventory/", 0, 0); err != nil {
			return err
		}
		for _, row := range []replication.Entry{
			{Key: "inventory/b", Data: json.RawMessage(`{"version":1}`)},
			{Key: "inventory/a", Data: json.RawMessage(`{"version":1}`)},
			{Key: "another-owner/a", Data: json.RawMessage(`{}`)},
		} {
			if err := tx.PutSection(ctx, "runtime", row); err != nil {
				return err
			}
		}
		if err := tx.PutSection(ctx, "security", replication.Entry{Key: "inventory/a", Data: json.RawMessage(`{}`)}); err != nil {
			return err
		}
		if _, err := tx.Apply(ctx, zone("staged", "staged.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			return err
		}
		if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "inventory/b", Data: json.RawMessage(`{"version":2}`)}); err != nil {
			return err
		}
		if err := tx.CheckSectionPrefixBounds(ctx, "runtime", "inventory/", 2, 26); err != nil {
			return err
		}
		rows, err := tx.ListSectionPrefix(ctx, "runtime", "inventory/")
		if err != nil {
			return err
		}
		if len(rows) != 2 || rows[0].Key != "inventory/a" || rows[1].Key != "inventory/b" || string(rows[1].Data) != `{"version":2}` {
			t.Fatal("prefix read missed ordered staged updates", rows)
		}
		clear(rows[0].Data)
		rows[1].Key = "modified"
		if err := tx.DeleteSection(ctx, "runtime", "inventory/b"); err != nil {
			return err
		}
		if err := tx.CheckSectionPrefixBounds(ctx, "runtime", "inventory/", 1, 13); err != nil {
			return err
		}
		rows, err = tx.ListSectionPrefix(ctx, "runtime", "inventory/")
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].Key != "inventory/a" || string(rows[0].Data) != `{"version":1}` {
			t.Fatal("returned rows alias storage or staged delete missed", rows)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after := imageOf(t, s)
	if after.Revision != before.Revision+1 || len(after.Payload.Runtime) != 2 || len(after.Payload.Security) != 1 || len(after.Payload.Objects) != 1 {
		t.Fatal("prefix reads broke command atomicity", after.Revision)
	}
	if err := held.CheckSectionPrefixBounds(t.Context(), "runtime", "inventory/", 257, 64<<20); !errors.Is(err, ErrTransactionEnded) {
		t.Fatal("retained preflight usable", err)
	}
	if _, err := held.ListSectionPrefix(t.Context(), "runtime", "inventory/"); !errors.Is(err, ErrTransactionEnded) {
		t.Fatal("retained list usable", err)
	}
}

func TestSectionPrefixLiteralPatternCharactersAndUnicodeBytes(t *testing.T) {
	for _, prefix := range []string{"inventory/%", "inventory/_", "inventory/\\", "inventory/猫😀/", "inventory/\x00", strings.Repeat("猫", 170) + "aa"} {
		t.Run(prefix, func(t *testing.T) {
			s := openTemp(t)
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				for _, key := range []string{prefix, "inventory/X", "Inventory/other", "other/inventory/%", "inventory/猫😁/"} {
					if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: key, Data: json.RawMessage(`"😀"`)}); err != nil {
						return err
					}
				}
				if err := tx.CheckSectionPrefixBounds(ctx, "runtime", prefix, 1, len(`"😀"`)); err != nil {
					return err
				}
				rows, err := tx.ListSectionPrefix(ctx, "runtime", prefix)
				if err != nil {
					return err
				}
				if len(rows) != 1 || rows[0].Key != prefix || string(rows[0].Data) != `"😀"` {
					t.Fatal("literal prefix selected foreign rows", rows)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSectionPrefixByteBudgetPoisonRollsBackObjectRowsAndAudit(t *testing.T) {
	s := openTemp(t)
	before := imageOf(t, s).Generation()
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		if _, err := tx.Apply(ctx, zone("rollback", "rollback.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			return err
		}
		if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "inventory/one", Data: json.RawMessage(`"😀"`)}); err != nil {
			return err
		}
		if err := tx.CheckSectionPrefixBounds(ctx, "runtime", "inventory/", 1, 6); err != nil {
			return err
		}
		if err := tx.CheckSectionPrefixBounds(ctx, "runtime", "inventory/", 1, 5); !errors.Is(err, replication.ErrTooLarge) {
			t.Fatal("bytes counted as Unicode characters", err)
		}
		if _, err := tx.ListSectionPrefix(ctx, "runtime", "inventory/"); !errors.Is(err, replication.ErrTooLarge) {
			t.Fatal("list ignored command poison", err)
		}
		return nil
	})
	if !errors.Is(err, replication.ErrTooLarge) || !imageOf(t, s).Generation().Same(before) {
		t.Fatal("ignored bounds failure committed", err)
	}
	audit, err := s.ReadAudit(t.Context(), AuditQuery{})
	if err != nil || len(audit) != 0 {
		t.Fatal("rolled-back prefix command leaked audit", audit, err)
	}
}

func TestSectionPrefixHistoricalPreflightRejectsBeforePayloadAllocation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rows, bytes int
		key, data   string
		large       int
	}{
		{name: "inventory-bytes", rows: 257, bytes: 64 << 20, key: "inventory/large", large: 64<<20 + 1},
		{name: "rows", rows: 0, bytes: 10, key: "inventory/one", data: `{}`},
		{name: "key-bytes", rows: 257, bytes: 64 << 20, key: "inventory/" + strings.Repeat("猫", 168), data: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemp(t)
			var err error
			if tc.large != 0 {
				_, err = s.w.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES ('runtime',?,CAST(zeroblob(?) AS TEXT))`, tc.key, tc.large)
			} else {
				_, err = s.w.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES ('runtime',?,?)`, tc.key, tc.data)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := s.Authority().Installed
			err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				_ = tx.CheckSectionPrefixBounds(ctx, "runtime", "inventory/", tc.rows, tc.bytes)
				return nil
			})
			if !errors.Is(err, replication.ErrTooLarge) || !before.Same(s.Authority().Installed) {
				t.Fatal("malformed historical preflight allowed no-op", err)
			}
		})
	}
}

func TestSectionPrefixForeignHistoricalCorruptionDoesNotEnterOwnerBudget(t *testing.T) {
	s := openTemp(t)
	for _, row := range []struct{ section, key, data string }{
		{"runtime", "inventory/one", `"😀"`},
		{"runtime", "other/" + strings.Repeat("猫", 171), "not JSON"},
		{"runtime", "other/invalid-utf8-" + string([]byte{0xff}), "not JSON"},
		{"security", "inventory/foreign", "not JSON"},
	} {
		if _, err := s.w.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, row.section, row.key, row.data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.w.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES ('runtime','other/large',CAST(zeroblob(?) AS TEXT))`, 64<<20+1); err != nil {
		t.Fatal(err)
	}
	before := s.Authority().Installed
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		if err := tx.CheckSectionPrefixBounds(ctx, "runtime", "inventory/", 1, 6); err != nil {
			return err
		}
		rows, err := tx.ListSectionPrefix(ctx, "runtime", "inventory/")
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(rows, []replication.Entry{{Key: "inventory/one", Data: json.RawMessage(`"😀"`)}}) {
			t.Fatal("foreign historical rows entered prefix read", rows)
		}
		if err := tx.CheckSectionPrefixBounds(ctx, "runtime", "missing/", 0, 0); err != nil {
			return err
		}
		empty, err := tx.ListSectionPrefix(ctx, "runtime", "missing/")
		if err != nil || empty == nil || len(empty) != 0 {
			t.Fatal("empty prefix result", empty, err)
		}
		return nil
	})
	if err != nil || !before.Same(s.Authority().Installed) {
		t.Fatal("foreign prefix blocked fenced read", err)
	}
}

func TestSectionPrefixInvalidArgumentsCancellationAndErrorRollback(t *testing.T) {
	for _, tc := range []struct {
		section, prefix string
		rows, bytes     int
	}{
		{"unknown", "inventory/", 257, 64 << 20}, {"runtime", "", 1, 100},
		{"runtime", strings.Repeat("猫", 171), 1, 100}, {"runtime", string([]byte{0xff}), 1, 100},
		{"runtime", "inventory/", -1, 100}, {"runtime", "inventory/", 1, -1},
		{"runtime", "inventory/", 100001, 100}, {"runtime", "inventory/", 1, 256<<20 + 1},
	} {
		t.Run(tc.section+tc.prefix, func(t *testing.T) {
			s := openTemp(t)
			before := imageOf(t, s).Generation()
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				if _, err := tx.Apply(ctx, zone("rollback", "rollback.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					return err
				}
				_ = tx.CheckSectionPrefixBounds(ctx, tc.section, tc.prefix, tc.rows, tc.bytes)
				return nil
			})
			if !errors.Is(err, replication.ErrInvalid) || !imageOf(t, s).Generation().Same(before) {
				t.Fatal("invalid preflight committed", err)
			}
			if !validSectionPrefix(tc.section, tc.prefix) {
				err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
					if _, err := tx.Apply(ctx, zone("rollback", "rollback.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
						return err
					}
					_, err := tx.ListSectionPrefix(ctx, tc.section, tc.prefix)
					return err
				})
				if !errors.Is(err, replication.ErrInvalid) || !imageOf(t, s).Generation().Same(before) {
					t.Fatal("invalid enumeration committed", err)
				}
			}
		})
	}
	for _, method := range []string{"check", "list"} {
		t.Run("canceled-"+method, func(t *testing.T) {
			s := openTemp(t)
			before := imageOf(t, s).Generation()
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				if _, err := tx.Apply(ctx, zone("rollback", "rollback.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					return err
				}
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				if method == "check" {
					_ = tx.CheckSectionPrefixBounds(canceled, "runtime", "inventory/", 257, 64<<20)
				} else {
					_, _ = tx.ListSectionPrefix(canceled, "runtime", "inventory/")
				}
				return nil
			})
			if !errors.Is(err, context.Canceled) || !imageOf(t, s).Generation().Same(before) {
				t.Fatal("ignored canceled read committed", err)
			}
		})
	}
}

func TestSectionPrefixSQLReadErrorsRollbackFullCommand(t *testing.T) {
	for _, method := range []string{"check", "list"} {
		t.Run(method, func(t *testing.T) {
			s := openTemp(t)
			before := imageOf(t, s).Generation()
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				if _, err := tx.Apply(ctx, zone("rollback", "rollback.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					return err
				}
				if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "inventory/staged", Data: json.RawMessage(`{}`)}); err != nil {
					return err
				}
				// Actual transactional schema fault; rollback restores the table too.
				if _, err := tx.tx.ExecContext(ctx, `DROP TABLE replication_sections`); err != nil {
					return err
				}
				if method == "check" {
					_ = tx.CheckSectionPrefixBounds(ctx, "runtime", "inventory/", 257, 64<<20)
					return nil
				}
				_, err := tx.ListSectionPrefix(ctx, "runtime", "inventory/")
				return err
			})
			if err == nil || !imageOf(t, s).Generation().Same(before) {
				t.Fatal("SQL read failure committed staged object/rows", err)
			}
			audit, err := s.ReadAudit(t.Context(), AuditQuery{})
			if err != nil || len(audit) != 0 {
				t.Fatal("SQL read failure leaked audit", audit, err)
			}
		})
	}
}

func TestSectionPrefixMalformedOwnerPayloadIsNotRepairedAndDecodeErrorRollsBack(t *testing.T) {
	s := openTemp(t)
	if _, err := s.w.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES ('runtime','inventory/broken','{broken')`); err != nil {
		t.Fatal(err)
	}
	before := s.Authority().Installed
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		if _, err := tx.Apply(ctx, zone("rollback", "rollback.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			return err
		}
		if err := tx.CheckSectionPrefixBounds(ctx, "runtime", "inventory/", 257, 64<<20); err != nil {
			return err
		}
		rows, err := tx.ListSectionPrefix(ctx, "runtime", "inventory/")
		if err != nil {
			return err
		}
		if len(rows) != 1 || string(rows[0].Data) != "{broken" {
			t.Fatal("historical payload silently repaired", rows)
		}
		var payload map[string]any
		return json.Unmarshal(rows[0].Data, &payload)
	})
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) || !before.Same(s.Authority().Installed) {
		t.Fatal("malformed owner row committed", err)
	}
	var objects, audits int
	if err := s.r.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM objects`).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if err := s.r.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if objects != 0 || audits != 0 {
		t.Fatal("owner decode failure leaked object/audit", objects, audits)
	}
}

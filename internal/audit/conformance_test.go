package audit_test

import (
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/auditstore"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/audit/audittest"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestConformanceMemory(t *testing.T) {
	audittest.Run(t, func(*testing.T) audittest.Store { return &audit.Memory{} })
}

func TestConformanceSQLite(t *testing.T) {
	audittest.Run(t, func(t *testing.T) audittest.Store {
		st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return auditstore.SQLite{Store: st}
	})
}

package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMigrationJournalSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := openAt(t, path, internalOptions)
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	r := &internal.MigrationRun{ObjectMeta: metav1.ObjectMeta{Name: "migration-test"}, Spec: internal.MigrationRunSpec{Source: "technitium", SourceURL: "https://dns.test", Actor: "admin", StartedAt: now, FinishedAt: &now, Status: "completed", Items: []internal.MigrationItem{{Kind: "DNSZone", Name: "home", Result: "created"}}, Zones: []internal.MigrationZone{{Name: "home", ZoneName: "home.test.", UID: "uid-1", OriginalNameservers: []string{"old.test."}, Nameservers: []string{"dns.test."}, Primary: "dns.test."}}, Cutovers: []internal.MigrationCutover{{StartedAt: now, FinishedAt: &now, Status: "completed", Nameservers: []string{"dns.test."}, Items: []internal.MigrationItem{{Kind: "DNSZone", Name: "home", Result: "updated"}}}}}}
	out, err := s.Apply(context.Background(), r, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, path, internalOptions)
	var got internal.MigrationRun
	if err := reopened.Get(context.Background(), store.KeyOf(out), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.(*internal.MigrationRun).Spec, got.Spec) || got.ResourceVersion != out.GetResourceVersion() {
		t.Fatal("journal changed after reopening database")
	}
}

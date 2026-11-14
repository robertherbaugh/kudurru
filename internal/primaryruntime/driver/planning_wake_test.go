package driver

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestPlanningRosterRetainsValidatedSourceAcrossSchedulingHints(t *testing.T) {
	f := setup(t)
	var reads atomic.Int64
	f.image.Validation.ValidateImage = func(*replication.Image) error { reads.Add(1); return nil }
	d, stop := f.start()
	defer stop()
	await(t, func() bool { return d.Status().Ready })
	before := reads.Load()
	// The real publication subscription and periodic retry ticks both wake
	// this driver. Neither changes the installed source or writer.
	for range 5 {
		f.pub.release()
		time.Sleep(20 * time.Millisecond)
	}
	if reads.Load() != before {
		t.Fatal("scheduling hints revalidated an unchanged complete source", before, reads.Load())
	}
	d.Wake()
	await(t, func() bool { return reads.Load() > before })
	before = reads.Load()
	f.zone("new", "new.example.")
	await(t, func() bool { return reads.Load() > before && f.active("new.example.", 1) })
	if status := d.Status(); !status.Ready || len(status.Zones) != 1 {
		t.Fatal("source change did not replace the complete planning roster", status)
	}
}

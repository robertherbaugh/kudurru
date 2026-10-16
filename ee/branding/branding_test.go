package branding

import (
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestProjectionRequiresOriginalFiniteEntitlement(t *testing.T) {
	now := time.Now()
	b := config.Branding{Name: "Saved organization", AccentColor: "#112233"}
	for _, tc := range []struct {
		allowed bool
		until   time.Time
	}{{false, now.Add(time.Hour)}, {true, time.Time{}}, {true, now}, {true, now.Add(-time.Second)}} {
		if got := Project(b, tc.allowed, tc.until, now); got != Default() {
			t.Fatal("inactive projection leaked saved settings")
		}
	}
	got := Project(b, true, now.Add(time.Hour), now)
	if !got.Enabled || got.Name != b.Name || got.AccentColor != b.AccentColor || got.ValidUntil == nil {
		t.Fatal("allowed projection unavailable")
	}
	if got := Project(config.Branding{}, true, now.Add(time.Hour), now); !got.Enabled || got.Name != "kudurru" {
		t.Fatal("empty configuration must remain editable")
	}
}

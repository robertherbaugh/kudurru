package upstream

import (
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/upstream"
)

func TestViewStatusRetainsMembershipAndSamplesOperationalCounters(t *testing.T) {
	f := startFake(t, 11)
	now := time.Now().UTC()
	set, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{
		"default": {Servers: []string{"original=" + f.addr}, Timeout: 50 * time.Millisecond},
	}, Health: upstream.HealthConfig{Failures: 1, BackoffMin: time.Minute, BackoffMax: time.Minute}}, upstream.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = set.Close() }()
	v, err := NewView(set, nil, false, func() bool { panic("held status checked cache-fill current predicate") })
	if err != nil {
		t.Fatal(err)
	}
	before, err := v.Status()
	if err != nil || len(before) != 1 || before[0].Server != "original" || !before[0].Healthy || before[0].ConsecutiveFailures != 0 {
		t.Fatal("initial captured membership/health", before, err)
	}
	if _, err := set.Exchange(t.Context(), "default", request("status.example.", false).Msg); err != nil {
		t.Fatal("native retained-set exchange", err)
	}
	f.mode.Store(1)
	if _, err := set.Exchange(t.Context(), "default", request("failure.example.", false).Msg); err == nil {
		t.Fatal("native timeout did not fail exchange")
	}
	after, err := v.Status()
	if err != nil || len(after) != 1 || after[0].Server != "original" || after[0].Healthy || after[0].ConsecutiveFailures != 1 || !after[0].BackoffUntil.Equal(now.Add(time.Minute)) || after[0].RTT <= 0 {
		t.Fatal("held view froze or replaced local operational counters", after, err)
	}
	if !before[0].Healthy || before[0].ConsecutiveFailures != 0 || !before[0].BackoffUntil.IsZero() {
		t.Fatal("later sample changed earlier detached output", before)
	}
	after[0].Server, after[0].ConsecutiveFailures = "mutated", 999
	again, err := v.Status()
	if err != nil || again[0].Server != "original" || again[0].ConsecutiveFailures != 1 {
		t.Fatal("status output mutated retained set", again, err)
	}
}

func TestViewStatusInvalidCapture(t *testing.T) {
	for _, v := range []*View{nil, {}, {current: func() bool { return true }}, {set: &upstream.Set{}, epoch: 1, current: func() bool { return true }}} {
		if _, err := v.Status(); err == nil {
			t.Fatal("invalid capture fabricated upstream status")
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

type healthDetailElector struct {
	holder  string
	leading bool
}

func (e *healthDetailElector) Leader() string                                         { return e.holder }
func (e *healthDetailElector) IsLeader() bool                                         { return e.leading }
func (*healthDetailElector) Run(context.Context, func(context.Context), func()) error { return nil }

func TestServingHealthDetailsFollowLocalObservations(t *testing.T) {
	checker := health.New(nil)
	elector := &healthDetailElector{}
	var snapshot *zonecache.Snapshot
	registerHealthDetails(checker, elector, "self", func() *zonecache.Snapshot { return snapshot })
	for _, tc := range []struct {
		name, holder string
		leading      bool
		state        zonecache.State
		age          time.Duration
		leader       string
		snapshot     bool
	}{
		{name: "unknown", state: zonecache.Unsynced},
		{name: "active self", holder: "self", leading: true, state: zonecache.Live, leader: "self"},
		{name: "old self term", holder: "self", state: zonecache.Live},
		{name: "remote observed", holder: "peer", state: zonecache.Live, leader: "peer"},
		{name: "fresh saved", holder: "peer", state: zonecache.FromSnapshot, age: time.Minute, leader: "peer", snapshot: true},
		{name: "stale saved", state: zonecache.FromSnapshot, age: 48 * time.Hour, snapshot: true},
		{name: "live recovery", holder: "self", leading: true, state: zonecache.Live, leader: "self"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			elector.holder, elector.leading = tc.holder, tc.leading
			snapshot = &zonecache.Snapshot{State: tc.state, SnapshotWrittenAt: time.Now().Add(-tc.age)}
			rec := httptest.NewRecorder()
			checker.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			var body struct {
				Leader string `json:"leader"`
				Age    string `json:"snapshotAge"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Leader != tc.leader {
				t.Fatalf("leader=%q, want %q", body.Leader, tc.leader)
			}
			if tc.snapshot {
				age, err := time.ParseDuration(body.Age)
				if err != nil || age < tc.age-time.Second || age > tc.age+time.Second {
					t.Fatalf("age=%q, want near %v", body.Age, tc.age)
				}
			} else if body.Age != "" {
				t.Fatalf("non-snapshot age=%q", body.Age)
			}
		})
	}
}

func TestRunServeHumanHealthDetails(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runServe: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("health fixture failed to stop")
		}
	})
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cfg.Listeners.Ops.Address+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err == nil {
			var body map[string]json.RawMessage
			err = json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			var leader, age string
			if err = json.Unmarshal(body["leader"], &leader); err != nil {
				t.Fatal("missing leader field")
			}
			if err = json.Unmarshal(body["snapshotAge"], &age); err != nil {
				t.Fatal("missing snapshotAge field")
			}
			if leader != cfg.Node.Name || age != "" {
				t.Fatalf("health leader=%q age=%q", leader, age)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("human health listener unavailable")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

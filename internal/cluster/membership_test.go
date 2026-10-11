package cluster

import (
	"context"
	"errors"
	"testing"
)

type stubElector struct {
	leading bool
	leader  string
}

func (s *stubElector) Run(ctx context.Context, _ func(context.Context), _ func()) error {
	<-ctx.Done()
	return nil
}
func (s *stubElector) IsLeader() bool { return s.leading }
func (s *stubElector) Leader() string { return s.leader }

func TestSelfMembership(t *testing.T) {
	ctx := context.Background()
	l, err := Self{Name: "a"}.Leader(ctx)
	if err != nil || !l.Self || l.Name != "a" {
		t.Fatalf("leader = %+v, %v", l, err)
	}
	peers, _ := Self{Name: "a"}.Peers(ctx)
	if len(peers) != 1 || !peers[0].Self {
		t.Fatalf("peers = %+v", peers)
	}
}

func TestStaticMembership(t *testing.T) {
	ctx := context.Background()
	members := []Peer{{Name: "a", Address: "10.0.0.11:8082"}, {Name: "b", Address: "10.0.0.12:8082"}}
	for _, tc := range []struct {
		name, self, leader string
		want               Peer
		err                bool
	}{
		{"remote leader", "b", "a", Peer{Name: "a", Address: "10.0.0.11:8082"}, false},
		{"self leads", "a", "a", Peer{Name: "a", Self: true}, false},
		{"no leader", "a", "", Peer{}, true},
		{"unknown leader", "a", "z", Peer{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Static{SelfName: tc.self, LeaderName: tc.leader, Members: members}
			got, err := s.Leader(ctx)
			if (err != nil) != tc.err || got != tc.want {
				t.Fatalf("Leader = %+v, %v", got, err)
			}
			if tc.err && !errors.Is(err, ErrNoLeader) {
				t.Errorf("error %v is not ErrNoLeader", err)
			}
		})
	}
	peers, _ := (&Static{SelfName: "c", Members: members}).Peers(ctx)
	if len(peers) != 3 || !peers[2].Self || peers[0].Self {
		t.Errorf("peers = %+v", peers)
	}
}

func TestElectorMembership(t *testing.T) {
	ctx := context.Background()
	el := &stubElector{}
	resolved := 0
	m := &ElectorMembership{SelfName: "pod-b", Elector: el, Resolve: func(_ context.Context, id string) (string, error) {
		resolved++
		if id == "broken" {
			return "", errors.New("no endpoint")
		}
		return id + ".kudurru-headless:8082", nil
	}}
	if _, err := m.Leader(ctx); !errors.Is(err, ErrNoLeader) {
		t.Errorf("no holder: %v", err)
	}
	el.leader = "pod-a"
	for range 3 {
		l, err := m.Leader(ctx)
		if err != nil || l.Address != "pod-a.kudurru-headless:8082" || l.Self {
			t.Fatalf("leader = %+v, %v", l, err)
		}
	}
	if resolved != 1 {
		t.Errorf("resolved %d times, want 1 (cached)", resolved)
	}
	el.leader = "broken"
	if _, err := m.Leader(ctx); err == nil {
		t.Error("resolve error swallowed")
	}
	el.leader, el.leading = "pod-b", true
	if l, _ := m.Leader(ctx); !l.Self {
		t.Errorf("leading node = %+v", l)
	}
	el.leading = false
	if _, err := m.Leader(ctx); !errors.Is(err, ErrNoLeader) {
		t.Errorf("own identity without a term: %v", err)
	}
	el.leader = "pod-a"
	if peers, _ := m.Peers(ctx); len(peers) != 2 {
		t.Errorf("peers = %+v", peers)
	}
}

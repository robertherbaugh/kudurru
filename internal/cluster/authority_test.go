package cluster

import (
	"context"
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
)

type authorityElector struct {
	leads  bool
	leader string
}

func (authorityElector) Run(context.Context, func(context.Context), func()) error { return nil }
func (e authorityElector) IsLeader() bool                                         { return e.leads }
func (e authorityElector) Leader() string                                         { return e.leader }

var _ store.Elector = authorityElector{}

// The API of an enrolled SQLite node names the holder of durable write
// authority as leader: this node while it holds it, its primary while it
// follows, and nobody while a fenced former primary still carries its own
// name or no primary is named.
func TestAuthorityMembership(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		elector store.Elector
		want    Peer
		none    bool
	}{
		"primary":                 {authorityElector{leads: true, leader: "a"}, Peer{Name: "a", Self: true}, false},
		"standalone without name": {authorityElector{leads: true}, Peer{Name: "a", Self: true}, false},
		"follower":                {authorityElector{leader: "b"}, Peer{Name: "b"}, false},
		"fenced former primary":   {authorityElector{leader: "a"}, Peer{}, true},
		"no named primary":        {authorityElector{}, Peer{}, true},
		"no elector":              {nil, Peer{}, true},
	} {
		m := Authority{SelfName: "a", Elector: tc.elector}
		got, err := m.Leader(ctx)
		if got != tc.want || errors.Is(err, ErrNoLeader) != tc.none || (err != nil) != tc.none {
			t.Errorf("%s: leader %+v, %v", name, got, err)
		}
		peers, err := m.Peers(ctx)
		if err != nil || len(peers) != 1 || peers[0] != (Peer{Name: "a", Self: true}) {
			t.Errorf("%s: peers %+v, %v", name, peers, err)
		}
	}
}

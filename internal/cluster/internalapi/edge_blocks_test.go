package internalapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/edgeblock"
)

// TestServerEdgeBlocks: the real TLS server answers /internal/edge/blocks
// with the active projection only, refuses a list an edge must not apply
// (503), requires the cluster token and answers 404 without an owner.
func TestServerEdgeBlocks(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	state, _, err := edgeblock.Add(edgeblock.Empty(), edgeblock.Request{Prefix: "192.0.2.0/24", Duration: time.Hour, Reason: "private note", Actor: "user:ops"}, "00000000000000a1", now)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = edgeblock.Add(state, edgeblock.Request{Prefix: "198.51.100.7", Duration: time.Minute}, "00000000000000a2", now)
	if err != nil {
		t.Fatal(err)
	}
	observed := now.Add(2 * time.Minute) // the second block has expired
	var serve *EdgeBlockList
	list := EdgeBlockListFrom("node-a", "cluster-1", state, observed)
	serve = &list
	n := startNode(t, CredentialOptions{Key: key(9), NodeName: "node-a"}, ServerOptions{
		EdgeBlocks: func(context.Context) (*EdgeBlockList, error) { return serve, nil },
	})
	got, err := n.client.EdgeBlocks(context.Background(), n.addr)
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "cluster-1" || got.Revision != 2 || len(got.Blocks) != 1 || got.Blocks[0].Prefix != "192.0.2.0/24" || !got.Blocks[0].Until.Equal(now.Add(time.Hour)) {
		t.Fatalf("edge blocks %+v", got)
	}
	bad := list
	bad.Blocks = []EdgeBlock{{ID: "00000000000000a1", Prefix: "10.0.0.0/8", Until: now}}
	serve = &bad
	if _, err := n.client.EdgeBlocks(context.Background(), n.addr); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("invalid list must be refused by the server: %v", err)
	}
	other := startNode(t, CredentialOptions{Key: key(8), NodeName: "node-x"}, ServerOptions{})
	if _, err := other.client.EdgeBlocks(context.Background(), n.addr); err == nil {
		t.Fatal("a client with another cluster key was served")
	}
	old := startNode(t, CredentialOptions{Key: key(9), NodeName: "node-c"}, ServerOptions{})
	if _, err := old.client.EdgeBlocks(context.Background(), old.addr); !errors.Is(err, ErrEdgeBlocksUnsupported) {
		t.Fatalf("without EdgeBlocks: %v", err)
	}
}

func TestEdgeBlockListValidate(t *testing.T) {
	until := time.Date(2026, 11, 10, 13, 0, 0, 0, time.UTC)
	ok := EdgeBlockList{Owner: "o", Blocks: []EdgeBlock{{ID: "00000000000000a1", Prefix: "192.0.2.0/24", Until: until}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, l := range map[string]EdgeBlockList{
		"owner":     {Blocks: ok.Blocks},
		"prefix":    {Owner: "o", Blocks: []EdgeBlock{{ID: "00000000000000a1", Prefix: "192.0.2.1/24", Until: until}}},
		"id":        {Owner: "o", Blocks: []EdgeBlock{{ID: "x", Prefix: "192.0.2.0/24", Until: until}}},
		"until":     {Owner: "o", Blocks: []EdgeBlock{{ID: "00000000000000a1", Prefix: "192.0.2.0/24"}}},
		"duplicate": {Owner: "o", Blocks: []EdgeBlock{ok.Blocks[0], {ID: "00000000000000a2", Prefix: "192.0.2.0/24", Until: until}}},
		"bounds":    {Owner: "o", Blocks: make([]EdgeBlock, edgeblock.MaxBlocks+1)},
	} {
		if err := l.Validate(); err == nil {
			t.Errorf("%s: invalid list accepted", name)
		}
	}
}

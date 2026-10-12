package internalapi

import (
	"context"
	"errors"
	"testing"
)

// TestServerZones: the real server answers /internal/zones from
// ServerOptions.Zones and 404 (ErrZonesUnsupported) without it.
func TestServerZones(t *testing.T) {
	want := ZoneList{Node: "node-a", Zones: []Zone{{Name: "example.com.", Serial: 7, Type: "primary"}}}
	n := startNode(t, CredentialOptions{Key: key(9), NodeName: "node-a"}, ServerOptions{
		Zones: func(context.Context) ZoneList { return want },
	})
	got, err := n.client.Zones(context.Background(), n.addr)
	if err != nil {
		t.Fatal(err)
	}
	if got.Node != want.Node || len(got.Zones) != 1 || got.Zones[0] != want.Zones[0] {
		t.Fatalf("zones %+v", got)
	}
	empty := startNode(t, CredentialOptions{Key: key(9), NodeName: "node-b"}, ServerOptions{
		Zones: func(context.Context) ZoneList { return ZoneList{Node: "node-b"} },
	})
	got, err = empty.client.Zones(context.Background(), empty.addr)
	if err != nil || got.Zones == nil || len(got.Zones) != 0 {
		t.Fatalf("no zones must be an empty list: %+v %v", got, err)
	}
	old := startNode(t, CredentialOptions{Key: key(9), NodeName: "node-c"}, ServerOptions{})
	if _, err := old.client.Zones(context.Background(), old.addr); !errors.Is(err, ErrZonesUnsupported) {
		t.Fatalf("without Zones: %v", err)
	}
}

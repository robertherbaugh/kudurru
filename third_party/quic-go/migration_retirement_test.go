package quic

import (
	"net"
	"testing"

	"github.com/quic-go/quic-go/internal/monotime"
	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"
	"github.com/quic-go/quic-go/internal/wire"
)

// Keep one CID for the current path and one for its replacement. A switch must
// free the previous path's reservation without retiring the new active one.
func TestPathManagerSuccessiveMigrationsReleasePreviousCID(t *testing.T) {
	reserved := map[pathID]bool{}
	pm := newPathManager(func(id pathID) (protocol.ConnectionID, bool) {
		if !reserved[id] && len(reserved) == 2 {
			return protocol.ConnectionID{}, false
		}
		reserved[id] = true
		return protocol.ParseConnectionID([]byte{byte(id + 1), 2, 3, 4}), true
	}, func(id pathID) {
		if !reserved[id] {
			t.Fatalf("retired unowned CID %d", id)
		}
		delete(reserved, id)
	}, utils.DefaultLogger)
	for i := range 12 {
		addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1000 + i}
		_, frames, _ := pm.HandlePacket(addr, monotime.Now(), nil, true)
		if len(frames) == 0 {
			t.Fatalf("CID supply exhausted on migration %d", i)
		}
		challenge := frames[0].Frame.(*wire.PathChallengeFrame)
		pm.HandlePathResponseFrame(&wire.PathResponseFrame{Data: challenge.Data})
		_, _, switchPath := pm.HandlePacket(addr, monotime.Now(), nil, true)
		if !switchPath {
			t.Fatal("validated path not admitted")
		}
		pm.SwitchToPath(addr)
		if len(reserved) != 1 || !reserved[pathID(i)] {
			t.Fatalf("migration %d retained wrong CID owners: %v", i, reserved)
		}
	}
}

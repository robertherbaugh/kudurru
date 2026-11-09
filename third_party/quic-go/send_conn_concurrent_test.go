package quic

import (
	"net"
	"runtime"
	"sync"
	"testing"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// State inspection and the event loop read capabilities while the send queue
// can permanently disable GSO after an actual kernel refusal.
func TestSendConnConcurrentGSOFailureObservation(t *testing.T) {
	if !platformSupportsGSO {
		t.Skip("GSO is not supported on this platform")
	}
	remote := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443}
	raw := NewMockRawConn(gomock.NewController(t))
	raw.EXPECT().LocalAddr()
	raw.EXPECT().capabilities().Return(connCapabilities{GSO: true}).AnyTimes()
	raw.EXPECT().WritePacket(gomock.Any(), remote, gomock.Any(), gomock.Any(), protocol.ECNCE).DoAndReturn(func(p []byte, _ net.Addr, _ []byte, gso uint16, _ protocol.ECN) (int, error) {
		if gso != 0 {
			return 0, errGSO
		}
		return len(p), nil
	}).AnyTimes()
	conn := newSendConn(raw, remote, packetInfo{}, utils.DefaultLogger)
	require.True(t, conn.capabilities().GSO)
	start := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		<-start
		for range 1000 {
			_ = conn.capabilities()
			runtime.Gosched()
		}
	}()
	close(start)
	for range 1000 {
		require.NoError(t, conn.Write([]byte("abcdef"), 4, protocol.ECNCE))
		runtime.Gosched()
	}
	readers.Wait()
	require.False(t, conn.capabilities().GSO)
}

//go:build linux

package dnsserver

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// Exercise the actual attached kernel program, including a changed client
// address/port. Packet offsets here are QUIC payload offsets, not UDP/IP offsets.
func TestQUICSocketKernelRouting(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		t.Run(address, func(t *testing.T) {
			packets, err := listenQUICPackets(address, 4)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = closeAllFunc(packets)() }()
			if err := attachQUICRouting(packets[0], len(packets)); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				client, err := net.Dial("udp", packets[0].LocalAddr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = client.Close() }()
				frames := [][]byte{{0xc0}, {0xc0, 0, 0, 0, 1, 0}, {0xc0, 0, 0, 0, 1, 21, 3}}
				for i := range 8 {
					short := make([]byte, 40)
					short[0], short[1] = 0x40, byte(i)
					long := make([]byte, 40)
					long[0], long[4], long[5], long[6] = 0xc0, 1, 16, byte(i)
					frames = append(frames, short, long)
				}
				for j, frame := range frames {
					want := 0
					if j >= 3 {
						want = ((j - 3) / 2) % 4
					}
					if _, err := client.Write(frame); err != nil {
						t.Fatal(err)
					}
					if err := packets[want].SetReadDeadline(time.Now().Add(time.Second)); err != nil {
						t.Fatal(err)
					}
					buf := make([]byte, 100)
					n, _, err := packets[want].ReadFrom(buf)
					if err != nil || !bytes.Equal(buf[:n], frame) {
						t.Fatalf("frame %d did not reach socket %d: %v", j, want, err)
					}
				}
			}
		})
	}
}

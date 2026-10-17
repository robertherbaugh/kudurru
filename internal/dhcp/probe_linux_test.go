//go:build linux

package dhcp

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These exercise frame construction/parsing only: no unit test sends traffic.
func TestARPProbeFrames(t *testing.T) {
	own, _ := net.ParseMAC("02:00:00:00:00:01")
	peer, _ := net.ParseMAC("02:00:00:00:00:02")
	address := netip.MustParseAddr("192.0.2.10")
	ownFrame := arpProbeFrame(own, address)
	if len(ownFrame) != 42 || !net.IP(ownFrame[28:32]).IsUnspecified() || !net.IP(ownFrame[38:42]).Equal(net.IP(address.AsSlice())) || arpConflict(ownFrame, own, address) {
		t.Fatal("invalid or self-conflicting ARP probe")
	}
	competing := arpProbeFrame(peer, address)
	if !arpConflict(competing, own, address) {
		t.Fatal("simultaneous probe missed")
	}
	reply := append([]byte(nil), competing...)
	binary.BigEndian.PutUint16(reply[20:22], 2)
	copy(reply[28:32], address.AsSlice())
	if !arpConflict(reply, own, address) {
		t.Fatal("live owner missed")
	}
	for _, mutate := range []func([]byte){
		func(b []byte) { b[12] = 0 }, func(b []byte) { b[19] = 16 }, func(b []byte) { b[21] = 3 }, func(b []byte) { b[31]++ },
	} {
		b := append([]byte(nil), reply...)
		mutate(b)
		if arpConflict(b, own, address) {
			t.Fatal("invalid/unrelated ARP reported as conflict")
		}
	}
	for n := 0; n < 42; n++ {
		if arpConflict(reply[:n], own, address) {
			t.Fatal("truncated ARP accepted")
		}
	}
}

func TestICMPChecksum(t *testing.T) {
	packet := []byte{8, 0, 0, 0, 1, 2, 3, 4, 5}
	binary.BigEndian.PutUint16(packet[2:4], internetChecksum(packet))
	if internetChecksum(packet) != 0 {
		t.Fatal("invalid echo checksum")
	}
	packet[8]++
	if internetChecksum(packet) == 0 {
		t.Fatal("corrupted echo checksum accepted")
	}
}

func TestProbeReceiveBoundedAndCancelable(t *testing.T) {
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(sockets[0]); _ = unix.Close(sockets[1]) }()
	match := func(data []byte, _ unix.Sockaddr) bool { return len(data) == 1 && data[0] == 7 }
	if conflict, err := receiveProbe(context.Background(), time.Millisecond, sockets[0], match); err != nil || conflict {
		t.Fatal("quiet timeout failed", conflict, err)
	}
	if _, err := unix.Write(sockets[1], []byte{7}); err != nil {
		t.Fatal(err)
	}
	if conflict, err := receiveProbe(context.Background(), time.Second, sockets[0], match); err != nil || !conflict {
		t.Fatal("queued response not matched", conflict, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if conflict, err := receiveProbe(ctx, time.Second, sockets[0], match); !errors.Is(err, context.Canceled) || conflict {
		t.Fatal("canceled probe did not fail closed", err)
	}
	closedFD := sockets[0]
	_ = unix.Close(closedFD)
	sockets[0] = -1
	if conflict, err := receiveProbe(context.Background(), time.Second, closedFD, match); err == nil || conflict {
		t.Fatal("broken socket did not fail closed", err)
	}
}

// Without CAP_NET_RAW, the ICMP probe uses an unprivileged datagram socket
// (a non-root pod). Loopback answers the echo, so it reports a conflict.
func TestProbeICMPWithoutRawSockets(t *testing.T) {
	raw := openRawICMP
	openRawICMP = func() (int, error) { return -1, unix.EPERM }
	t.Cleanup(func() { openRawICMP = raw })
	dgram, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_ICMP)
	if err != nil {
		t.Skipf("unprivileged ICMP sockets are not permitted (net.ipv4.ping_group_range): %v", err)
	}
	_ = unix.Close(dgram)
	cfg := Config{Interface: "lo", ServerIP: netip.MustParseAddr("127.0.0.1"), ConflictDetection: "icmp", ProbeTimeout: 500 * time.Millisecond}
	conflict, err := probeAddress(context.Background(), cfg, netip.MustParseAddr("127.0.0.1"))
	if err != nil || !conflict {
		t.Fatalf("probe = %v, %v; want a conflict from loopback", conflict, err)
	}
}

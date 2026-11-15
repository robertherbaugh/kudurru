//go:build linux

package dnsserver

import (
	"net"

	"golang.org/x/sys/unix"
)

// SO_ATTACH_REUSEPORT_CBPF inspects UDP payload bytes, independent of the IP
// family. The QUIC header form and destination CID are unprotected (RFC 9000
// sections 17.2 and 17.3). Short packets have a fixed 16-byte destination CID;
// long packets carry its length. Invalid/truncated input deterministically goes
// to socket zero for the QUIC parser to reject. This filter grants no authority.
func attachQUICRouting(pc net.PacketConn, count int) error {
	conn, err := pc.(*net.UDPConn).SyscallConn()
	if err != nil {
		return err
	}
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_B | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x80, Jt: 2},
		{Code: unix.BPF_LD | unix.BPF_B | unix.BPF_ABS, K: 1},
		{Code: unix.BPF_JMP | unix.BPF_JA, K: 4},
		{Code: unix.BPF_LD | unix.BPF_B | unix.BPF_ABS, K: 5},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: 0, Jt: 4},
		{Code: unix.BPF_JMP | unix.BPF_JGT | unix.BPF_K, K: 20, Jt: 3},
		{Code: unix.BPF_LD | unix.BPF_B | unix.BPF_ABS, K: 6},
		{Code: unix.BPF_ALU | unix.BPF_MOD | unix.BPF_K, K: uint32(count)}, // #nosec G115 -- 1..256
		{Code: unix.BPF_RET | unix.BPF_A},
		{Code: unix.BPF_RET | unix.BPF_K, K: 0},
	}
	var sockErr error
	err = conn.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptSockFprog(int(fd), unix.SOL_SOCKET, unix.SO_ATTACH_REUSEPORT_CBPF, &unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}) // #nosec G115 -- socket descriptor and fixed filter length
	})
	if err != nil {
		return err
	}
	return sockErr
}

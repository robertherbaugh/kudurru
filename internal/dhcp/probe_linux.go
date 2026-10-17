//go:build linux

package dhcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

func probeAddress(ctx context.Context, cfg Config, address netip.Addr) (bool, error) {
	if cfg.ConflictDetection == "disabled" {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	iface, err := net.InterfaceByName(cfg.Interface)
	if err != nil {
		return false, fmt.Errorf("dhcp: probe interface: %w", err)
	}
	if cfg.ConflictDetection == "arp" {
		return probeARP(ctx, cfg, *iface, address)
	}
	if cfg.ConflictDetection == "icmp" {
		return probeICMP(ctx, cfg, *iface, address)
	}
	return false, fmt.Errorf("dhcp: unsupported conflict detection %q", cfg.ConflictDetection)
}

// ARP probes have sender protocol address zero (RFC 5227), so they do not
// populate peers' ARP caches with an address that has not been assigned yet.
func arpProbeFrame(mac net.HardwareAddr, address netip.Addr) []byte {
	frame := make([]byte, 42)
	for i := range 6 {
		frame[i] = 255
	}
	copy(frame[6:12], mac)
	binary.BigEndian.PutUint16(frame[12:14], unix.ETH_P_ARP)
	binary.BigEndian.PutUint16(frame[14:16], 1)
	binary.BigEndian.PutUint16(frame[16:18], unix.ETH_P_IP)
	frame[18], frame[19] = 6, 4
	binary.BigEndian.PutUint16(frame[20:22], 1)
	copy(frame[22:28], mac)
	copy(frame[38:42], address.AsSlice())
	return frame
}

func arpConflict(frame []byte, own net.HardwareAddr, address netip.Addr) bool {
	if len(frame) < 42 || binary.BigEndian.Uint16(frame[12:14]) != unix.ETH_P_ARP ||
		binary.BigEndian.Uint16(frame[14:16]) != 1 || binary.BigEndian.Uint16(frame[16:18]) != unix.ETH_P_IP ||
		frame[18] != 6 || frame[19] != 4 || bytes.Equal(frame[22:28], own) {
		return false
	}
	op := binary.BigEndian.Uint16(frame[20:22])
	if op != 1 && op != 2 {
		return false
	}
	sender, target := netip.AddrFrom4([4]byte(frame[28:32])), netip.AddrFrom4([4]byte(frame[38:42]))
	return sender == address || op == 1 && sender.IsUnspecified() && target == address
}

func probeARP(ctx context.Context, cfg Config, iface net.Interface, address netip.Addr) (bool, error) {
	if cfg.RelayOnly || len(cfg.TrustedRelays) != 0 || !cfg.usable(address) || len(iface.HardwareAddr) != 6 {
		return false, fmt.Errorf("dhcp: ARP probe requires a direct Ethernet scope")
	}
	// htons without assuming the host's byte order.
	protocol := binary.NativeEndian.Uint16([]byte{0x08, 0x06})
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, int(protocol))
	if err != nil {
		return false, fmt.Errorf("dhcp: ARP probe socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	sa := &unix.SockaddrLinklayer{Protocol: protocol, Ifindex: iface.Index, Halen: 6, Addr: [8]byte{255, 255, 255, 255, 255, 255}}
	if err = unix.Bind(fd, sa); err != nil {
		return false, fmt.Errorf("dhcp: ARP probe bind: %w", err)
	}
	if err = unix.Sendto(fd, arpProbeFrame(iface.HardwareAddr, address), 0, sa); err != nil {
		return false, fmt.Errorf("dhcp: ARP probe send: %w", err)
	}
	return receiveProbe(ctx, cfg.ProbeTimeout, fd, func(data []byte, _ unix.Sockaddr) bool { return arpConflict(data, iface.HardwareAddr, address) })
}

// openRawICMP opens the raw ICMP probe socket; tests replace it.
var openRawICMP = func() (int, error) {
	return unix.Socket(unix.AF_INET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.IPPROTO_ICMP)
}

func probeICMP(ctx context.Context, cfg Config, iface net.Interface, address netip.Addr) (bool, error) {
	// Without CAP_NET_RAW (a non-root pod), fall back to an unprivileged ICMP
	// datagram socket, which net.ipv4.ping_group_range permits in container
	// runtimes' pod network namespaces. The kernel then owns the echo
	// identifier and strips the IP header from replies.
	datagram := false
	fd, err := openRawICMP()
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		datagram = true
		fd, err = unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.IPPROTO_ICMP)
	}
	if err != nil {
		return false, fmt.Errorf("dhcp: ICMP probe socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err = unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface.Name); err != nil {
		return false, err
	}
	if err = unix.Bind(fd, &unix.SockaddrInet4{Addr: cfg.ServerIP.As4()}); err != nil {
		return false, err
	}
	echo := make([]byte, 20)
	echo[0] = 8
	if _, err = rand.Read(echo[4:]); err != nil {
		return false, err
	}
	binary.BigEndian.PutUint16(echo[2:4], internetChecksum(echo))
	if err = unix.Sendto(fd, echo, 0, &unix.SockaddrInet4{Addr: address.As4()}); err != nil {
		return false, fmt.Errorf("dhcp: ICMP probe send: %w", err)
	}
	return receiveProbe(ctx, cfg.ProbeTimeout, fd, func(data []byte, source unix.Sockaddr) bool {
		peer, ok := source.(*unix.SockaddrInet4)
		if !ok || peer.Addr != address.As4() {
			return false
		}
		if datagram {
			// The kernel matched the identifier; compare sequence and payload.
			return len(data) >= len(echo) && data[0] == 0 && data[1] == 0 && bytes.Equal(data[6:len(echo)], echo[6:])
		}
		if len(data) < 20 || data[0]>>4 != 4 {
			return false
		}
		offset := int(data[0]&15) * 4
		if offset < 20 || len(data) < offset+len(echo) {
			return false
		}
		reply := data[offset:]
		return reply[0] == 0 && reply[1] == 0 && internetChecksum(reply) == 0 && bytes.Equal(reply[4:], echo[4:])
	})
}

func internetChecksum(data []byte) uint16 {
	var sum uint32
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data[:2]))
		data = data[2:]
	}
	if len(data) == 1 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// Each wait is capped at 25ms for prompt context cancellation. A quiet timeout
// means no conflict was observed; socket failures and parent deadlines fail closed.
func receiveProbe(ctx context.Context, timeout time.Duration, fd int, matches func([]byte, unix.Sockaddr) bool) (bool, error) {
	if fd < 0 || fd > math.MaxInt32 {
		return false, fmt.Errorf("dhcp: invalid conflict probe socket")
	}
	deadline := time.Now().Add(timeout)
	buffer := make([]byte, 2048)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, max(1, int(min(remaining, 25*time.Millisecond)/time.Millisecond)))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return false, fmt.Errorf("dhcp: conflict probe socket failed")
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, source, err := unix.Recvfrom(fd, buffer, 0)
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		if matches(buffer[:n], source) {
			return true, nil
		}
	}
}

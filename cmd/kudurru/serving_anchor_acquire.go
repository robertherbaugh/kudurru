package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

const (
	anchorEndpointLimit = 32
	anchorSelectedLimit = 3
	anchorEndpointTime  = 4 * time.Second
	anchorWireLimit     = 65535
)

var (
	errAnchorAcquisition = errors.New("root anchor acquisition refused")
	errAnchorWire        = errors.New("root anchor response invalid")
	errAnchorTruncated   = errors.New("root anchor response truncated")
)

// anchorWirePlan retains the original lifetime, endpoint selection and request
// before DNS acquisition. It supplies no trust or mutation authority. A caller
// must separately capture its Store operation and policy before invoking it.
// Production polls select TCP upfront; an explicitly selected UDP response that
// is truncated ends that acquisition instead of renewing its receipt stamp.
type anchorWirePlan struct {
	original context.Context
	deadline time.Time
	network  string
	selected []netip.AddrPort
	request  []byte
	id       uint16
	used     atomic.Bool
}

func captureAnchorWirePlan(ctx context.Context, endpoints []netip.AddrPort, network string, offset uint64) (*anchorWirePlan, error) {
	if ctx == nil || (network != "tcp" && network != "udp") || len(endpoints) == 0 || len(endpoints) > anchorEndpointLimit {
		return nil, errAnchorAcquisition
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining <= 0 || remaining > 30*time.Second {
		return nil, errAnchorAcquisition
	}
	// Normalize and reject duplicates before choosing a bounded detached slice.
	// A zone/name cannot invoke an ambient resolver or alter numeric routing.
	normalized := make([]netip.AddrPort, 0, len(endpoints))
	for _, endpoint := range endpoints {
		addr := endpoint.Addr()
		if !addr.IsValid() || addr.Zone() != "" || addr.IsUnspecified() || addr.IsMulticast() || endpoint.Port() == 0 {
			return nil, errAnchorAcquisition
		}
		endpoint = netip.AddrPortFrom(addr.Unmap(), endpoint.Port())
		for _, previous := range normalized {
			if endpoint == previous {
				return nil, errAnchorAcquisition
			}
		}
		normalized = append(normalized, endpoint)
	}
	selected := make([]netip.AddrPort, 0, min(anchorSelectedLimit, len(normalized)))
	offset %= uint64(len(normalized))
	for i := 0; i < cap(selected); i++ {
		selected = append(selected, normalized[(offset+uint64(i))%uint64(len(normalized))])
	}
	request := new(dns.Msg)
	request.SetQuestion(".", dns.TypeDNSKEY)
	request.RecursionDesired = false
	request.SetEdns0(1232, true)
	packet, err := request.Pack()
	if err != nil {
		return nil, errAnchorAcquisition
	}
	return &anchorWirePlan{original: ctx, deadline: deadline, network: network, selected: selected, request: packet, id: request.Id}, nil
}

// acquireAnchorWire owns its sockets and joins cancellation before returning.
// stamp must be the original operation's concrete, bounded receipt sampler.
// It runs synchronously at the first complete response, before parsing, header
// correlation, cancellation rechecks, close, or any other response processing.
// The generic value can carry a driver-owned stamp or bootstrap clock sample;
// it does not offer a constructor for either. No response is authenticated here.
func acquireAnchorWire[S any](plan *anchorWirePlan, stamp func() (S, error)) ([]byte, S, error) {
	var zero S
	if plan == nil || plan.original == nil || stamp == nil {
		return nil, zero, errAnchorAcquisition
	}
	if !plan.used.CompareAndSwap(false, true) {
		return nil, zero, errAnchorAcquisition
	}
	for _, endpoint := range plan.selected {
		if err := plan.original.Err(); err != nil {
			return nil, zero, err
		}
		if !time.Now().Before(plan.deadline) {
			return nil, zero, context.DeadlineExceeded
		}
		raw, received, complete, err := exchangeAnchorWire(plan, endpoint, stamp)
		if complete {
			// A complete response consumes the acquisition even on invalid input
			// or a sampler refusal. Never attach a new stamp to replacement bytes.
			if err != nil {
				return nil, zero, err
			}
			if err := plan.original.Err(); err != nil {
				return nil, zero, err
			}
			if !time.Now().Before(plan.deadline) {
				return nil, zero, context.DeadlineExceeded
			}
			if err := correlateAnchorWire(raw, plan.id); err != nil {
				return nil, zero, err
			}
			return raw, received, nil
		}
		// Only incomplete transport acquisition may try the next originally
		// selected numeric endpoint. There are at most three wire exchanges.
	}
	if err := plan.original.Err(); err != nil {
		return nil, zero, err
	}
	return nil, zero, errAnchorAcquisition
}

func exchangeAnchorWire[S any](plan *anchorWirePlan, endpoint netip.AddrPort, stamp func() (S, error)) ([]byte, S, bool, error) {
	var zero S
	requestSize := len(plan.request)
	if requestSize == 0 || requestSize > anchorWireLimit {
		return nil, zero, false, errAnchorAcquisition
	}
	deadline := minAnchorDeadline(plan.deadline, time.Now().Add(anchorEndpointTime))
	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.DialContext(plan.original, plan.network, endpoint.String())
	if err != nil {
		return nil, zero, false, errAnchorAcquisition
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(plan.original, func() { _ = conn.Close(); close(closed) })
	defer func() {
		if !stop() {
			<-closed
		}
		_ = conn.Close()
	}()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, zero, false, errAnchorAcquisition
	}
	packet := plan.request
	if plan.network == "tcp" {
		packet = make([]byte, 2+requestSize)
		binary.BigEndian.PutUint16(packet, uint16(requestSize))
		copy(packet[2:], plan.request)
	}
	if n, err := conn.Write(packet); err != nil || n != len(packet) {
		return nil, zero, false, errAnchorAcquisition
	}
	if plan.network == "tcp" {
		var size [2]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return nil, zero, false, errAnchorAcquisition
		}
		// The selected frame, even an empty/malformed one, is terminal.
		raw := make([]byte, int(binary.BigEndian.Uint16(size[:])))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return nil, zero, false, errAnchorAcquisition
		}
		received, err := stamp()
		return raw, received, true, err
	}
	raw := make([]byte, anchorWireLimit)
	n, err := conn.Read(raw)
	if err != nil {
		return nil, zero, false, errAnchorAcquisition
	}
	received, err := stamp()
	return raw[:n], received, true, err
}

func minAnchorDeadline(original, endpoint time.Time) time.Time {
	if original.Before(endpoint) {
		return original
	}
	return endpoint
}

func correlateAnchorWire(raw []byte, id uint16) error {
	if len(raw) < 12 || binary.BigEndian.Uint16(raw) != id || raw[2]&0x80 == 0 || raw[2]&0x78 != 0 || binary.BigEndian.Uint16(raw[4:]) != 1 {
		return errAnchorWire
	}
	if raw[2]&0x02 != 0 {
		return errAnchorTruncated
	}
	name, end, err := dns.UnpackDomainName(raw, 12)
	if err != nil || name != "." || end+4 > len(raw) || binary.BigEndian.Uint16(raw[end:]) != dns.TypeDNSKEY || binary.BigEndian.Uint16(raw[end+2:]) != dns.ClassINET {
		return errAnchorWire
	}
	return nil
}

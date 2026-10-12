package proxyproto

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestPacketConn(t *testing.T) {
	p, err := NewPolicy([]string{"127.0.0.1"}, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pc := &PacketConn{PacketConn: raw, Policy: p}
	defer func() { _ = pc.Close() }()

	client, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	// A forged datagram first (no TLV), then a plain one, then a proxied one:
	// ReadFrom must skip the first silently.
	for _, b := range [][]byte{
		append(header(t, CommandProxy, nil), "forged"...),
		[]byte("plain"),
		append(header(t, CommandProxy, secretTLV(testSecret)), "proxied"...),
	} {
		if _, err := client.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)

	n, addr, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "plain" || addr.String() != client.LocalAddr().String() {
		t.Fatalf("got %q from %s", buf[:n], addr)
	}

	n, addr, err = pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	pa, ok := addr.(*Addr)
	if string(buf[:n]) != "proxied" || !ok || pa.Client != realClient || pa.String() != realClient.String() || pa.Network() != "udp" {
		t.Fatalf("got %q from %#v", buf[:n], addr)
	}
	if AddrPort(pa) != realClient {
		t.Fatal("AddrPort(*Addr) must be the client")
	}
	if p.Dropped(ReasonUnauthenticated) != 1 {
		t.Fatalf("forged datagram not counted: %d", p.Dropped(ReasonUnauthenticated))
	}

	// Replies to the proxied address go to the proxy socket.
	if _, err := pc.WriteTo([]byte("reply"), addr); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = client.Read(buf)
	if err != nil || string(buf[:n]) != "reply" {
		t.Fatalf("reply = %q, %v", buf[:n], err)
	}
}

func TestListener(t *testing.T) {
	p, err := NewPolicy([]string{"127.0.0.1"}, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := &Listener{Listener: raw, Policy: p, HeaderTimeout: 500 * time.Millisecond}
	defer func() { _ = ln.Close() }()

	type result struct {
		remote net.Addr
		data   []byte
		err    error
	}
	serve := func() <-chan result {
		ch := make(chan result, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				ch <- result{err: err}
				return
			}
			defer func() { _ = c.Close() }()
			// A caller deadline set before the first Read must survive the
			// header read.
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			data, err := io.ReadAll(c)
			ch <- result{remote: c.RemoteAddr(), data: data, err: err}
		}()
		return ch
	}
	send := func(b []byte, closeWrite bool) {
		t.Helper()
		c, err := net.Dial("tcp", raw.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if _, err := c.Write(b); err != nil {
			t.Fatal(err)
		}
		if closeWrite {
			_ = c.(*net.TCPConn).CloseWrite()
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.Copy(io.Discard, c)
	}

	t.Run("authenticated", func(t *testing.T) {
		ch := serve()
		send(append(header(t, CommandProxy, secretTLV(testSecret)), "payload"...), true)
		r := <-ch
		if r.err != nil || string(r.data) != "payload" {
			t.Fatalf("data=%q err=%v", r.data, r.err)
		}
		if pa, ok := r.remote.(*Addr); !ok || pa.Client != realClient {
			t.Fatalf("remote = %v", r.remote)
		}
	})
	t.Run("plain", func(t *testing.T) {
		ch := serve()
		send([]byte("\x00\x05hello"), true)
		r := <-ch
		if r.err != nil || string(r.data) != "\x00\x05hello" {
			t.Fatalf("data=%q err=%v", r.data, r.err)
		}
		if _, ok := r.remote.(*Addr); ok {
			t.Fatal("plain connection must not be proxied")
		}
	})
	t.Run("forged", func(t *testing.T) {
		ch := serve()
		send(append(header(t, CommandProxy, secretTLV([]byte("x"))), "payload"...), true)
		r := <-ch
		var de *DropError
		if !errors.As(r.err, &de) || de.Reason != ReasonUnauthenticated || len(r.data) != 0 {
			t.Fatalf("data=%q err=%v", r.data, r.err)
		}
	})
	t.Run("slow header times out", func(t *testing.T) {
		ch := serve()
		start := time.Now()
		send(header(t, CommandProxy, secretTLV(testSecret))[:10], false)
		r := <-ch
		var de *DropError
		if !errors.As(r.err, &de) || de.Reason != ReasonMalformed {
			t.Fatalf("err=%v", r.err)
		}
		if el := time.Since(start); el > 1500*time.Millisecond {
			t.Fatalf("header timeout not applied: %s", el)
		}
	})
	if got := p.DroppedTotal(); got != 2 {
		t.Fatalf("DroppedTotal = %d, want 2", got)
	}
}

func TestConnProxiedAndPending(t *testing.T) {
	p, err := NewPolicy([]string{"127.0.0.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	c := &Conn{Conn: a, policy: p, timeout: time.Second}
	go func() { _, _ = b.Write([]byte("xyz")) }()
	buf := make([]byte, 1)
	var got bytes.Buffer
	for got.Len() < 3 {
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got.Write(buf[:n])
	}
	if got.String() != "xyz" || c.Proxied() {
		t.Fatalf("got %q proxied=%v", got.String(), c.Proxied())
	}
}

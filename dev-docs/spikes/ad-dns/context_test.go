package main

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/types"
)

func testContext() *securityContext {
	return &securityContext{key: types.EncryptionKey{KeyType: 18, KeyValue: bytes.Repeat([]byte{0x42}, 32)},
		expires: time.Now().Add(time.Hour), minimum: 100}
}

func clientMIC(t *testing.T, c *securityContext, seq uint64, payload []byte, flags byte) []byte {
	t.Helper()
	m := gssapi.MICToken{Flags: flags, SndSeqNum: seq, Payload: payload}
	if err := m.SetChecksum(c.key, keyusage.GSSAPI_INITIATOR_SIGN); err != nil {
		t.Fatal(err)
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMICSecurity(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T, *securityContext)
	}{
		{"tampering does not consume sequence", func(t *testing.T, c *securityContext) {
			mac := clientMIC(t, c, 100, []byte("real update"), 0)
			if c.verify([]byte("forged update"), mac) == nil {
				t.Fatal("accepted tampering")
			}
			if err := c.verify([]byte("real update"), mac); err != nil {
				t.Fatal(err)
			}
		}},
		{"replay and receive window", func(t *testing.T, c *securityContext) {
			payload := []byte("update")
			for _, seq := range []uint64{100, 102, 101, 165} {
				if err := c.verify(payload, clientMIC(t, c, seq, payload, 0)); err != nil {
					t.Fatalf("sequence %d: %v", seq, err)
				}
			}
			for _, seq := range []uint64{99, 100, 101, 102, 165} {
				if c.verify(payload, clientMIC(t, c, seq, payload, 0)) == nil {
					t.Fatalf("accepted old/duplicate sequence %d", seq)
				}
			}
		}},
		{"direction and flags", func(t *testing.T, c *securityContext) {
			for _, flags := range []byte{1, 2, 4, 8, 255} {
				if c.verify([]byte("update"), clientMIC(t, c, 100, []byte("update"), flags)) == nil {
					t.Fatalf("accepted flags %d", flags)
				}
			}
		}},
		{"expired context", func(t *testing.T, c *securityContext) {
			c.expires = time.Now().Add(-time.Second)
			if c.verify([]byte("update"), clientMIC(t, c, 100, []byte("update"), 0)) == nil {
				t.Fatal("verified expired context")
			}
			if _, err := c.sign([]byte("reply")); err == nil {
				t.Fatal("signed expired context")
			}
		}},
		{"signed reply direction", func(t *testing.T, c *securityContext) {
			mac, err := c.sign([]byte("reply"))
			if err != nil {
				t.Fatal(err)
			}
			var token gssapi.MICToken
			if err := token.Unmarshal(mac, true); err != nil {
				t.Fatal(err)
			}
			token.Payload = []byte("reply")
			if ok, err := token.Verify(c.key, keyusage.GSSAPI_ACCEPTOR_SIGN); !ok || err != nil {
				t.Fatalf("reply verification: %v", err)
			}
		}},
		{"negotiated sequence service", func(t *testing.T, c *securityContext) {
			c.ordered = true
			payload := []byte("update")
			for _, seq := range []uint64{102, 100, 100, 102, 101, 102} {
				want := seq == c.minimum
				err := c.verify(payload, clientMIC(t, c, seq, payload, 0))
				if (err == nil) != want {
					t.Fatalf("sequence %d, accepted=%v, want=%v", seq, err == nil, want)
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) { test.run(t, testContext()) })
	}
}

func TestConcurrentReplay(t *testing.T) {
	c := testContext()
	payload := []byte("update")
	mac := clientMIC(t, c, 100, payload, 0)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if c.verify(payload, mac) == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d concurrent copies", accepted.Load())
	}
}

func FuzzMIC(f *testing.F) {
	f.Add([]byte{4, 4, 0, 255, 255, 255, 255, 255}, []byte("update"))
	f.Fuzz(func(_ *testing.T, mac, payload []byte) {
		_ = testContext().verify(payload, mac)
	})
}

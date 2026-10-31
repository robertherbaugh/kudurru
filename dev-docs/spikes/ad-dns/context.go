package main

import (
	"errors"
	"math"
	"sync"
	"time"

	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/types"
)

type securityContext struct {
	mu        sync.Mutex
	key       types.EncryptionKey
	principal string
	expires   time.Time
	send      uint64
	minimum   uint64
	ordered   bool
	highest   uint64
	seen      uint64 // 64-token receive window; bit zero is highest.
}

func (c *securityContext) sign(payload []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !time.Now().Before(c.expires) || c.send == math.MaxUint64 {
		return nil, errors.New("context expired or exhausted")
	}
	t := gssapi.MICToken{Flags: gssapi.MICTokenFlagSentByAcceptor, SndSeqNum: c.send, Payload: payload}
	if err := t.SetChecksum(c.key, keyusage.GSSAPI_ACCEPTOR_SIGN); err != nil {
		return nil, errors.New("could not sign reply")
	}
	mac, err := t.Marshal()
	if err == nil {
		c.send++
	}
	return mac, err
}

func (c *securityContext) verify(payload, mac []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !time.Now().Before(c.expires) {
		return errors.New("context expired")
	}
	var t gssapi.MICToken
	if err := t.Unmarshal(mac, false); err != nil || t.Flags != 0 {
		// No acceptor subkey was issued. Reject unknown flags and sealed MICs.
		return errors.New("invalid initiator MIC")
	}
	t.Payload = payload
	if ok, err := t.Verify(c.key, keyusage.GSSAPI_INITIATOR_SIGN); err != nil || !ok {
		return errors.New("invalid MIC checksum")
	}
	seq := t.SndSeqNum
	if seq < c.minimum {
		return errors.New("sequence precedes context")
	}
	if c.ordered {
		if seq != c.minimum || seq == math.MaxUint64 {
			return errors.New("out-of-order or exhausted MIC sequence")
		}
		c.minimum++
		return nil
	}
	// Update the receive window only after authentication. Forged tokens must
	// not advance it and suppress a legitimate request.
	if c.seen == 0 || seq > c.highest {
		shift := seq - c.highest
		if c.seen == 0 || shift >= 64 {
			c.seen = 1
		} else {
			c.seen = c.seen<<shift | 1
		}
		c.highest = seq
		return nil
	}
	distance := c.highest - seq
	if distance >= 64 || c.seen&(uint64(1)<<distance) != 0 {
		return errors.New("replayed or old MIC")
	}
	c.seen |= uint64(1) << distance
	return nil
}

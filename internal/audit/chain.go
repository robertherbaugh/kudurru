package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// ChainHash is the hash of ev chained to prev, the Hash of the event
// before it (§11.4 tamper evidence): SHA-256 over the length-prefixed
// fields and prev. Time counts in microseconds, so a store with
// microsecond timestamps (Postgres) reproduces it; Before and After are
// hashed byte for byte and must be stored as text, never re-encoded.
// ID, Hash and PrevHash are not hashed.
func ChainHash(ev Event, prev string) string {
	h := sha256.New()
	for _, f := range []string{
		"v1", strconv.FormatInt(ev.Time.UnixMicro(), 10), ev.Node,
		ev.Actor.Kind, ev.Actor.ID, ev.Actor.Name, strings.Join(ev.Actor.Groups, "\n"), ev.Actor.Auth,
		ev.Action, ev.Resource.Kind, ev.Resource.Namespace, ev.Resource.Name,
		ev.Result, ev.Reason, ev.Authz, ev.Manager, ev.ResourceVersion,
		string(ev.Before), string(ev.After),
		ev.Request.ID, ev.Request.IP, ev.Request.UA, ev.Request.Method, ev.Request.Path,
		prev,
	} {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{':'})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Chain sets ev.PrevHash to prev and ev.Hash to ChainHash(ev, prev).
func Chain(ev *Event, prev string) {
	ev.PrevHash = prev
	ev.Hash = ChainHash(*ev, prev)
}

// VerifyChain checks events read oldest first: every event's Hash is
// ChainHash over its fields and PrevHash, and every PrevHash is the Hash
// of the event before it. The first event's PrevHash is not checked
// against anything (it may follow pruned events). It names the first
// event that does not verify.
func VerifyChain(events []Event) error {
	var prev *Event
	for i := range events {
		ev := &events[i]
		if prev != nil && ev.PrevHash != prev.Hash {
			return fmt.Errorf("audit: event %d does not follow event %d (prev_hash %s, want %s)", ev.ID, prev.ID, ev.PrevHash, prev.Hash)
		}
		if want := ChainHash(*ev, ev.PrevHash); ev.Hash != want {
			return fmt.Errorf("audit: event %d was changed (hash %s, want %s)", ev.ID, ev.Hash, want)
		}
		prev = ev
	}
	return nil
}

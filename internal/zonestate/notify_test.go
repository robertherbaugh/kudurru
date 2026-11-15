package zonestate

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

func TestPrepareNotifyPreservesHistoricalBytesAndMetadata(t *testing.T) {
	a := fixture(t)
	a.Expiry = testNow()
	p, err := Prepare(a, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	// A historically accepted alternate framing must keep its actual root.
	var root *zk.ZoneChunk
	var m manifest
	for _, c := range p.Chunks {
		if c.Spec.Digest == p.Head.Spec.RootDigest {
			root = c
		}
	}
	if err := json.Unmarshal(root.Spec.Data, &m); err != nil || len(m.Parts) != 1 {
		t.Fatal(err)
	}
	chunks := map[string]*zk.ZoneChunk{}
	for _, c := range p.Chunks {
		if c.Spec.Digest == m.Parts[0] {
			data := append([]byte(" \n"), c.Spec.Data...)
			historical := chunk(data)
			m.Parts[0], m.Bytes = historical.Spec.Digest, int64(len(data))
			chunks[historical.Spec.Digest] = historical
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	root = chunk(b)
	chunks[root.Spec.Digest] = root
	p.Head.Spec.RootDigest = root.Spec.Digest
	p.Head.UID = types.UID("real-carrier-uid")
	p.Head.ResourceVersion = "opaque-rv"
	p.Head.Labels = map[string]string{"carrier": "preserved"}
	get := func(d string) (*zk.ZoneChunk, error) { return chunks[d], nil }
	before := p.Head.DeepCopy()
	next, err := PrepareNotify(p.Head, get, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Head, before) || !exactNotifyRearm(before, next.Head) ||
		!reflect.DeepEqual(next.Head.ObjectMeta, before.ObjectMeta) || len(next.Chunks) != len(chunks) {
		t.Fatal("manual intent changed immutable evidence or metadata")
	}
	for _, c := range next.Chunks {
		if !reflect.DeepEqual(c, chunks[c.Spec.Digest]) {
			t.Fatal("original chunk was regenerated")
		}
	}
	reencoded, err := Prepare(a, before, Limits{})
	if err != nil || reencoded.Head.Spec.RootDigest == before.Spec.RootDigest {
		t.Fatal("fixture did not exercise historical root preservation", err)
	}
	next.Head.Labels["carrier"] = "changed"
	next.Chunks[0].Spec.Data[0] ^= 1
	if !reflect.DeepEqual(p.Head, before) || bytes.Equal(next.Chunks[0].Spec.Data, chunks[next.Chunks[0].Spec.Digest].Spec.Data) {
		t.Fatal("prepared intent aliases input")
	}
}

func TestNotifyRearmExactWholeSpecWhitelist(t *testing.T) {
	p, err := Prepare(fixture(t), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	old := p.Head
	old.ResourceVersion = "rv"
	next := old.DeepCopy()
	next.Spec.CommitSequence++
	next.Spec.NotifySequence = next.Spec.CommitSequence
	next.Spec.NotifySerial = next.Spec.Serial
	e := Expected{Zone: old.Spec.Zone, ResourceVersion: old.ResourceVersion, CommitSequence: old.Spec.CommitSequence, RootDigest: old.Spec.RootDigest, AuthEpoch: old.Spec.AuthEpoch}
	if err := CheckTransition(old, e, next); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*zk.ZoneState)
	}{
		{"root", func(h *zk.ZoneState) {
			h.Spec.RootDigest = zk.Digest([]byte("other expiry/source/config/reservations"))
		}},
		{"auth epoch", func(h *zk.ZoneState) { h.Spec.AuthEpoch++ }},
		{"auth digest", func(h *zk.ZoneState) { h.Spec.AuthEpoch++; h.Spec.AuthDigest = zk.Digest([]byte("other policy")) }},
		{"tombstone", func(h *zk.ZoneState) { h.Spec.Lifecycle = zk.LifecycleTombstoned }},
		{"sequence", func(h *zk.ZoneState) { h.Spec.CommitSequence++ }},
		{"old sequence", func(h *zk.ZoneState) { h.Spec.NotifySequence = old.Spec.CommitSequence }},
		{"notify serial", func(h *zk.ZoneState) { h.Spec.NotifySerial++ }},
		{"format", func(h *zk.ZoneState) { h.Spec.FormatVersion++ }},
		{"zone", func(h *zk.ZoneState) { h.Spec.Zone.UID = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := next.DeepCopy()
			tc.mutate(h)
			if exactNotifyRearm(old, h) || CheckTransition(old, e, h) == nil {
				t.Fatal("combined transition accepted")
			}
		})
	}
	stale := e
	stale.ResourceVersion = "stale"
	if !errors.Is(CheckTransition(old, stale, next), ErrConflict) {
		t.Fatal("stale rearm admitted")
	}
	ack := next.DeepCopy()
	ack.ResourceVersion = "next"
	ack.Spec.CommitSequence++
	ack.Spec.NotifySequence, ack.Spec.NotifySerial = 0, 0
	e.ResourceVersion, e.CommitSequence = "next", next.Spec.CommitSequence
	next.ResourceVersion = "next"
	if err := CheckTransition(next, e, ack); err != nil {
		t.Fatal("existing exact acknowledgment rejected", err)
	}
}

func TestPrepareNotifyRejectsPendingCorruptOverflowAndCapacity(t *testing.T) {
	p, err := Prepare(fixture(t), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	get := func(d string) (*zk.ZoneChunk, error) {
		for _, c := range p.Chunks {
			if c.Spec.Digest == d {
				return c, nil
			}
		}
		return nil, ErrInvalid
	}
	for _, mutate := range []func(*zk.ZoneState){
		func(h *zk.ZoneState) { h.Spec.NotifySequence, h.Spec.NotifySerial = 1, h.Spec.Serial },
		func(h *zk.ZoneState) { h.Spec.NotifySerial = 1 },
		func(h *zk.ZoneState) { h.Spec.CommitSequence = math.MaxUint64 },
		func(h *zk.ZoneState) { h.Spec.Lifecycle = zk.LifecycleTombstoned },
		func(h *zk.ZoneState) { h.Spec.Serial++ },
	} {
		h := p.Head.DeepCopy()
		mutate(h)
		if _, err := PrepareNotify(h, get, Limits{}); err == nil {
			t.Fatal("unusable intent accepted")
		}
	}
	if _, err := PrepareNotify(p.Head, get, Limits{MaxChunks: 1}); !errors.Is(err, ErrCapacity) {
		t.Fatal("chunk budget ignored", err)
	}
	h := p.Head.DeepCopy()
	h.Annotations = map[string]string{"oversized": strings.Repeat("x", zk.MaxHeadBytes+1)}
	if _, err := PrepareNotify(h, get, Limits{}); !errors.Is(err, ErrCapacity) {
		t.Fatal("copy before bounds", err)
	}
	for _, mutate := range []func(*zk.ZoneState){
		func(h *zk.ZoneState) { h.APIVersion = strings.Repeat("x", zk.MaxHeadBytes+1) },
		func(h *zk.ZoneState) { h.Spec.RootDigest = strings.Repeat("a", zk.MaxHeadBytes+1) },
	} {
		h := p.Head.DeepCopy()
		mutate(h)
		if _, err := PrepareNotify(h, func(string) (*zk.ZoneChunk, error) { t.Fatal("read before carrier preflight"); return nil, ErrInvalid }, Limits{}); !errors.Is(err, ErrCapacity) {
			t.Fatal("unbounded carrier before validation/copy", err)
		}
	}
	if _, err := PrepareNotify(p.Head, func(string) (*zk.ZoneChunk, error) { return nil, ErrConflict }, Limits{}); !errors.Is(err, ErrConflict) {
		t.Fatal("getter error swallowed", err)
	}
}

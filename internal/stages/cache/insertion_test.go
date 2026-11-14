package cache

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"golang.org/x/sync/singleflight"
)

type insertionFunc func(func() error) error

func (f insertionFunc) Apply(fn func() error) error { return f(fn) }

func TestInsertionCapturedBeforeDetachedFetchAndSharedByJoiners(t *testing.T) {
	h := newHarness(t, nil)
	req := request("held.example.", dns.TypeA, 1)
	ctx, cancel := context.WithCancel(t.Context())
	req.Ctx = ctx
	defer cancel()
	key, _ := h.stage.key(req)
	started, finish := make(chan struct{}), make(chan struct{})
	var captures, applies atomic.Int32
	var captured context.Context
	h.stage.CaptureInsertion = func(fr *pipeline.Request) (Insertion, error) {
		captures.Add(1)
		captured = fr.Ctx
		deadline, finite := captured.Deadline()
		if !finite || time.Until(deadline) > h.stage.settings().FetchTimeout || fr.Deadline != deadline {
			return nil, errors.New("fetch insertion lost its original finite lifetime")
		}
		return insertionFunc(func(fn func() error) error {
			applies.Add(1)
			if err := captured.Err(); err != nil {
				return err
			}
			return fn()
		}), nil
	}
	next := func(fr *pipeline.Request) (*pipeline.Response, error) {
		if captures.Load() != 1 || fr.Ctx != captured {
			return nil, errors.New("acquisition preceded original insertion capture")
		}
		close(started)
		<-finish
		return forwarded("192.0.2.4", 30)(fr)
	}
	first := h.stage.fetch(key, req, next)
	<-started
	second := h.stage.fetch(key, req, next)
	cancel() // independent cache work keeps its own original bound
	close(finish)
	for _, result := range []<-chan singleflight.Result{first, second} {
		if r := <-result; r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	h.stage.Wait()
	if captures.Load() != 1 || applies.Load() != 1 {
		t.Fatal("joiner recaptured or reused leader insertion", captures.Load(), applies.Load())
	}
	if _, ok := h.stage.c.Peek(key); !ok {
		t.Fatal("successful original fill missing")
	}
}

func TestInsertionRefusalPreventsEveryFetchMutation(t *testing.T) {
	refused := errors.New("original admission retired")
	for _, mode := range []string{"capture", "nil", "store", "delete", "failed-refresh"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, nil)
			h.stage.SetSettings(Settings{MaxTTL: time.Second})
			req := request("original.example.", dns.TypeA, 1)
			key, _ := h.stage.key(req)
			initial, _ := forwarded("192.0.2.1", 1)(req)
			h.stage.storeWithSettings(key, initial, h.stage.c.Generation(), h.stage.settings())
			h.clk.Advance(2 * time.Second)
			before, ok := h.stage.c.Peek(key)
			if !ok || before.State != cachedata.Stale || before.Hold {
				t.Fatal("fixture did not establish stale unheld data")
			}
			var calls atomic.Int32
			h.stage.CaptureInsertion = func(*pipeline.Request) (Insertion, error) {
				if mode == "capture" {
					return nil, refused
				}
				if mode == "nil" {
					return nil, nil
				}
				return insertionFunc(func(func() error) error { return refused }), nil
			}
			next := func(fr *pipeline.Request) (*pipeline.Response, error) {
				calls.Add(1)
				out, err := forwarded("192.0.2.2", 300)(fr)
				switch mode {
				case "delete":
					out.Security.State = pipeline.ValidationBogus
				case "failed-refresh":
					return nil, errors.New("upstream unavailable")
				}
				return out, err
			}
			r := <-h.stage.fetch(key, req, next)
			h.stage.Wait()
			want := refused
			if mode == "nil" {
				want = pipeline.ErrServingCapture
			}
			if !errors.Is(r.Err, want) {
				t.Fatal("refusal lost", r.Err)
			}
			if (mode == "capture" || mode == "nil") && calls.Load() != 0 {
				t.Fatal("refused capture still acquired data")
			}
			after, ok := h.stage.c.Peek(key)
			if !ok || !bytes.Equal(before.Wire, after.Wire) || after.Hold || before.TTL != after.TTL || before.Metadata != after.Metadata {
				t.Fatal("refused admission mutated cache")
			}
		})
	}
}

func TestInsertionWaitPreservesOriginalTTLAndGeneration(t *testing.T) {
	for _, mode := range []string{"wait", "expired", "flush", "security-expired"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, nil)
			req := request("ttl.example.", dns.TypeA, 1)
			key, _ := h.stage.key(req)
			h.stage.CaptureInsertion = func(*pipeline.Request) (Insertion, error) {
				return insertionFunc(func(fn func() error) error {
					switch mode {
					case "wait":
						h.clk.Advance(4 * time.Second)
					case "expired", "security-expired":
						h.clk.Advance(30 * time.Second)
					case "flush":
						h.stage.c.Flush()
					}
					return fn()
				}), nil
			}
			next := func(fr *pipeline.Request) (*pipeline.Response, error) {
				out, err := forwarded("192.0.2.5", 30)(fr)
				if mode == "security-expired" {
					out.Security = pipeline.Security{State: pipeline.ValidationSecure, ValidUntil: h.clk.Now().Add(5 * time.Second)}
				}
				return out, err
			}
			if r := <-h.stage.fetch(key, req, next); r.Err != nil {
				t.Fatal(r.Err)
			}
			h.stage.Wait()
			entry, ok := h.stage.c.Peek(key)
			if mode != "wait" {
				if ok {
					t.Fatal("expired or flushed fill was inserted")
				}
				return
			}
			if !ok || entry.Age != 4*time.Second || entry.TTL != 30*time.Second {
				t.Fatal("queued insertion renewed original TTL", entry)
			}
			answer := h.stage.answer(req, key, entry, -1, false)
			if answer == nil || answer.Msg.Answer[1].Header().Ttl != 26 {
				t.Fatal("wire TTL ignored admission wait")
			}
		})
	}
}

func TestManagedCacheDomainAndRestoredPromotion(t *testing.T) {
	current := func() bool { return true }
	if _, err := NewManagedView(Settings{}, false, current, [32]byte{}); err == nil {
		t.Fatal("zero managed domain accepted")
	}
	a, err := NewManagedView(Settings{}, false, current, [32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewManagedView(Settings{}, false, current, [32]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	static, err := NewView(Settings{}, false, current)
	if err != nil {
		t.Fatal(err)
	}
	base := pipeline.CacheScope{ID: [32]byte{3}, Epoch: 4}
	if a.scope(base).ID == b.scope(base).ID || a.scope(base).ID == static.scope(base).ID {
		t.Fatal("managed owner/local domain omitted from raw scope")
	}
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "static", true: "managed"}[managed], func(t *testing.T) {
			h := newHarness(t, nil)
			view := static
			if managed {
				view = a
			}
			scope := view.scope(base)
			old := scope
			old.Epoch += 1000
			req := request("restored.example.", dns.TypeA, 1)
			key := cachedata.MakeScopedKey(old, req.Question().Name, dns.TypeA, dns.ClassINET, false, false, netip.Prefix{})
			h.stage.c.SetWithMetadata(key, []byte("original"), cachedata.Positive, time.Minute, cachedata.Metadata{Scope: old})
			var dump bytes.Buffer
			if err := h.stage.c.Dump(&dump); err != nil {
				t.Fatal(err)
			}
			h.stage.c.Flush()
			if n, err := h.stage.c.Load(&dump); err != nil || n != 1 {
				t.Fatal("real dump load", n, err)
			}
			var admissions int
			h.stage.CaptureInsertion = func(*pipeline.Request) (Insertion, error) {
				return insertionFunc(func(fn func() error) error { admissions++; return fn() }), nil
			}
			if err := h.stage.rebind(req, view, scope); err != nil {
				t.Fatal(err)
			}
			newKey := cachedata.MakeScopedKey(scope, req.Question().Name, dns.TypeA, dns.ClassINET, false, false, netip.Prefix{})
			_, promoted := h.stage.c.Peek(newKey)
			if promoted == managed || admissions != map[bool]int{false: 1, true: 0}[managed] {
				t.Fatal("restored promotion bypassed static admission or adopted managed data", promoted, admissions)
			}
		})
	}
}

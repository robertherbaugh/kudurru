package recursor

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type mutationTestSource struct {
	domain            [32]byte
	captures, applies int
	captureErr        error
	before            func(int)
	active            bool
	retired           bool
	after             func()
}

func (s *mutationTestSource) Domain() [32]byte { return s.domain }
func (s *mutationTestSource) Capture(context.Context) (CacheMutation, error) {
	s.captures++
	if s.captureErr != nil {
		return nil, s.captureErr
	}
	if s.retired {
		return nil, ErrCacheAdmission
	}
	return &mutationTestAttempt{source: s, id: s.captures}, nil
}

type mutationTestAttempt struct {
	source *mutationTestSource
	id     int
	used   bool
}

func (a *mutationTestAttempt) Apply(fn func() error) error {
	if a.used {
		return ErrCacheAdmission
	}
	a.used = true
	s := a.source
	if s.before != nil {
		s.before(a.id)
	}
	if s.retired {
		return ErrCacheAdmission
	}
	if s.active {
		return errors.New("nested cache admission")
	}
	s.active = true
	defer func() { s.active = false }()
	s.applies++
	err := fn()
	if s.after != nil {
		s.after()
	}
	return err
}

type mutationTestExchange struct {
	base   Exchanger
	before func(*dns.Msg)
}

func (e mutationTestExchange) Exchange(ctx context.Context, m *dns.Msg, server netip.AddrPort, tcp bool) (*dns.Msg, error) {
	e.before(m)
	return e.base.Exchange(ctx, m, server, tcp)
}

func mutationTestContext(t *testing.T, s *mutationTestSource) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return WithCacheMutations(ctx, s)
}

func TestNativeMutationRealPeerProofAdmission(t *testing.T) {
	for _, nsec3 := range []bool{false, true} {
		for _, wildcard := range []bool{false, true} {
			t.Run(fmt.Sprintf("nsec3=%t/wildcard=%t", nsec3, wildcard), func(t *testing.T) {
				var lines []string
				if wildcard {
					lines = []string{"*. 300 IN A 192.0.2.4"}
				}
				r, count := aggressivePeer(t, nsec3, lines, nil, nil)
				r.requireCacheMutations = true
				source := &mutationTestSource{}
				r.ex = mutationTestExchange{r.ex, func(*dns.Msg) {
					if source.active || source.captures == 0 {
						t.Error("network inside final admission or before original capture")
					}
				}}
				ctx := mutationTestContext(t, source)
				m, v, err := r.ExchangeWithValidation(ctx, clientQuery("one.branch.example.", dns.TypeA))
				if err != nil {
					t.Fatal(err)
				}
				code := dns.RcodeNameError
				if wildcard {
					code = dns.RcodeSuccess
				}
				want(t, m, v.State, code, SecSecure)
				if source.applies == 0 || r.denials.entries.Len() == 0 {
					t.Fatal("actual validated proof cache not populated")
				}
				before := count.Load()
				m, v, err = r.ExchangeWithValidation(ctx, clientQuery("two.branch.example.", dns.TypeA))
				if err != nil {
					t.Fatal(err)
				}
				want(t, m, v.State, code, SecSecure)
				if count.Load() != before {
					t.Fatal("admitted proof no longer synthesizes another name")
				}
				if _, _, err := r.ExchangeWithValidation(t.Context(), clientQuery("two.branch.example.", dns.TypeA)); !errors.Is(err, ErrCacheAdmission) {
					t.Fatal("required source omitted on warm proof", err)
				}
			})
		}
	}
}

func TestNativeMutationAllCacheKindsAndNoNestedAdmission(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	r.requireCacheMutations = true
	source := &mutationTestSource{}
	prefixes := map[string]bool{}
	source.after = func() {
		r.cache.mu.Lock()
		defer r.cache.mu.Unlock()
		for key := range r.cache.m {
			prefixes[strings.Split(key, "|")[0]] = true
		}
	}
	r.ex = mutationTestExchange{r.ex, func(*dns.Msg) {
		if source.active || source.captures == 0 {
			t.Error("acquisition crossed final admission")
		}
	}}
	ctx := mutationTestContext(t, source)
	for _, name := range []string{"www.example.com.", "www.glueless.org."} {
		m, v, err := r.ExchangeWithValidation(ctx, clientQuery(name, dns.TypeA))
		if err != nil {
			t.Fatal(err)
		}
		if m.Rcode != dns.RcodeSuccess || v.State != SecSecure {
			t.Fatal("signed acquisition failed", name, m, v)
		}
	}
	for _, prefix := range []string{"c", "a", "k", "d", "r"} {
		if !prefixes[prefix] {
			t.Fatal("missing actual native cache target", prefix, prefixes)
		}
	}
	if source.applies < 5 || source.captures < source.applies {
		t.Fatal("missing independent capture/consumption", source.captures, source.applies)
	}
}

func TestNativeMutationRequiredSourceAndPreAcquisitionRefusal(t *testing.T) {
	for _, kind := range []string{"missing", "foreign", "capture", "held-wire"} {
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t)
			r := w.resolver()
			r.requireCacheMutations = true
			source := &mutationTestSource{}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if kind == "foreign" {
				source.domain[0] = 1
			}
			if kind == "capture" {
				source.captureErr = ErrCacheAdmission
			}
			if kind != "missing" {
				ctx = WithCacheMutations(ctx, source)
			}
			calls := 0
			r.ex = mutationTestExchange{r.ex, func(*dns.Msg) {
				calls++
				if source.captures == 0 {
					t.Error("wire preceded capture")
				}
				if kind == "held-wire" {
					source.retired = true
				}
			}}
			m, _, err := r.ExchangeWithValidation(ctx, clientQuery("www.example.com.", dns.TypeA))
			if !errors.Is(err, ErrCacheAdmission) || m != nil || r.CacheLen() != 0 || r.denials.entries.Len() != 0 {
				t.Fatal("unavailable original acquired/cache-filled", m, err, r.CacheLen())
			}
			if kind == "held-wire" {
				if calls == 0 {
					t.Fatal("held wire target not reached")
				}
			} else if calls != 0 {
				t.Fatal("capture refusal still acquired", calls)
			}
		})
	}
}

func TestNativeMutationEachEntryRefusesBeforeWrite(t *testing.T) {
	for _, kind := range []string{"keys", "ds", "address"} {
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t)
			r := w.resolver()
			_, _ = ask(t, r, "www.glueless.org.", dns.TypeA)
			_, _ = ask(t, r, "www.example.com.", dns.TypeA)
			source := &mutationTestSource{before: func(int) {}}
			source.before = func(id int) {
				if id == 1 {
					source.retired = true
				}
			}
			ctx := mutationTestContext(t, source)
			q := r.newQuery(ctx, r.set.Load())
			prefix, name := kKeys, "example.com."
			if kind == "ds" {
				prefix = kDS
			}
			if kind == "address" {
				prefix, name = kAddr, "ns.hosting.net."
			}
			key := q.key(prefix, name)
			r.cache.mu.Lock()
			e := r.cache.m[key]
			if e != nil {
				delete(r.cache.m, key)
				r.cache.l.Remove(e)
			}
			r.cache.mu.Unlock()
			switch kind {
			case "keys":
				q.keys(name)
			case "ds":
				q.dsFor(name)
			case "address":
				q.resolveAddrs(name)
			}
			if !source.retired || !errors.Is(q.st.cacheErr, ErrCacheAdmission) {
				t.Fatal("target final gate not reached", kind, q.st.cacheErr)
			}
			if _, ok := r.cache.get(key, time.Now()); ok {
				t.Fatal("refused target wrote entry", kind)
			}
		})
	}
}

func TestNativeMutationOriginalTTLFlushAndCancellation(t *testing.T) {
	for _, kind := range []string{"origin", "flush", "cancel", "proof-expiry"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			r, _ := aggressivePeer(t, false, nil, nil, func() time.Time { return now })
			_, _ = ask(t, r, "one.example.", dns.TypeA)
			old := r.newQuery(t.Context(), r.set.Load())
			proof := r.denials.candidates(old.key("denial|", "example."), old.st.denialEpoch, now, 1)[0]
			r.Flush()
			source := &mutationTestSource{}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			ctx = WithCacheMutations(ctx, source)
			q := r.newQuery(ctx, r.set.Load())
			batch, err := q.captureMutations()
			if err != nil {
				t.Fatal(err)
			}
			origin := now
			key := q.key(kAddr, "held.example.")
			if kind != "proof-expiry" {
				q.queueCacheSet(batch, key, addrSet{}, 20*time.Second, origin)
			}
			q.rememberDenialInto(proof, batch)
			source.before = func(int) {
				switch kind {
				case "origin":
					now = origin.Add(15 * time.Second)
				case "flush":
					r.Flush()
				case "cancel":
					cancel()
				case "proof-expiry":
					now = proof.until
				}
			}
			batch.commit()
			if kind == "origin" {
				if _, ok := r.cache.get(key, now); !ok {
					t.Fatal("unexpired original TTL lost")
				}
				if _, ok := r.cache.get(key, origin.Add(20*time.Second)); ok {
					t.Fatal("queue renewed original TTL")
				}
			} else if r.CacheLen() != 0 || r.denials.entries.Len() != 0 {
				t.Fatal("stale/canceled mutation entered", kind)
			}
			if kind == "cancel" && !errors.Is(q.st.cacheErr, context.Canceled) {
				t.Fatal("original cancellation lost", q.st.cacheErr)
			}
		})
	}
}

func TestNativeMutationOneResponseBatchesProofs(t *testing.T) {
	r, _ := aggressivePeer(t, false, nil, nil, nil)
	_, _ = ask(t, r, "one.example.", dns.TypeA)
	original := r.newQuery(t.Context(), r.set.Load())
	proof := r.denials.candidates(original.key("denial|", "example."), original.st.denialEpoch, time.Now(), 1)[0]
	r.Flush()
	source := &mutationTestSource{}
	q := r.newQuery(mutationTestContext(t, source), r.set.Load())
	batch, err := q.captureMutations()
	if err != nil {
		t.Fatal(err)
	}
	q.rememberDenialInto(proof, batch)
	q.rememberDenialInto(proof, batch)
	if source.applies != 0 || r.denials.entries.Len() != 0 {
		t.Fatal("proof mutated before response completion")
	}
	batch.commit()
	if source.applies != 1 || r.denials.entries.Len() != 2 {
		t.Fatal("one response did not share one admission", source.applies, r.denials.entries.Len())
	}
	batch.commit()
	if source.applies != 1 || !errors.Is(q.st.cacheErr, ErrCacheAdmission) {
		t.Fatal("response batch reused")
	}
}

func TestNativeMutationChecksInsideContendedCacheLocks(t *testing.T) {
	for _, kind := range []string{"entry", "proof"} {
		t.Run(kind, func(t *testing.T) {
			c, d := newLRU(10), newDenialCache()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			checkEntered := make(chan struct{})
			finished := make(chan struct{})
			started := make(chan struct{})
			valid := true
			guard := func() bool { close(checkEntered); return valid }
			if kind == "entry" {
				c.mu.Lock()
			} else {
				d.mu.Lock()
			}
			unlock := func() {
				if kind == "entry" {
					c.mu.Unlock()
				} else {
					d.mu.Unlock()
				}
			}
			go func() {
				defer close(finished)
				close(started)
				if kind == "entry" {
					c.setUntil("original", 1, time.Now().Add(time.Minute), 0, guard)
				} else {
					d.putIf("original", 0, &denialProof{}, 1, guard)
				}
			}()
			<-started
			select {
			case <-checkEntered:
				unlock()
				t.Fatal("validity checked before native cache exclusion")
			case <-time.After(20 * time.Millisecond):
			}
			valid = false
			unlock()
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case <-checkEntered:
			default:
				t.Fatal("actual write guard omitted")
			}
			if c.len() != 0 || d.entries.Len() != 0 {
				t.Fatal("late invalid data entered actual cache")
			}
		})
	}
}

type mutationDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c mutationDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestNativeMutationElapsedDeadlineBeforeCancellationSignal(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	ctx := mutationDeadlineContext{Context: context.Background(), deadline: time.Now().Add(-time.Second)}
	if ctx.Err() != nil {
		t.Fatal("fixture must delay cancellation signal")
	}
	q := r.newQuery(ctx, r.set.Load())
	q.queueCacheSet(nil, q.key(kAddr, "held.example."), addrSet{}, time.Minute, time.Now())
	if r.CacheLen() != 0 {
		t.Fatal("elapsed original deadline admitted native data before timer signal")
	}
}

// skippingSource admits every cache attempt and consumes it without running
// its writes, as the node's root source does after an ordinary canonical
// commit (D377): the attempt is still checked, but no write of a superseded
// data generation reaches the native cache.
type skippingSource struct{ captures, applies int }

func (*skippingSource) Domain() [32]byte { return [32]byte{} }
func (s *skippingSource) Capture(context.Context) (CacheMutation, error) {
	s.captures++
	return &skippingAttempt{source: s}, nil
}

type skippingAttempt struct {
	source *skippingSource
	used   bool
}

func (a *skippingAttempt) Apply(func() error) error {
	if a.used {
		return ErrCacheAdmission
	}
	a.used = true
	a.source.applies++
	return nil
}

// TestNativeMutationSkippedWritesStillResolve: a validating resolution whose
// every native cache write is skipped (the window after an ordinary canonical
// commit, before the successor publication) gives the same response code,
// security state and answers as a normal resolution, across delegations,
// CNAMEs and DNAMEs between zones, glueless name servers, insecure
// delegations and NSEC/NSEC3 denials. Without its own cache the resolution
// asks the authorities again for delegations and DNSSEC keys it would
// otherwise remember; the logged counts measure that cost.
func TestNativeMutationSkippedWritesStillResolve(t *testing.T) {
	w := newWorld(t)
	for _, tc := range []struct {
		name  string
		qtype uint16
	}{
		{"www.example.com.", dns.TypeA},
		{"www.example.com.", dns.TypeAAAA},
		{"www.example.com.", dns.TypeMX},
		{"alias.example.com.", dns.TypeA},
		{"far.example.com.", dns.TypeA},
		{"x.wild.example.com.", dns.TypeA},
		{"a.b.deep.example.com.", dns.TypeA},
		{"www.old.example.com.", dns.TypeA},
		{"txt.example.com.", dns.TypeTXT},
		{"nope.example.com.", dns.TypeA},
		{"www.insecure.com.", dns.TypeA},
		{"www.unsigned.org.", dns.TypeA},
		{"www.nsec3.org.", dns.TypeA},
		{"x.wild.nsec3.org.", dns.TypeA},
		{"nope.nsec3.org.", dns.TypeA},
		{"www.glueless.org.", dns.TypeA},
	} {
		t.Run(tc.name+"/"+dns.TypeToString[tc.qtype], func(t *testing.T) {
			// Both start cold, so the counts compare one uncached lookup.
			reference := w.resolver()
			reference.requireCacheMutations = true
			skipped := w.resolver()
			skipped.requireCacheMutations = true
			w.net.reset()
			wantMsg, wantV, err := reference.ExchangeWithValidation(mutationTestContext(t, &mutationTestSource{}), clientQuery(tc.name, tc.qtype))
			if err != nil {
				t.Fatal(err)
			}
			refAsked := len(w.net.queries())
			w.net.reset()
			source := &skippingSource{}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			gotMsg, gotV, err := skipped.ExchangeWithValidation(WithCacheMutations(ctx, source), clientQuery(tc.name, tc.qtype))
			if err != nil {
				t.Fatal("skipped writes failed the resolution", err)
			}
			skipAsked := len(w.net.queries())
			if gotMsg.Rcode != wantMsg.Rcode || gotV.State != wantV.State || len(gotMsg.Answer) != len(wantMsg.Answer) || len(gotMsg.Ns) != len(wantMsg.Ns) {
				t.Fatalf("skipped writes changed the result: %s %s %d/%d answer/authority, want %s %s %d/%d\n%v",
					dns.RcodeToString[gotMsg.Rcode], gotV.State, len(gotMsg.Answer), len(gotMsg.Ns),
					dns.RcodeToString[wantMsg.Rcode], wantV.State, len(wantMsg.Answer), len(wantMsg.Ns), gotMsg)
			}
			for i := range wantMsg.Answer {
				if gotMsg.Answer[i].String() != wantMsg.Answer[i].String() {
					t.Fatalf("answer %d is %v, want %v", i, gotMsg.Answer[i], wantMsg.Answer[i])
				}
			}
			// An attempt with no queued write is never applied.
			if source.applies == 0 || source.applies > source.captures {
				t.Fatal("cache attempts not consumed", source.captures, source.applies)
			}
			if skipped.CacheLen() != 0 {
				t.Fatal("a skipped write reached the native cache", skipped.CacheLen())
			}
			// The worst case of this world (an NSEC3 denial) asks 60; keep a
			// cold lookup with skipped writes well inside the per-query budget.
			if skipAsked > maxQueries*3/4 {
				t.Fatalf("skipped writes cost %d authority queries of the budget %d", skipAsked, maxQueries)
			}
			t.Logf("%s %s: %s %s, authority queries %d with writes, %d with writes skipped",
				tc.name, dns.TypeToString[tc.qtype], dns.RcodeToString[gotMsg.Rcode], gotV.State, refAsked, skipAsked)
		})
	}
}

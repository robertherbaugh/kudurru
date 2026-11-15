package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
)

type loginLimitTransport struct {
	http.RoundTripper
	writes *atomic.Int32
	before func()
	lost   bool
}

func (r loginLimitTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	write := q.Method == http.MethodPut && strings.HasSuffix(q.URL.Path, "/"+loginLimitName)
	if write {
		r.writes.Add(1)
		if r.before != nil {
			r.before()
		}
	}
	res, err := r.RoundTripper.RoundTrip(q)
	if write && r.lost && err == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return nil, errors.New("test: lost committed login-limit response")
	}
	return res, err
}
func TestLoginLimitActualReplicasRestartConflictAndLostReply(t *testing.T) {
	requireEnv(t)
	const ns = "login-limit-test"
	if err := shared.admin.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	makeLimit := func(s *Store) *loginlimit.Limiter {
		t.Helper()
		l, err := loginlimit.New(s, loginlimit.Policy{Failures: 5, Window: 5 * time.Minute, Lockout: 5 * time.Minute}, loginlimit.Policy{Failures: 10, Window: 15 * time.Minute, Lockout: 15 * time.Minute}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	a := newTestStore(t, Options{Namespace: ns})
	b := newTestStore(t, Options{Namespace: ns})
	la, lb := makeLimit(a), makeLimit(b)
	for i := range 5 {
		l := la
		if i%2 == 1 {
			l = lb
		}
		q, err := l.Begin(ctx, "192.0.2.1", "alice")
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Finish(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := makeLimit(newTestStore(t, Options{Namespace: ns}))
	_, err := restarted.Begin(ctx, "192.0.2.1", "other")
	var locked *loginlimit.Locked
	if !errors.As(err, &locked) {
		t.Fatal("restart lost shared IP failures", err)
	}
	var saved corev1.Secret
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: loginLimitName}, &saved); err != nil {
		t.Fatal(err)
	}
	uid := saved.UID
	readCounts := func() (uint64, int) {
		t.Helper()
		if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: loginLimitName}, &saved); err != nil {
			t.Fatal(err)
		}
		var state struct {
			Sequence uint64
			Entries  map[string]struct{ Pending []json.RawMessage }
		}
		if err := json.Unmarshal(saved.Data["state"], &state); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range state.Entries {
			n += len(e.Pending)
		}
		return state.Sequence, n
	}
	beforeSequence, beforePending := readCounts()
	if strings.Contains(string(saved.Data["state"]), "alice") {
		t.Fatal("raw login identity stored")
	}

	var writes atomic.Int32
	cfg := rest.CopyConfig(shared.cfg)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return loginLimitTransport{RoundTripper: rt, writes: &writes, lost: true}
	})
	lost := makeLimit(newStoreOn(t, cfg, Options{Namespace: ns}))
	q, err := lost.Begin(ctx, "lost", "lost")
	if !errors.Is(err, loginlimit.ErrUnavailable) || writes.Load() != 1 {
		t.Fatal("unknown write replayed or granted", err, writes.Load())
	}
	if err := q.Finish(ctx, true); !errors.Is(err, loginlimit.ErrUnavailable) {
		t.Fatal("unknown reserve returned usable ticket", err)
	}
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: loginLimitName}, &saved); err != nil || saved.UID != uid {
		t.Fatal("lost reply changed owner", err)
	}
	afterSequence, afterPending := readCounts()
	if afterSequence != beforeSequence+1 || afterPending != beforePending+2 {
		t.Fatal("actual unknown reservation not stored exactly once", beforeSequence, afterSequence, beforePending, afterPending)
	}

	writes.Store(0)
	cfg = rest.CopyConfig(shared.cfg)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return loginLimitTransport{RoundTripper: rt, writes: &writes, before: func() {
			q, err := lb.Begin(ctx, "interposed", "interposed")
			if err != nil {
				t.Fatal(err)
			}
			if err := q.Finish(ctx, false); err != nil {
				t.Fatal(err)
			}
		}}
	})
	racing := makeLimit(newStoreOn(t, cfg, Options{Namespace: ns}))
	if _, err := racing.Begin(ctx, "race", "race"); !errors.Is(err, loginlimit.ErrUnavailable) || writes.Load() != 1 {
		t.Fatal("original CAS was retried", err, writes.Load())
	}
	before := writes.Load()
	dead, stop := context.WithCancel(ctx)
	stop()
	if _, err := racing.Begin(dead, "canceled", "canceled"); !errors.Is(err, context.Canceled) || writes.Load() != before {
		t.Fatal("canceled admission wrote", err)
	}
	finalSequence, finalPending := readCounts()
	if finalSequence != afterSequence+2 || finalPending != afterPending {
		t.Fatal("refused CAS mutated shared counters", finalSequence, finalPending)
	}
	const foreignNS = "login-limit-foreign"
	if err := shared.admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: foreignNS}}); err != nil {
		t.Fatal(err)
	}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: loginLimitName, Namespace: foreignNS}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"foreign": []byte("retained")}}
	if err := shared.admin.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	foreignLimit := makeLimit(newTestStore(t, Options{Namespace: foreignNS}))
	if _, err := foreignLimit.Begin(ctx, "foreign", "foreign"); !errors.Is(err, loginlimit.ErrUnavailable) {
		t.Fatal("foreign secret adopted", err)
	}
	var unchanged corev1.Secret
	if err := shared.admin.Get(ctx, client.ObjectKeyFromObject(foreign), &unchanged); err != nil || unchanged.UID != foreign.UID || unchanged.ResourceVersion != foreign.ResourceVersion {
		t.Fatal("foreign secret changed", err)
	}
}

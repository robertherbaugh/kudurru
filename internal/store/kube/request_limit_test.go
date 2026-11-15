package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/requestlimit"
)

type requestLimitTransport struct {
	http.RoundTripper
	writes *atomic.Int32
	before func()
	lost   bool
}

func (r requestLimitTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	write := q.Method == http.MethodPut && strings.HasSuffix(q.URL.Path, "/"+requestLimitName)
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
		return nil, errors.New("test: lost committed browser-budget response")
	}
	return res, err
}

func TestRequestLimitActualReplicasRestartConflictAndLostReply(t *testing.T) {
	requireEnv(t)
	const ns = "request-limit-test"
	if err := shared.admin.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	limit := func(s *Store) *requestlimit.Limiter {
		t.Helper()
		l, err := requestlimit.New(s, nil)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	ctx, cancel := context.WithTimeout(t.Context(), requestlimit.Lifetime)
	defer cancel()
	a, b := newTestStore(t, Options{Namespace: ns}), newTestStore(t, Options{Namespace: ns})
	la, lb := limit(a), limit(b)
	for i := range 5 {
		l := la
		if i%2 == 1 {
			l = lb
		}
		if err := l.Admit(ctx, requestlimit.Start, "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := limit(newTestStore(t, Options{Namespace: ns}))
	var locked *loginlimit.Locked
	if err := restarted.Admit(ctx, requestlimit.Start, "192.0.2.1"); !errors.As(err, &locked) {
		t.Fatal("restart bypassed shared requests", err)
	}
	read := func() corev1.Secret {
		t.Helper()
		var s corev1.Secret
		if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: requestLimitName}, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	count := func(s corev1.Secret) int {
		t.Helper()
		var d struct{ Entries map[string]json.RawMessage }
		if err := json.Unmarshal(s.Data["state"], &d); err != nil {
			t.Fatal(err)
		}
		return len(d.Entries)
	}
	before := read()
	if bytes.Contains(before.Data["state"], []byte("192.0.2.1")) || count(before) != 1 {
		t.Fatal("private durable document invalid")
	}
	var writes atomic.Int32
	cfg := rest.CopyConfig(shared.cfg)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return requestLimitTransport{RoundTripper: rt, writes: &writes, lost: true}
	})
	lost := limit(newStoreOn(t, cfg, Options{Namespace: ns}))
	if err := lost.Admit(ctx, requestlimit.Start, "192.0.2.2"); !errors.Is(err, requestlimit.ErrUnavailable) || writes.Load() != 1 {
		t.Fatal("unknown charge granted or retried", err, writes.Load())
	}
	after := read()
	if after.UID != before.UID || after.ResourceVersion == before.ResourceVersion || count(after) != 2 {
		t.Fatal("actual lost-reply charge not retained exactly once")
	}
	writes.Store(0)
	cfg = rest.CopyConfig(shared.cfg)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return requestLimitTransport{RoundTripper: rt, writes: &writes, before: func() {
			if err := lb.Admit(ctx, requestlimit.Callback, "192.0.2.3"); err != nil {
				t.Fatal(err)
			}
		}}
	})
	racing := limit(newStoreOn(t, cfg, Options{Namespace: ns}))
	if err := racing.Admit(ctx, requestlimit.Callback, "192.0.2.4"); !errors.Is(err, requestlimit.ErrUnavailable) || writes.Load() != 1 {
		t.Fatal("original conflict retried or granted", err, writes.Load())
	}
	last := read()
	if last.UID != before.UID || count(last) != 3 {
		t.Fatal("refused CAS changed document")
	}
	dead, stop := context.WithCancel(ctx)
	stop()
	if err := racing.Admit(dead, requestlimit.Start, "192.0.2.5"); !errors.Is(err, context.Canceled) || writes.Load() != 1 {
		t.Fatal("canceled admission wrote", err)
	}
	// A different Secret owner must remain byte-for-byte unadopted.
	const foreignNS = "request-limit-foreign"
	if err := shared.admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: foreignNS}}); err != nil {
		t.Fatal(err)
	}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: requestLimitName, Namespace: foreignNS}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"foreign": []byte("retained")}}
	if err := shared.admin.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := limit(newTestStore(t, Options{Namespace: foreignNS})).Admit(ctx, requestlimit.Start, "192.0.2.1"); !errors.Is(err, requestlimit.ErrUnavailable) {
		t.Fatal("foreign secret adopted", err)
	}
	var unchanged corev1.Secret
	if err := shared.admin.Get(ctx, client.ObjectKeyFromObject(foreign), &unchanged); err != nil || unchanged.UID != foreign.UID || unchanged.ResourceVersion != foreign.ResourceVersion {
		t.Fatal("foreign owner changed", err)
	}
}

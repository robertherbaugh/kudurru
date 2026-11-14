package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGeneratedCLITransportActualRouterPrefixAndErrors(t *testing.T) {
	f := newRouterCLI(t)
	var mu sync.Mutex
	var lastURI string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastURI = r.RequestURI
		mu.Unlock()
		http.StripPrefix("/control/dns", f.server.Config.Handler).ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	server := proxy.URL + "/control/dns"
	for _, name := range []string{"alpha", "beta"} {
		group := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"app": "contract"}}}
		if _, err := f.store.Apply(t.Context(), group, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal("seed actual router object")
		}
	}
	out, _, err := f.harness.run("--server", server, "get", "clientgroups", "-n", "default", "-o", "json")
	var list struct {
		Items []public.ClientGroup `json:"items"`
	}
	if err != nil || json.Unmarshal([]byte(out), &list) != nil || len(list.Items) != 2 {
		t.Fatal("CLI did not reach the actual router through the prefix")
	}
	c, err := apiclient.New(server, apiclient.Options{Token: apitest.BearerPrefix + "admin"})
	if err != nil {
		t.Fatal("construct prefixed CLI client")
	}
	q := url.Values{"labelSelector": {"app=contract"}}
	items, err := c.ListAll(t.Context(), "/objects/clientgroups?namespace=default", q, 1)
	if err != nil || len(items) != 2 || q.Get("limit") != "" || q.Get("continue") != "" {
		t.Fatal("generated transport lost inline query, filter, continuation or caller query ownership")
	}
	if _, err := c.PutObject(t.Context(), "clientgroups", "default", "alpha", "stale", list.Items[0]); !apiclient.IsStatus(err, http.StatusPreconditionFailed) {
		t.Fatal("generated write lost original If-Match refusal")
	}
	c.SetToken(apitest.BearerPrefix + "viewer")
	err = c.DeleteObject(t.Context(), "clientgroups", "default", "alpha")
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) || apiErr.Key != api.KeyForbidden || apiErr.Missing == nil {
		t.Fatal("generated transport lost structured permission metadata")
	}
	c.SetToken(apitest.BearerPrefix + "admin")
	_, err = c.GetObject(t.Context(), "clientgroups", "default", "a/b c%")
	if !apiclient.IsKey(err, api.KeyNotFound) {
		t.Fatal("escaped object segment changed the selected operation")
	}
	mu.Lock()
	uri := lastURI
	mu.Unlock()
	if uri != "/control/dns/api/v1/objects/clientgroups/default/a%2Fb%20c%25" {
		t.Fatal("prefix or escaped segment was lost or double escaped")
	}
	if f.count(http.MethodPut, "/api/v1/objects/clientgroups/default/alpha") != 1 || f.count(http.MethodDelete, "/api/v1/objects/clientgroups/default/alpha") != 1 {
		t.Fatal("refused write retried through another transport")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Get(ctx, "/version", nil, nil); !errors.Is(err, context.Canceled) || f.count(http.MethodGet, "/api/v1/version") != 0 {
		t.Fatal("canceled generated request reached the router or lost its cause")
	}
}

func TestGeneratedCLITransportActualRouterSSEResumeAndDeadline(t *testing.T) {
	f := newRouterCLI(t)
	c, err := apiclient.New(f.server.URL, apiclient.Options{Token: apitest.BearerPrefix + "operator", Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal("construct stream client")
	}
	for _, id := range []uint64{10, 11} {
		f.tail.Publish(&querylog.Event{ID: id, Time: time.Now(), Node: "contract-node", Proto: pipeline.ProtoUDP, Client: netip.MustParseAddr("192.0.2.4"), Name: "resume.example.", QType: 1, QClass: 1, Status: pipeline.StatusForwarded})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := make(chan apiclient.Event, 4)
	done := make(chan error, 1)
	go func() {
		done <- c.Subscribe(ctx, "/queries/stream", nil, apiclient.SubscribeOptions{LastEventID: "10"}, func(event apiclient.Event) error {
			seen <- event
			if event.ID == "12" {
				return apiclient.ErrStopSubscription
			}
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("actual stream returned an error")
			}
		case <-time.After(3 * time.Second):
			t.Error("actual stream did not join")
		}
	})
	select {
	case event := <-seen:
		if event.ID != "11" {
			t.Fatal("actual SSE resume ignored Last-Event-ID")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("actual SSE resume did not produce its retained event")
	}
	// A non-streaming client timeout must not bound the live SSE body.
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		done <- err
		t.Fatal("SSE inherited the ordinary request timeout")
	case <-timer.C:
	}
	f.tail.Publish(&querylog.Event{ID: 12, Time: time.Now(), Node: "contract-node", Proto: pipeline.ProtoUDP, Client: netip.MustParseAddr("192.0.2.4"), Name: "live.example.", QType: 1, QClass: 1, Status: pipeline.StatusForwarded})
	select {
	case event := <-seen:
		var row api.StreamRow
		if event.ID != "12" || json.Unmarshal(event.Data, &row) != nil || row.Domain != "live.example" {
			t.Fatal("actual SSE lost its live event after the ordinary timeout")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("actual live event did not arrive")
	}
	if f.count(http.MethodGet, "/api/v1/queries/stream") != 1 {
		t.Fatal("resume unexpectedly reopened the actual stream")
	}
}

func TestGeneratedCLITransportActualSQLiteCookiesAndCSRF(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	var logins, mints, logouts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/auth/login":
			logins.Add(1)
		case "POST /api/v1/auth/tokens":
			mints.Add(1)
		case "POST /api/v1/auth/logout":
			logouts.Add(1)
		}
		f.server.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := apiclient.New(srv.URL, apiclient.Options{HTTPClient: loginTOTPCurrentClient(t, f)})
	if err != nil {
		t.Fatal("construct actual cookie client")
	}
	me, err := c.Login(t.Context(), "root-command", "initial-password")
	if err != nil || me.CSRF == "" || me.Principal.Name != "root-command" {
		t.Fatal("actual cookie login/me chain failed")
	}
	c.SetCSRF("")
	if _, err := c.CreateToken(t.Context(), apiclient.CreateTokenRequest{}); !apiclient.IsStatus(err, http.StatusForbidden) {
		t.Fatal("missing CSRF was not refused by actual production auth")
	}
	c.SetCSRF(me.CSRF)
	token, err := c.CreateToken(t.Context(), apiclient.CreateTokenRequest{Description: "generated transport"})
	if err != nil || token.Secret == "" {
		t.Fatal("cookie/CSRF did not authorize actual token mint")
	}
	c.SetToken(token.Secret)
	if _, err := c.Me(t.Context()); err != nil {
		t.Fatal("generated transport lost new bearer credential")
	}
	c.SetToken("")
	if err := c.Logout(t.Context()); err != nil {
		t.Fatal("actual cookie logout failed")
	}
	if _, err := c.Me(t.Context()); !apiclient.IsKey(err, api.KeyUnauthenticated) {
		t.Fatal("logout retained cookie authority")
	}
	if logins.Load() != 1 || mints.Load() != 2 || logouts.Load() != 1 {
		t.Fatal("auth success/refusal was replayed")
	}
	if c.Token() != "" || strings.Contains(c.Server(), token.Secret) {
		t.Fatal("transport retained bearer after explicit removal")
	}
}

package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

func TestStaticCommandsRefuseBeforeForwardingOrBodyValidation(t *testing.T) {
	var forwarded int
	h := newHarness(t, func(o *Options) {
		o.Config.Cluster.Role = "static"
		o.AuthHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			forwarded++
			w.WriteHeader(http.StatusOK)
		})
	})
	h.seed("api", zoneObj("existing-api", "original.example."))
	if r := h.do("PUT", BasePath+"/objects/dnszones/existing-api", "admin", zoneDoc("existing-api", "replacement.example.")).want(http.StatusServiceUnavailable); r.errKey() != KeyReadOnly {
		t.Fatal("static mode only protected file-owned objects")
	}
	for _, op := range []struct{ method, path string }{
		{"PUT", "/objects/dnszones/example"},
		{"POST", "/apply"},
		{"PATCH", "/settings"},
		{"POST", "/iam/users"},
		{"POST", "/iam/users/alice/password"},
		{"POST", "/iam/rolebindings"},
		{"PUT", "/iam/rolebindings/default/admin"},
		{"DELETE", "/iam/rolebindings/default/admin"},
		{"POST", "/auth/password"},
		{"POST", "/auth/totp/setup"},
		{"POST", "/auth/tokens"},
		{"DELETE", "/auth/tokens/example"},
		{"POST", "/integrations/default/example/sync"},
		{"POST", "/integrations/default/example/actions/restart"},
		{"POST", "/imports/apply"},
	} {
		t.Run(op.method+op.path, func(t *testing.T) {
			r := h.do(op.method, BasePath+op.path, "admin", map[string]any{}).want(http.StatusServiceUnavailable)
			if r.errKey() != KeyReadOnly {
				t.Fatal("static refusal missing", r.errKey())
			}
		})
	}
	if forwarded != 0 {
		t.Fatal("static mutation reached identity handler")
	}
	h.do("POST", BasePath+"/iam/users", "", map[string]any{}).want(http.StatusUnauthorized)
	h.do("POST", BasePath+"/iam/users", "viewer", map[string]any{}).want(http.StatusForbidden)
	h.do("POST", BasePath+"/auth/login", "", LoginRequest{Username: "admin", Password: "test-password"}).want(http.StatusOK)
	h.do("GET", BasePath+"/iam/users", "admin", nil).want(http.StatusOK)
	if forwarded != 2 {
		t.Fatal("static mode blocked authentication or inventory reads")
	}
	var info ClusterInfo
	h.do("GET", BasePath+"/cluster", "admin", nil).want(http.StatusOK).json(&info)
	if !info.ReadOnly {
		t.Fatal("static startup without loader advertised writes")
	}
	var schema List[SettingKey]
	h.do("GET", BasePath+"/settings/_schema", "admin", nil).want(http.StatusOK).json(&schema)
	for _, key := range schema.Items {
		if key.Scope == "cluster" && !key.ReadOnly {
			t.Fatal("static settings schema advertised writable cluster key", key.Path)
		}
	}
}

func TestStaticPeerStatusRequiresConfiguredName(t *testing.T) {
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{7}, 32), NodeName: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: creds, Status: func(context.Context) internalapi.Status {
		ready := true
		return internalapi.Status{Node: "another-node", Ready: &ready}
	}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(peer.Handler())
	ts.TLS = creds.ServerTLS()
	ts.StartTLS()
	defer ts.Close()
	h := newHarness(t, func(o *Options) {
		o.Config.Cluster.Role = "static"
		o.InternalClient = internalapi.NewClient(creds, internalapi.ClientOptions{})
		o.Membership = &cluster.Static{SelfName: "node-a", LeaderName: "node-a", Members: []cluster.Peer{
			{Name: "node-a"}, {Name: "node-b", Address: strings.TrimPrefix(ts.URL, "https://")},
		}}
	})
	var nodes List[NodeInfo]
	h.do(http.MethodGet, BasePath+"/nodes", "admin", nil).want(http.StatusOK).json(&nodes)
	if len(nodes.Items) != 2 || nodes.Items[1].Name != "node-b" || nodes.Items[1].Ready != nil || nodes.Items[1].Error == "" {
		t.Fatal("shared credentials admitted another node's status as the configured peer", nodes.Items)
	}
}

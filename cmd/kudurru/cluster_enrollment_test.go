package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
)

func TestClusterActivationCLIActualRouter(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	f.s.opts.Config.Node.DataDir = t.TempDir()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); f.server.ServeHTTP(w, r) }))
	defer srv.Close()
	execute := func(args ...string) (string, error) {
		var out strings.Builder
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--server", srv.URL, "--token", f.token, "-o", "json", "cluster"}, args...))
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}
	output, err := execute("enrollment")
	if err != nil {
		t.Fatal("CLI review refused", err)
	}
	var review api.ClusterEnrollmentInfo
	if json.Unmarshal([]byte(output), &review) != nil || !review.Dormant {
		t.Fatal("review missing")
	}
	n := requests.Load()
	if _, err := execute("activate"); err == nil || requests.Load() != n {
		t.Fatal("missing explicit review performed I/O")
	}
	args := []string{"activate", "--cluster-id", review.ClusterID, "--member-id", review.MemberID, "--source-digest", strings.Repeat("a", 64), "--owner-digest", review.OwnerDigest, "--name", "primary", "--address", "127.0.0.1:9443", "--reason", "reviewed activation"}
	if _, err := execute(args...); err == nil || requests.Load() != n+1 {
		t.Fatal("stale command fetched/retried or succeeded", err)
	}
	args[6] = review.SourceDigest
	output, err = execute(args...)
	if err != nil || requests.Load() != n+2 {
		t.Fatal("single reviewed activation refused or retried", err)
	}
	var receipt api.ClusterActivationReceipt
	if json.Unmarshal([]byte(output), &receipt) != nil || !receipt.Committed || !receipt.RestartRequired {
		t.Fatal("CLI omitted committed restart receipt")
	}
}

func TestClusterActivationLegacyStaticAndPermissionRefusals(t *testing.T) {
	for _, role := range []string{"primary", "static"} {
		t.Run(role, func(t *testing.T) {
			f := newRouterCLI(t, func(o *api.Options) { o.Config.Cluster.Role = role })
			args := []string{"cluster", "activate", "--cluster-id", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "--member-id", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "--source-digest", strings.Repeat("a", 64), "--owner-digest", strings.Repeat("b", 64), "--name", "node", "--address", "127.0.0.1:9443", "--reason", "reviewed"}
			if out, _, err := f.run(args...); err == nil || out != "" {
				t.Fatal("unsupported runtime activated")
			}
			if f.count("POST", "/api/v1/cluster/activate") != 1 {
				t.Fatal("unsupported activation retried")
			}
			f.env["KUDURRU_TOKEN"] = apitest.BearerPrefix + "viewer"
			if out, _, err := f.run(args...); err == nil || out != "" {
				t.Fatal("viewer activated")
			}
		})
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cluster"
)

func TestClusterStatusRealRouter(t *testing.T) {
	for _, static := range []bool{false, true} {
		name := "single"
		if static {
			name = "static"
		}
		t.Run(name, func(t *testing.T) {
			f := newRouterCLI(t, func(o *api.Options) {
				if static {
					o.Config.Cluster.Role = "static"
					o.Membership = &cluster.Static{SelfName: o.NodeName, LeaderName: o.NodeName, Members: []cluster.Peer{
						{Name: o.NodeName}, {Name: "peer"},
					}}
				}
			})
			wantNodes := 1
			if static {
				wantNodes = 2
			}
			for _, format := range []string{"json", "yaml"} {
				out, stderr, err := f.run("cluster", "status", "-o", format)
				if err != nil || stderr != "" {
					t.Fatal("cluster status did not cross the authenticated route", err, stderr)
				}
				data := []byte(out)
				if format == "yaml" {
					data, err = yaml.YAMLToJSON(data)
					if err != nil {
						t.Fatal(err)
					}
				}
				var got api.ClusterInfo
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				if got.Driver != "memory" || got.ClusterID != "" || got.Epoch != 0 || got.Leader != "contract-node" || got.Node != "contract-node" || got.Nodes != wantNodes || got.ReadOnly != static {
					t.Fatal("cluster status changed the server's observed shape", got)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(data, &fields); err != nil || fields["readOnly"] == nil || fields["took_ms"] == nil {
					t.Fatal("structured status lost original response fields")
				}
			}
			for _, format := range []string{"table", "wide"} {
				out, _, err := f.run("cluster", "status", "-o", format)
				lines := strings.Split(strings.TrimSpace(out), "\n")
				if err != nil || len(lines) != 2 || !strings.Contains(lines[0], "READ-ONLY") {
					t.Fatal("cluster table missing header or single observation", out, err)
				}
				want := "contract-node memory - contract-node 1 false"
				if static {
					want = "contract-node memory - contract-node 2 true"
				}
				if format == "wide" {
					want += " - 0"
				}
				if strings.Join(strings.Fields(lines[1]), " ") != want {
					t.Fatal("cluster table fabricated or omitted observed fields", out)
				}
			}
			if f.count(http.MethodGet, "/api/v1/cluster") != 4 || f.count(http.MethodGet, "/api/v1/nodes") != 0 {
				t.Fatal("cluster status must issue one status observation per invocation")
			}
		})
	}
}

// Against an enrolled follower the status names the role and the primary, the
// structured output carries the capability lists, and a command that the
// follower does not forward fails once with the server's typed refusal and
// the client's hint.
func TestClusterStatusAndRefusalOnAFollower(t *testing.T) {
	f := newRouterCLI(t, func(o *api.Options) {
		o.ClusterRole = func(context.Context) api.ClusterRole { return api.ClusterRole{Follower: true, Primary: "node-b"} }
	})
	out, _, err := f.run("cluster", "status")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err != nil || len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "NODE DRIVER ROLE LEADER NODES READ-ONLY" ||
		strings.Join(strings.Fields(lines[1]), " ") != "contract-node memory follower node-b 1 false" {
		t.Fatal("cluster status does not show the follower's role and primary", out, err)
	}
	out, _, err = f.run("cluster", "status", "-o", "json")
	var got api.ClusterInfo
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || got.Role != "follower" || got.Primary != "node-b" || !got.PrimaryRequired ||
		!slices.Contains(got.ForwardedOperations, "apply") || !slices.Contains(got.LocalOperations, "flushCache") {
		t.Fatal("structured cluster status lost the follower capability", out, err)
	}
	_, _, err = f.run("lists", "refresh", "ads")
	var refusal *apiclient.Error
	if !errors.As(err, &refusal) || refusal.StatusCode != http.StatusServiceUnavailable || refusal.Key != api.KeyPrimaryRequired ||
		!strings.Contains(refusal.Error(), "follower of node-b") || !strings.Contains(refusal.Error(), "hint: send it to the API of the primary node-b") || refusal.RetryAfter != 0 {
		t.Fatal("refused follower command lost the server's typed refusal", err)
	}
	if hint := errorHint(err); !strings.Contains(hint, "run it against the primary") {
		t.Fatal("no client hint for primary_required:", hint)
	}
	if f.count(http.MethodPost, "/api/v1/blocklists/default/ads/refresh") != 1 {
		t.Fatal("the refused command was repeated or never sent")
	}
}

func TestClusterStatusRefusals(t *testing.T) {
	f := newRouterCLI(t)
	f.env["KUDURRU_TOKEN"] = apitest.BearerPrefix + "zoneadmin"
	out, _, err := f.run("cluster", "status", "-o", "json")
	if !apiclient.IsKey(err, api.KeyForbidden) || out != "" || f.count(http.MethodGet, "/api/v1/cluster") != 1 {
		t.Fatal("cluster status lost the actual permission refusal or retried it", err)
	}
	f.env["KUDURRU_TOKEN"] = "invalid-test-credential"
	out, _, err = f.run("cluster", "status")
	if !apiclient.IsKey(err, api.KeyUnauthenticated) || out != "" || f.count(http.MethodGet, "/api/v1/cluster") != 2 {
		t.Fatal("cluster status lost the actual authentication refusal or retried it", err)
	}
	for _, args := range [][]string{
		{"cluster", "status", "unexpected"},
		{"cluster", "status", "-o", "invalid"},
		{"--transport", "kubernetes", "cluster", "status"},
	} {
		if _, _, err := f.run(args...); err == nil {
			t.Fatal("invalid cluster invocation succeeded", args)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr syncBuffer
	if err := f.runCtx(ctx, &stdout, &stderr, "--server", f.server.URL, "cluster", "status"); err == nil || stdout.String() != "" {
		t.Fatal("canceled cluster observation returned a status")
	}
	if f.count(http.MethodGet, "/api/v1/cluster") != 2 {
		t.Fatal("invalid or canceled cluster observation reached the server")
	}
}

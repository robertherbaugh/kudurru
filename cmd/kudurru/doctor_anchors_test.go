package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestDoctorAnchorsCheck(t *testing.T) {
	var status api.RootAnchorStatus
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.BasePath+"/cluster/anchors" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer srv.Close()
	c, err := apiclient.New(srv.URL, apiclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	run := func() (string, *doctorReport) {
		var out strings.Builder
		rep := &doctorReport{say: func(format string, args ...any) { _, _ = fmt.Fprintf(&out, format, args...) }}
		checkAnchors(t.Context(), rep, c, cfg)
		return out.String(), rep
	}
	if out, rep := run(); out != "" || rep.failures+rep.warnings != 0 {
		t.Fatal("static anchors were checked", out)
	}
	// A joined member whose own file does not set anchorMode takes managed
	// anchors from the primary's image: the node's effective mode decides.
	status = api.RootAnchorStatus{AnchorMode: "managed", Owner: api.RootAnchorOwner{Phase: "managed", Keys: []api.RootAnchorKey{{Status: "Valid"}}}, Warnings: []string{"this member refuses the primary's root-anchor state (carried_revocation)"}}
	if out, rep := run(); !strings.Contains(out, "anchors: WARN managed, 1 keys") || !strings.Contains(out, "carried_revocation") || rep.warnings != 1 {
		t.Fatal("managed anchors taken from the cluster were not checked", out)
	}
	status = api.RootAnchorStatus{AnchorMode: "static", Warnings: []string{"x"}}
	if out, rep := run(); out != "" || rep.failures+rep.warnings != 0 {
		t.Fatal("effective static anchors were checked", out)
	}
	code = http.StatusNotFound
	if out, rep := run(); out != "" || rep.failures+rep.warnings != 0 {
		t.Fatal("a static node without the route was warned", out)
	}
	code = http.StatusOK
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	if out, rep := run(); out != "" || rep.failures+rep.warnings != 0 {
		t.Fatal("the node's effective static mode did not win over the host file", out)
	}
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	status = api.RootAnchorStatus{Owner: api.RootAnchorOwner{Phase: "managed", Keys: []api.RootAnchorKey{{Status: "Valid"}}}, Updater: api.RootAnchorWorker{State: "running"}, Supervisor: api.RootAnchorWorker{State: "not_running"}}
	if out, rep := run(); !strings.Contains(out, "anchors: OK managed, 1 keys, updater running, supervisor not_running") || rep.failures+rep.warnings != 0 {
		t.Fatal("healthy anchors", out)
	}
	status.Warnings = []string{"the root-anchor updater stopped: x"}
	if out, rep := run(); !strings.Contains(out, "anchors: WARN") || !strings.Contains(out, "WARNING anchors: the root-anchor updater stopped: x") || rep.warnings != 1 {
		t.Fatal("warned anchors", out)
	}
	status.Owner.Quarantined = true
	if out, rep := run(); !strings.Contains(out, "anchors: FAIL every trusted root key is lost") || rep.failures != 1 {
		t.Fatal("quarantined anchors", out)
	}
	code = http.StatusServiceUnavailable
	if out, rep := run(); !strings.Contains(out, "anchors: WARN status unavailable") || rep.failures != 0 {
		t.Fatal("unavailable anchors", out)
	}
}

package webhook

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
)

func TestObservationScheduleRetainsNegotiatedCapabilities(t *testing.T) {
	for _, clients := range []bool{false, true} {
		name := "actions"
		if clients {
			name = "clients"
		}
		t.Run(name, func(t *testing.T) {
			var changed atomic.Bool
			var unexpected atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", integrationMedia)
				switch r.URL.Path {
				case "/provider/":
					d := actionDeclaration()
					if clients != changed.Load() {
						d = inventoryDeclaration()
					}
					_ = json.NewEncoder(w).Encode(d)
				case "/provider/actions":
					_ = json.NewEncoder(w).Encode([]integration.ActionSpec{{Name: "refresh", Verb: "integrations.act", Params: json.RawMessage(`{"type":"object"}`)}})
				default:
					unexpected.Add(1)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			p, _ := lifecycleProvider(t, server, &lifecycleHost{}, nil)
			if _, err := p.ObservationSchedule(); !errors.Is(err, host.ErrUnavailable) {
				t.Fatal("unnegotiated provider has a schedule")
			}
			if _, err := p.Roles(); !errors.Is(err, host.ErrUnavailable) {
				t.Fatal("unnegotiated provider has roles")
			}
			if _, err := p.Check(t.Context()); err != nil {
				t.Fatal(err)
			}
			want := time.Duration(0)
			if clients {
				want = time.Minute
			}
			if interval, err := p.ObservationSchedule(); err != nil || interval != want {
				t.Fatal("wrong negotiated inventory cadence")
			}
			if roles, err := p.Roles(); err != nil || roles != (host.Roles{Observer: clients}) {
				t.Fatalf("negotiated roles: %+v %v", roles, err)
			}
			changed.Store(true)
			if _, err := p.Check(t.Context()); !errors.Is(err, host.ErrFenced) {
				t.Fatal("changed declaration was accepted")
			}
			if _, err := p.ObservationSchedule(); !errors.Is(err, host.ErrFenced) {
				t.Fatal("changed provider retained a schedule")
			}
			if _, err := p.Roles(); !errors.Is(err, host.ErrFenced) {
				t.Fatal("changed provider retained its roles")
			}
			changed.Store(false)
			if _, err := p.ObservationSchedule(); !errors.Is(err, host.ErrFenced) {
				t.Fatal("ABA restored a schedule")
			}
			if unexpected.Load() != 0 {
				t.Fatal("schedule resolution performed inventory I/O")
			}
		})
	}
}

package unifi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

// writePeer is a TLS double of the UniFi surfaces the D401 writes use:
// networks with their DHCP detail, DNS policies and the legacy station
// command. Failure knobs make one specific answer fail.
type writePeer struct {
	mu       sync.Mutex
	networks []map[string]any
	details  map[string]map[string]any
	policies []map[string]any
	// failPut answers the PUT with that sequence number 500 without storing it;
	// ignorePut answers every PUT 200 without storing it.
	failPut, puts int
	ignorePut     bool
	stationRC     string
	writes        []string
}

func newWritePeer(t *testing.T, c public.UniFiIntegration) (*Provider, *writePeer) {
	t.Helper()
	w := &writePeer{details: map[string]map[string]any{}, stationRC: "ok"}
	w.addNetwork(networkID, "LAN", "GATEWAY", "SERVER")
	s := newTLSFixture(t, w.serve)
	loan := fixtureLoan()
	loan.writes = true
	return provisionFixture(t, s, c, loan), w
}

func (w *writePeer) addNetwork(id, name, management, mode string) {
	w.networks = append(w.networks, map[string]any{"id": id, "name": name, "vlanId": 10, "enabled": true, "management": management})
	w.details[id] = map[string]any{"id": id, "name": name, "vlanId": 10, "enabled": true, "management": management, "metadata": map[string]any{"origin": "USER_DEFINED"},
		"ipv4Configuration": map[string]any{"hostIpAddress": "192.0.2.1", "prefixLength": 24, "dhcpConfiguration": map[string]any{"mode": mode, "leaseTimeSeconds": 86400}}}
}

func (w *writePeer) written() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.writes)
}

func (w *writePeer) serve(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.Header.Get("X-API-Key") != fixtureKey {
		rw.WriteHeader(http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		w.writes = append(w.writes, r.Method+" "+r.URL.Path)
	}
	base := "/proxy/network/integration/v1"
	sites := base + "/sites/" + siteID
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base+"/info":
		_ = json.NewEncoder(rw).Encode(map[string]any{"applicationVersion": "10.6.106"})
	case r.Method == http.MethodGet && r.URL.Path == base+"/sites":
		fixturePage(rw, r, []map[string]string{{"id": siteID, "internalReference": "default"}})
	case r.Method == http.MethodGet && r.URL.Path == sites+"/networks":
		fixturePage(rw, r, w.networks)
	case r.Method == http.MethodGet && r.URL.Path == sites+"/dns/policies":
		fixturePage(rw, r, w.policies)
	case strings.HasPrefix(r.URL.Path, sites+"/networks/"):
		id := strings.TrimPrefix(r.URL.Path, sites+"/networks/")
		detail, ok := w.details[id]
		if !ok {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(rw).Encode(detail)
			return
		}
		w.puts++
		var body map[string]any
		if r.Method != http.MethodPut || json.NewDecoder(r.Body).Decode(&body) != nil || w.puts == w.failPut {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !w.ignorePut {
			body["id"], body["metadata"] = id, detail["metadata"]
			w.details[id] = body
		}
		_ = json.NewEncoder(rw).Encode(body)
	case r.Method == http.MethodPost && r.URL.Path == "/proxy/network/api/s/default/cmd/stamgr":
		_ = json.NewEncoder(rw).Encode(map[string]any{"meta": map[string]any{"rc": w.stationRC}, "data": []any{}})
	default:
		rw.WriteHeader(http.StatusNotFound)
	}
}

func dhcpConfig(networks ...string) public.UniFiIntegration {
	return public.UniFiIntegration{Legacy: true, Push: public.UniFiPush{DHCPDNSServers: true, DNSServers: []string{"192.0.2.53"}, Networks: networks, ClientActions: true}}
}

func dhcpInvoke(t *testing.T, p *Provider, params string) (DHCPPlan, error) {
	t.Helper()
	raw, err := p.Invoke(context.Background(), ActionDHCPDNSServers, json.RawMessage(params))
	var out DHCPPlan
	if err == nil && json.Unmarshal(raw, &out) != nil {
		t.Fatal("DHCP answer", string(raw))
	}
	return out, err
}

func dhcpApply(t *testing.T, p *Provider) (DHCPPlan, error) {
	t.Helper()
	preview, err := dhcpInvoke(t, p, `{"mode":"preview"}`)
	if err != nil {
		t.Fatal("DHCP preview", err)
	}
	return dhcpInvoke(t, p, `{"mode":"apply","digest":"`+preview.Digest+`"}`)
}

// A network that is missing, named twice or has no gateway DHCP server is
// named by the preview and refuses the apply before any write.
func TestDHCPApplyRefusesUnusableNetworksBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*writePeer)
		action string
	}{
		{"missing", func(*writePeer) {}, "missing"},
		{"duplicated", func(w *writePeer) {
			w.addNetwork("77777777-7777-4777-8777-777777777777", "IoT", "GATEWAY", "SERVER")
			w.addNetwork("88888888-8888-4888-8888-888888888888", "IoT", "GATEWAY", "SERVER")
		}, "missing"},
		{"switch", func(w *writePeer) { w.addNetwork("77777777-7777-4777-8777-777777777777", "IoT", "SWITCH", "SERVER") }, "unsupported"},
		{"relay", func(w *writePeer) { w.addNetwork("77777777-7777-4777-8777-777777777777", "IoT", "GATEWAY", "RELAY") }, "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, w := newWritePeer(t, dhcpConfig("LAN", "IoT"))
			w.mu.Lock()
			tc.setup(w)
			w.mu.Unlock()
			preview, err := dhcpInvoke(t, p, `{"mode":"preview"}`)
			if err != nil || len(preview.Changes) != 2 || preview.Changes[0].Action != "update" || preview.Changes[1].Action != tc.action {
				t.Fatalf("preview: %+v %v", preview, err)
			}
			if _, err := dhcpApply(t, p); !errors.Is(err, host.ErrUnsupported) {
				t.Fatal("unusable network did not refuse the apply", err)
			}
			if writes := w.written(); len(writes) != 0 {
				t.Fatal("refused apply wrote", writes)
			}
		})
	}
}

// A failed PUT on the second network stops the apply with an error (the
// first network's write stands and is not replayed); a write the controller
// answers but does not keep is drift; a repeat apply of a converged plan
// sends nothing.
func TestDHCPApplyPartialFailureDriftAndRepeat(t *testing.T) {
	second := "77777777-7777-4777-8777-777777777777"
	p, w := newWritePeer(t, dhcpConfig("LAN", "IoT"))
	w.mu.Lock()
	w.addNetwork(second, "IoT", "GATEWAY", "SERVER")
	w.failPut = 2
	w.mu.Unlock()
	if _, err := dhcpApply(t, p); err == nil || errors.Is(err, host.ErrUnsupported) || errors.Is(err, host.ErrStalePreview) {
		t.Fatal("failed second PUT was not an error", err)
	}
	if writes := w.written(); len(writes) != 2 || !strings.HasSuffix(writes[0], networkID) || !strings.HasSuffix(writes[1], second) {
		t.Fatal("partial apply", writes)
	}
	// The next reviewed apply sees the first network converged and writes
	// only the second.
	w.mu.Lock()
	w.failPut = 0
	w.mu.Unlock()
	out, err := dhcpApply(t, p)
	if err != nil || out.Applied != 1 || out.Unchanged != 1 {
		t.Fatal("resumed apply", out, err)
	}
	if writes := w.written(); len(writes) != 3 || !strings.HasSuffix(writes[2], second) {
		t.Fatal("resumed apply rewrote a converged network", writes)
	}
	out, err = dhcpApply(t, p)
	if err != nil || out.Applied != 0 || out.Unchanged != 2 || len(w.written()) != 3 {
		t.Fatal("repeat apply wrote", out, err, w.written())
	}
	// Someone reverts the servers and the controller then drops the write.
	w.mu.Lock()
	w.details[networkID]["ipv4Configuration"].(map[string]any)["dhcpConfiguration"].(map[string]any)["dnsServerIpAddressesOverride"] = []any{"192.0.2.1"}
	w.ignorePut = true
	w.mu.Unlock()
	if _, err := dhcpApply(t, p); !errors.Is(err, ErrDrift) {
		t.Fatal("unkept write was not drift", err)
	}
	if writes := w.written(); len(writes) != 4 {
		t.Fatal("drifting apply", writes)
	}
}

// A station command answered with anything but rc ok is not acknowledged.
func TestStationCommandRequiresAcknowledgment(t *testing.T) {
	p, w := newWritePeer(t, dhcpConfig("LAN"))
	raw, err := p.Invoke(context.Background(), ActionBlockClient, json.RawMessage(`{"mac":"00:11:22:AA:bb:CC"}`))
	if err != nil || !strings.Contains(string(raw), `"mac":"00:11:22:aa:bb:cc"`) {
		t.Fatal("block", string(raw), err)
	}
	w.mu.Lock()
	w.stationRC = "error"
	w.mu.Unlock()
	if _, err := p.Invoke(context.Background(), ActionUnblockClient, json.RawMessage(`{"mac":"00:11:22:33:44:55"}`)); !errors.Is(err, ErrInvalid) {
		t.Fatal("unacknowledged station command", err)
	}
	if writes := w.written(); len(writes) != 2 {
		t.Fatal("station commands", writes)
	}
}

func mirrorIntent(t *testing.T, p *Provider, desired public.Endpoint) *kinds.IntegrationOperation {
	t.Helper()
	p.mu.RLock()
	owner, destination := p.owner, p.mirrorDestination()
	p.mu.RUnlock()
	built, err := plan.Build(plan.Input{Owner: owner, Destination: destination, ConfigDigest: kinds.Digest([]byte("config")), CredentialDigest: kinds.Digest([]byte("credential")), SourceDigest: kinds.Digest([]byte("source")), Policy: plan.PolicySync, DesiredComplete: true, CurrentComplete: true, Desired: []public.Endpoint{desired}})
	if err != nil || len(built.Operations) != 1 {
		t.Fatal("plan", err)
	}
	return built.Intent(0, time.Now().Add(-time.Second))
}

// Apply rereads the site and refuses, before any write, when a foreign
// policy or a claim of the whole name appeared since the plan.
func TestMirrorApplyRefusesForeignPoliciesAtTheName(t *testing.T) {
	enabled := true
	for name, row := range map[string]map[string]any{
		"foreign A":     {"id": "99999999-9999-4999-8999-999999999999", "type": "A_RECORD", "enabled": enabled, "domain": "web.example", "ipv4Address": "192.0.2.99"},
		"foreign CNAME": {"id": "99999999-9999-4999-8999-999999999999", "type": "CNAME_RECORD", "enabled": enabled, "domain": "web.example", "targetDomain": "other.example"},
		"claim":         {"id": "99999999-9999-4999-8999-999999999999", "type": "FORWARD_DOMAIN", "enabled": enabled, "domain": "web.example", "ipAddress": "192.0.2.9"},
	} {
		t.Run(name, func(t *testing.T) {
			p, w := newWritePeer(t, public.UniFiIntegration{Push: public.UniFiPush{MirrorRecords: public.UniFiRecordMirror{Enabled: true, Domains: []string{"example"}}}})
			op := mirrorIntent(t, p, public.Endpoint{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80"}})
			w.mu.Lock()
			w.policies = append(w.policies, row)
			w.mu.Unlock()
			receipt, err := p.Apply(context.Background(), op)
			if !errors.Is(err, ErrDrift) || receipt.Complete {
				t.Fatal("foreign policy at the name did not refuse", receipt, err)
			}
			if writes := w.written(); len(writes) != 0 {
				t.Fatal("refused apply wrote", writes)
			}
		})
	}
}

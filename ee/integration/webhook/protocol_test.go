package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/integration"
)

func actionDeclaration() declaration {
	return declaration{Filters: []string{"example.test"}, Capabilities: []string{"actions"}, Manifest: integration.Manifest{
		Type: "fixture", DisplayName: "Fixture provider", Version: "1", IOClass: "local_polling",
		Capabilities: []integration.Capability{integration.CapActions}, ConfigSchema: json.RawMessage(`{"type":"object"}`),
	}}
}

func TestProtocolActualTLSNegotiationRetainsExactAdvertisement(t *testing.T) {
	var changed atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", integrationMedia)
		if r.URL.Path == "/provider/actions" {
			name := "refresh"
			if changed.Load() {
				name = "replace"
			}
			_ = json.NewEncoder(w).Encode([]integration.ActionSpec{{Name: name, Verb: "integrations.act", Params: json.RawMessage(`{"type":"object","additionalProperties":false}`)}})
			return
		}
		_ = json.NewEncoder(w).Encode(actionDeclaration())
	}))
	defer server.Close()
	h := &requestHost{}
	tr := serverTransport(t, h, server)
	first, err := tr.negotiate(t.Context())
	if err != nil || !first.has("actions") || first.has("records") || len(first.actions) != 1 || first.actions[0].Name != "refresh" {
		t.Fatalf("initial advertisement: %v", err)
	}
	stable, err := tr.negotiate(t.Context())
	if err != nil || stable.digest != first.digest {
		t.Fatalf("equal advertisement identity: %v", err)
	}
	changed.Store(true)
	next, err := tr.negotiate(t.Context())
	if err != nil || next.digest == first.digest || next.actions[0].Name != "replace" || first.actions[0].Name != "refresh" {
		t.Fatalf("detached changed advertisement: %v", err)
	}
	if len(h.mutations) != 6 {
		t.Fatal("negotiation did not charge each actual read")
	}
	for _, mutation := range h.mutations {
		if mutation {
			t.Fatal("negotiation performed a mutation")
		}
	}
}

func TestProtocolExternalDNSNegotiatesOnlyWireRecords(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", externalMedia)
		_, _ = w.Write([]byte(`{"include":["example.test"],"exclude":["private.example.test"]}`))
	}))
	defer server.Close()
	a, err := serverTransport(t, &requestHost{}, server).negotiate(t.Context())
	if err != nil || !a.has("records") || a.has("actions") || a.media != externalMedia || len(a.filter.Include) != 1 || calls.Load() != 1 {
		t.Fatalf("external-dns negotiation: %v", err)
	}
}

func TestProtocolMalformedOrUnadvertisedActionsRefuseWholeCandidate(t *testing.T) {
	good, err := json.Marshal(actionDeclaration())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, declaration, actions, media string }{
		{"null-declaration", "null", "[]", integrationMedia},
		{"unknown-media", string(good), "[]", "application/json"},
		{"unknown-field", strings.TrimSuffix(string(good), "}") + `,"extra":true}`, "[]", integrationMedia},
		{"duplicate-case-field", strings.TrimSuffix(string(good), "}") + `,"CAPABILITIES":["actions"]}`, "[]", integrationMedia},
		{"mismatched-capability", strings.Replace(string(good), `"Actions"`, `"RecordSink"`, 1), "[]", integrationMedia},
		{"credential-echo", strings.Replace(string(good), "Fixture provider", `explicit\u002dtest\u002dtoken`, 1), "[]", integrationMedia},
		{"null-action-array", string(good), "null", integrationMedia},
		{"null-action", string(good), "[null]", integrationMedia},
		{"action-traversal", string(good), `[{"name":"../escape","verb":"integrations.act","params":{}}]`, integrationMedia},
		{"action-verb", string(good), `[{"name":"refresh","verb":"users.write","params":{}}]`, integrationMedia},
		{"null-schema", string(good), `[{"name":"refresh","verb":"integrations.act","params":null}]`, integrationMedia},
		{"schema-duplicate", string(good), `[{"name":"refresh","verb":"integrations.act","params":{"type":"object","Type":"array"}}]`, integrationMedia},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.media)
				body := tc.declaration
				if r.URL.Path == "/provider/actions" {
					body = tc.actions
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			if a, err := serverTransport(t, &requestHost{}, server).negotiate(t.Context()); a != nil || !errors.Is(err, errProtocol) {
				t.Fatal("malformed peer produced a usable advertisement")
			}
		})
	}
}

func TestProtocolOriginalCancellationBetweenAdvertisementReads(t *testing.T) {
	var peerCalls, charges atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		peerCalls.Add(1)
		w.Header().Set("Content-Type", integrationMedia)
		_ = json.NewEncoder(w).Encode(actionDeclaration())
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := &requestHost{before: func(context.Context) error {
		if charges.Add(1) == 2 {
			cancel()
		}
		return nil
	}}
	a, err := serverTransport(t, h, server).negotiate(ctx)
	if a != nil || !errors.Is(err, context.Canceled) || peerCalls.Load() != 1 || charges.Load() != 2 {
		t.Fatal("next read adopted authority beyond original cancellation")
	}
}

func TestProtocolJSONWorkBoundsAndStrictKeys(t *testing.T) {
	for _, raw := range []string{
		`{"a":1,"A":2}`, `{} {}`, strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34),
		`{"filters":[],"filterſ":[]}`, `{"k":1,"K":2}`, "{\"a\":\"\xff\"}",
		"[" + strings.Repeat("0,", maxItems) + "0]", `{"escaped":1,"esc\u0061ped":2}`,
	} {
		if validJSON([]byte(raw)) == nil {
			t.Fatal("ambiguous or unbounded JSON accepted")
		}
	}
	if validJSON([]byte(`{"a":[1,{"b":true}],"c":null}`)) != nil {
		t.Fatal("bounded ordinary JSON refused")
	}
}

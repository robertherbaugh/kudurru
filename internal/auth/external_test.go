package auth

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestExternalProvenanceIsNotSerializedOrLoggedWithPrincipal(t *testing.T) {
	p := Principal{External: &ExternalProof{ProviderUID: "provider-instance", Origin: "object", SessionID: "private-session-proof", Evidence: "private-claims-proof"}}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{string(raw), fmt.Sprint(p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p)} {
		if strings.Contains(output, p.External.Evidence) || strings.Contains(output, p.External.SessionID) {
			t.Fatal("principal formatting disclosed owner evidence")
		}
	}
}

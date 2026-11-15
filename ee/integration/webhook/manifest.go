package webhook

import (
	"encoding/json"

	"github.com/robertherbaugh/kudurru/ee/integration"
)

// New constructs the mounted records/clients/actions/hooks adapter without
// I/O. Records are published only to the configured domains, with
// ownership kept in a kudurru TXT registry at the peer and every receipt
// confirmed by a reread, never by the wire answer (D380). A peer that
// advertises hooks needs the separate hmacKey; root produces its events
// (D379).
func New() integration.Integration { return newProvider() }

func manifest() integration.Manifest {
	return integration.Manifest{Type: "webhook", DisplayName: "Webhook", Version: "1", IOClass: "local_polling",
		Capabilities: []integration.Capability{integration.CapActions, integration.CapClientSource, integration.CapHook, integration.CapRecordSink},
		SecretKeys:   []integration.SecretKey{{Path: "bearerToken"}, {Path: "ca"}, {Path: "clientCertificate"}, {Path: "clientKey"}, {Path: "hmacKey"}},
		ConfigSchema: json.RawMessage(`{"type":"object","required":["endpoint"],"additionalProperties":false,"properties":{"endpoint":{"type":"string","maxLength":2048},"timeout":{"type":"string"},"domains":{"type":"array","maxItems":256,"items":{"type":"string","maxLength":253}},"policy":{"type":"string","enum":["sync","upsert-only","create-only"]}}}`),
	}
}

func init() { integration.Register(manifest(), New) }

// maxHMACKey is the largest event signing key, in bytes; 32 is the least.
const maxHMACKey = 4096

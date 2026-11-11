package webhook

import (
	"encoding/json"

	"github.com/robertherbaugh/kudurru/ee/integration"
)

// New constructs the mounted clients/actions/hooks adapter without I/O.
// Peers that advertise records refuse: remote record ownership is a separate
// prerequisite, not inferred from wire success. A peer that advertises hooks
// needs the separate hmacKey; root produces its events (D379).
func New() integration.Integration { return newProvider() }

func manifest() integration.Manifest {
	return integration.Manifest{Type: "webhook", DisplayName: "Webhook", Version: "1", IOClass: "local_polling",
		Capabilities: []integration.Capability{integration.CapActions, integration.CapClientSource, integration.CapHook},
		SecretKeys:   []integration.SecretKey{{Path: "bearerToken"}, {Path: "ca"}, {Path: "clientCertificate"}, {Path: "clientKey"}, {Path: "hmacKey"}},
		ConfigSchema: json.RawMessage(`{"type":"object","required":["endpoint"],"additionalProperties":false,"properties":{"endpoint":{"type":"string","maxLength":2048},"timeout":{"type":"string"}}}`),
	}
}

func init() { integration.Register(manifest(), New) }

// maxHMACKey is the largest event signing key, in bytes; 32 is the least.
const maxHMACKey = 4096

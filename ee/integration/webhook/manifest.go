package webhook

import (
	"encoding/json"

	"github.com/robertherbaugh/kudurru/ee/integration"
)

// New constructs the mounted clients/actions adapter without I/O. Peers that
// require records or hooks refuse: remote record ownership and durable root
// event production are separate prerequisites, not inferred from wire success.
func New() integration.Integration {
	p := newProvider()
	p.deliveryAllowed = false
	return p
}

func manifest(delivery bool) integration.Manifest {
	m := integration.Manifest{Type: "webhook", DisplayName: "Webhook", Version: "1", IOClass: "local_polling",
		Capabilities: []integration.Capability{integration.CapActions, integration.CapClientSource},
		SecretKeys:   []integration.SecretKey{{Path: "bearerToken"}, {Path: "ca"}, {Path: "clientCertificate"}, {Path: "clientKey"}},
		ConfigSchema: json.RawMessage(`{"type":"object","required":["endpoint"],"additionalProperties":false,"properties":{"endpoint":{"type":"string","maxLength":2048},"timeout":{"type":"string"}}}`),
	}
	if delivery {
		m.Capabilities = append(m.Capabilities, integration.CapHook)
		m.SecretKeys = append(m.SecretKeys, integration.SecretKey{Path: "hmacKey"})
	}
	return m
}

func init() { integration.Register(manifest(false), New) }

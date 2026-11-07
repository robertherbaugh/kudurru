package unifi

import (
	"encoding/json"

	"github.com/robertherbaugh/kudurru/ee/integration"
)

func manifest() integration.Manifest {
	return integration.Manifest{Type: "unifi", DisplayName: "UniFi Network", Version: "1", Capabilities: []integration.Capability{integration.CapClientSource}, IOClass: "local_polling", SecretKeys: []integration.SecretKey{{Path: "apiKey", Verify: "integration/v1/info"}}, Docs: "https://developer.ui.com/network/v10.6.106/", ConfigSchema: json.RawMessage(`{"type":"object","required":["endpoint"],"additionalProperties":false,"properties":{"endpoint":{"type":"string","maxLength":2048,"pattern":"^https://[^/]+.*$"},"tlsFingerprint":{"type":"string","pattern":"^sha256:[a-fA-F0-9]{64}$"},"site":{"type":"string","minLength":1,"maxLength":253},"poll":{"type":"string"},"legacy":{"type":"boolean"},"inventory":{"type":"object","required":["zoneRef"],"additionalProperties":false,"properties":{"zoneRef":{"type":"object","required":["name"],"additionalProperties":false,"properties":{"name":{"type":"string"},"namespace":{"type":"string"}}},"reverseZones":{"type":"string","enum":["auto","disabled"]},"nameTemplate":{"type":"string","maxLength":500},"ttl":{"type":"integer","minimum":1,"maximum":86400},"includeNetworks":{"type":"array","maxItems":256,"items":{"type":"string"},"uniqueItems":true}}},"groups":{"type":"object","additionalProperties":false,"properties":{"byNetwork":{"type":"object","maxProperties":256,"additionalProperties":{"type":"string"}},"byNote":{"type":"string","maxLength":128}}},"push":{"type":"object","additionalProperties":false,"properties":{"dhcpDNSServers":{"type":"boolean"},"mirrorRecords":{"type":"object","additionalProperties":false,"properties":{"enabled":{"type":"boolean"},"types":{"type":"array","items":{"type":"string","enum":["A","CNAME"]},"maxItems":2}}}}}}}`)}
}
func init() { integration.Register(manifest(), func() integration.Integration { return New() }) }

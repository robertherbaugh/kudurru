package cloudflare

import (
	"encoding/json"

	"github.com/robertherbaugh/kudurru/ee/integration"
)

func manifest() integration.Manifest {
	return integration.Manifest{Type: "cloudflare", DisplayName: "Cloudflare", Version: "1", Capabilities: []integration.Capability{integration.CapRecordSink}, IOClass: "cloud_polling", SecretKeys: []integration.SecretKey{{Path: "apiToken", Verify: "user/tokens/verify"}}, Docs: "https://developers.cloudflare.com/api/resources/dns/subresources/records/", ConfigSchema: json.RawMessage(`{"type":"object","required":["zones"],"additionalProperties":false,"properties":{"zones":{"type":"array","minItems":1,"maxItems":256,"items":{"type":"object","required":["zone"],"additionalProperties":false,"properties":{"zone":{"type":"string","maxLength":253},"zoneID":{"type":"string","pattern":"^[a-f0-9]{32}$"},"policy":{"type":"string","enum":["sync","upsert-only","create-only"]},"adoptUnowned":{"type":"boolean"},"adoptFrom":{"type":"object","required":["externalDNSOwner"],"additionalProperties":false,"properties":{"externalDNSOwner":{"type":"string","minLength":1,"maxLength":253}}},"defaults":{"type":"object","additionalProperties":false,"properties":{"proxied":{"type":"boolean"},"ttl":{"type":"integer","minimum":1,"maximum":86400},"comment":{"type":"string","maxLength":500}}},"select":{"type":"object","additionalProperties":false,"properties":{"labels":{"type":"object","additionalProperties":{"type":"string"},"maxProperties":64},"types":{"type":"array","maxItems":64,"items":{"type":"string"}},"exclude":{"type":"array","maxItems":256,"items":{"type":"string"}}}},"translate":{"type":"object","maxProperties":64,"additionalProperties":{"type":"string"}}}}}}}`)}
}
func init() { integration.Register(manifest(), func() integration.Integration { return New() }) }

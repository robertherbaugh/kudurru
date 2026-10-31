package cloudflare

import (
	"encoding/json"
	"math"
	"strings"

	sdk "github.com/cloudflare/cloudflare-go/v7/dns"
	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func (p *Provider) post(e public.Endpoint, target string, z resolvedZone) (sdk.RecordBatchParamsPost, error) {
	typ := sdk.RecordBatchParamsPostsType(e.RecordType)
	if !typ.IsKnown() {
		return sdk.RecordBatchParamsPost{}, ErrInvalid
	}
	props := map[string]string{}
	for _, option := range e.ProviderSpecific {
		props[option.Name] = option.Value
	}
	comment := stampComment(props["cloudflare/comment"], p.owner)
	if len(comment) > z.commentLimit {
		return sdk.RecordBatchParamsPost{}, ErrInvalid
	}
	// Set the service fields directly: the SDK root helpers also import every
	// service and retain their init registrations in the production binary.
	body := sdk.RecordBatchParamsPost{}
	body.Name.Value, body.Name.Present = strings.TrimSuffix(e.DNSName, "."), true
	body.Type.Value, body.Type.Present = typ, true
	body.TTL.Value, body.TTL.Present = sdk.TTL(e.RecordTTL), true
	body.Comment.Value, body.Comment.Present = comment, true
	body.Proxied.Value, body.Proxied.Present = props["cloudflare/proxied"] == "true", true
	body.Content.Value, body.Content.Present = target, true
	body.PrivateRouting.Value, body.PrivateRouting.Present = props["cloudflare/private-routing"] == "true", true
	settings := map[string]bool{}
	for _, key := range []string{"ipv4-only", "ipv6-only", "flatten-cname"} {
		if props["cloudflare/"+key] == "true" {
			settings[strings.ReplaceAll(key, "-", "_")] = true
		}
	}
	body.Settings.Value, body.Settings.Present = settings, true
	tags := []string{}
	if value := props["cloudflare/tags"]; value != "" {
		if json.Unmarshal([]byte(value), &tags) != nil {
			return sdk.RecordBatchParamsPost{}, ErrInvalid
		}
	}
	body.Tags.Value, body.Tags.Present = tags, true
	rr, err := dns.NewRR(e.DNSName + " 1 IN " + e.RecordType + " " + target)
	if err != nil {
		return sdk.RecordBatchParamsPost{}, ErrInvalid
	}
	var data map[string]any
	switch r := rr.(type) {
	case *dns.MX:
		body.Content.Value, body.Content.Present = strings.TrimSuffix(r.Mx, "."), true
		body.Priority.Value, body.Priority.Present = float64(r.Preference), true
	case *dns.LOC:
		coordinate := func(v uint32, positive, negative string) (float64, float64, float64, string) {
			n := int64(v) - (1 << 31)
			direction := positive
			if n < 0 {
				n = -n
				direction = negative
			}
			return float64(n / 3600000), float64(n % 3600000 / 60000), float64(n%60000) / 1000, direction
		}
		latD, latM, latS, latDir := coordinate(r.Latitude, "N", "S")
		longD, longM, longS, longDir := coordinate(r.Longitude, "E", "W")
		meters := func(v uint8) float64 { return float64(v>>4) * math.Pow10(int(v&15)) / 100 }
		data = map[string]any{"lat_degrees": latD, "lat_minutes": latM, "lat_seconds": latS, "lat_direction": latDir, "long_degrees": longD, "long_minutes": longM, "long_seconds": longS, "long_direction": longDir, "altitude": float64(int64(r.Altitude)-10000000) / 100, "size": meters(r.Size), "precision_horz": meters(r.HorizPre), "precision_vert": meters(r.VertPre)}
	case *dns.SRV:
		data = map[string]any{"priority": float64(r.Priority), "weight": float64(r.Weight), "port": float64(r.Port), "target": r.Target}
	case *dns.CAA:
		data = map[string]any{"flags": float64(r.Flag), "tag": r.Tag, "value": r.Value}
	case *dns.DS:
		data = map[string]any{"key_tag": float64(r.KeyTag), "algorithm": float64(r.Algorithm), "digest_type": float64(r.DigestType), "digest": r.Digest}
	case *dns.DNSKEY:
		data = map[string]any{"flags": float64(r.Flags), "protocol": float64(r.Protocol), "algorithm": float64(r.Algorithm), "public_key": r.PublicKey}
	case *dns.CERT:
		data = map[string]any{"type": float64(r.Type), "key_tag": float64(r.KeyTag), "algorithm": float64(r.Algorithm), "certificate": r.Certificate}
	case *dns.TLSA:
		data = map[string]any{"usage": float64(r.Usage), "selector": float64(r.Selector), "matching_type": float64(r.MatchingType), "certificate": r.Certificate}
	case *dns.SMIMEA:
		data = map[string]any{"usage": float64(r.Usage), "selector": float64(r.Selector), "matching_type": float64(r.MatchingType), "certificate": r.Certificate}
	case *dns.SSHFP:
		data = map[string]any{"algorithm": float64(r.Algorithm), "type": float64(r.Type), "fingerprint": r.FingerPrint}
	case *dns.NAPTR:
		data = map[string]any{"order": float64(r.Order), "preference": float64(r.Preference), "flags": r.Flags, "service": r.Service, "regex": r.Regexp, "replacement": r.Replacement}
	case *dns.URI:
		data = map[string]any{"priority": float64(r.Priority), "weight": float64(r.Weight), "target": r.Target}
	case *dns.SVCB:
		parts := strings.SplitN(target, " ", 3)
		value := ""
		if len(parts) > 2 {
			value = parts[2]
		}
		data = map[string]any{"priority": float64(r.Priority), "target": r.Target, "value": value}
	case *dns.HTTPS:
		parts := strings.SplitN(target, " ", 3)
		value := ""
		if len(parts) > 2 {
			value = parts[2]
		}
		data = map[string]any{"priority": float64(r.Priority), "target": r.Target, "value": value}
	}
	if data != nil {
		body.Content.Value, body.Content.Present = "", true
		body.Data.Value, body.Data.Present = data, true
	}
	return body, nil
}

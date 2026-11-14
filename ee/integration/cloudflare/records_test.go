package cloudflare

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	sdk "github.com/cloudflare/cloudflare-go/v7/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

func TestSDKCompletePagingAndDriftRejectWholeSnapshot(t *testing.T) {
	for _, mode := range []string{"complete", "drifting-total", "drifting-content", "duplicate-id", "missing-page", "bad-total", "bad-record", "out-of-zone", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			for i := 0; i < 501; i++ {
				f.add(fmt.Sprintf("name-%d.example.com", i), "A", "192.0.2.1", 300, "")
			}
			f.mu.Lock()
			lists := 0
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/dns_records") {
					return false
				}
				lists++
				switch mode {
				case "drifting-total":
					if lists == 2 {
						f.next++
						id := fmt.Sprintf("%032x", f.next)
						f.rows[id] = map[string]any{"id": id, "name": "new.example.com", "type": "A", "content": "192.0.2.1", "ttl": 300, "proxied": false}
					}
				case "drifting-content":
					if lists == 3 {
						for _, row := range f.rows {
							row["content"] = "192.0.2.2"
							break
						}
					}
				case "duplicate-id":
					for _, row := range f.rows {
						row["id"] = strings.Repeat("3", 32)
					}
				case "missing-page":
					if lists == 2 {
						writeJSON(w, map[string]any{"success": true, "result": []any{}, "result_info": map[string]any{"page": 2, "per_page": 500, "count": 0, "total_count": 501}})
						return true
					}
				case "bad-total":
					writeJSON(w, map[string]any{"success": true, "result": []any{}, "result_info": map[string]any{"page": 1, "per_page": 500, "count": 0, "total_count": maxRecords + 1}})
					return true
				case "bad-record":
					for _, row := range f.rows {
						delete(row, "ttl")
						break
					}
				case "out-of-zone":
					for _, row := range f.rows {
						row["name"] = "foreign.example.net"
						break
					}
				case "unknown":
					for _, row := range f.rows {
						row["type"] = "UNPARSEABLE"
						break
					}
				}
				return false
			}
			f.mu.Unlock()
			snapshot, err := f.p.Records(f.ctx, f.pub.Destination)
			if mode == "complete" {
				if err != nil || !snapshot.Complete || len(snapshot.Records) != 501 || lists != 4 {
					t.Fatal(snapshot.Complete, len(snapshot.Records), lists, err)
				}
			} else if err == nil || snapshot.Complete || len(snapshot.Records) != 0 {
				t.Fatal("partial observation escaped", snapshot, err)
			}
		})
	}
}
func TestRawWireRecordConversionsUseStructuredDataAndNormalize(t *testing.T) {
	f := newFixture(t)
	z, _ := f.p.destination(f.pub.Destination)
	cases := []struct{ typ, target string }{
		{"A", "192.0.2.1"}, {"AAAA", "2001:db8::1"}, {"CNAME", "target.example.com."}, {"NS", "ns.example.com."}, {"PTR", "target.example.com."},
		{"TXT", `"hello world" "second chunk"`}, {"MX", "10 mail.example.com."}, {"CAA", `0 issue "letsencrypt.org"`},
		{"SRV", "1 2 443 target.example.com."}, {"DS", "12345 13 2 AABBCCDD"}, {"DNSKEY", "257 3 13 AQID"}, {"CERT", "1 12345 13 AQID"},
		{"TLSA", "3 1 1 AABBCCDD"}, {"SMIMEA", "3 1 1 AABBCCDD"}, {"SSHFP", "1 2 AABBCCDD"}, {"NAPTR", `10 20 "U" "E2U+sip" "!^.*$!sip:info@example.com!" .`},
		{"URI", `1 2 "https://example.com/path"`}, {"SVCB", `1 target.example.com. alpn="h2,h3" port="443"`}, {"HTTPS", `1 target.example.com. alpn="h2"`},
		{"LOC", "37 47 0 N 122 23 0 W 10m"}, {"OPENPGPKEY", "AQID"},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			e := public.Endpoint{DNSName: "record.example.com.", RecordType: tc.typ, RecordTTL: 300, Targets: []string{tc.target}, ProviderSpecific: []public.ProviderSpecific{{Name: "cloudflare/proxied", Value: "false"}, {Name: "cloudflare/comment", Value: "a  preserved comment "}}}
			want, err := plan.Normalize(e)
			if err != nil {
				t.Fatal(err)
			}
			body, err := f.p.post(want, want.Targets[0], z)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := body.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			_ = json.Unmarshal(encoded, &raw)
			raw["id"] = strings.Repeat("a", 32)
			// Cloudflare may include a target-only content alongside complete data.
			if raw["data"] != nil {
				raw["content"] = "not-complete-rdata"
			}
			encoded, _ = json.Marshal(raw)
			var response sdk.RecordResponse
			if json.Unmarshal(encoded, &response) != nil {
				t.Fatal("SDK unmarshal")
			}
			got, err := f.p.convert(response, z)
			if err != nil || !slices.Equal(got.Endpoint.Targets, want.Targets) || !slices.Equal(got.Endpoint.ProviderSpecific, want.ProviderSpecific) {
				t.Fatalf("round trip %+v want %+v: %v", got.Endpoint, want, err)
			}
		})
	}
}
func TestSDKConversionRejectsMissingStructuredFieldsAndPreservesUnknownRR(t *testing.T) {
	f := newFixture(t)
	z, _ := f.p.destination(f.pub.Destination)
	for _, raw := range []string{
		`{"type":"CAA","data":{"flags":0,"tag":"issue"}}`,
		`{"type":"SRV","data":{"priority":0,"weight":0,"target":"host.example.com"}}`,
		`{"type":"SVCB","data":{"priority":1,"target":"host.example.com"}}`,
		`{"type":"TXT"}`, `{"type":"MX","content":"mail.example.com"}`,
	} {
		var fields map[string]any
		_ = json.Unmarshal([]byte(raw), &fields)
		fields["id"] = strings.Repeat("a", 32)
		fields["name"] = "record.example.com"
		fields["ttl"] = 300
		encoded, _ := json.Marshal(fields)
		var rr sdk.RecordResponse
		_ = json.Unmarshal(encoded, &rr)
		if _, err := f.p.convert(rr, z); err == nil {
			t.Fatal("malformed structured record accepted", raw)
		}
	}
	id := f.add("unknown.example.com", "TYPE65280", `\# 2 00AA`, 300, "")
	snapshot := f.snapshot()
	if len(snapshot.Records) != 1 || snapshot.Records[0].IDs[0] != id || snapshot.Records[0].Endpoint.RecordType != "TYPE65280" {
		t.Fatal("unknown wire RR lost", snapshot)
	}
	if _, err := f.p.post(snapshot.Records[0].Endpoint, snapshot.Records[0].Endpoint.Targets[0], z); err == nil {
		t.Fatal("unsupported RR mutated")
	}
}
func TestMarkersBindCurrentUIDAndPreserveHumanWhitespace(t *testing.T) {
	f := newFixture(t)
	old := f.h.owner
	old.UID = "old-uid"
	for _, tc := range []struct {
		comment string
		owned   bool
		human   string
	}{
		{marker(f.h.owner), true, ""}, {stampComment("a  b ", f.h.owner), true, "a  b "}, {marker(old), false, marker(old)},
		{marker(f.h.owner) + " " + marker(f.h.owner), false, marker(f.h.owner) + " " + marker(f.h.owner)},
		{"text kudurru:bad", false, "text kudurru:bad"}, {"human only", false, "human only"},
	} {
		owner, human := commentOwner(tc.comment, f.h.owner)
		if (owner.IntegrationUID == f.h.owner.UID) != tc.owned || human != tc.human {
			t.Fatal(owner, human, tc)
		}
	}
	if marker(old) == marker(f.h.owner) || len(marker(f.h.owner)) > 100 {
		t.Fatal("marker identity or bounds")
	}
}
func TestSDKUnrecognizedHeritageFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.add("custom-www.example.com", "TXT", `"heritage=external-dns,external-dns/owner=old,external-dns/resource=service/default/web"`, 300, "")
	snapshot, err := f.p.Records(f.ctx, f.pub.Destination)
	if !errors.Is(err, ErrInvalid) || snapshot.Complete {
		t.Fatal("unmatched heritage accepted", snapshot, err)
	}
}

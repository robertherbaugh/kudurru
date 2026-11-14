package auditdocument

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
)

func TestPublicRedactionPreservesEvidence(t *testing.T) {
	input := json.RawMessage(`{"metadata":{"name":"record","labels":{"app":"dns"},"annotations":{"custom":"fixture-secret"},"managedFields":[{"arbitrary":"fixture-secret"}]},"spec":{"zoneRef":{"name":"home"},"endpoints":[{"dnsName":"www.example.","recordType":"TXT","targets":["safe record content"]}],"generation":18446744073709551615,"credential":{"secretRef":{"name":"cloud","namespace":"dns","key":"token"}},"nested":[{"Password":"fixture-secret","private_key":"fixture-secret","Authorization":"fixture-secret","apiKey":"fixture-secret","access-token":"fixture-secret","cookie":"fixture-secret","dsn":"fixture-secret"}]}}`)
	before := bytes.Clone(input)
	safe := Redact("DNSRecordSet", input)
	if !bytes.Equal(input, before) {
		t.Fatal("redactor mutated caller document")
	}
	if bytes.Contains(safe, []byte("fixture-secret")) || bytes.Contains(safe, []byte("first")) {
		t.Fatal("public credential field survived")
	}
	for _, piece := range []string{"record", "app", "dns", "zoneRef", "home", "www.example.", "TXT", "safe record content", "18446744073709551615", "secretRef", "cloud", "namespace", "key"} {
		if !bytes.Contains(safe, []byte(piece)) {
			t.Fatal("safe change evidence removed")
		}
	}
	if !bytes.Equal(safe, Redact("DNSRecordSet", safe)) {
		t.Fatal("public redaction not idempotent")
	}
}

func TestPrivateManifestAndTokenAllowlist(t *testing.T) {
	for _, kind := range runtimekinds.Private() {
		t.Run(kind, func(t *testing.T) {
			input := json.RawMessage(`{"metadata":{"name":"id","annotations":{"opaque":"fixture-secret"}},"spec":{"owner":{"kind":"User","name":"alice"},"description":"safe label","scopes":["viewer"],"secretHash":"fixture-secret","data":"fixture-secret"},"status":{"opaque":"fixture-secret"}}`)
			safe := Redact(kind, input)
			if kind != "APIToken" {
				if safe != nil {
					t.Fatal("private document retained")
				}
				return
			}
			if bytes.Contains(safe, []byte("fixture-secret")) || bytes.Contains(safe, []byte("metadata")) || bytes.Contains(safe, []byte("status")) {
				t.Fatal("token allowlist failed")
			}
			if !bytes.Contains(safe, []byte("safe label")) || !bytes.Contains(safe, []byte("viewer")) || !bytes.Contains(safe, []byte("alice")) {
				t.Fatal("token evidence lost")
			}
		})
	}
}

func TestUntrustedSummariesAndMalformedDocuments(t *testing.T) {
	for _, input := range []string{`{"password":"fixture-secret"`, `"fixture-secret"`, `{"safe":1} "fixture-secret"`, `{"password":"first","pass\u0077ord":"fixture-secret"}`, strings.Repeat(`{"nested":`, 130) + `{}` + strings.Repeat(`}`, 130), `{"spec":{"password":"fixture-secret"},"spec":{"safe":"value"}}`, strings.Repeat("!", 8192)} {
		if Redact("runtime", json.RawMessage(input)) != nil {
			t.Fatal("malformed or unstructured input retained")
		}
	}
	for _, kind := range []string{"runtime", "APIToken"} {
		for _, marker := range []string{"true", "false", `"true"`} {
			safe := Redact(kind, json.RawMessage(`{"truncated":`+marker+`,"prefix":"fixture-secret","sha256":"fixture-secret","bytes":8000}`))
			if bytes.Contains(safe, []byte("fixture-secret")) {
				t.Fatal("untrusted summary retained a credential prefix or digest")
			}
		}
	}
}

func TestPrivateKindCaseDoesNotExposeCarrier(t *testing.T) {
	for _, kind := range runtimekinds.Private() {
		safe := Redact(strings.ToLower(kind), json.RawMessage(`{"opaque":"fixture-private"}`))
		if kind != "APIToken" && safe != nil {
			t.Fatal("private-kind spelling bypassed omission")
		}
		if bytes.Contains(safe, []byte("fixture-private")) {
			t.Fatal("private-kind spelling exposed data")
		}
	}
}

func TestAbsentDocumentsRemainAbsent(t *testing.T) {
	for _, kind := range append(runtimekinds.Private(), "DNSRecordSet") {
		for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(" \n null \t")} {
			if Redact(kind, raw) != nil {
				t.Fatal("absent document invented state")
			}
		}
	}
}

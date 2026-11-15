package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestTSIGKeyDecoderBoundedStrictAndRedacted(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x63}, 32))
	create := `{"keyName":"STORED.EXAMPLE","algorithm":"hmac-sha256","secret":"` + secret + `"}`
	decoded, err := decodeTSIGKeyChange(TSIGKeyCreate, "", []byte(create))
	if err != nil || decoded.KeyName != "stored.example." || decoded.Algorithm != "hmac-sha256." || len(decoded.Secret) != 32 {
		t.Fatal("valid explicit create refused")
	}
	for _, body := range []string{
		"", "null", "[]", `{}`, create + ` {}`, create + ` null`,
		strings.Replace(create, `"secret":`, `"secret":null,"duplicate":`, 1),
		strings.Replace(create, `"secret":`, `"secret":"duplicate","secret":`, 1),
		strings.Replace(create, `"keyName":`, `"unexpected":`, 1),
		strings.Replace(create, secret, "not-base64-credential", 1),
		strings.Replace(create, secret, secret+`\n`, 1),
		strings.Replace(create, secret, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 513)), 1),
		strings.Replace(create, `"hmac-sha256"`, `null`, 1),
		strings.Replace(create, `"hmac-sha256"`, `""`, 1),
		strings.Replace(create, `"STORED.EXAMPLE"`, `"bad name"`, 1),
		strings.Repeat(" ", maxTSIGKeyBody) + create,
	} {
		if _, err := decodeTSIGKeyChange(TSIGKeyCreate, "", []byte(body)); err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "not-base64-credential") {
			t.Fatal("invalid body accepted or echoed")
		}
	}
	binding := `{"uid":"original-owner","resourceVersion":"17"}`
	for _, op := range []TSIGKeyOperation{TSIGKeyRevoke, TSIGKeyReseal} {
		if _, err := decodeTSIGKeyChange(op, "stored.example.", []byte(binding)); err != nil {
			t.Fatal("reviewed binding refused")
		}
		for _, body := range []string{`{"uid":"original-owner"}`, `{"uid":null,"resourceVersion":"17"}`, strings.TrimSuffix(binding, "}") + `,"secret":""}`, strings.TrimSuffix(binding, "}") + `,"algorithm":""}`} {
			if _, err := decodeTSIGKeyChange(op, "stored.example.", []byte(body)); err == nil {
				t.Fatal("inapplicable or absent binding accepted")
			}
		}
	}
	replacement := strings.TrimSuffix(binding, "}") + `,"secret":"` + secret + `"}`
	if _, err := decodeTSIGKeyChange(TSIGKeyReplace, "stored.example.", []byte(replacement)); err != nil {
		t.Fatal("explicit replace refused")
	}
	if _, err := decodeTSIGKeyChange(TSIGKeyOperation("apply"), "stored.example.", []byte(binding)); err == nil {
		t.Fatal("generic mutation intent accepted")
	}
	for _, private := range []any{decoded, CreateStoredTSIGKeyBody{Secret: secret}, ReplaceStoredTSIGKeyBody{Secret: secret}} {
		wire, err := json.Marshal(private)
		if err != nil || strings.Contains(string(wire), secret) || strings.Contains(fmt.Sprintf("%+v %#v", private, private), secret) {
			t.Fatal("write-only input diagnostic leaked")
		}
	}
}

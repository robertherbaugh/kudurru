package servingauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestSecurityDeterministicExplicitAndStrict(t *testing.T) {
	s := EmptySecurity(7)
	s.GrantFloors = []GrantFloor{{FormatVersion: 1, Grant: "viewer", ValidAfter: time.Unix(100, 0)}, {FormatVersion: 1, Grant: "admin", ValidAfter: time.Unix(200, 0)}}
	rows, err := EncodeSecurity(s)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(s.GrantFloors)
	again, err := EncodeSecurity(s)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(rows)
	b, _ := json.Marshal(again)
	if !bytes.Equal(a, b) {
		t.Fatal("encoding depends on map/order")
	}
	decoded, err := DecodeSecurity(rows)
	if err != nil || decoded.Epoch != 7 || len(decoded.GrantFloors) != 2 {
		t.Fatal(decoded.Epoch, err)
	}
	for _, what := range []string{"absent", "null-set", "version", "duplicate-key", "case-alias", "duplicate-row", "unlisted", "unknown-row", "counter", "missing-row"} {
		t.Run(what, func(t *testing.T) {
			rows, _ := EncodeSecurity(EmptySecurity(1))
			switch what {
			case "absent":
				rows = nil
			case "null-set":
				rows[0].Data = []byte(`{"formatVersion":2,"epoch":"1","grantFloors":null,"accountFloors":[],"providerFloors":[],"sessions":[],"totp":[]}`)
			case "version":
				rows[0].Data = []byte(`{"formatVersion":99,"epoch":"1","grantFloors":[],"accountFloors":[],"providerFloors":[],"sessions":[],"totp":[]}`)
			case "duplicate-key":
				rows[0].Data = []byte(`{"formatVersion":2,"formatVersion":1}`)
			case "case-alias":
				rows[0].Data = []byte(`{"formatVersion":2,"FormatVersion":1,"epoch":"1","grantFloors":[],"accountFloors":[],"providerFloors":[],"sessions":[],"totp":[]}`)
			case "duplicate-row":
				rows = append(rows, rows[0])
			case "unlisted":
				rows = append(rows, replication.Entry{Key: "auth/v1/other", Data: json.RawMessage(`{}`)})
			case "unknown-row":
				rows = append(rows, replication.Entry{Key: "auth/v2/other", Data: json.RawMessage(`{}`)})
			case "counter":
				rows[0].Data = []byte(`{"formatVersion":2,"epoch":"01","grantFloors":[],"accountFloors":[],"providerFloors":[],"sessions":[],"totp":[]}`)
			case "missing-row":
				rows[0].Data = []byte(`{"formatVersion":2,"epoch":"1","grantFloors":["auth/v1/grant/missing"],"accountFloors":[],"providerFloors":[],"sessions":[],"totp":[]}`)
			}
			if _, err := DecodeSecurity(rows); err == nil {
				t.Fatal("malformed state accepted")
			}
		})
	}
}
func TestSecurityCapsBeforeTypedDecoding(t *testing.T) {
	for _, rows := range [][]replication.Entry{
		make([]replication.Entry, MaxRows+1),
		{{Key: RootKey, Data: json.RawMessage(strings.Repeat(" ", MaxRootBytes+1))}},
		{{Key: "auth/v1/session/abcdefghijklmnop", Data: json.RawMessage(strings.Repeat(" ", MaxRowBytes+1))}},
	} {
		if _, err := DecodeSecurity(rows); !errors.Is(err, ErrBounds) {
			t.Fatal(err)
		}
	}
	if boundedJSON([]byte(strings.Repeat("[", 33)+"0"+strings.Repeat("]", 33)), MaxRowBytes) {
		t.Fatal("depth cap bypassed")
	}
	if boundedJSON([]byte(`{} {}`), MaxRowBytes) {
		t.Fatal("trailing JSON accepted")
	}
	s := EmptySecurity(1)
	s.GrantFloors = make([]GrantFloor, MaxRows)
	if _, err := EncodeSecurity(s); !errors.Is(err, ErrBounds) {
		t.Fatal(err)
	}
}

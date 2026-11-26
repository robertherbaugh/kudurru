package servingauth

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// referenceBoundedJSON is boundedJSON as it was before it shared its walk
// with the owner-name check.
func referenceBoundedJSON(raw []byte, capBytes int) bool {
	if len(raw) == 0 || len(raw) > capBytes || !utf8.Valid(raw) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var walk func(int) bool
	walk = func(depth int) bool {
		nodes++
		if depth > 32 || nodes > 50000 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				tok, e := d.Token()
				key, ok := tok.(string)
				if e != nil || !ok || seen[key] {
					return false
				}
				seen[key] = true
				if !walk(depth + 1) {
					return false
				}
			}
		case '[':
			count := 0
			for d.More() {
				count++
				if count > MaxRows || !walk(depth+1) {
					return false
				}
			}
		default:
			return false
		}
		_, err = d.Token()
		return err == nil
	}
	if !walk(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

// boundedOwnerJSON checks the bound and the exact owner names in one walk. It
// must accept exactly what boundedJSON followed by ownerFields accepted.
func TestBoundedOwnerJSONMatchesBoundedJSONAndOwnerFields(t *testing.T) {
	at := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	account := AccountRef{Kind: "LocalUser", Name: "alice", UID: "uid-1"}
	provider := ProviderRef{ID: "corp", UID: "uid-2", Origin: "object"}
	owners := map[reflect.Type]any{
		reflect.TypeFor[GrantFloor]():            GrantFloor{FormatVersion: 1, Grant: "viewer@zone:home", ValidAfter: at},
		reflect.TypeFor[AccountFloor]():          AccountFloor{FormatVersion: 1, Account: account, ValidAfter: at},
		reflect.TypeFor[ProviderFloor]():         ProviderFloor{FormatVersion: 1, Provider: "corp", UID: "uid-2", ValidAfter: at},
		reflect.TypeFor[SessionRecord]():         SessionRecord{FormatVersion: 1, ID: strings.Repeat("s", 20), Account: account, Grants: []string{"viewer@zone:home"}, IssuedAt: at},
		reflect.TypeFor[ExternalSessionRecord](): ExternalSessionRecord{Account: account, Provider: provider},
		reflect.TypeFor[RootEntry]():             RootEntry{FormatVersion: RootFormatVersion, Epoch: 1, GrantFloors: []string{"a", "b"}},
	}
	// Each document is tried against every owner type.
	docs := []string{
		``, `null`, `true`, `1`, `"x"`, `[]`, `[{}]`, `{}`, `{} {}`, `{"a":1}{"b":2}`,
		`{"formatVersion":1,"formatVersion":1}`, `{"FormatVersion":1}`, `{"formatversion":1}`,
		`{"formatVersion":1,"extra":true}`, `{"":1}`, `{"-":1}`,
		`{"account":null}`, `{"account":[]}`, `{"account":"x"}`, `{"account":{}}`,
		`{"account":{"kind":"LocalUser","Kind":"x"}}`, `{"account":{"kind":"LocalUser","extra":1}}`,
		`{"account":{"kind":"LocalUser","name":"a","name":"b"}}`, `{"account":{"kind":{"nested":[1,2]}}}`,
		`{"provider":null}`, `{"provider":{"id":"corp","origin":"object"}}`, `{"provider":{"ID":"corp"}}`,
		`{"provider":"corp"}`, `{"grant":"g","grant":"h"}`, `{"grant":"g"}`, `{"grantFloors":["a","b"]}`,
		strings.Repeat("[", 33) + strings.Repeat("]", 33), `{"formatVersion":1` + strings.Repeat(` `, 10) + `}`,
		"{\"grant\":\"\xff\"}",
	}
	for _, owner := range owners {
		raw, err := json.Marshal(owner)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, string(raw))
		// Every member name in another case, and every member dropped or doubled.
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil {
			t.Fatal(err)
		}
		for name, value := range members {
			for _, renamed := range []string{strings.ToUpper(name), strings.ToLower(name), name + "x"} {
				docs = append(docs, strings.Replace(string(raw), `"`+name+`":`, `"`+renamed+`":`, 1))
			}
			docs = append(docs, strings.Replace(string(raw), `"`+name+`":`+string(value), `"`+name+`":null`, 1))
			docs = append(docs, strings.Replace(string(raw), `{`, `{"`+name+`":`+string(value)+`,`, 1))
		}
	}
	for typ := range owners {
		for _, doc := range docs {
			raw := []byte(doc)
			want := referenceBoundedJSON(raw, MaxRootBytes) && ownerFields(raw, typ)
			if got := boundedOwnerJSON(raw, MaxRootBytes, typ); got != want {
				t.Errorf("%s %q: boundedOwnerJSON %t, boundedJSON and ownerFields %t", typ.Name(), doc, got, want)
			}
		}
	}
	for _, doc := range docs {
		raw := []byte(doc)
		if got, want := boundedJSON(raw, MaxRootBytes), referenceBoundedJSON(raw, MaxRootBytes); got != want {
			t.Errorf("boundedJSON %q: %t, want %t", doc, got, want)
		}
	}
}

package zonestate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
)

func TestPreflightRejectsBeforeMalformedRecordOrCopies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Aggregate)
	}{
		{"aliased grant amplification", func(a *Aggregate) {
			types := make([]uint16, 65535)
			a.Policy.Grants = make([]dynamicupdate.Grant, 4096)
			for i := range a.Policy.Grants {
				a.Policy.Grants[i] = dynamicupdate.Grant{Key: "key.", Owner: "*.example.", Types: types}
			}
		}},
		{"reservation source", func(a *Aggregate) {
			a.Reservations = []dynamicupdate.Reservation{{Name: "a.example.", Source: strings.Repeat("s", 513)}}
		}},
		{"receipt version", func(a *Aggregate) {
			a.Receipts = []Receipt{{Principal: dynamicupdate.Principal{Version: strings.Repeat("v", 513)}}}
		}},
		{"receipt key", func(a *Aggregate) {
			a.Receipts = []Receipt{{Principal: dynamicupdate.Principal{Key: strings.Repeat("k", 1021)}}}
		}},
		{"provenance", func(a *Aggregate) { a.Records[0].Provenance.CredentialVersion = strings.Repeat("v", 513) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := fixture(t)
			a.Records[0].Wire = []byte{0}
			tc.change(a)
			if _, err := Prepare(a, nil, Limits{}); !errors.Is(err, ErrCapacity) {
				t.Fatalf("preflight must precede record decode: %v", err)
			}
		})
	}
	a := fixture(t)
	a.Reservations = make([]dynamicupdate.Reservation, 100)
	for i := range a.Reservations {
		a.Reservations[i] = dynamicupdate.Reservation{Name: "a.example.", Source: "system"}
	}
	if err := aggregatePreflight(a, Limits{MaxAggregateBytes: 10000}.Defaults()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("cumulative reservation workspace: %v", err)
	}
}

func TestStoredJSONTypesPreflightBeforeTypedDecode(t *testing.T) {
	var a Aggregate
	data := []byte(`{"policy":{"grants":[{"types":[` + strings.Repeat("1,", 65535) + `1]}]},"unknown":true}`)
	if err := strictDecodeAggregate(data, &a, Limits{}.Defaults()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("count preflight: %v", err)
	}
	if len(a.Policy.Grants) != 0 {
		t.Fatal("typed slices allocated before preflight")
	}
	data = []byte(`{"policy":{"grants":[{"types":[` + strings.Repeat("1,", 40000) + `1]},{"types":[` + strings.Repeat("1,", 40000) + `1]}]}}`)
	if err := strictDecodeAggregate(data, &a, Limits{}.Defaults()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("total types: %v", err)
	}
	data, _ = json.Marshal(fixture(t))
	if err := strictDecodeAggregate(data, &a, Limits{}.Defaults()); err != nil {
		t.Fatal(err)
	}
}

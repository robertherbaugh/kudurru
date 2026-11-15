package servingauth

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCheckpointManifestRejectsMissingNullAndInvalidTimes(t *testing.T) {
	for _, value := range []string{"missing", "null", `"1969-12-31T23:59:59Z"`, `123`, `"garbage"`} {
		t.Run(value, func(t *testing.T) {
			rows, err := EncodeSecurity(EmptySecurity(1))
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]json.RawMessage
			_ = json.Unmarshal(rows[0].Data, &root)
			if value == "missing" {
				delete(root, "revocationCheckpoint")
			} else {
				root["revocationCheckpoint"] = json.RawMessage(value)
			}
			rows[0].Data, _ = json.Marshal(root)
			if _, err := DecodeSecurity(rows); err == nil {
				t.Fatal("accepted missing/invalid checkpoint in complete image")
			}
			if err := ValidateEntry("security", rows[0]); err == nil {
				t.Fatal("accepted missing/invalid checkpoint row")
			}
		})
	}
	state := EmptySecurity(5)
	state.RevocationCheckpoint = time.Date(2026, 11, 15, 1, 2, 3, 4, time.FixedZone("offset", 3600))
	rows, err := EncodeSecurity(state)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSecurity(rows)
	if err != nil || !got.RevocationCheckpoint.Equal(state.RevocationCheckpoint) {
		t.Fatal("checkpoint lost in complete roundtrip", err)
	}
	state.RevocationCheckpoint = time.Unix(-1, 0)
	if _, err := EncodeSecurity(state); !errors.Is(err, ErrInvalid) {
		t.Fatal("encoded invalid checkpoint", err)
	}
}

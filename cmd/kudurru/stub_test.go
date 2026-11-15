package main

import (
	"encoding/json"
	"testing"
)

func TestZonesTableStubUsesNodeRuntime(t *testing.T) {
	for _, tc := range []struct{ runtime, serial, state, node string }{
		{`null`, "-", "Unknown", "-"},
		{`{"node":"dns-b","state":"routing","serial":0}`, "0", "routing", "dns-b"},
		{`{"node":"dns-b","state":"routing","serial":4294967295}`, "4294967295", "routing", "dns-b"},
		{`{"node":"dns-b","state":"expired","serial":42}`, "42 (last)", "expired", "dns-b"},
	} {
		t.Run(tc.state+tc.serial, func(t *testing.T) {
			var z zoneSummary
			body := `{"name":"stub","zoneName":"stub.","type":"stub","serial":1234,"recordCount":99,"ready":true,"stubRuntime":` + tc.runtime + `}`
			if err := json.Unmarshal([]byte(body), &z); err != nil {
				t.Fatal(err)
			}
			row := zoneTable([]zoneSummary{z}).Rows[0]
			if row[3] != tc.serial || row[4] != "-" || row[5] != tc.state || row[7] != tc.node {
				t.Fatalf("local route lost: %v", row)
			}
		})
	}
}

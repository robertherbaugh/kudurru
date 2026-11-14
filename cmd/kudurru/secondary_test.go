package main

import (
	"encoding/json"
	"testing"
)

func TestZonesTableSecondaryUsesNodeRuntime(t *testing.T) {
	for _, tc := range []struct{ name, runtime, serial, state, node string }{
		{"no local state", "null", "-", "Unknown", "-"},
		{"serial wrap", `{"node":"dns-b","state":"serving","serial":4294967295,"recordCount":4}`, "4294967295", "serving", "dns-b"},
		{"serial zero", `{"node":"dns-b","state":"serving","serial":0,"recordCount":4}`, "0", "serving", "dns-b"},
		{"expired", `{"node":"dns-b","state":"expired","serial":7,"recordCount":4}`, "7 (last)", "expired", "dns-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var z zoneSummary
			body := `{"name":"example","zoneName":"example.","type":"secondary","serial":1234,"recordCount":99,"ready":true,"secondaryRuntime":` + tc.runtime + `}`
			if err := json.Unmarshal([]byte(body), &z); err != nil {
				t.Fatal(err)
			}
			table := zoneTable([]zoneSummary{z})
			row := table.Rows[0]
			if row[3] != tc.serial || row[5] != tc.state || row[7] != tc.node {
				t.Fatalf("local state lost: %v", row)
			}
		})
	}
}

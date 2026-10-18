package main

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/robertherbaugh/kudurru/internal/cluster"
)

func TestSupervisorEndpointsIncludeUnreadyAndDeduplicateDualStack(t *testing.T) {
	src := &cluster.StaticSource{Endpoints: []cluster.Endpoint{
		{Name: "edge-a", Address: "10.0.0.1:8084", InternalAddress: "10.0.0.1:8085", Ready: false},
		{Name: "edge-a", Address: "[fd00::1]:8084", InternalAddress: "[fd00::1]:8085"},
		{Name: "edge-b", Address: "[fd00::2]:8084", InternalAddress: "[fd00::2]:8085"},
	}}
	got, err := supervisorEndpoints(src, true)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].EdgeURL != "http://10.0.0.1:8084" || got[1].VIPURL != "http://[fd00::2]:8085" {
		t.Fatalf("endpoints = %+v", got)
	}
}

func TestEdgeStatusLocalURLs(t *testing.T) {
	t.Setenv("KUDURRU_VIP_SUPERVISED", "")
	t.Setenv("KUDURRU_EDGE_SUPERVISORS", "")
	t.Setenv("KUDURRU_EDGE_STATUS_URL", "http://127.0.0.1:8084")
	t.Setenv("KUDURRU_VIP_STATUS_URL", "http://127.0.0.1:8085")
	src, vip, err := edgeStatusSource(nil, "node-a")
	if err != nil || !vip {
		t.Fatalf("vip=%v err=%v", vip, err)
	}
	got, err := src(context.Background())
	if err != nil || len(got) != 1 || got[0].Node != "node-a" {
		t.Fatalf("endpoints=%+v err=%v", got, err)
	}
}

func TestEdgeStatusVIPSupervisedValidation(t *testing.T) {
	for _, tc := range []struct {
		value   string
		wantVIP bool
		wantErr bool
	}{
		{"", false, false},
		{"false", false, false},
		{"true", true, false},
		{"invalid", false, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("KUDURRU_EDGE_SUPERVISORS", "endpointslice:default/edge-status")
			t.Setenv("KUDURRU_EDGE_STATUS_URL", "")
			t.Setenv("KUDURRU_VIP_STATUS_URL", "")
			t.Setenv("KUDURRU_VIP_SUPERVISED", tc.value)
			src, vip, err := edgeStatusSource(&rest.Config{Host: "http://127.0.0.1"}, "node-a")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "KUDURRU_VIP_SUPERVISED") || src != nil {
					t.Fatalf("invalid setting: src=%v err=%v", src == nil, err)
				}
				return
			}
			if err != nil || vip != tc.wantVIP || src == nil {
				t.Fatalf("vip=%v err=%v, want vip=%v", vip, err, tc.wantVIP)
			}
		})
	}
}

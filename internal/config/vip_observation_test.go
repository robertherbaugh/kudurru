package config

import (
	"strings"
	"testing"
)

func TestVIPObservationHostConfigAndNoSupervisorOverrides(t *testing.T) {
	path := supervisorFile(t, `vip:
  observation:
    provider: metallb
    services: [{name: dns, address: 192.0.2.53, namespace: apps, service: dns}]
`)
	cfg, err := Load(path, []string{"KUDURRU_VIP__OBSERVATION__MAXAGE=20s"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VIP.Observation.Provider != "metallb" || len(cfg.VIP.Observation.Services) != 1 || cfg.VIP.Observation.MaxAge.String() != "20s" {
		t.Fatal("typed observation file/env lost")
	}
	for _, key := range Schema {
		if strings.HasPrefix(key.Path, "vip.observation.") {
			if !key.HostOnly || key.Scope != ScopeNode || !key.RestartRequired {
				t.Errorf("observation key not host-only: %s", key.Path)
			}
			if _, err := LoadSupervisors("", nil, map[string]any{key.Path: key.Default}); err == nil {
				t.Errorf("serve-only observation key admitted as supervisor flag: %s", key.Path)
			}
		}
	}
	for name, body := range map[string]string{
		"missing Service":           "vip: {observation: {provider: kube-vip}}",
		"invalid provider":          "vip: {observation: {provider: bgp}}",
		"bad acquisition bounds":    "vip: {observation: {provider: metallb, services: [{name: dns, address: 192.0.2.53, namespace: apps, service: dns}], timeout: 6s}}",
		"invalid Service namespace": "vip: {observation: {services: [{name: dns, address: 192.0.2.53, namespace: '../other', service: dns}]}}",
		"duplicate VIP":             "vip: {observation: {services: [{name: dns, address: 192.0.2.53, namespace: apps, service: dns}, {name: dns, address: 192.0.2.54, namespace: apps, service: other}]}}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(supervisorFile(t, body), nil); err == nil {
				t.Fatal("invalid observation config accepted")
			}
		})
	}
	if _, err := Load("", []string{`KUDURRU_VIP__OBSERVATION__SERVICES=[]`}); err == nil {
		t.Fatal("structured environment refusal broadened")
	}
}

package config

import "testing"

func TestVIPNotificationObservationHostSettings(t *testing.T) {
	file := supervisorFile(t, `vip:
  vips: [{name: dns, address: 192.0.2.53/24, interface: eth0, vrid: 53}]
  observation:
    provider: keepalived
    runDir: /run/notification-source
`)
	cfg, err := Load(file, nil)
	if err != nil || cfg.VIP.Observation.Provider != "keepalived" || cfg.VIP.Observation.RunDir != "/run/notification-source" || len(cfg.VIP.VIPs) != 1 {
		t.Fatal("typed keepalived notification config failed")
	}
	if _, err := Load("", []string{"KUDURRU_VIP__OBSERVATION__PROVIDER=keepalived"}); err == nil {
		t.Fatal("keepalived observation accepted without immutable mappings")
	}
	if _, err := Load(file, []string{"KUDURRU_VIP__OBSERVATION__RUNDIR=relative"}); err == nil {
		t.Fatal("relative journal source accepted")
	}
	if _, err := LoadSupervisors("", nil, map[string]any{"vip.observation.runDir": "/run/other"}); err == nil {
		t.Fatal("observation directory admitted as supervisor override")
	}
}

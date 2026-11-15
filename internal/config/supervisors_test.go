package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func supervisorFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSupervisorLayers(t *testing.T) {
	path := supervisorFile(t, `edge:
  backends: [file=127.0.0.1:5353]
  dynblocks: true
  internalPort: 8082
  debounce: 3s
vip:
  vips: [{name: dns, address: 192.0.2.53/24, interface: eth0, vrid: 53}]
  preemptDelay: {dns: 20}
  priorityIndex: 0
`)
	cfg, err := LoadSupervisors(path, []string{
		"KUDURRU_EDGE__BACKENDS=env=127.0.0.2:5353", "KUDURRU_EDGE__DEBOUNCE=4s",
		"KUDURRU_VIP__PREEMPTDELAY={\"dns\":30}",
	}, map[string]any{"edge.dynblocks": false, "edge.internalPort": 0, "vip.preemptDelay": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Edge.Backends, []string{"env=127.0.0.2:5353"}) || cfg.Edge.Dynblocks || cfg.Edge.InternalPort != 0 || cfg.Edge.Debounce.Std() != 4*time.Second {
		t.Fatal("supervisor file/environment/final precedence failed")
	}
	if len(cfg.VIP.VIPs) != 1 || len(cfg.VIP.PreemptDelay) != 0 || !cfg.SupervisorConfigured("vip.priorityIndex") {
		t.Fatal("VIP records, replacement map or priority provenance lost")
	}
	if slices.Contains(cfg.EnvLocked, "vip.preemptDelay") {
		t.Fatal("overridden environment value remains locked")
	}
	defaults, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Edge.BackendPort != 5353 || defaults.Edge.InternalPort != 8082 || !defaults.Edge.PollZones || defaults.Edge.StopTimeout.Std() != 10*time.Second || defaults.VIP.HMACKeyFile != "/run/secrets/vrrp_hmac_{vrid}" || defaults.VIP.StopTimeout.Std() != 15*time.Second || defaults.SupervisorConfigured("vip.priorityIndex") {
		t.Fatal("supervisor defaults changed")
	}
}

func TestSupervisorOverridesAreNarrow(t *testing.T) {
	for _, name := range []string{"KUDURRU_EDGE__THREADS=invalid", "KUDURRU_EDGE__THREADS_FILE=/nonexistent/f40a"} {
		cfg, err := LoadSupervisors("", []string{name}, map[string]any{"edge.threads": 3})
		if err != nil || cfg.Edge.Threads != 3 {
			t.Fatal("explicit supervisor override did not skip its lower environment value")
		}
		if _, err := Load("", []string{name}); err == nil {
			t.Fatal("ordinary load skipped invalid environment")
		}
		if _, err := LoadSupervisors("", []string{name}, map[string]any{"edge.dynblocks": true}); err == nil {
			t.Fatal("unrelated explicit flag skipped invalid environment")
		}
	}
	path := supervisorFile(t, "edge: {threads: invalid, renderer: invalid}\n")
	if _, err := LoadSupervisors(path, nil, map[string]any{"edge.threads": 2, "edge.renderer": "lua"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"edge.unknown", "node.name", "listeners.api.address", "vip", "upstreams.groups"} {
		if _, err := LoadSupervisors("", nil, map[string]any{key: "ignored"}); err == nil {
			t.Errorf("non-supervisor override accepted: %s", key)
		}
	}
	for _, env := range []string{"KUDURRU_EDGE__UNKNOWN=1", "KUDURRU_LOG__FORMAT=bad", "KUDURRU_UPSTREAMS__GROUPS={}", "KUDURRU_VIP__PRIORITYINDEX=bad"} {
		if _, err := LoadSupervisors("", []string{env}, map[string]any{"edge.threads": 3}); err == nil {
			t.Errorf("unrelated bad environment accepted: %s", strings.Split(env, "=")[0])
		}
	}
	if _, err := LoadSupervisors("", []string{"KUDURRU_EDGE__THREADS=invalid"}, map[string]any{"edge.threads": -1, "edge.renderer": "invalid"}); err == nil {
		t.Fatal("invalid final override accepted")
	}
}

func TestSupervisorStructuredEnvironment(t *testing.T) {
	cfg, err := Load("", []string{
		`KUDURRU_VIP__VIPS=[{"name":"dns","address":"192.0.2.53/24","interface":"eth0","vrid":53}]`,
		`KUDURRU_VIP__PREEMPTDELAY={"dns":30}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.VIP.VIPs) != 1 || cfg.VIP.VIPs[0].VRID != 53 || cfg.VIP.PreemptDelay["dns"] != 30 {
		t.Fatal("typed VIP JSON environment lost")
	}
	for _, env := range []string{
		`KUDURRU_VIP__VIPS=null`, `KUDURRU_VIP__VIPS={}`, `KUDURRU_VIP__VIPS=invalid`,
		`KUDURRU_VIP__VIPS=[{"name":"dns","address":"192.0.2.53/24","interface":"eth0","vrid":53.5}]`,
		`KUDURRU_VIP__PREEMPTDELAY=null`, `KUDURRU_VIP__PREEMPTDELAY=[]`, `KUDURRU_VIP__PREEMPTDELAY={"dns":1.5}`,
	} {
		if _, err := Load("", []string{env}); err == nil {
			t.Fatal("malformed structured environment accepted")
		}
	}
}

func TestSupervisorValidationAndSettingsFileRefusal(t *testing.T) {
	for name, body := range map[string]string{
		"missing prefix": "vip: {vips: [{name: dns, address: 192.0.2.53, interface: eth0, vrid: 53}]}",
		"bad VRID":       "vip: {vips: [{name: dns, address: 192.0.2.53/24, interface: eth0, vrid: 256}]}",
		"bad name":       "vip: {vips: [{name: 'dns\n}', address: 192.0.2.53/24, interface: eth0, vrid: 53}]}",
		"duplicate":      "vip: {vips: [{name: dns, address: 192.0.2.53/24, interface: eth0, vrid: 53}, {name: dns, address: 192.0.2.53/32, interface: eth1, vrid: 53}]}",
		"unknown delay":  "vip: {preemptDelay: {absent: 20}}", "negative timeout": "edge: {stopTimeout: -1s}",
		"bad frontend": "edge: {dot: 'not:port'}", "unknown protocol": "edge: {protocols: [dnscrypt]}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(supervisorFile(t, body), nil); err == nil {
				t.Fatal("invalid supervisor config accepted")
			}
		})
	}
	for _, section := range []string{"edge", "vip"} {
		settings := supervisorFile(t, section+": {logLevel: debug}\n")
		if _, err := Load("", []string{"KUDURRU_SETTINGS_FILE=" + settings}); err == nil || !strings.Contains(err.Error(), "node") {
			t.Fatal("cluster settings file accepted node supervisor key")
		}
	}
}

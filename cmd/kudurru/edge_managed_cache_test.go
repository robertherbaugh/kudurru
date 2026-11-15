package main

import (
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestEdgeManagedCacheHostLayersAndExplicitFalse(t *testing.T) {
	key, ok := config.Lookup("edge.managedCacheBypass")
	if !ok || !key.HostOnly || !key.RestartRequired || key.Default != false {
		t.Fatal("managed bypass must be an opt-in host startup setting")
	}
	for _, renderer := range []string{"yaml", "lua"} {
		t.Run(renderer, func(t *testing.T) {
			args := []string{"edge", "render", "--renderer", renderer, "--backends", "node=127.0.0.1:5353"}
			want := supervisorRender(t, append(args, "--managed-cache-bypass")...)
			if !strings.Contains(want, "kudurru-managed-no-cache") {
				t.Fatal("explicit managed flag did not reach actual renderer")
			}
			file := writeTemp(t, "edge: {managedCacheBypass: true}\n")
			if got := supervisorRender(t, append([]string{"--config", file}, args...)...); got != want {
				t.Fatal("host file differs from explicit managed bypass flag")
			}
			t.Setenv("KUDURRU_EDGE__MANAGEDCACHEBYPASS", "true")
			if got := supervisorRender(t, args...); got != want {
				t.Fatal("host environment did not retain managed bypass")
			}
			if got := supervisorRender(t, append(args, "--managed-cache-bypass=false")...); strings.Contains(got, "kudurru-managed-no-cache") {
				t.Fatal("explicit false did not select static startup policy")
			}
		})
	}
}

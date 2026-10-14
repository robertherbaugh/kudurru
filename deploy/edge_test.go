package deploy_test

import (
	"fmt"
	"path/filepath"
	"testing"
)

// The edge derives the internal API token and CA from the same keys as
// serve. Missing credentials silently disable zone polling and send local
// zones through the cached resolver pool.
func TestChartEdgeInternalCredentials(t *testing.T) {
	for _, mode := range []string{"deployment", "daemonset", "sidecar"} {
		for _, keys := range []struct {
			name     string
			values   string
			current  string
			previous string
		}{
			{name: "generated", current: "kudurru-encryption-key"},
			{name: "existing", values: "auth: {encryptionKeySecret: my-key}\n", current: "my-key"},
			{name: "rotation", values: "auth: {encryptionKeySecret: my-key, encryptionKeyPreviousSecret: my-old-key}\n", current: "my-key", previous: "my-old-key"},
		} {
			t.Run(mode+"/"+keys.name, func(t *testing.T) {
				out, err := renderChart(t, keys.values+fmt.Sprintf("edge: {enabled: true, mode: %s}\n", mode))
				if err != nil {
					t.Fatal(err)
				}
				r := newRendered(t, out)
				r.checkGeneric(t)
				pod := r.pod(t)
				if mode != "sidecar" {
					kind := "Deployment"
					if mode == "daemonset" {
						kind = "DaemonSet"
					}
					pod = mapOf(dig(r.wantEdge(t, kind), "spec", "template"))
				}
				c := r.container(t, pod, "dnsdist")
				if c == nil {
					t.Fatal("no dnsdist container")
				}
				checkEdgeKeyFile(t, pod, c, "KUDURRU_ENCRYPTION_KEY_FILE", "/etc/kudurru/secrets/encryption/key", keys.current)
				if keys.previous != "" {
					checkEdgeKeyFile(t, pod, c, "KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE", "/etc/kudurru/secrets/encryption-previous/key", keys.previous)
				} else {
					for _, e := range list(c["env"]) {
						if dig(e, "name") == "KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE" {
							t.Error("edge references a previous key when none is configured")
						}
					}
				}
			})
		}
	}
}

func checkEdgeKeyFile(t *testing.T, pod, c map[string]any, env, path, secret string) {
	t.Helper()
	foundEnv := false
	for _, e := range list(c["env"]) {
		if dig(e, "name") == env {
			foundEnv = true
			if dig(e, "value") != path {
				t.Errorf("%s = %v, want %s", env, dig(e, "value"), path)
			}
		}
	}
	if !foundEnv {
		t.Errorf("edge lacks %s; zone polling needs internal credentials", env)
	}
	for _, m := range list(c["volumeMounts"]) {
		if dig(m, "mountPath") != filepath.Dir(path) {
			continue
		}
		if dig(m, "readOnly") != true {
			t.Errorf("%s must be mounted read-only", path)
		}
		for _, v := range list(dig(pod, "spec", "volumes")) {
			if dig(v, "name") == dig(m, "name") {
				if dig(v, "secret", "secretName") != secret || dig(v, "secret", "defaultMode") != 0o440 {
					t.Errorf("%s must mount Secret %s with mode 0440: %v", path, secret, v)
				}
				return
			}
		}
		t.Fatalf("%s mount has no volume", path)
	}
	t.Errorf("edge lacks the Secret mount for %s", path)
}

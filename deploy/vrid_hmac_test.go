package deploy_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/edge/keepalived"
)

const twoHMACVIPs = "edge: {enabled: true}\nvip:\n  provider: keepalived\n  keepalived:\n    interface: eth0\n    vips:\n      - {name: a, address: 10.0.0.53/24, vrid: 53}\n      - {name: b, address: 10.0.0.54/24, vrid: 54}\n"

func hmacPod(t *testing.T, values string) (*rendered, map[string]any) {
	t.Helper()
	out, err := renderChart(t, values)
	if err != nil {
		t.Fatal("HMAC chart render failed", err)
	}
	r := newRendered(t, out)
	r.checkGeneric(t)
	return r, mapOf(dig(r.wantEdge(t, "DaemonSet"), "spec", "template"))
}

func hmacVolume(t *testing.T, pod map[string]any, name string) map[string]any {
	t.Helper()
	for _, v := range list(dig(pod, "spec", "volumes")) {
		if dig(v, "name") == name {
			return mapOf(v)
		}
	}
	t.Fatal("HMAC volume missing", name)
	return nil
}

func hmacInit(t *testing.T, pod map[string]any) map[string]any {
	t.Helper()
	init := list(dig(pod, "spec", "initContainers"))
	if len(init) != 1 || dig(init[0], "name") != "vrrp-keys" {
		t.Fatal("private key init missing")
	}
	return mapOf(init[0])
}

func TestChartVRIDHMACReferences(t *testing.T) {
	for _, tc := range []struct {
		name, values string
		per          bool
		files        []string
		refs         []any
	}{
		{name: "default-shared", values: twoHMACVIPs, files: []string{"vrrp-hmac"}},
		{name: "existing-shared", values: twoHMACVIPs + "    existingSecret: external-vrrp\n", files: []string{"vrrp-hmac"}},
		{name: "per-vrid", values: twoHMACVIPs + "    hmacKeySecrets:\n      - {vrid: 53, secretName: auth-a, key: hmac}\n      - {vrid: 54, secretName: auth-b, key: hmac}\n", per: true, files: []string{"vrrp_hmac_53", "vrrp_hmac_54"}, refs: []any{"auth-a/hmac", "auth-b/hmac"}},
		{name: "one-secret-explicit-keys", values: twoHMACVIPs + "    hmacKeySecrets:\n      - {vrid: 54, secretName: auth.example, key: key-54}\n      - {vrid: 53, secretName: auth.example, key: key-53}\n", per: true, files: []string{"vrrp_hmac_54", "vrrp_hmac_53"}, refs: []any{"auth.example/key-54", "auth.example/key-53"}},
		{name: "single", values: "edge: {enabled: true}\nvip: {provider: keepalived, address: 10.0.0.53/24, keepalived: {interface: eth0, hmacKeySecrets: [{vrid: 53, secretName: auth-a, key: hmac}]}}", per: true, files: []string{"vrrp_hmac_53"}, refs: []any{"auth-a/hmac"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, pod := hmacPod(t, tc.values)
			init := hmacInit(t, pod)
			if !slices.Equal(list(init["command"]), []any{"/usr/bin/timeout", "30", "/bin/sh", "-ec"}) {
				t.Fatal("key staging must have a bounded execution deadline")
			}
			if dig(init, "securityContext", "runAsUser") != 0 || dig(init, "securityContext", "runAsGroup") != 0 || dig(init, "securityContext", "allowPrivilegeEscalation") != false || dig(init, "securityContext", "readOnlyRootFilesystem") != true || !slices.Equal(list(dig(init, "securityContext", "capabilities", "drop")), []any{"ALL"}) {
				t.Fatal("key init permissions widened")
			}
			if dig(init, "resources", "limits", "cpu") != "100m" || dig(init, "resources", "limits", "memory") != "32Mi" {
				t.Fatal("key init resource bounds missing")
			}
			private := hmacVolume(t, pod, "vrrp")
			if dig(private, "emptyDir", "medium") != "Memory" || dig(private, "emptyDir", "sizeLimit") != "2Mi" {
				t.Fatal("key files must stay in bounded private tmpfs")
			}
			script := str(list(init["args"])[0])
			if !strings.Contains(script, "for key_file in "+strings.Join(tc.files, " ")+"; do") || !strings.Contains(script, "head -c 4097") || !strings.Contains(script, "chmod 0600") || strings.Contains(script, "set -x") {
				t.Fatal("key script must use only selected filenames and bounded private reads")
			}
			source := hmacVolume(t, pod, "vrrp-source")
			if tc.per {
				r.wantNoObject(t, "Secret", "kudurru-vrrp")
				if dig(source, "projected", "defaultMode") != 256 {
					t.Fatal("source key mode changed")
				}
				var refs, files []any
				for _, raw := range list(dig(source, "projected", "sources")) {
					if dig(raw, "secret", "optional") != false {
						t.Fatal("missing selected Secret must stop startup")
					}
					items := list(dig(raw, "secret", "items"))
					if len(items) != 1 {
						t.Fatal("must project only selected keys")
					}
					refs = append(refs, str(dig(raw, "secret", "name"))+"/"+str(dig(items[0], "key")))
					files = append(files, dig(items[0], "path"))
				}
				if !slices.Equal(refs, tc.refs) {
					t.Fatal("selected key references changed")
				}
				want := make([]any, len(tc.files))
				for i, f := range tc.files {
					want[i] = f
				}
				if !slices.Equal(files, want) {
					t.Fatal("VRID projection identity changed")
				}
			} else {
				want := "kudurru-vrrp"
				if tc.name == "existing-shared" {
					want = "external-vrrp"
				} else {
					r.wantObject(t, "Secret", want)
				}
				if dig(source, "secret", "secretName") != want || dig(source, "secret", "optional") != false || dig(source, "secret", "defaultMode") != 256 || len(list(dig(source, "secret", "items"))) != 1 || dig(source, "secret", "items", 0, "key") != "vrrp-hmac" {
					t.Fatal("shared Secret selection changed")
				}
			}
			for _, raw := range list(dig(pod, "spec", "containers")) {
				c := mapOf(raw)
				for _, m := range list(c["volumeMounts"]) {
					if dig(m, "name") == "vrrp-source" {
						t.Fatal("raw source keys mounted into running container")
					}
					if dig(m, "name") == "vrrp" && (c["name"] != "keepalived" || dig(m, "readOnly") != true) {
						t.Fatal("private HMAC files exposed outside keepalived")
					}
				}
			}
			k := r.container(t, pod, "keepalived")
			if init["image"] != k["image"] {
				t.Fatal("key init must use the pinned keepalived image")
			}
			c := keepalived.Config{RouterID: "helm-test", UnicastSrc: "10.0.0.11", Peers: []string{"10.0.0.12"}}
			for _, raw := range list(k["args"]) {
				arg := str(raw)
				if value, ok := strings.CutPrefix(arg, "--vip="); ok {
					v, err := keepalived.ParseVIP(value)
					if err != nil {
						t.Fatal(err)
					}
					c.VIPs = append(c.VIPs, v)
				}
				if value, ok := strings.CutPrefix(arg, "--hmac-key-file="); ok {
					c.HMACKeyFile = value
				}
			}
			wantPattern := "/etc/kudurru/secrets/vrrp/vrrp-hmac"
			if tc.per {
				wantPattern = "/etc/kudurru/secrets/vrrp/vrrp_hmac_{vrid}"
			}
			if c.HMACKeyFile != wantPattern {
				t.Fatal("chart HMAC flag changed")
			}
			conf, err := keepalived.Render(c)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range c.VIPs {
				path := strings.ReplaceAll(wantPattern, "{vrid}", fmt.Sprint(v.VRID))
				section := strings.SplitN(string(conf), "vrrp_instance VI_"+v.Name+" {", 2)
				if len(section) != 2 || !strings.Contains(strings.SplitN(section[1], "vrrp_instance ", 2)[0], "key 1 file:"+path+"\n") {
					t.Fatal("supervisor render mismatched VRID key")
				}
			}
		})
	}
}

func TestChartVRIDHMACRefusals(t *testing.T) {
	for _, tc := range []struct{ name, extra, want string }{
		{"missing-vrid", "[{secretName: auth-a, key: hmac}]", "vrid must"},
		{"duplicate", "[{vrid: 53, secretName: auth-a, key: hmac}, {vrid: 53, secretName: auth-b, key: hmac}]", "VRIDs must be unique"},
		{"missing-mapping", "[{vrid: 53, secretName: auth-a, key: hmac}]", "cover every"},
		{"mismatched", "[{vrid: 55, secretName: auth-a, key: hmac}]", "no configured VIP"},
		{"zero", "[{vrid: 0, secretName: auth-a, key: hmac}]", "vrid must"},
		{"range", "[{vrid: 256, secretName: auth-a, key: hmac}]", "vrid must"},
		{"fraction", "[{vrid: 53.5, secretName: auth-a, key: hmac}]", "vrid must"},
		{"missing-name", "[{vrid: 53, key: hmac}]", "secretName is required"},
		{"bad-name", "[{vrid: 53, secretName: invalid/name, key: hmac}]", "valid Kubernetes Secret name"},
		{"long-label", "[{vrid: 53, secretName: " + strings.Repeat("a", 64) + ", key: hmac}]", "labels must"},
		{"missing-key", "[{vrid: 53, secretName: auth-a}]", "key is required"},
		{"empty-key", "[{vrid: 53, secretName: auth-a, key: ''}]", "valid Kubernetes Secret key"},
		{"key-path", "[{vrid: 53, secretName: auth-a, key: ../escape}]", "valid Kubernetes Secret key"},
		{"namespace", "[{vrid: 53, secretName: auth-a, key: hmac, namespace: elsewhere}]", "accept only"},
		{"raw-value", "[{vrid: 53, secretName: auth-a, key: hmac, value: withheld-marker}]", "accept only"},
		{"not-list", "{}", "must be a list"},
		{"not-reference", "[withheld-marker]", "must be Secret references"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := renderChart(t, twoHMACVIPs+"    hmacKeySecrets: "+tc.extra+"\n")
			// Report categorically: unexpected diagnostics must not echo a raw key.
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "withheld-marker") {
				t.Fatal("expected categorical HMAC reference refusal")
			}
		})
	}
	for _, shared := range []string{"existingSecret: auth-shared", "hmacSecret: withheld-marker"} {
		values := twoHMACVIPs + "    " + shared + "\n    hmacKeySecrets: [{vrid: 53, secretName: auth-a, key: hmac}]\n"
		if _, err := renderChart(t, values); err == nil || !strings.Contains(err.Error(), "cannot be combined") || strings.Contains(err.Error(), "withheld-marker") {
			t.Fatal("mixed shared/per-VRID selection must refuse categorically")
		}
	}
	if _, err := renderChart(t, "vip: {keepalived: {hmacKeySecrets: [{vrid: 53, secretName: auth-a, key: hmac}]}}"); err == nil || !strings.Contains(err.Error(), "requires vip.provider") {
		t.Fatal("inactive per-VRID configuration silently ignored")
	}
	values := strings.Replace(twoHMACVIPs, "vrid: 54", "vrid: '053'", 1)
	if _, err := renderChart(t, values); err == nil || !strings.Contains(err.Error(), "VRIDs must be unique") {
		t.Fatal("numeric aliases bypassed VIP uniqueness")
	}
	refs := strings.Repeat("{vrid: 53, secretName: auth-a, key: hmac},", 256)
	if _, err := renderChart(t, twoHMACVIPs+"    hmacKeySecrets: ["+refs+"]\n"); err == nil || !strings.Contains(err.Error(), "at most 255") {
		t.Fatal("unbounded HMAC references accepted")
	}
}

func TestChartVRIDHMACRotationAndMaximum(t *testing.T) {
	values := twoHMACVIPs + "    hmacKeySecrets: [{vrid: 53, secretName: auth-a-v1, key: hmac}, {vrid: 54, secretName: auth-b, key: hmac}]\n"
	_, before := hmacPod(t, values)
	_, after := hmacPod(t, strings.Replace(values, "auth-a-v1", "auth-a-v2", 1))
	if reflect.DeepEqual(before, after) {
		t.Fatal("Secret reference rotation did not change Pod template")
	}
	if dig(hmacVolume(t, after, "vrrp-source"), "projected", "sources", 0, "secret", "name") != "auth-a-v2" {
		t.Fatal("new key reference lost")
	}
	var vips, refs []string
	for i := 1; i <= 255; i++ {
		vips = append(vips, fmt.Sprintf("{name: vip-%d, address: 10.55.%d.%d/24, vrid: %d}", i, i/250, i%250+1, i))
		refs = append(refs, fmt.Sprintf("{vrid: %d, secretName: auth-many, key: key-%d}", i, i))
	}
	values = "edge: {enabled: true}\nvip: {provider: keepalived, keepalived: {interface: eth0, vips: [" + strings.Join(vips, ",") + "], hmacKeySecrets: [" + strings.Join(refs, ",") + "]}}"
	_, pod := hmacPod(t, values)
	if len(list(dig(hmacVolume(t, pod, "vrrp-source"), "projected", "sources"))) != 255 {
		t.Fatal("maximum valid VRID mapping lost references")
	}
}

func TestChartVRIDHMACStaging(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int
		missing bool
		wantOK  bool
	}{
		{name: "private-copy", size: 32, wantOK: true},
		{name: "maximum-file", size: 4096, wantOK: true},
		{name: "empty", size: 0},
		{name: "oversize", size: 4097},
		{name: "missing", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, pod := hmacPod(t, twoHMACVIPs)
			source, private := t.TempDir(), t.TempDir()
			if !tc.missing {
				if err := os.WriteFile(filepath.Join(source, "vrrp-hmac"), bytes.Repeat([]byte{'a'}, tc.size), 0o440); err != nil { //nolint:gosec // Simulate fsGroup-readable projected input; output must be private.
					t.Fatal(err)
				}
			}
			script := str(list(hmacInit(t, pod)["args"])[0])
			script = strings.ReplaceAll(strings.ReplaceAll(script, "/vrrp-source", source), "/vrrp-private", private)
			cmd := exec.Command("sh", "-ec", script) //nolint:gosec // Fixed chart script and test-owned directories; no user input.
			// Always keep output private so future errors cannot expose supplied keys.
			if err := cmd.Run(); (err == nil) != tc.wantOK {
				t.Fatal("unexpected bounded HMAC staging result")
			}
			path := filepath.Join(private, "vrrp-hmac")
			if info, err := os.Stat(path); err == nil {
				if info.Size() > 4097 {
					t.Fatal("key read exceeded bound")
				}
				if tc.wantOK && info.Mode().Perm() != 0o600 {
					t.Fatal("key copy is not private")
				}
			} else if tc.wantOK {
				t.Fatal("key copy missing")
			}
			if tc.wantOK {
				a, err := os.ReadFile(filepath.Join(source, "vrrp-hmac")) //nolint:gosec // Test-owned fixed filename.
				if err != nil {
					t.Fatal(err)
				} //nolint:gosec // Test-owned fixed filenames.
				b, err := os.ReadFile(path) //nolint:gosec // Test-owned fixed filename.
				if err != nil {
					t.Fatal(err)
				} //nolint:gosec // Test-owned fixed filename.
				if !bytes.Equal(a, b) {
					t.Fatal("staging changed key bytes")
				}
			}
		})
	}
}

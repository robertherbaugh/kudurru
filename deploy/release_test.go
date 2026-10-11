package deploy_test

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	yamlv3 "go.yaml.in/yaml/v3"
)

// TestGoreleaserConfig pins the release contract of .goreleaser.yaml
// (docs/architecture.md §12, D68): CGO_ENABLED=0 binaries for linux and
// darwin on amd64 and arm64, distroless nonroot ko images for linux/amd64
// and linux/arm64 plus a -debug variant, deb/rpm/apk with the systemd unit,
// cosign signatures. `goreleaser check` in CI validates the schema.
func TestGoreleaserConfig(t *testing.T) {
	root := repoRoot(t)
	b := readRepoFile(t, ".goreleaser.yaml")
	var cfg map[string]any
	if err := yamlv3.Unmarshal(b, &cfg); err != nil {
		t.Fatalf(".goreleaser.yaml does not parse: %v", err)
	}
	if cfg["version"] != 2 {
		t.Errorf("goreleaser config version = %v, want 2", cfg["version"])
	}
	builds := list(cfg["builds"])
	if len(builds) != 1 {
		t.Fatalf("want one build, got %d", len(builds))
	}
	build := mapOf(builds[0])
	if !slices.Contains(list(build["env"]), any("CGO_ENABLED=0")) {
		t.Errorf("build must set CGO_ENABLED=0")
	}
	for _, want := range []string{"linux", "darwin"} {
		if !slices.Contains(list(build["goos"]), any(want)) {
			t.Errorf("goos lacks %s", want)
		}
	}
	for _, want := range []string{"amd64", "arm64"} {
		if !slices.Contains(list(build["goarch"]), any(want)) {
			t.Errorf("goarch lacks %s", want)
		}
	}
	tags := map[string]bool{}
	for _, k := range list(cfg["kos"]) {
		km := mapOf(k)
		if !strings.HasPrefix(str(km["base_image"]), "gcr.io/distroless/static:") || !strings.Contains(str(km["base_image"]), "nonroot") {
			t.Errorf("ko %v: base image %v is not distroless static nonroot", km["id"], km["base_image"])
		}
		if !slices.Equal(list(km["platforms"]), []any{"linux/amd64", "linux/arm64"}) {
			t.Errorf("ko %v: platforms %v, want linux/amd64 and linux/arm64 (D68)", km["id"], km["platforms"])
		}
		if !slices.Contains(list(km["env"]), any("CGO_ENABLED=0")) {
			t.Errorf("ko %v must build with CGO_ENABLED=0", km["id"])
		}
		for _, tg := range list(km["tags"]) {
			tags[str(tg)] = true
		}
	}
	if !tags["{{ .Version }}"] || !tags["{{ .Version }}-debug"] {
		t.Errorf("images need the version tag and the -debug variant, got %v", tags)
	}
	for _, n := range list(cfg["nfpms"]) {
		nm := mapOf(n)
		for _, f := range []string{"deb", "rpm", "apk"} {
			if !slices.Contains(list(nm["formats"]), any(f)) {
				t.Errorf("nfpm lacks %s", f)
			}
		}
		unit := false
		for _, c := range list(nm["contents"]) {
			src := str(mapOf(c)["src"])
			if src != "" {
				if _, err := os.Stat(filepath.Join(root, src)); err != nil {
					t.Errorf("nfpm content %s: %v", src, err)
				}
			}
			if str(mapOf(c)["dst"]) == "/usr/lib/systemd/system/kudurru.service" {
				unit = true
			}
		}
		if !unit {
			t.Errorf("packages must install the systemd unit")
		}
		for _, s := range mapOf(nm["scripts"]) {
			st, err := os.Stat(filepath.Join(root, str(s)))
			if err != nil {
				t.Errorf("nfpm script %v: %v", s, err)
			} else if st.Mode()&0o111 == 0 {
				t.Errorf("nfpm script %v is not executable", s)
			}
		}
	}
	if len(list(cfg["signs"])) == 0 || len(list(cfg["docker_signs"])) == 0 {
		t.Errorf("releases are cosign-signed (signs and docker_signs)")
	}
}

// TestSystemdUnit checks the unit's ExecStart against the package layout and
// loads the packaged kudurru.yaml through config.Load.
func TestSystemdUnit(t *testing.T) {
	sections := map[string]map[string]string{}
	var cur string
	sc := bufio.NewScanner(bytes.NewReader(readRepoFile(t, "deploy/systemd/kudurru.service")))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			cur = strings.Trim(line, "[]")
			sections[cur] = map[string]string{}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || cur == "" {
			t.Errorf("malformed unit line %q", line)
			continue
		}
		sections[cur][k] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	svc := sections["Service"]
	if svc["ExecStart"] != "/usr/bin/kudurru serve --config /etc/kudurru/kudurru.yaml" {
		t.Errorf("ExecStart = %q", svc["ExecStart"])
	}
	if svc["User"] != "kudurru" || svc["AmbientCapabilities"] != "CAP_NET_BIND_SERVICE" || svc["StateDirectory"] != "kudurru" {
		t.Errorf("unit must run as kudurru with only CAP_NET_BIND_SERVICE and StateDirectory=kudurru: %v", svc)
	}
	if sections["Install"]["WantedBy"] != "multi-user.target" {
		t.Errorf("unit is not installable")
	}
	body := readRepoFile(t, "deploy/systemd/kudurru.yaml")
	cfg := checkConfig(t, "deploy/systemd/kudurru.yaml", body, nil)
	if cfg.Node.DataDir != "/var/lib/kudurru" {
		t.Errorf("packaged dataDir %s is not the unit's StateDirectory", cfg.Node.DataDir)
	}
	env := readRepoFile(t, "deploy/systemd/kudurru.env")
	for _, line := range strings.Split(string(env), "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "#")
		if name, _, ok := strings.Cut(line, "="); ok && strings.HasPrefix(name, "KUDURRU_") {
			if err := checkEnvName(name); err != nil {
				t.Errorf("kudurru.env: %v", err)
			}
		}
	}
}

// TestDockerfile keeps the local image equivalent to the release image.
func TestDockerfile(t *testing.T) {
	s := string(readRepoFile(t, "Dockerfile"))
	for _, want := range []string{"CGO_ENABLED=0", "FROM gcr.io/distroless/static:nonroot AS release", "FROM gcr.io/distroless/static:debug-nonroot AS debug", "USER 65532:65532", `"kudurru", "health", "--ready"`} {
		if !strings.Contains(s, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	if i, j := strings.Index(s, "AS debug"), strings.Index(s, "AS release"); i > j {
		t.Errorf("the release stage must be last so a plain docker build produces it")
	}
}

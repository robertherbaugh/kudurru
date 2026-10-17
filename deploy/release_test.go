package deploy_test

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	yamlv3 "go.yaml.in/yaml/v3"
)

// TestGoreleaserConfig pins the release contract of .goreleaser.yaml
// (dev-docs/design-record.md §12, D68): CGO_ENABLED=0 binaries for linux and
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
	publish := string(readRepoFile(t, ".github/workflows/publish.yml"))
	for _, k := range list(cfg["kos"]) {
		km := mapOf(k)
		// publish.yml passes the kudurru-base digest (images/base); the
		// fallback is the distroless image that base is built on.
		env, fallback, ok := koBase(str(km["base_image"]))
		if !ok {
			t.Errorf("ko %v: base image %v is not {{ envOrDefault \"<VAR>\" \"<distroless>\" }}", km["id"], km["base_image"])
			continue
		}
		if !strings.HasPrefix(fallback, "gcr.io/distroless/static:") || !strings.Contains(fallback, "nonroot") {
			t.Errorf("ko %v: base image %v is not distroless static nonroot", km["id"], fallback)
		}
		if !digestRefRE.MatchString(fallback) {
			t.Errorf("ko %v: base image %v is not pinned as tag@sha256:<digest>", km["id"], fallback)
		}
		if !strings.Contains(publish, "\n          "+env+": ghcr.io/robertherbaugh/kudurru-base@${{ steps.") {
			t.Errorf("ko %v: publish.yml does not set %s to a kudurru-base digest", km["id"], env)
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
	// A release candidate publishes <version>-rc.<N> only; the release moves
	// latest (dev-docs/release-checklist.md, "Release pipeline").
	if !tags["{{ if not .Prerelease }}latest{{ end }}"] {
		t.Errorf("images need latest for releases only, got %v", tags)
	}
	for tg := range tags {
		if strings.Contains(tg, "latest") && !strings.Contains(tg, "if not .Prerelease") {
			t.Errorf("image tag %q moves latest on a release candidate", tg)
		}
	}
	// Every binary embeds dashboard/dist: the before hook builds the dashboard.
	if !slices.Contains(list(mapOf(cfg["before"])["hooks"]), any("make dashboard-build")) {
		t.Errorf("before hooks must run make dashboard-build, or the binaries embed the placeholder dashboard")
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

// digestRefRE matches an image reference pinned as tag@digest.
// koBaseRE is a ko base_image of .goreleaser.yaml: a variable with the
// pinned distroless fallback.
var koBaseRE = regexp.MustCompile(`^\{\{ envOrDefault "([A-Z_]+)" "([^"]+)" \}\}$`)

// koBase splits a ko base_image into its variable and its fallback.
func koBase(ref string) (env, fallback string, ok bool) {
	m := koBaseRE.FindStringSubmatch(ref)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

var digestRefRE = regexp.MustCompile(`^[a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`)

// TestDockerfile keeps the local image equivalent to the release image,
// including the digest-pinned distroless bases of the ko entries.
func TestDockerfile(t *testing.T) {
	s := string(readRepoFile(t, "Dockerfile"))
	var cfg struct {
		Kos []struct {
			ID        string `yaml:"id"`
			BaseImage string `yaml:"base_image"`
		} `yaml:"kos"`
	}
	if err := yamlv3.Unmarshal(readRepoFile(t, ".goreleaser.yaml"), &cfg); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{}
	for _, k := range cfg.Kos {
		_, base[k.ID], _ = koBase(k.BaseImage)
	}
	if base["kudurru"] == "" || base["kudurru-debug"] == "" {
		t.Fatalf("ko entries kudurru and kudurru-debug not found: %v", base)
	}
	for _, want := range []string{"CGO_ENABLED=0", "FROM " + base["kudurru"] + " AS release", "FROM " + base["kudurru-debug"] + " AS debug", "USER 65532:65532", `"kudurru", "health", "--ready"`} {
		if !strings.Contains(s, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	if i, j := strings.Index(s, "AS debug"), strings.Index(s, "AS release"); i > j {
		t.Errorf("the release stage must be last so a plain docker build produces it")
	}
	// ko's layout, which the derived images copy from either image.
	if !strings.Contains(s, "COPY --from=build /out/kudurru /ko-app/kudurru") || !strings.Contains(s, `ENTRYPOINT ["/ko-app/kudurru"]`) {
		t.Errorf("the Dockerfile must put the binary where ko does (/ko-app/kudurru)")
	}
	for _, f := range []string{"images/dnsdist/Dockerfile", "images/keepalived/Dockerfile"} {
		if !strings.Contains(string(readRepoFile(t, f)), "COPY --from=kudurru /ko-app/kudurru /usr/local/bin/kudurru") {
			t.Errorf("%s must copy /ko-app/kudurru, the path of the ko release image", f)
		}
	}
	// The -hostnetwork variant: the ko binary with the file capability, on
	// the base of the kudurru ko entry.
	hn := string(readRepoFile(t, "images/hostnetwork/Dockerfile"))
	for _, want := range []string{"FROM " + base["kudurru"] + "\n", "COPY --from=kudurru /ko-app/kudurru",
		"ARG DHCP_CAPABILITIES=false", "false) file_caps=cap_net_bind_service=ep", "true) file_caps=cap_net_bind_service,cap_net_raw=ep",
		`setcap "$file_caps" /out/kudurru`, `test "$(getcap /out/kudurru)" = "/out/kudurru $file_caps"`,
		"USER 65532:65532", `ENTRYPOINT ["/ko-app/kudurru"]`} {
		if !strings.Contains(hn, want) {
			t.Errorf("images/hostnetwork/Dockerfile lacks %q", want)
		}
	}
	// The ko images' base, on the distroless image of the kudurru ko entry.
	kb := string(readRepoFile(t, "images/base/Dockerfile"))
	if !strings.Contains(kb, "ARG BASE_IMAGE="+base["kudurru"]+"\n") || !strings.Contains(kb, "USER 65532:65532") {
		t.Errorf("images/base/Dockerfile must default BASE_IMAGE to %s and run as 65532", base["kudurru"])
	}
	// Every image has /var/lib/kudurru (0755) and /run/kudurru (1777) owned
	// by 65532: a fresh named volume takes their owner and mode
	// (hack/volume-check.sh runs the images).
	for f, src := range map[string]string{"Dockerfile": s, "images/hostnetwork/Dockerfile": hn, "images/base/Dockerfile": kb} {
		for _, want := range []string{"chmod 0755 /out/var/lib/kudurru && chmod 1777 /out/run/kudurru", "chown 65532:65532 /out/var/lib/kudurru /out/run/kudurru", " /out/var/lib/ /var/lib/\n", " /out/run/ /run/\n"} {
			if !strings.Contains(src, want) {
				t.Errorf("%s lacks %q (the writable directories)", f, want)
			}
		}
	}
}

// TestVersionFile checks VERSION, the source of the release candidate and
// release tags (hack/release-tag.sh) and of RELEASE_VERSION in the Makefile.
func TestVersionFile(t *testing.T) {
	v := strings.TrimSpace(string(readRepoFile(t, "VERSION")))
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(v) {
		t.Errorf("VERSION is %q, want MAJOR.MINOR.PATCH without a v or a suffix", v)
	}
	if !strings.Contains(string(readRepoFile(t, "Makefile")), "RELEASE_VERSION := $(shell tr -d '[:space:]' < VERSION") {
		t.Errorf("the Makefile's RELEASE_VERSION must come from VERSION")
	}
}

// TestReleaseTagScript runs hack/release-tag.sh in a scratch repository
// through a release cycle: candidates, a release, re-runs and the refusals.
func TestReleaseTagScript(t *testing.T) {
	for _, tool := range []string{"git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "hack"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hack", "release-tag.sh"), readRepoFile(t, "hack/release-tag.sh"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...) //nolint:gosec // the test drives git in its scratch repository with its own arguments
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	commit := func(version, file string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(version+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if file != "" {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(file), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		git("add", "-A")
		git("commit", "-q", "--allow-empty", "-m", "c")
	}
	// run returns the key=value output, or the error message with ok false.
	run := func(mode string) (map[string]string, string, bool) {
		t.Helper()
		cmd := exec.Command("bash", "hack/release-tag.sh", mode) //nolint:gosec // mode is one of the test's own literals
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GITHUB_OUTPUT=")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, stderr.String(), false
		}
		out := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
			k, v, _ := strings.Cut(line, "=")
			out[k] = v
		}
		return out, "", true
	}
	want := func(mode string, kv ...string) {
		t.Helper()
		out, msg, ok := run(mode)
		if !ok {
			t.Fatalf("%s failed: %s", mode, msg)
		}
		for i := 0; i < len(kv); i += 2 {
			if out[kv[i]] != kv[i+1] {
				t.Errorf("%s: %s = %q, want %q (%v)", mode, kv[i], out[kv[i]], kv[i+1], out)
			}
		}
	}
	refuse := func(mode, contains string) {
		t.Helper()
		out, msg, ok := run(mode)
		if ok {
			t.Fatalf("%s succeeded with %v, want a refusal containing %q", mode, out, contains)
		}
		if !strings.Contains(msg, contains) {
			t.Errorf("%s: %q does not contain %q", mode, msg, contains)
		}
	}

	git("init", "-q", "-b", "main")
	commit("v0.1", "")
	refuse("rc", "want MAJOR.MINOR.PATCH")
	commit("0.1.0", "a")
	want("rc", "version", "0.1.0", "tag", "v0.1.0-rc.1", "exists", "false", "previous", "")
	refuse("release", "no release candidate for 0.1.0")
	git("tag", "-a", "v0.1.0-rc.1", "-m", "rc.1")
	want("rc", "tag", "v0.1.0-rc.1", "exists", "true")
	for n := 2; n <= 10; n++ {
		commit("0.1.0", "")
		git("tag", "v0.1.0-rc."+strconv.Itoa(n))
	}
	commit("0.1.0", "b")
	want("rc", "tag", "v0.1.0-rc.11", "previous", "v0.1.0-rc.10")
	refuse("release", "does not match v0.1.0-rc.10")
	git("tag", "v0.1.0-rc.11")
	want("release", "tag", "v0.1.0", "exists", "false", "previous", "", "rc", "v0.1.0-rc.11")
	git("tag", "-a", "v0.1.0", "-m", "v0.1.0")
	want("release", "tag", "v0.1.0", "exists", "true")
	refuse("rc", "v0.1.0 is already released")
	commit("0.1.0", "c")
	refuse("release", "v0.1.0 is already released")
	commit("0.2.0", "")
	want("rc", "tag", "v0.2.0-rc.1", "previous", "v0.1.0")
	git("tag", "v0.2.0-rc.1")
	want("release", "tag", "v0.2.0", "previous", "v0.1.0", "rc", "v0.2.0-rc.1")
}

// TestWorkflowActionsPinned: every third-party action in the workflows is
// pinned by commit SHA with its version as a comment (Dependabot updates
// both, .github/dependabot.yml); local reusable workflows are exempt.
func TestWorkflowActionsPinned(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("workflows: %v %v", files, err)
	}
	uses := regexp.MustCompile(`(?m)^\s*(?:-\s+)?uses:\s*(\S+)(.*)$`)
	pinned := regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)
	version := regexp.MustCompile(`^\s+# v\d+\.\d+\.\d+$`)
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // repository files
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range uses.FindAllStringSubmatch(string(b), -1) {
			ref, rest := m[1], m[2]
			if strings.HasPrefix(ref, "./") {
				continue
			}
			if !pinned.MatchString(ref) || !version.MatchString(rest) {
				t.Errorf("%s: %s%s is not pinned as owner/repo@<sha> # vX.Y.Z", filepath.Base(f), ref, rest)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(repoRoot(t), ".github", "dependabot.yml")); err != nil {
		t.Errorf("the github-actions pins need .github/dependabot.yml: %v", err)
	}
}

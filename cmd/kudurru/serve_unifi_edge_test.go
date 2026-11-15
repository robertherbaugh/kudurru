package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/ee/edge"
	edgednsdist "github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// startUniFiHostnameEdge is opt-in because ordinary tests do not require a
// container daemon. The acceptance uses only an already-present pinned image,
// production topology/renderer, and one named owned container on the test daemon.
func startUniFiHostnameEdge(t *testing.T, cfg *config.Config, secret []byte) string {
	t.Helper()
	if os.Getenv("KUDURRU_F18C_DNSDIST") != "1" {
		return ""
	}
	const daemon = "unix:///tmp/kudurru-lab-docker.sock"
	const image = kudurrutest.DnsdistImage
	address := offlineFreeAddress(t, "tcp")
	_, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := edge.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	topology, err := edge.Build(edge.Options{ServerID: "f18c-hostnames", Listen: []string{"127.0.0.1"}, Protocols: []edge.Protocol{edge.Do53}, Ports: map[edge.Protocol]int{edge.Do53: port}, API: offlineFreeAddress(t, "tcp"), Console: offlineFreeAddress(t, "tcp"), ProxyProtocolSecret: string(secret)}, []edge.Backend{{ID: edge.BackendID(cfg.Node.Name), Name: cfg.Node.Name, Address: cfg.Listeners.DNS[0].Address, Weight: 100, Order: 1}}, creds)
	if err != nil {
		t.Fatal(err)
	}
	files, err := (edgednsdist.YAMLRenderer{}).Render(topology)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file.Name), file.Data, 0600); err != nil {
			t.Fatal("write private edge fixture")
		}
	}
	name := fmt.Sprintf("kudurru-f18c-%d", time.Now().UnixNano())
	args := []string{"--host", daemon, "run", "--detach", "--pull=never", "--name", name, "--network", "host", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only", "--label", "io.kudurru.f18c=" + name, "--mount", "type=bind,src=" + dir + ",dst=" + dir + ",readonly", "--entrypoint", "dnsdist", image, "--supervised", "--disable-syslog", "-C", filepath.Join(dir, "dnsdist.yml")}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // fixed test daemon/image, generated owned resource and paths; no shell
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Run(); err != nil {
		t.Fatal("start owned pinned dnsdist failed")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "docker", "--host", daemon, "rm", "--force", name) //nolint:gosec // remove only this test's generated named container
		command.Stdout, command.Stderr = io.Discard, io.Discard
		if err := command.Run(); err != nil {
			t.Error("owned dnsdist did not join")
		}
	})
	until := time.Now().Add(10 * time.Second)
	for {
		q := new(dns.Msg)
		q.SetQuestion(edge.HealthQName, dns.TypeTXT)
		a, _, err := (&dns.Client{Timeout: 200 * time.Millisecond}).ExchangeContext(t.Context(), q, address)
		if err == nil && a.Rcode == dns.RcodeSuccess && len(a.Answer) == 1 {
			return address
		}
		if time.Now().After(until) {
			t.Fatal("pinned dnsdist did not route to mounted backend")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

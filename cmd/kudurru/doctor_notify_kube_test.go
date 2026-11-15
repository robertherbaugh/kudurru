package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

// Called by the actual Kubernetes startup/restart/offline/wrong-owner fixture.
// The diagnostic must use the same complete installed source as the listeners.
func kubeTSIGDoctorRoundTrip(t *testing.T, cfg *config.Config, key dnsserver.TSIGKey, available bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doctor.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key.Secret)), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := *cfg
	probe.Notify.Source = "127.0.0.2"
	probe.TSIGKeys = []config.TSIGKey{{Name: key.Name, Algorithm: key.Algorithm, SecretFile: path}}
	var output strings.Builder
	report := &doctorReport{say: func(format string, args ...any) { _, _ = fmt.Fprintf(&output, format, args...) }}
	checkSelfNotify(t.Context(), report, &probe, cfg.Listeners.DNS[0].Address, key.Name)
	if available {
		if report.failures != 0 || !strings.Contains(output.String(), "observed egress 127.0.0.2") || !strings.Contains(output.String(), "loopback confirms the local path only") {
			t.Fatal("Kubernetes diagnostic did not attest to the authenticated UDP source")
		}
	} else if report.failures != 1 || strings.Contains(output.String(), "notify: OK") {
		t.Fatal("Kubernetes diagnostic admitted an unavailable or wrong-owner key source")
	}
	if !available {
		return
	}
	for _, mode := range []string{"unsigned", "wrong-key", "tcp", "malformed"} {
		t.Run("doctor-"+mode, func(t *testing.T) {
			q := new(dns.Msg).SetNotify(ingress.HealthName)
			client := &dns.Client{Timeout: time.Second, Dialer: &net.Dialer{LocalAddr: &net.UDPAddr{IP: net.ParseIP("127.0.0.2")}}}
			if mode != "unsigned" {
				secret := key.Secret
				if mode == "wrong-key" {
					secret = bytes.Repeat([]byte{0x99}, 32)
				}
				ring, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: key.Name, Algorithm: key.Algorithm, Secret: secret})
				if err != nil {
					t.Fatal(err)
				}
				client.TsigProvider = dnsserver.TSIGProvider(ring)
				q.SetTsig(key.Name, key.Algorithm, 30, time.Now().Unix())
			}
			if mode == "tcp" {
				client.Net, client.Dialer = "tcp", nil
			}
			if mode == "malformed" {
				q.Question[0].Qtype = dns.TypeTXT
			}
			answer, _, err := client.ExchangeContext(t.Context(), q, cfg.Listeners.DNS[0].Address)
			if answer == nil && err == nil {
				t.Fatal("diagnostic returned no outcome")
			}
			if answer != nil && (answer.Rcode == dns.RcodeSuccess || len(answer.Answer) != 0) {
				t.Fatal("invalid Kubernetes diagnostic exposed an address")
			}
		})
	}
}

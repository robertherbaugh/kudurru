package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func doctorNotifyFixture(t *testing.T) *primaryNetworkFixture {
	t.Helper()
	return newPrimaryNetworkFixture(t, func(p *primaryRuntime) pipeline.Next {
		return p.doctorNotify(func(req *pipeline.Request) (*pipeline.Response, error) {
			return pipeline.Reply(req, dns.RcodeNotAuth, pipeline.StatusAuthoritative), nil
		})
	})
}

func doctorNotifyMessage() *dns.Msg {
	m := new(dns.Msg)
	m.SetNotify(ingress.HealthName)
	m.SetTsig("updates.", dns.HmacSHA256, 30, time.Now().Unix())
	return m
}

func TestDoctorNotifyNativeRoundTripAndCLI(t *testing.T) {
	f := doctorNotifyFixture(t)
	secretPath := filepath.Join(t.TempDir(), "notify.key")
	secret, _, _ := f.keys.Lookup("updates.")
	if err := os.WriteFile(secretPath, []byte(base64.StdEncoding.EncodeToString(secret)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Notify.Source = "127.0.0.2"
	cfg.TSIGKeys = []config.TSIGKey{{Name: "updates", Algorithm: "hmac-sha256", SecretFile: secretPath}}
	cfg.Listeners.DNS = []config.DNSListener{{Address: f.listeners.Addr(dnsserver.ProtoUDP).String(), Protocols: []string{"udp"}}}
	var out strings.Builder
	report := &doctorReport{say: func(format string, args ...any) { _, _ = fmt.Fprintf(&out, format, args...) }}
	before, err := f.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkSelfNotify(t.Context(), report, cfg, "", "updates")
	if report.failures != 0 || !strings.Contains(out.String(), "observed egress 127.0.0.2") || !strings.Contains(out.String(), "loopback confirms the local path only") {
		t.Fatalf("authenticated native self-NOTIFY did not report the selected source: %s", out.String())
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	configPath := doctorConfig(t, t.TempDir(), busy)
	b, err := os.ReadFile(configPath) //nolint:gosec // test-owned temporary configuration
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, []byte(fmt.Sprintf("notify: { source: 127.0.0.2 }\ntsigKeys:\n- { name: updates, algorithm: hmac-sha256, secretFile: %s }\n", secretPath))...)
	if err := os.WriteFile(configPath, b, 0o600); err != nil { //nolint:gosec // test-owned temporary configuration
		t.Fatal(err)
	}
	h := newHarness(t)
	cliOut, _, err := h.run("--config", configPath, "doctor", "--skip-api", "--skip-ports", "--notify", "--notify-target", f.listeners.Addr(dnsserver.ProtoUDP).String(), "--notify-key", "updates")
	if err != nil || !strings.Contains(cliOut, "notify: OK") || !strings.Contains(cliOut, "observed egress 127.0.0.2") {
		t.Fatal("doctor CLI did not complete the authenticated native round trip")
	}
	cliOut, _, err = h.run("--config", configPath, "doctor", "--skip-api", "--skip-ports", "--notify-key", "absent")
	if err == nil || !strings.Contains(cliOut, "notify: FAIL") {
		t.Fatal("doctor CLI did not return a failure for an unavailable credential")
	}

	after, err := f.store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("diagnostic mutated durable state", err)
	}

	t.Run("unsigned", func(t *testing.T) {
		m := new(dns.Msg)
		m.SetNotify(ingress.HealthName)
		response, _, err := (&dns.Client{Timeout: time.Second}).Exchange(m, f.listeners.Addr(dnsserver.ProtoUDP).String())
		if err != nil || response == nil || response.Rcode != dns.RcodeRefused || len(response.Answer) != 0 {
			t.Fatal("unsigned diagnostic was not refused without address data")
		}
	})
	for _, mode := range []string{"wrong-key", "unknown-key", "expired", "tcp", "malformed", "other-zone"} {
		t.Run(mode, func(t *testing.T) {
			m := doctorNotifyMessage()
			client := &dns.Client{Timeout: time.Second, TsigProvider: dnsserver.TSIGProvider(f.keys)}
			switch mode {
			case "wrong-key":
				wrong, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "updates", Algorithm: "hmac-sha256", Secret: bytes.Repeat([]byte{9}, 32)})
				if err != nil {
					t.Fatal(err)
				}
				client.TsigProvider = dnsserver.TSIGProvider(wrong)
			case "unknown-key":
				m.IsTsig().Hdr.Name = "unknown."
				client.TsigSecret = map[string]string{"unknown.": base64.StdEncoding.EncodeToString(secret)}
				client.TsigProvider = nil
			case "expired":
				m.IsTsig().TimeSigned -= 60
				m.IsTsig().Fudge = 1
			case "tcp":
				client.Net = "tcp"
			case "malformed":
				m.Question[0].Qtype = dns.TypeTXT
			case "other-zone":
				m.Question[0].Name = "other.test."
			}
			proto := dnsserver.ProtoUDP
			if mode == "tcp" {
				proto = dnsserver.ProtoTCP
			}
			response, _, err := client.Exchange(m, f.listeners.Addr(proto).String())
			if err == nil && response != nil && (response.Rcode == dns.RcodeSuccess || len(response.Answer) != 0) {
				t.Fatal("unauthorized or malformed diagnostic returned address data")
			}
		})
	}
}

func TestDoctorNotifyCannotAdoptNewPublicationKeysOrWriter(t *testing.T) {
	for _, mode := range []string{"publication", "keys", "writer", "not-ready"} {
		t.Run(mode, func(t *testing.T) {
			f := doctorNotifyFixture(t)
			f.pauseNotify.Store(true)
			done := make(chan *dns.Msg, 1)
			go func() {
				r, _, _ := (&dns.Client{Timeout: 3 * time.Second, TsigProvider: dnsserver.TSIGProvider(f.keys)}).Exchange(doctorNotifyMessage(), f.listeners.Addr(dnsserver.ProtoUDP).String())
				done <- r
			}()
			select {
			case <-f.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("native request did not capture its original lease")
			}
			switch mode {
			case "keys":
				keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "updates", Algorithm: "hmac-sha256", Secret: bytes.Repeat([]byte{11}, 32)})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.keyrings.Replace(t.Context(), keys); err != nil {
					t.Fatal(err)
				}
			case "writer":
				term := f.store.Authority().Term
				term.Epoch++
				term.PrimaryID = "doctor-successor"
				term.PromotionID = "doctor-successor"
				if _, err := f.store.ObserveAuthority(t.Context(), term); err != nil {
					t.Fatal(err)
				}
			default:
				if err := f.store.Command(t.Context(), f.store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					var z public.DNSZone
					if err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &z); err != nil {
						return err
					}
					z.Spec.Nameservers = []string{"changed.example."}
					_, err := tx.Apply(ctx, &z, store.ApplyOptions{Manager: store.ManagerAPI})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if mode == "not-ready" {
					f.wait(t, func() bool { return f.publisher.Status().ProtectedReady })
				}
			}
			f.resume <- struct{}{}
			r := <-done
			if r != nil && (r.Rcode == dns.RcodeSuccess || len(r.Answer) != 0) {
				t.Fatal("stale request borrowed successor publication, credentials or writer")
			}
		})
	}
}

func TestDoctorNotifyBoundedTimeoutSourceAndCancellation(t *testing.T) {
	f := doctorNotifyFixture(t)
	secret, _, _ := f.keys.Lookup("updates.")
	path := filepath.Join(t.TempDir(), "notify.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(secret)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	cfg.TSIGKeys = []config.TSIGKey{{Name: "updates", Algorithm: "hmac-sha256", SecretFile: path}}
	blackhole, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blackhole.Close() }()
	for _, mode := range []string{"timeout", "cancel", "source", "missing-key"} {
		t.Run(mode, func(t *testing.T) {
			var out strings.Builder
			rep := &doctorReport{say: func(format string, args ...any) { _, _ = fmt.Fprintf(&out, format, args...) }}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg.Notify.Source = ""
			target, key := blackhole.LocalAddr().String(), "updates"
			if mode == "cancel" {
				cancel()
			}
			if mode == "source" {
				cfg.Notify.Source = "192.0.2.123"
			}
			if mode == "missing-key" {
				key = "absent"
			}
			start := time.Now()
			checkSelfNotify(ctx, rep, cfg, target, key)
			if rep.failures != 1 || strings.Contains(out.String(), "notify: OK") || time.Since(start) > doctorNotifyTimeout+time.Second {
				t.Fatal("failed probe did not stay bounded and explicit")
			}
		})
	}
	if _, err := doctorNotifyAddress(&dns.Msg{}); err == nil {
		t.Fatal("unsigned empty response accepted")
	}
}

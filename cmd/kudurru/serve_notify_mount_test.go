package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func TestRunServeSQLiteManualNotifyRestartAndRearm(t *testing.T) {
	cfg, secret := mountedTestConfig(t)
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var refused, bad atomic.Bool
	var calls atomic.Int64
	var serial atomic.Uint32
	refused.Store(true)
	started := make(chan struct{})
	receiver := &dns.Server{PacketConn: packet, TsigSecret: map[string]string{"updates.": secret}, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		if m.Opcode != dns.OpcodeNotify || len(m.Question) != 1 || m.Question[0].Name != "notify.test." || m.IsTsig() == nil || w.TsigStatus() != nil || len(m.Answer) != 1 {
			bad.Store(true)
		} else if soa, ok := m.Answer[0].(*dns.SOA); !ok {
			bad.Store(true)
		} else {
			serial.Store(soa.Serial)
		}
		calls.Add(1)
		response := new(dns.Msg)
		response.SetReply(m)
		if refused.Load() {
			response.Rcode = dns.RcodeRefused
		}
		response.SetTsig("updates.", dns.HmacSHA256, 300, time.Now().Unix())
		if err := w.WriteMsg(response); err != nil {
			bad.Store(true)
		}
	})}
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.ActivateAndServe() }()
	<-started
	t.Cleanup(func() {
		if err := receiver.Shutdown(); err != nil {
			t.Error(err)
		}
		if err := <-receiverDone; err != nil {
			t.Error(err)
		}
	})
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "notify"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "notify.test.", Nameservers: []string{"ns.notify.test."}, Policy: public.ZonePolicy{Notify: public.NotifySpecified, NotifyTargets: []string{packet.LocalAddr().String()}, TransferTSIGKeyRefs: []string{"updates."}}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "notify-seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "notify"}, Endpoints: []public.Endpoint{{DNSName: "seed.notify.test.", RecordType: "A", Targets: []string{"192.0.2.74"}}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	var pending zk.ZoneState
	for restart := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runServe(ctx, cfg, "") }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("notify mount shutdown failed", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("notify mount shutdown did not join")
			}
		}
		t.Cleanup(stop)
		awaitMountedDNS(t, cfg, "seed.notify.test.", "192.0.2.74")
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &contractClient{t: t, base: "http://" + cfg.Listeners.API.Address + api.BasePath, http: &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: mountedRetryTransport{}}}
		var login api.SessionInfo
		client.call(http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		client.csrf = login.CSRF
		// NOTIFY itself uses a direct transport: never retry an admitted intent.
		client.http.Transport = http.DefaultTransport
		path := filepath.Join(cfg.Node.DataDir, sqlite.FileName)
		if restart == 0 {
			before := awaitMountedNotifyHead(t, path, func(head zk.ZoneState) bool { return head.Spec.NotifySequence == 0 })
			mountedManualNotify(t, client, packet.LocalAddr().String())
			pending = awaitMountedNotifyHead(t, path, func(head zk.ZoneState) bool { return head.Spec.NotifySequence != 0 && calls.Load() > 0 })
			if pending.Spec.CommitSequence != before.Spec.CommitSequence+1 || pending.Spec.NotifySequence != pending.Spec.CommitSequence || pending.Spec.NotifySerial != before.Spec.Serial || pending.Spec.RootDigest != before.Spec.RootDigest || pending.Spec.ContentHash != before.Spec.ContentHash || pending.Spec.Serial != before.Spec.Serial {
				t.Fatal("manual NOTIFY changed committed DNS data or failed to persist exact intent")
			}
			stop()
			refused.Store(false)
			continue
		}
		acked := awaitMountedNotifyHead(t, path, func(head zk.ZoneState) bool { return head.Spec.NotifySequence == 0 })
		if bad.Load() || serial.Load() != pending.Spec.Serial || acked.Spec.Serial != pending.Spec.Serial || acked.Spec.RootDigest != pending.Spec.RootDigest || acked.Spec.CommitSequence != pending.Spec.CommitSequence+1 {
			t.Fatal("restart did not deliver and acknowledge the original signed intent")
		}
		beforeCalls := calls.Load()
		mountedManualNotify(t, client, packet.LocalAddr().String())
		rearmed := awaitMountedNotifyHead(t, path, func(head zk.ZoneState) bool {
			return head.Spec.NotifySequence == 0 && head.Spec.CommitSequence >= acked.Spec.CommitSequence+2
		})
		if bad.Load() || calls.Load() <= beforeCalls || rearmed.Spec.CommitSequence != acked.Spec.CommitSequence+2 || rearmed.Spec.Serial != acked.Spec.Serial || rearmed.Spec.RootDigest != acked.Spec.RootDigest || rearmed.Spec.ContentHash != acked.Spec.ContentHash {
			t.Fatal("explicit NOTIFY after acknowledgment failed to rearm unchanged committed data")
		}
		stop()
	}
}

func awaitMountedNotifyHead(t *testing.T, path string, ready func(zk.ZoneState) bool) zk.ZoneState {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		// Each observation opens and closes a read-only connection. No extra
		// database handle survives the actual server's close/reopen boundary.
		dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		var raw string
		err = db.QueryRowContext(t.Context(), `SELECT data FROM objects WHERE kind='ZoneState' AND json_extract(data, '$.spec.zone.objectName')='notify'`).Scan(&raw)
		closeErr := db.Close()
		var head zk.ZoneState
		if err != nil || closeErr != nil || json.Unmarshal([]byte(raw), &head) != nil || head.Validate() != nil {
			t.Fatal("could not read the actual durable notification head", err, closeErr)
		}
		if ready(head) {
			return head
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable NOTIFY did not converge: sequence=%d pending=%d", head.Spec.CommitSequence, head.Spec.NotifySequence)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func mountedManualNotify(t *testing.T, client *contractClient, target string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, client.base+"/zones/notify/notify", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Kudurru-CSRF", client.csrf)
		response, err := client.http.Do(req)
		if err != nil {
			t.Fatal("manual NOTIFY transport failed")
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
		_ = response.Body.Close()
		if err != nil || len(body) > 4096 {
			t.Fatal("manual NOTIFY response exceeded its fixture bounds")
		}
		if response.StatusCode == http.StatusAccepted {
			var result api.NotifyResult
			if json.Unmarshal(body, &result) != nil || result.Zone != "notify" || len(result.Targets) != 1 || result.Targets[0] != target {
				t.Fatal("manual NOTIFY returned an incorrect durable acceptance result")
			}
			return
		}
		var failure struct {
			Error struct{ Key string } `json:"error"`
		}
		_ = json.Unmarshal(body, &failure)
		if response.StatusCode != http.StatusServiceUnavailable || failure.Error.Key != "generation_pending" || len(response.Header.Values("Set-Cookie")) != 0 || response.Header.Get("Location") != "" || time.Now().After(deadline) {
			t.Fatalf("manual NOTIFY refused: HTTP %d prehandler_pending=%t", response.StatusCode, failure.Error.Key == "generation_pending")
		}
		// This explicit middleware refusal precedes the handler and transaction.
		time.Sleep(100 * time.Millisecond)
	}
}

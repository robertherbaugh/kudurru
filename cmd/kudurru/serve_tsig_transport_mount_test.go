package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	stored "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

// Real runServe sockets use encrypted durable rows, not a fixture key lookup.
// Restart phases qualify replacement, previous-key decrypt and current-only
// reseal. No refused mutation is retried and diagnostics omit private values.
func TestRunServeSQLiteStoredTSIGTransportsAndEncryptionRestart(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	files, roots := tsigMountedCertificate(t)
	tsigMountedTLSListeners(t, cfg, files)
	sealer, err := tsigkeys.NewSealer(bytes.Repeat([]byte{8}, 32), nil)
	if err != nil {
		t.Fatal("prepare original envelope owner")
	}
	st, closeStore := tsigMountedStore(t, cfg)
	material, err := tsigkeys.NewMaterial(tsigkeys.Owner{ClusterID: st.Info().ClusterID, UID: "mounted-stored-tsig", KeyName: "stored.", Algorithm: dns.HmacSHA256}, bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal("prepare stored credential")
	}
	row := tsigMountedPersist(t, st, sealer, material, "")
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "signed"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "signed.test.", Nameservers: []string{"ns.signed.test."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "signed"}, Endpoints: []public.Endpoint{{DNSName: "seed.signed.test.", RecordType: "A", Targets: []string{"192.0.2.91"}}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal("seed actual mounted authority")
		}
	}
	closeStore()
	original := material.Key()
	for phase := range 4 {
		stop := tsigMountedStart(t, cfg)
		awaitMountedDNS(t, cfg, "seed.signed.test.", "192.0.2.91")
		clientKeys, err := dnsserver.NewStaticKeyring(material.Key())
		if err != nil {
			t.Fatal("prepare immutable client")
		}
		for _, protocol := range []string{"udp", "tcp", "dot", "doh", "doq", "doh3"} {
			t.Logf("signed restart exchange: phase=%d protocol=%s", phase, protocol)
			query := new(dns.Msg)
			query.SetQuestion("seed.signed.test.", dns.TypeA)
			query.Id = 0
			query.SetTsig("stored.", dns.HmacSHA256, 30, time.Now().Unix())
			wire, mac, err := dns.TsigGenerateWithProvider(query, dnsserver.TSIGProvider(clientKeys), "", false)
			if err != nil {
				t.Fatal("sign original request")
			}
			answerWire := tsigMountedExchange(t, cfg, roots, protocol, wire)
			answer := new(dns.Msg)
			if answer.Unpack(answerWire) != nil || answer.Id != 0 || answer.Rcode != dns.RcodeSuccess || !answer.Authoritative || len(answer.Answer) != 1 || answer.IsTsig() == nil {
				t.Fatalf("phase %d protocol %s lost signed authoritative response", phase, protocol)
			}
			if dns.TsigVerifyWithProvider(bytes.Clone(answerWire), dnsserver.TSIGProvider(clientKeys), mac, false) != nil {
				t.Fatalf("phase %d protocol %s lost original request MAC/key", phase, protocol)
			}
			if a, ok := answer.Answer[0].(*dns.A); !ok || a.A.String() != "192.0.2.91" {
				t.Fatal("signed response lost durable answer")
			}
		}
		if phase > 0 {
			oldKeys, err := dnsserver.NewStaticKeyring(original)
			if err != nil {
				t.Fatal("prepare revoked client")
			}
			query := new(dns.Msg)
			query.SetQuestion("seed.signed.test.", dns.TypeA)
			query.SetTsig("stored.", dns.HmacSHA256, 30, time.Now().Unix())
			wire, _, err := dns.TsigGenerateWithProvider(query, dnsserver.TSIGProvider(oldKeys), "", false)
			if err != nil {
				t.Fatal("sign original replaced credential request")
			}
			answer := new(dns.Msg)
			if answer.Unpack(tsigMountedExchange(t, cfg, roots, "tcp", wire)) != nil || answer.Rcode != dns.RcodeNotAuth || answer.IsTsig() == nil || answer.IsTsig().Error != dns.RcodeBadSig || answer.IsTsig().MAC != "" {
				t.Fatal("replaced credential gained successor response authority")
			}
		}
		stop()
		if phase == 3 {
			break
		}
		st, closeStore = tsigMountedStore(t, cfg)
		switch phase {
		case 0:
			owner, epoch := material.Binding()
			material, err = tsigkeys.NewMaterial(owner, bytes.Repeat([]byte{0x52}, 32))
			if err != nil {
				t.Fatal("prepare original replacement intent")
			}
			_, nextEpoch := material.Binding()
			if nextEpoch == epoch {
				t.Fatal("replacement retained an old credential epoch")
			}
			row = tsigMountedPersist(t, st, sealer, material, row.ResourceVersion)
		case 1:
			// Existing envelope must decrypt using the explicit previous key.
			t.Setenv(keys.EnvKey, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
			t.Setenv(keys.EnvPreviousKey, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
		case 2:
			sealer, err = tsigkeys.NewSealer(bytes.Repeat([]byte{9}, 32), bytes.Repeat([]byte{8}, 32))
			if err != nil {
				t.Fatal("prepare captured reseal owner")
			}
			_, epoch := material.Binding()
			row = tsigMountedPersist(t, st, sealer, material, row.ResourceVersion)
			opened, _, err := sealer.Open(row)
			if err != nil {
				t.Fatal("read actual resealed row")
			}
			_, restoredEpoch := opened.Binding()
			if restoredEpoch != epoch {
				t.Fatal("reseal replaced credential epoch")
			}
			t.Setenv(keys.EnvPreviousKey, "")
		}
		closeStore()
	}
}

func tsigMountedStore(t *testing.T, cfg *config.Config) (*sqlite.Store, func()) {
	t.Helper()
	v, err := validator.New()
	if err != nil {
		t.Fatal("prepare mounted schema")
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal("open actual mounted durable state")
	}
	sql, ok := st.(*sqlite.Store)
	if !ok {
		t.Fatal("mounted fixture did not use SQLite")
	}
	closeStore := func() {
		localErr, storeErr := local.Close(), st.Close()
		if localErr != nil || storeErr != nil {
			t.Fatal("close actual mounted durable state")
		}
	}
	t.Cleanup(func() { _ = local.Close(); _ = st.Close() })
	return sql, closeStore
}

func tsigMountedPersist(t *testing.T, st *sqlite.Store, sealer *tsigkeys.Sealer, material *tsigkeys.Material, rv string) *stored.TSIGKey {
	t.Helper()
	row, err := sealer.Seal(material)
	if err != nil {
		t.Fatal("seal mounted durable credential")
	}
	object, err := st.Apply(t.Context(), row, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: rv == "", ExpectedResourceVersion: rv})
	if err != nil {
		t.Fatal("persist actual encrypted credential")
	}
	return object.(*stored.TSIGKey)
}

func tsigMountedStart(t *testing.T, cfg *config.Config) func() {
	t.Helper()
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
				t.Fatal("mounted TSIG root did not shut down cleanly")
			}
		case <-time.After(15 * time.Second):
			t.Fatal("mounted TSIG root did not join original lifetime")
		}
	}
	t.Cleanup(stop)
	return stop
}

func tsigMountedCertificate(t *testing.T) (config.TLSFiles, *x509.CertPool) {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	pair := server.TLS.Certificates[0]
	private, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal("encode fixture TLS key")
	}
	dir := t.TempDir()
	files := config.TLSFiles{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")}
	if os.WriteFile(files.CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0o600) != nil || os.WriteFile(files.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0o600) != nil {
		t.Fatal("write private fixture TLS material")
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return files, roots
}

func tsigMountedExchange(t *testing.T, cfg *config.Config, roots *x509.CertPool, protocol string, wire []byte) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	tlsConfig := &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	if len(wire) > dns.MaxMsgSize {
		t.Fatal("fixture DNS message exceeds wire bound")
	}
	if protocol == "doh" || protocol == "doh3" {
		var client *http.Client
		var address string
		if protocol == "doh" {
			transport := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}
			defer transport.CloseIdleConnections()
			client, address = &http.Client{Transport: transport}, cfg.Listeners.DoH[0].Address
		} else {
			transport := &http3.Transport{TLSClientConfig: tlsConfig}
			defer func() { _ = transport.Close() }()
			client, address = &http.Client{Transport: transport}, cfg.Listeners.DoH3[0].Address
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+address+"/dns-query", bytes.NewReader(wire))
		if err != nil {
			t.Fatal("prepare actual encrypted HTTP request")
		}
		request.Header.Set("Content-Type", "application/dns-message")
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("actual encrypted HTTP exchange failed: protocol=%s err=%v context=%v", protocol, err, ctx.Err())
		}
		defer func() { _ = response.Body.Close() }()
		wantVersion := 2
		if protocol == "doh3" {
			wantVersion = 3
		}
		if response.StatusCode != http.StatusOK || response.ProtoMajor != wantVersion {
			t.Fatalf("actual HTTP version/status mismatch: protocol=%s status=%d version=%s", protocol, response.StatusCode, response.Proto)
		}
		out, err := io.ReadAll(io.LimitReader(response.Body, dns.MaxMsgSize+1))
		if err != nil || len(out) > dns.MaxMsgSize {
			t.Fatalf("read bounded encrypted HTTP response: protocol=%s bytes=%d err=%v context=%v", protocol, len(out), err, ctx.Err())
		}
		return out
	}
	if protocol == "doq" {
		tlsConfig.MinVersion = tls.VersionTLS13
		tlsConfig.NextProtos = []string{"doq"}
		connection, err := quic.DialAddr(ctx, cfg.Listeners.DoQ[0].Address, tlsConfig, nil)
		if err != nil {
			t.Fatal("open verified actual DoQ connection")
		}
		defer func() { _ = connection.CloseWithError(0, "") }()
		stream, err := connection.OpenStreamSync(ctx)
		if err != nil {
			t.Fatal("open bounded actual DoQ stream")
		}
		deadline, _ := ctx.Deadline()
		_ = stream.SetDeadline(deadline)
		frame := binary.BigEndian.AppendUint16(nil, uint16(len(wire))) //nolint:gosec // DNS wire length checked above.
		frame = append(frame, wire...)
		if n, err := stream.Write(frame); err != nil || n != len(frame) {
			t.Fatal("write original framed DoQ bytes")
		}
		if stream.Close() != nil {
			t.Fatal("finish original DoQ query")
		}
		out, err := io.ReadAll(io.LimitReader(stream, dns.MaxMsgSize+3))
		if err != nil || len(out) < 2 || int(binary.BigEndian.Uint16(out)) != len(out)-2 {
			t.Fatal("read bounded original DoQ response")
		}
		return out[2:]
	}
	address := cfg.Listeners.DNS[0].Address
	var connection net.Conn
	var err error
	if protocol == "dot" {
		address = cfg.Listeners.DoT[0].Address
		connection, err = (&tls.Dialer{Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		connection, err = (&net.Dialer{}).DialContext(ctx, protocol, address)
	}
	if err != nil {
		t.Fatal("open actual native DNS connection")
	}
	defer func() { _ = connection.Close() }()
	deadline, _ := ctx.Deadline()
	_ = connection.SetDeadline(deadline)
	if protocol == "udp" {
		if n, err := connection.Write(wire); err != nil || n != len(wire) {
			t.Fatal("write original UDP request")
		}
		out := make([]byte, dns.MaxMsgSize)
		n, err := connection.Read(out)
		if err != nil {
			t.Fatal("read bounded UDP response")
		}
		return out[:n]
	}
	frame := binary.BigEndian.AppendUint16(nil, uint16(len(wire))) //nolint:gosec // DNS wire length checked above.
	frame = append(frame, wire...)
	if n, err := connection.Write(frame); err != nil || n != len(frame) {
		t.Fatal("write original framed native DNS request")
	}
	var length [2]byte
	if _, err := io.ReadFull(connection, length[:]); err != nil {
		t.Fatal("read original native DNS frame length")
	}
	out := make([]byte, int(binary.BigEndian.Uint16(length[:])))
	if _, err := io.ReadFull(connection, out); err != nil {
		t.Fatal("read bounded native DNS frame")
	}
	return out
}

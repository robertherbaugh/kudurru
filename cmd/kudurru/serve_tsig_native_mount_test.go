package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

func TestRunServeSQLiteStoredTSIGNativeUpdateTransferNotify(t *testing.T) {
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg, _ := mountedTestConfig(t)
	cfg.TSIGKeys = nil
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.DNSSEC.Enabled = false
	st, closeStore := tsigMountedStore(t, cfg)
	material, err := tsigkeys.NewMaterial(tsigkeys.Owner{ClusterID: st.Info().ClusterID, UID: "native-stored-owner", KeyName: "stored.", Algorithm: dns.HmacSHA256}, bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatal("prepare original durable native credential")
	}
	sealer, err := tsigkeys.NewSealer(bytes.Repeat([]byte{8}, 32), nil)
	if err != nil {
		t.Fatal("prepare original native encryption")
	}
	tsigMountedPersist(t, st, sealer, material, "")
	keys, err := dnsserver.NewStaticKeyring(material.Key())
	if err != nil {
		t.Fatal("prepare original native peer")
	}
	target, notified := tsigMountedNotifyTarget(t, keys)
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "signed"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "signed.test.", Nameservers: []string{"ns.signed.test."}, Policy: public.ZonePolicy{
			ZoneTransfer: public.AccessAllow, TransferTSIGKeyRefs: []string{"stored."},
			Notify: public.NotifySpecified, NotifyTargets: []string{target}, NotifyTSIGKeyRef: "stored.",
			Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "stored.", Name: "*.signed.test.", Types: []string{"A"}}},
		}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "signed"}, Endpoints: []public.Endpoint{{DNSName: "seed.signed.test.", RecordType: "A", Targets: []string{"192.0.2.91"}}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal("seed actual signed primary policy")
		}
	}
	closeStore()
	stop := tsigMountedStart(t, cfg)
	awaitMountedDNS(t, cfg, "seed.signed.test.", "192.0.2.91")
	// Initial materialization includes the seed but creates no NOTIFY intent.
	// Observe source, complete serving and writer-worker readiness together;
	// none reserves authority for this fixture's later single data mutation.
	tsigMountedAwaitNativeReady(t, cfg, keys)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	before := tsigMountedNativeHead(ctx, t, cfg)
	cancel()
	if before.Spec.Lifecycle != zk.LifecycleActive || before.Spec.NotifySequence != 0 || before.Spec.NotifySerial != 0 || notified.acknowledged.Load() != 0 {
		t.Fatal("initialized native head did not retain its actual zero-intent baseline")
	}
	initial := notified.acknowledged.Load()

	query := new(dns.Msg)
	query.SetUpdate("signed.test.")
	query.Insert([]dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "dynamic.signed.test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP("192.0.2.94")}})
	query.SetTsig("stored.", dns.HmacSHA256, 30, time.Now().Unix())
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	client := &dns.Client{Net: "tcp", Timeout: 5 * time.Second, TsigProvider: dnsserver.TSIGProvider(keys)}
	answer, _, err := client.ExchangeContext(ctx, query, cfg.Listeners.DNS[0].Address)
	if err != nil || answer == nil || answer.Rcode != dns.RcodeSuccess || answer.IsTsig() == nil {
		// Diagnostics borrow only the original UPDATE's remaining budget. They
		// neither retry it nor manufacture current writer/publication authority.
		tsigMountedNativeUpdateDiagnostics(ctx, t, cfg, query, answer, err, before)
		cancel()
		t.Fatal("one original stored-key UPDATE failed final admission or response verification")
	}
	cancel()
	// Peer acknowledgment and exact durable clearing share this original
	// five-second observation; a successful UDP write alone is not a receipt.
	tsigMountedAwaitNativeNotify(t, cfg, notified, initial+1, before)
	awaitMountedDNS(t, cfg, "dynamic.signed.test.", "192.0.2.94")

	transfer := &secondary.TransferClient{Keyring: keys}
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	records, err := transfer.AXFR(ctx, &public.DNSZone{Spec: public.DNSZoneSpec{ZoneName: "signed.test.", Type: public.ZoneTypeSecondary, Secondary: &public.SecondarySpec{TransferProtocol: "tcp", TSIGKeyRef: "stored."}}}, cfg.Listeners.DNS[0].Address)
	cancel()
	if err != nil {
		t.Fatal("actual stored-key native AXFR lost original chained response verification")
	}
	seed, dynamic := false, false
	for _, record := range records {
		if address, ok := record.(*dns.A); ok {
			seed = seed || (address.Hdr.Name == "seed.signed.test." && address.A.String() == "192.0.2.91")
			dynamic = dynamic || (address.Hdr.Name == "dynamic.signed.test." && address.A.String() == "192.0.2.94")
		}
	}
	if !seed || !dynamic {
		t.Fatal("signed native AXFR omitted the accepted primary mutation")
	}
	stop()
	tsigMountedAssertPortsReusable(t, cfg)
}

// Only fixed categories, booleans and counters may enter setup diagnostics.
type tsigNativeSetupObservation struct {
	Attempts                                                                int
	SourceRead, SourceClose                                                 string
	Statuses, Notifications, Heads, Unavailable, Unaligned                  int
	SourceReady                                                             bool
	HealthError                                                             string
	HealthStatus                                                            int
	HealthDecoded, DNSRegistered, DNSReady, ServingRegistered, ServingReady bool
	ProbeAttempted                                                          bool
	ProbeError                                                              string
	ProbeResponse                                                           bool
	ProbeRcode, ProbeTSIGError                                              int
	ProbeSigned, ProbeVerified, ProbeIDMatches, ProbeValid, ProbeLoopback   bool
}

func tsigMountedAwaitNativeReady(t *testing.T, cfg *config.Config, keys *dnsserver.StaticKeyring) {
	t.Helper()
	// Replace the old source barrier's five-second budget; do not append a
	// separate health/probe budget. All requests remain read-only observations.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(cfg.Node.DataDir, sqlite.FileName), RawQuery: "mode=ro"}).String()
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	health := &http.Client{Timeout: time.Second, Transport: transport}
	probe := &dns.Client{Net: "udp", Timeout: doctorNotifyTimeout, TsigProvider: dnsserver.TSIGProvider(keys)}
	for attempts := 1; ; attempts++ {
		observed := tsigNativeSetupObservation{Attempts: attempts, HealthStatus: -1, ProbeRcode: -1, ProbeTSIGError: -1}
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("native setup source open unavailable: error_class=%s", tsigNativeErrorClass(err))
		}
		err = db.QueryRowContext(ctx, controlMountedSourceQuery).Scan(&observed.Statuses, &observed.Notifications, &observed.Heads, &observed.Unavailable, &observed.Unaligned)
		closeErr := db.Close()
		observed.SourceRead, observed.SourceClose = tsigNativeErrorClass(err), tsigNativeErrorClass(closeErr)
		if err != nil || closeErr != nil {
			t.Fatalf("native setup source read/close unavailable: observed=%+v", observed)
		}
		observed.SourceReady = observed.Statuses == 0 && observed.Notifications == 0 && observed.Heads == 1 && observed.Unavailable == 0 && observed.Unaligned == 0
		if observed.SourceReady && tsigMountedNativeSetupHealth(ctx, cfg, health, &observed) && tsigMountedNativeSetupProbe(ctx, cfg, probe, &observed) && ctx.Err() == nil {
			t.Logf("native setup read-only readiness observed: %+v", observed)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("native setup did not observe source/serving/writer readiness: context_class=%s observed=%+v", tsigNativeErrorClass(ctx.Err()), observed)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func tsigMountedNativeSetupHealth(ctx context.Context, cfg *config.Config, client *http.Client, observed *tsigNativeSetupObservation) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cfg.Listeners.Ops.Address+"/healthz", nil)
	if err != nil {
		observed.HealthError = tsigNativeErrorClass(err)
		return false
	}
	response, err := client.Do(request)
	if err != nil {
		observed.HealthError = tsigNativeErrorClass(err)
		return false
	}
	observed.HealthStatus = response.StatusCode
	data, readErr := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	closeErr := response.Body.Close()
	observed.HealthError = tsigNativeErrorClass(errors.Join(readErr, closeErr))
	var body struct {
		Components map[string]struct{ Ready bool }
	}
	observed.HealthDecoded = readErr == nil && closeErr == nil && len(data) <= 16<<10 && json.Unmarshal(data, &body) == nil
	dnsComponent, dnsRegistered := body.Components["dns"]
	servingComponent, servingRegistered := body.Components["serving"]
	observed.DNSRegistered, observed.DNSReady = dnsRegistered, dnsComponent.Ready
	observed.ServingRegistered, observed.ServingReady = servingRegistered, servingComponent.Ready
	// Status and component evaluations are successive observations, not a lease.
	return response.StatusCode == http.StatusOK && observed.HealthDecoded && dnsRegistered && dnsComponent.Ready && servingRegistered && servingComponent.Ready
}

func tsigMountedNativeSetupProbe(ctx context.Context, cfg *config.Config, client *dns.Client, observed *tsigNativeSetupObservation) bool {
	// The reserved health-name handler never schedules a refresh, sends an
	// outbound zone NOTIFY or mutates a primary. Each probe is an independent
	// observation with its own original MAC; no UPDATE exists to replay here.
	query := new(dns.Msg)
	query.SetNotify(ingress.HealthName)
	query.SetTsig("stored.", dns.HmacSHA256, 30, time.Now().Unix())
	observed.ProbeAttempted = true
	probeCtx, cancel := context.WithTimeout(ctx, doctorNotifyTimeout)
	defer cancel()
	answer, _, err := client.ExchangeContext(probeCtx, query, cfg.Listeners.DNS[0].Address)
	observed.ProbeError, observed.ProbeResponse = tsigNativeErrorClass(err), answer != nil
	if answer != nil {
		observed.ProbeRcode, observed.ProbeIDMatches = answer.Rcode, answer.Id == query.Id
		if signature := answer.IsTsig(); signature != nil {
			observed.ProbeSigned, observed.ProbeTSIGError = true, int(signature.Error)
			observed.ProbeVerified = err == nil
		}
	}
	if err != nil || !observed.ProbeVerified || !observed.ProbeIDMatches || observed.ProbeTSIGError != 0 {
		return false
	}
	address, validationErr := doctorNotifyAddress(answer)
	observed.ProbeValid, observed.ProbeLoopback = validationErr == nil, validationErr == nil && address.IsLoopback()
	return observed.ProbeValid && observed.ProbeLoopback
}

type tsigNativeNotifyPeer struct {
	received, verified, valid, acknowledged atomic.Int64
	headerRejected, invalid, writeFailures  atomic.Int64
	serial                                  atomic.Uint32
}

func tsigMountedNotifyTarget(t *testing.T, keys *dnsserver.StaticKeyring) (string, *tsigNativeNotifyPeer) {
	t.Helper()
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("bind actual native NOTIFY peer")
	}
	peer := new(tsigNativeNotifyPeer)
	started, done := make(chan struct{}), make(chan error, 1)
	server := &dns.Server{PacketConn: listener, TsigProvider: dnsserver.TSIGProvider(keys), NotifyStartedFunc: func() { close(started) }, MsgAcceptFunc: func(header dns.Header) dns.MsgAcceptAction {
		action := dns.DefaultMsgAcceptFunc(header)
		if action != dns.MsgAccept {
			peer.headerRejected.Add(1)
		}
		return action
	}, MsgInvalidFunc: func(_ []byte, _ error) { peer.invalid.Add(1) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, query *dns.Msg) {
		peer.received.Add(1)
		if query.Opcode != dns.OpcodeNotify || query.IsTsig() == nil || w.TsigStatus() != nil {
			t.Error("durable native NOTIFY lost captured stored-key authentication")
			return
		}
		peer.verified.Add(1)
		if len(query.Question) != 1 || query.Question[0].Name != "signed.test." || query.Question[0].Qtype != dns.TypeSOA {
			t.Error("durable native NOTIFY changed original zone")
			return
		}
		if len(query.Answer) != 1 {
			t.Error("durable native NOTIFY omitted its original SOA")
			return
		}
		soa, ok := query.Answer[0].(*dns.SOA)
		if !ok || soa.Hdr.Name != "signed.test." {
			t.Error("durable native NOTIFY changed its original SOA zone")
			return
		}
		peer.valid.Add(1)
		answer := new(dns.Msg)
		answer.SetReply(query)
		answer.Authoritative = true
		answer.SetTsig(query.IsTsig().Hdr.Name, query.IsTsig().Algorithm, 300, time.Now().Unix())
		if w.WriteMsg(answer) != nil {
			peer.writeFailures.Add(1)
			t.Error("write original signed native NOTIFY acknowledgment")
			return
		}
		peer.serial.Store(soa.Serial)
		peer.acknowledged.Add(1)
	})}
	go func() { done <- server.ActivateAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if server.ShutdownContext(ctx) != nil {
			t.Error("actual NOTIFY peer did not shut down")
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error("actual NOTIFY peer exited unsuccessfully")
			}
		case <-ctx.Done():
			t.Error("actual NOTIFY peer did not join")
		}
	})
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("actual NOTIFY peer did not start")
	}
	return listener.LocalAddr().String(), peer
}

// Each observation reads only the actual committed zone head, with the original
// observation context; no extra authoritative writer or private key read.
func tsigMountedNativeHead(ctx context.Context, t *testing.T, cfg *config.Config) zk.ZoneState {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(cfg.Node.DataDir, sqlite.FileName), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal("open bounded native head observation")
	}
	var count int
	var raw string
	err = db.QueryRowContext(ctx, `SELECT count(*), COALESCE(min(data),'') FROM objects WHERE kind='ZoneState' AND json_extract(data,'$.spec.zone.objectName')='signed'`).Scan(&count, &raw)
	closeErr := db.Close()
	var head zk.ZoneState
	if err != nil || closeErr != nil || count != 1 || json.Unmarshal([]byte(raw), &head) != nil || head.Validate() != nil || head.Spec.Zone.Apex != "signed.test." {
		t.Fatal("read or validate actual bounded native head")
	}
	return head
}

func tsigMountedAwaitNativeNotify(t *testing.T, cfg *config.Config, peer *tsigNativeNotifyPeer, want int64, before zk.ZoneState) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		head := tsigMountedNativeHead(ctx, t, cfg)
		// The one data commit arms intent; one later metadata CAS clears it.
		// Match the actual signed peer serial, original zone/policy, and changed
		// data so the initial zero intent cannot masquerade as an acknowledgment.
		if peer.acknowledged.Load() >= want &&
			head.Spec.Zone == before.Spec.Zone && head.Spec.Lifecycle == zk.LifecycleActive &&
			head.Spec.CommitSequence == before.Spec.CommitSequence+2 &&
			head.Spec.AuthEpoch == before.Spec.AuthEpoch && head.Spec.AuthDigest == before.Spec.AuthDigest &&
			head.Spec.RootDigest != before.Spec.RootDigest && head.Spec.ContentHash != before.Spec.ContentHash &&
			dynamicupdate.RFC1982Greater(head.Spec.Serial, before.Spec.Serial) && head.Spec.Serial == peer.serial.Load() &&
			head.Spec.NotifySequence == 0 && head.Spec.NotifySerial == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("native NOTIFY did not reach signed peer and exact durable acknowledgment: handled=%d verified=%d valid=%d written=%d header_rejected=%d invalid=%d write_failed=%d sequence=%d baseline=%d pending=%d notify_serial=%d head_serial=%d peer_serial=%d", peer.received.Load(), peer.verified.Load(), peer.valid.Load(), peer.acknowledged.Load(), peer.headerRejected.Load(), peer.invalid.Load(), peer.writeFailures.Load(), head.Spec.CommitSequence, before.Spec.CommitSequence, head.Spec.NotifySequence, head.Spec.NotifySerial, head.Spec.Serial, peer.serial.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// These are fixed diagnostic categories, never raw transport/TSIG/SQL errors.
func tsigNativeErrorClass(err error) string {
	var network net.Error
	var protocol *dns.Error
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.As(err, &network) && network.Timeout():
		return "timeout"
	case errors.Is(err, dns.ErrSig):
		return "tsig_signature"
	case errors.Is(err, dns.ErrSecret):
		return "tsig_key"
	case errors.Is(err, dns.ErrKeyAlg):
		return "tsig_algorithm"
	case errors.Is(err, dns.ErrTime):
		return "tsig_time"
	case errors.Is(err, dns.ErrNoSig):
		return "tsig_missing"
	case errors.Is(err, dns.ErrAuth):
		return "tsig_auth"
	case errors.Is(err, dns.ErrId):
		return "dns_id"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "short_read"
	case errors.As(err, &network):
		return "network"
	case errors.As(err, &protocol):
		return "dns_protocol"
	default:
		return "other"
	}
}

func tsigMountedNativeUpdateDiagnostics(ctx context.Context, t *testing.T, cfg *config.Config, query, answer *dns.Msg, exchangeErr error, before zk.ZoneState) {
	t.Helper()
	rcode, tsigError := -1, -1
	signed, verified, idMatches := false, false, false
	if answer != nil {
		rcode, idMatches = answer.Rcode, answer.Id == query.Id
		if signature := answer.IsTsig(); signature != nil {
			signed, tsigError = true, int(signature.Error)
			// This original Client verifies the original reply bytes/request MAC
			// with its fixed provider. ErrId is set only after that read succeeds.
			verified = exchangeErr == nil || errors.Is(exchangeErr, dns.ErrId)
		}
	}
	operation := "none"
	var network *net.OpError
	if errors.As(exchangeErr, &network) {
		switch network.Op {
		case "dial", "read", "write":
			operation = network.Op
		default:
			operation = "other"
		}
	}
	t.Logf("native UPDATE original exchange: error_class=%s operation=%s context_class=%s response=%t rcode=%d tsig=%t tsig_error=%d tsig_verified=%t id_matches=%t baseline_sequence=%d baseline_serial=%d baseline_pending=%d", tsigNativeErrorClass(exchangeErr), operation, tsigNativeErrorClass(ctx.Err()), answer != nil, rcode, signed, tsigError, verified, idMatches, before.Spec.CommitSequence, before.Spec.Serial, before.Spec.NotifySequence)
	tsigMountedNativeFailureSource(ctx, t, cfg, before)
	// This later public health observation is not the failed request's lease.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cfg.Listeners.Ops.Address+"/healthz", nil)
	if err != nil {
		t.Logf("native UPDATE later health unavailable: error_class=%s", tsigNativeErrorClass(err))
		return
	}
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Logf("native UPDATE later health unavailable: error_class=%s", tsigNativeErrorClass(err))
		return
	}
	var body struct {
		Components map[string]struct{ Ready bool }
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	closeErr := response.Body.Close()
	decoded := readErr == nil && closeErr == nil && json.Unmarshal(data, &body) == nil
	dnsComponent, dnsRegistered := body.Components["dns"]
	servingComponent, servingRegistered := body.Components["serving"]
	t.Logf("native UPDATE later public health: status=%d decoded=%t dns_registered=%t dns_ready=%t serving_registered=%t serving_ready=%t", response.StatusCode, decoded, dnsRegistered, dnsComponent.Ready, servingRegistered, servingComponent.Ready)
}

// A read-only snapshot observes persisted state after refusal; it supplies no
// writer token, publication lease or original request admission capability.
func tsigMountedNativeFailureSource(ctx context.Context, t *testing.T, cfg *config.Config, before zk.ZoneState) {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(cfg.Node.DataDir, sqlite.FileName), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Logf("native UPDATE later source open unavailable: error_class=%s", tsigNativeErrorClass(err))
		return
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Logf("native UPDATE later source snapshot unavailable: error_class=%s", tsigNativeErrorClass(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT data FROM objects WHERE kind='ZoneState' AND namespace='' AND name=?`, before.Name).Scan(&raw)
	var head zk.ZoneState
	valid := err == nil && json.Unmarshal([]byte(raw), &head) == nil && head.Validate() == nil
	t.Logf("native UPDATE later original head: read_class=%s valid=%t same_zone=%t same_root=%t same_policy=%t sequence=%d serial=%d pending=%d notify_serial=%d", tsigNativeErrorClass(err), valid, valid && head.Spec.Zone == before.Spec.Zone, valid && head.Spec.RootDigest == before.Spec.RootDigest, valid && head.Spec.AuthEpoch == before.Spec.AuthEpoch && head.Spec.AuthDigest == before.Spec.AuthDigest, head.Spec.CommitSequence, head.Spec.Serial, head.Spec.NotifySequence, head.Spec.NotifySerial)
	var pending, notify, heads, unavailable, unaligned int
	err = tx.QueryRowContext(ctx, controlMountedSourceQuery).Scan(&pending, &notify, &heads, &unavailable, &unaligned)
	t.Logf("native UPDATE later source counters: read_class=%s statuses=%d notifications=%d heads=%d unavailable=%d unaligned=%d", tsigNativeErrorClass(err), pending, notify, heads, unavailable, unaligned)
	// Read only the existing durable authority row, never open another Store
	// or derive a fresh WriterToken. Durable Writable is not a live writer proof.
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='replication_authority'`).Scan(&raw)
	var authority replication.Authority
	decoded := err == nil && json.Unmarshal([]byte(raw), &authority) == nil
	t.Logf("native UPDATE later durable authority: read_class=%s decoded=%t term_valid=%t same_cluster=%t standalone=%t primary=%t writable=%t epoch=%d incarnation=%d", tsigNativeErrorClass(err), decoded, decoded && authority.Term.Validate() == nil, decoded && authority.Term.ClusterID == before.Spec.Zone.ClusterID, decoded && authority.Mode == replication.Standalone, decoded && authority.Mode == replication.Primary, decoded && authority.Writable(), authority.Term.Epoch, authority.Incarnation)
}

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func TestRunServeSQLiteStoredTSIGBackgroundTCP(t *testing.T) {
	runTSIGMountedBackground(t, "tcp", config.TLSFiles{}, true)
}

// Isolate system-root initialization in the existing test executable. Only this
// child trusts the temporary loopback CA. Production still uses system roots;
// no host trust files, transfer limits or production injection hooks change.
func TestRunServeSQLiteStoredTSIGBackgroundTLS(t *testing.T) {
	const childEnv = "KUDURRU_TEST_TSIG_BACKGROUND_TLS"
	if os.Getenv(childEnv) == "1" {
		runTSIGMountedBackground(t, "tls", config.TLSFiles{CertFile: os.Getenv("SSL_CERT_FILE"), KeyFile: os.Getenv("KUDURRU_TEST_TSIG_TLS_KEY_FILE")}, false)
		return
	}
	files, _ := tsigMountedCertificate(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("locate original compiled test executable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestRunServeSQLiteStoredTSIGBackgroundTLS$", "-test.count=1", "-test.timeout=80s")
	command.Env = append(os.Environ(), childEnv+"=1", "SSL_CERT_FILE="+files.CertFile, "KUDURRU_TEST_TSIG_TLS_KEY_FILE="+files.KeyFile)
	// Keep child diagnostics private even if startup or signature checking fails.
	outputDir := os.Getenv("KUDURRU_TEST_TSIG_ARTIFACT_DIR")
	if outputDir == "" {
		outputDir = t.TempDir()
	}
	output, err := os.CreateTemp(outputDir, "tsig-tls-child-*.log")
	if err != nil {
		t.Fatal("prepare private TLS child output")
	}
	defer func() { _ = output.Close() }()
	command.Stdout, command.Stderr = output, output
	if err := command.Run(); err != nil {
		t.Fatal("isolated actual TLS background acquisition failed; child fixture output is private at " + output.Name())
	}
}

func runTSIGMountedBackground(t *testing.T, protocol string, files config.TLSFiles, lifecycle bool) {
	t.Helper()
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg, _ := mountedTestConfig(t)
	cfg.TSIGKeys = nil // A file-only or static provider cannot satisfy this test.
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.DNSSEC.Enabled = false
	cfg.Blocking.Enabled = true
	cfg.Blocking.DefaultResponse = "nxdomain"
	st, closeStore := tsigMountedStore(t, cfg)
	material, err := tsigkeys.NewMaterial(tsigkeys.Owner{ClusterID: st.Info().ClusterID, UID: "mounted-acquisition-owner", KeyName: "stored.", Algorithm: dns.HmacSHA256}, bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal("prepare original acquisition credential")
	}
	sealer, err := tsigkeys.NewSealer(bytes.Repeat([]byte{8}, 32), nil)
	if err != nil {
		t.Fatal("prepare original acquisition encryption")
	}
	row := tsigMountedPersist(t, st, sealer, material, "")
	primary := tsigMountedPrimary(t, material, protocol, files)
	var secondary *public.DNSZone
	var list *public.BlockList
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "acquired"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypeSecondary, ZoneName: "acquired.test.", Secondary: &public.SecondarySpec{Primaries: []string{primary.address}, TransferProtocol: protocol, TSIGKeyRef: "stored."}}},
		&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "acquired", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Name: "native", Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.test.", Primaries: []string{primary.address}, TransferProtocol: protocol, TSIGKeyRef: "stored."}}}}},
		&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: public.PolicySpec{BlockListRefs: []string{"acquired"}}},
	} {
		value, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI})
		if err != nil {
			t.Fatal("seed actual mounted acquisition configuration")
		}
		switch value := value.(type) {
		case *public.DNSZone:
			secondary = value
		case *public.BlockList:
			list = value
		}
	}
	closeStore()
	stop := tsigMountedStart(t, cfg)
	awaitMountedDNS(t, cfg, "host.acquired.test.", "192.0.2.92")
	tsigMountedAwaitRcode(t, cfg, "blocked.test.", dns.RcodeNameError, true)
	if primary.soa.Load() == 0 || primary.secondary.Load() == 0 || primary.policy.Load() == 0 {
		t.Fatal("mounted factories did not authenticate actual upstream SOA and both transfers")
	}
	if !lifecycle {
		stop()
		return
	}

	// No later response can confirm data or extend its captured expiry.
	primary.available.Store(false)
	stop()
	original := tsigMountedArtifacts(t, cfg, secondary, list)
	st, closeStore = tsigMountedStore(t, cfg)
	row = tsigMountedPersist(t, st, sealer, material, row.ResourceVersion)
	closeStore()
	stop = tsigMountedStart(t, cfg)
	awaitMountedDNS(t, cfg, "host.acquired.test.", "192.0.2.92")
	tsigMountedAwaitRcode(t, cfg, "blocked.test.", dns.RcodeNameError, true)
	stop()
	if restored := tsigMountedArtifacts(t, cfg, secondary, list); restored != original {
		t.Fatal("offline reseal/restart changed acquired owner or original confirmation/expiry")
	}

	// Same bytes with a new durable epoch must exclude BOTH old cache entries.
	// No source is changed behind a running writer or by a raw-SQL bypass.
	st, closeStore = tsigMountedStore(t, cfg)
	owner, oldEpoch := material.Binding()
	material, err = tsigkeys.NewMaterial(owner, bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal("prepare independent same-byte credential replacement")
	}
	_, nextEpoch := material.Binding()
	if oldEpoch == nextEpoch {
		t.Fatal("same-byte replacement reused old credential epoch")
	}
	tsigMountedPersist(t, st, sealer, material, row.ResourceVersion)
	closeStore()
	stop = tsigMountedStart(t, cfg)
	tsigMountedAwaitRcode(t, cfg, "host.acquired.test.", dns.RcodeServerFailure, true)
	tsigMountedAwaitRcode(t, cfg, "blocked.test.", dns.RcodeServerFailure, true)
	// A separately scheduled new job may now use its own original capture.
	primary.available.Store(true)
	awaitMountedDNS(t, cfg, "host.acquired.test.", "192.0.2.92")
	tsigMountedAwaitRcode(t, cfg, "blocked.test.", dns.RcodeNameError, true)
	primary.available.Store(false)
	stop()
	fresh := tsigMountedArtifacts(t, cfg, secondary, list)
	for i := range fresh {
		if fresh[i].Credential == original[i].Credential || !fresh[i].ConfirmedAt.After(original[i].ConfirmedAt) || !fresh[i].ExpiresAt.After(fresh[i].ConfirmedAt) {
			t.Fatal("fresh acquisition adopted an old credential owner or lost its absolute expiry")
		}
	}

	stop = tsigMountedStart(t, cfg)
	awaitMountedDNS(t, cfg, "host.acquired.test.", "192.0.2.92")
	tsigMountedAwaitRcode(t, cfg, "blocked.test.", dns.RcodeNameError, true)
	// Observation waits for the original wall-clock deadline. It never changes
	// a production clock, retries a mutation, or refreshes either source.
	deadline := fresh[0].ExpiresAt
	if fresh[1].ExpiresAt.After(deadline) {
		deadline = fresh[1].ExpiresAt
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal("original mounted expiry observation canceled")
	}
	tsigMountedAwaitRcode(t, cfg, "host.acquired.test.", dns.RcodeServerFailure, true)
	tsigMountedAwaitRcode(t, cfg, "blocked.test.", dns.RcodeServerFailure, true)
	stop()
	if expired := tsigMountedArtifacts(t, cfg, secondary, list); expired != fresh {
		t.Fatal("offline expiration/restart changed original acquired metadata")
	}
}

type tsigMountedUpstream struct {
	address                string
	available              atomic.Bool
	soa, secondary, policy atomic.Int64
}

func tsigMountedPrimary(t *testing.T, material *tsigkeys.Material, protocol string, files config.TLSFiles) *tsigMountedUpstream {
	t.Helper()
	keys, err := dnsserver.NewStaticKeyring(material.Key())
	if err != nil {
		t.Fatal("prepare immutable original upstream provider")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("bind actual acquisition primary")
	}
	if protocol == "tls" {
		certificate, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
		if err != nil {
			_ = listener.Close()
			t.Fatal("load private upstream TLS fixture")
		}
		listener = tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}, NextProtos: []string{"dot"}})
	}
	upstream := &tsigMountedUpstream{address: listener.Addr().String()}
	upstream.available.Store(true)
	started, done := make(chan struct{}), make(chan error, 1)
	server := &dns.Server{Listener: listener, TsigProvider: dnsserver.TSIGProvider(keys), NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, query *dns.Msg) {
		defer func() { _ = w.Close() }()
		if query.IsTsig() == nil || w.TsigStatus() != nil {
			t.Error("actual acquisition request lacked original valid TSIG")
			return
		}
		answer := new(dns.Msg)
		answer.SetReply(query)
		answer.Authoritative = true
		if !upstream.available.Load() {
			answer.Rcode = dns.RcodeServerFailure
		} else if len(query.Question) != 1 {
			t.Error("actual acquisition sent malformed question")
			return
		} else {
			origin := query.Question[0].Name
			if origin != "acquired.test." && origin != "policy.test." {
				t.Error("actual acquisition selected unexpected origin")
				return
			}
			soa := &dns.SOA{Hdr: dns.RR_Header{Name: origin, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 30}, Ns: "ns." + origin, Mbox: "hostmaster." + origin, Serial: 1, Refresh: 20, Retry: 1, Expire: 60, Minttl: 5}
			answer.Answer = []dns.RR{soa}
			switch query.Question[0].Qtype {
			case dns.TypeSOA:
				upstream.soa.Add(1)
			case dns.TypeAXFR, dns.TypeIXFR:
				answer.Answer = append(answer.Answer, &dns.NS{Hdr: dns.RR_Header{Name: origin, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 30}, Ns: "ns." + origin})
				if origin == "acquired.test." {
					upstream.secondary.Add(1)
					answer.Answer = append(answer.Answer, &dns.A{Hdr: dns.RR_Header{Name: "host." + origin, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP("192.0.2.92")})
				} else {
					upstream.policy.Add(1)
					answer.Answer = append(answer.Answer, &dns.CNAME{Hdr: dns.RR_Header{Name: "blocked.test." + origin, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 30}, Target: "."})
				}
				answer.Answer = append(answer.Answer, dns.Copy(soa))
			default:
				t.Error("actual acquisition used unexpected operation")
				return
			}
		}
		answer.SetTsig(query.IsTsig().Hdr.Name, query.IsTsig().Algorithm, 300, time.Now().Unix())
		if w.WriteMsg(answer) != nil {
			t.Error("write bounded signed acquisition response")
		}
	})}
	go func() { done <- server.ActivateAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if server.ShutdownContext(ctx) != nil {
			t.Error("actual acquisition primary did not shut down")
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error("actual acquisition primary exited unsuccessfully")
			}
		case <-ctx.Done():
			t.Error("actual acquisition primary did not join")
		}
	})
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("actual acquisition primary did not start")
	}
	return upstream
}

func tsigMountedAwaitRcode(t *testing.T, cfg *config.Config, name string, want int, ready bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	client := &dns.Client{Net: "tcp", Timeout: 200 * time.Millisecond}
	for {
		query := new(dns.Msg)
		query.SetQuestion(name, dns.TypeA)
		answer, _, err := client.ExchangeContext(ctx, query, cfg.Listeners.DNS[0].Address)
		if err == nil && answer != nil && answer.Rcode == want && len(answer.Answer) == 0 && (!ready || offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == 200) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("mounted captured acquisition never published expected rcode %d", want)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type tsigMountedArtifact struct {
	Credential             string
	ConfirmedAt, ExpiresAt time.Time
}

// Read accepted envelopes only after cancellation/join. Never log their bytes.
func tsigMountedArtifacts(t *testing.T, cfg *config.Config, secondary *public.DNSZone, list *public.BlockList) [2]tsigMountedArtifact {
	t.Helper()
	local, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(cfg.Node.DataDir, localdb.FileName))
	if err != nil {
		t.Fatal("open joined acquisition cache")
	}
	defer func() {
		if local.Close() != nil {
			t.Error("close joined acquisition cache")
		}
	}()
	var result [2]tsigMountedArtifact
	blob, err := local.GetBlob(t.Context(), "secondary/"+zone.SecondaryIdentity(secondary))
	if err != nil || json.Unmarshal(blob.Data, &result[0]) != nil {
		t.Fatal("read original accepted secondary metadata")
	}
	id := rpz.SourceIdentity(list, 0)
	markerBlob, err := local.GetBlob(t.Context(), "rpz/"+id+"/accepted")
	var marker struct{ Slot uint8 }
	if err != nil || json.Unmarshal(markerBlob.Data, &marker) != nil || marker.Slot > 1 {
		t.Fatal("read original accepted RPZ marker")
	}
	candidate := "candidate-0"
	if marker.Slot == 1 {
		candidate = "candidate-1"
	}
	blob, err = local.GetBlob(t.Context(), "rpz/"+id+"/"+candidate)
	if err != nil || json.Unmarshal(blob.Data, &result[1]) != nil {
		t.Fatal("read original accepted RPZ metadata")
	}
	for _, data := range result {
		if data.Credential == "" || data.ConfirmedAt.IsZero() || !data.ExpiresAt.After(data.ConfirmedAt) {
			t.Fatal("accepted metadata lost original credential/time ownership")
		}
	}
	return result
}

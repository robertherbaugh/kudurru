package main

import (
	"bytes"
	"context"
	"crypto"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
)

func offlineAnchorFixture(t *testing.T) (string, accepted.Snapshot) {
	t.Helper()
	path := filepath.Join(t.TempDir(), sqlite.FileName)
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	writer, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx = sqlite.WithWriterToken(ctx, writer)
	if err := s.Command(ctx, writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		return runtimeowners.Initialize(ctx, tx, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindAnchorLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	load := func() accepted.Snapshot {
		t.Helper()
		image, err := s.ReadGeneration(ctx)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		return owner
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ED25519}
	private, err := key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	seed := key.ToDS(dns.SHA256).String()
	policy, err := (config.Recursion{AnchorMode: config.RootAnchorsManaged, DNSSEC: true, TrustAnchors: []string{seed}}).RootAnchorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	for _, revoke := range []bool{false, true} {
		owner := load()
		update, err := s.CaptureAnchorUpdate(ctx, owner.Version())
		if err != nil {
			t.Fatal(err)
		}
		original, err := persistence.CaptureTransition(ctx, owner.Version(), clock)
		if err != nil {
			t.Fatal(err)
		}
		if revoke {
			key.Flags |= dns.REVOKE
		}
		inception, expiration := time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix()
		if inception < 0 || inception > math.MaxUint32 || expiration < 0 || expiration > math.MaxUint32 {
			t.Fatal("fixture clock outside DNS signature range")
			return "", accepted.Snapshot{}
		}
		sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 3600}, Algorithm: key.Algorithm, KeyTag: key.KeyTag(), SignerName: ".", Inception: uint32(inception), Expiration: uint32(expiration)}
		if err := sig.Sign(private.(crypto.Signer), []dns.RR{key}); err != nil {
			t.Fatal(err)
		}
		message := new(dns.Msg)
		message.SetQuestion(".", dns.TypeDNSKEY)
		message.Response = true
		message.Authoritative = true
		message.Answer = []dns.RR{dns.Copy(key), sig}
		var state trustanchors.State
		var proof trustanchors.Observation
		if revoke {
			state, err = owner.State()
			if err == nil {
				proof, err = trustanchors.Verify(state, message, clock.Sample())
			}
		} else {
			state, proof, err = trustanchors.Bootstrap(trustanchors.Seed{ClusterID: owner.Inspect().ClusterID, PolicyID: policy.PolicyID, DS: policy.Seeds}, message, clock.Sample())
		}
		if err != nil {
			t.Fatal(err)
		}
		completion, err := s.AnchorCommand(update, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if revoke {
				_, err := persistence.Advance(ctx, tx, original, proof)
				return err
			}
			_, err := persistence.Bootstrap(ctx, tx, original, state, proof)
			return err
		})
		if err != nil || completion.Outcome() != sqlite.AnchorCommitted {
			t.Fatal("fixture missed actual canonical loss", err)
		}
	}
	lost := load()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path, lost
}

func TestOfflineAnchorCommandRecoveryAuditAndRestart(t *testing.T) {
	path, before := offlineAnchorFixture(t)
	seed, reply := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", reply)
	root := newRoot()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetContext(t.Context())
	root.SetArgs([]string{"anchors", "recover", "--database", path, "--offline", "--expected-digest", before.Version().Digest, "--reason", "verified replacement root ceremony", "--ds", seed.DS[0], "--root-server", endpoint.String()})
	if err := root.Execute(); err != nil {
		t.Fatal(err, output.String())
	}
	var result anchorOfflineResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "committed" || result.Quarantined || result.Recoveries != 1 || result.Version.Generation != before.Version().Generation+1 || reads.Load() != 1 {
		t.Fatal("command omitted actual recovery disposition", result, reads.Load())
	}
	opts := anchorOfflineOptions{database: path, offline: true, timeout: 10 * time.Second}
	status, err := runAnchorOffline(t.Context(), opts, false)
	if err != nil || status.Version != result.Version || status.Quarantined || status.Recoveries != 1 {
		t.Fatal("physical reopen lost recovery", status, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s, err := sqlite.OpenExisting(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	image, err := s.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := owner.State()
	if err != nil {
		t.Fatal(err)
	}
	old, err := before.State()
	if err != nil {
		t.Fatal(err)
	}
	for _, prior := range old.Inspect().Keys {
		found := false
		for _, key := range after.Inspect().Keys {
			if reflect.DeepEqual(prior, key) {
				found = true
			}
		}
		if !found {
			t.Fatal("operator recovery changed permanent revoked key")
		}
	}
	entries, err := s.ReadAudit(ctx, sqlite.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.Action != "trustanchors.recover" {
			continue
		}
		count++
		if entry.Authz != "local-os" || entry.Actor != fmt.Sprintf("os:uid:%d", os.Geteuid()) || entry.Reason != "verified replacement root ceremony" || entry.Result != sqlite.AuditSuccess {
			t.Fatal("recovery audit lost original OS attribution")
		}
		var prior, next accepted.Version
		if json.Unmarshal(entry.Before, &prior) != nil || json.Unmarshal(entry.After, &next) != nil || prior != before.Version() || next != result.Version {
			t.Fatal("audit does not bind exact recovery versions")
		}
	}
	if count != 1 {
		t.Fatal("missing or duplicate committed recovery audit", count)
	}
	if err := s.VerifyAudit(ctx); err != nil {
		t.Fatal("recovery audit broke the authenticated chain", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opts.expected = before.Version().Digest
	opts.reason = "stale command"
	opts.ds = seed.DS
	opts.servers = []string{endpoint.String()}
	refused, err := runAnchorOffline(t.Context(), opts, true)
	if err == nil || refused.Outcome != "not-attempted" || reads.Load() != 1 {
		t.Fatal("old recovery replayed acquisition", refused, err)
	}
}

func TestOfflineAnchorRecoveryRefusalRollsBackTrustAndAudit(t *testing.T) {
	for _, mode := range []string{"unauthenticated", "audit-failure", "canceled-receipt", "wrong-version"} {
		t.Run(mode, func(t *testing.T) {
			path, before := offlineAnchorFixture(t)
			seed, reply := anchorWireSignedReply(t)
			if mode == "audit-failure" {
				db, err := sql.Open("sqlite", "file:"+path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.ExecContext(t.Context(), `CREATE TRIGGER recovery_audit_refusal BEFORE INSERT ON audit WHEN NEW.action='trustanchors.recover' BEGIN SELECT RAISE(ABORT, 'test recovery audit refusal'); END`)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			endpoint, reads := anchorWireServer(t, "tcp", func(raw []byte) []byte {
				data := reply(raw)
				if mode == "unauthenticated" {
					m := new(dns.Msg)
					if err := m.Unpack(data); err != nil {
						t.Error(err)
						return nil
					}
					m.Answer = m.Answer[:1]
					data, _ = m.Pack()
				}
				if mode == "canceled-receipt" {
					cancel()
				}
				return data
			})
			opts := anchorOfflineOptions{database: path, offline: true, timeout: 10 * time.Second, expected: before.Version().Digest, reason: "refusal fixture", ds: seed.DS, servers: []string{endpoint.String()}}
			if mode == "wrong-version" {
				opts.expected = strings.Repeat("0", 64)
			}
			result, err := runAnchorOffline(ctx, opts, true)
			if err == nil {
				t.Fatal("invalid recovery accepted")
			}
			if mode == "audit-failure" && (result == nil || result.Outcome != "rolled-back") {
				t.Fatal("actual audit refusal lost rollback disposition", result, err)
			}
			if mode == "wrong-version" && reads.Load() != 0 {
				t.Fatal("wrong version acquired replacement input")
			}
			status, err := runAnchorOffline(t.Context(), opts, false)
			if err != nil || status.Version != before.Version() || !status.Quarantined || status.Recoveries != 0 {
				t.Fatal("refused command changed original trust", status, err)
			}
			auditCtx, stopAudit := context.WithTimeout(t.Context(), 10*time.Second)
			defer stopAudit()
			s, err := sqlite.OpenExisting(auditCtx, path, sqlite.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			entries, err := s.ReadAudit(auditCtx, sqlite.AuditQuery{Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Action == "trustanchors.recover" {
					t.Fatal("refused recovery committed success audit")
				}
			}
		})
	}
}

func TestOfflineAnchorCommandRequiresExplicitStoppedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), sqlite.FileName)
	for _, opts := range []anchorOfflineOptions{{database: path, timeout: time.Second}, {database: path, offline: true, timeout: time.Minute}, {database: path, offline: true, timeout: time.Second}} {
		if _, err := runAnchorOffline(t.Context(), opts, true); err == nil {
			t.Fatal("missing recovery admission accepted")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refused command created database", err)
		}
	}
}

func TestOfflineAnchorRecoveredTrustServesAfterRestart(t *testing.T) {
	path, lost := offlineAnchorFixture(t)
	endpoint, replacement, tcpKeys, udpKeys := anchorRecursiveServer(t)
	result, err := runAnchorOffline(t.Context(), anchorOfflineOptions{database: path, offline: true, timeout: 10 * time.Second, expected: lost.Version().Digest, reason: "independently verified replacement", ds: replacement, servers: []string{endpoint}}, true)
	if err != nil || result.Outcome != "committed" {
		t.Fatal("actual root acquisition failed recovery", result, err)
	}
	oldState, err := lost.State()
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := mountedTestConfig(t)
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg.Node.DataDir = filepath.Dir(path)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Mode: "recursive", Strategy: "parallel_best"}}
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	cfg.Upstreams.Recursion.DNSSEC = true
	cfg.Upstreams.Recursion.TrustAnchors = oldState.Inspect().Seeds // Original configured policy, never replacement/reseed.
	cfg.Upstreams.Recursion.RootHints = []string{endpoint}
	cfg.Upstreams.Recursion.IPv6 = "off"
	files, roots := tsigMountedCertificate(t)
	tsigMountedTLSListeners(t, cfg, files)
	for range 2 {
		stop := tsigMountedStart(t, cfg)
		deadline := time.NewTimer(10 * time.Second)
		for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != 200 {
			select {
			case <-deadline.C:
				t.Fatal("recovered runtime did not publish complete trust")
			case <-time.After(20 * time.Millisecond):
			}
		}
		deadline.Stop()
		for _, protocol := range []string{"udp", "tcp", "dot", "doh", "doq", "doh3"} {
			q := new(dns.Msg)
			q.SetQuestion(".", dns.TypeDNSKEY)
			q.SetEdns0(1232, true)
			q.Id = 0
			raw, err := q.Pack()
			if err != nil {
				t.Fatal(err)
			}
			answer := new(dns.Msg)
			if err := answer.Unpack(tsigMountedExchange(t, cfg, roots, protocol, raw)); err != nil {
				t.Fatal(err)
			}
			if answer.Rcode != dns.RcodeSuccess || !answer.AuthenticatedData || len(answer.Answer) != 2 {
				t.Fatal("recovered native validation/output refused", protocol, answer)
			}
			var key *dns.DNSKEY
			var signature *dns.RRSIG
			for _, rr := range answer.Answer {
				switch value := rr.(type) {
				case *dns.DNSKEY:
					key = value
				case *dns.RRSIG:
					signature = value
				}
			}
			if key == nil || signature == nil || signature.Verify(key, []dns.RR{key}) != nil || key.ToDS(dns.SHA256).String() != replacement[0] {
				t.Fatal("output did not authenticate explicit replacement key", protocol)
			}
		}
		stop()
		status, err := runAnchorOffline(t.Context(), anchorOfflineOptions{database: path, offline: true, timeout: 10 * time.Second}, false)
		if err != nil || status.Version != result.Version || status.Quarantined || tcpKeys.Load() != 1 || udpKeys.Load() == 0 {
			t.Fatal("restart renewed/reseeded recovery or skipped native acquisition", status, err, tcpKeys.Load(), udpKeys.Load())
		}
	}
}

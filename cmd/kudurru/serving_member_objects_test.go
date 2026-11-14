package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// The primary executes forwarded object mutations through the public API's
// own command bodies inside the actual member command transaction.
type rootMemberHarness struct {
	f       *servingCommandFixture
	send    func(clusterreplica.Command) (int, clusterreplica.Outcome)
	sendErr *error
	command func(id, operation string, payload any) clusterreplica.Command
	ref     string
	// client sends every command; its six-second timeout may be raised by a
	// test whose commands legitimately run longer.
	client *http.Client
}

// newRootMemberHarness mounts the production member listener over the actual
// serving runtime with a real public API server for forwarded command bodies.
func newRootMemberHarness(t *testing.T) *rootMemberHarness {
	t.Helper()
	var local, remote *memberidentity.Identity
	f := newServingCommandFixture(t, func(s *servingRuntime) {
		s.opts.Config.Node.DataDir = t.TempDir()
		local, remote = enrollServingMemberFixture(t, s, filepath.Join(s.opts.Config.Node.DataDir, memberIdentityFile))
	})
	server, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, CanonicalSettings: f.s, ObjectCommand: f.s.ObjectCommand, ZoneCommand: f.s.ZoneCommand, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	f.s.objectAPI.Store(server)
	credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{41}, 32), NodeName: "root-member-objects"})
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := f.s.prepareMemberListener(t.Context(), credentials)
	if err != nil {
		t.Fatal(err)
	}
	url := serveRootMemberListener(t, internalapi.ServerOptions{Credentials: credentials, Enrolled: enrolled})
	clientTLS, err := remote.ClientTLS(local.Trust())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: clientTLS}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 6 * time.Second}
	var sendErr error
	send := func(command clusterreplica.Command) (int, clusterreplica.Outcome) {
		t.Helper()
		sendErr = nil
		body, err := json.Marshal(command)
		if err != nil {
			t.Fatal(err)
		}
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+clusterreplica.PathWrite, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+credentials.Token())
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(clusterreplica.HeaderCluster, command.ExpectedTerm.ClusterID)
		r.Header.Set(clusterreplica.HeaderMember, remote.Trust().MemberID())
		response, err := client.Do(r)
		if err != nil {
			// The member server may end a refused exchange without a body.
			sendErr = err
			return 0, clusterreplica.Outcome{}
		}
		defer func() { _ = response.Body.Close() }()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		var out clusterreplica.Outcome
		if response.StatusCode == http.StatusOK && json.Unmarshal(raw, &out) != nil {
			t.Fatal("undecodable member outcome")
		}
		return response.StatusCode, out
	}
	lease, err := f.s.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	value, _ := lease.Bundle().Component("auth")
	raw := servingauth.Credential{Kind: servingauth.APIToken, Value: f.token}
	principal, err := value.(*servingauth.View).Authenticate(t.Context(), raw, f.s.opts.Now())
	lease.Release()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := clusterreplica.NewCredentialProof(raw)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := proof.Reference()
	if err != nil {
		t.Fatal(err)
	}
	command := func(id, operation string, in any) clusterreplica.Command {
		payload, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 2, ID: id, ExpectedTerm: f.s.opts.Store.Authority().Term,
			Hops: 1, IssuedAt: f.now, NotAfter: f.now.Add(time.Minute), Operation: operation, Credential: proof,
			Subject: clusterreplica.SubjectRef{Kind: principal.Kind, ID: principal.ID, Provider: principal.Provider, CredentialKind: "api-token", CredentialID: reference, IssuedAt: principal.CredentialIssuedAt},
			Payload: payload}
	}
	return &rootMemberHarness{f: f, send: send, sendErr: &sendErr, command: command, ref: reference, client: client}
}

func TestRootMemberObjectCommandActualHTTP(t *testing.T) {
	h := newRootMemberHarness(t)
	f, send, reference := h.f, h.send, h.ref
	var err error
	command := func(id string, in api.ForwardedObject) clusterreplica.Command {
		operation := memberObjectWrite
		if in.Operation == api.ForwardedDelete {
			operation = memberObjectDelete
		}
		return h.command(id, operation, in)
	}
	document := func(v any) json.RawMessage {
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	zone := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "forwarded"},
		"spec": map[string]any{"zoneName": "forwarded.test.", "type": "primary", "nameservers": []any{"ns1.forwarded.test."},
			"soa": map[string]any{"mname": "ns1.forwarded.test.", "rname": "hostmaster.forwarded.test."}}}
	status, out := send(command("create-zone", api.ForwardedObject{Operation: api.ForwardedWrite, Kind: "DNSZone", Name: "forwarded", Document: document(zone)}))
	var result api.ForwardedObjectResult
	if status != http.StatusOK || out.Status != http.StatusOK || json.Unmarshal(out.Body, &result) != nil || result.Result != api.ResultCreated {
		t.Fatal("forwarded zone create refused", status, out.Status, string(out.Body))
	}
	record := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet", "metadata": map[string]any{"name": "www", "namespace": "default"},
		"spec": map[string]any{"zoneRef": map[string]any{"name": "forwarded"}, "endpoints": []any{map[string]any{"dnsName": "www.forwarded.test.", "recordType": "A", "targets": []any{"192.0.2.10"}}}}}
	write := command("create-record", api.ForwardedObject{Operation: api.ForwardedWrite, Kind: "DNSRecordSet", Namespace: "default", Name: "www", CreateOnly: true, Document: document(record)})
	status, out = send(write)
	if status != http.StatusOK || out.Status != http.StatusOK || json.Unmarshal(out.Body, &result) != nil || result.Result != api.ResultCreated {
		t.Fatal("forwarded record create refused", status, out.Status, string(out.Body))
	}
	var stored public.DNSRecordSet
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "www"}, &stored); err != nil || store.ManagedBy(&stored) != store.ManagerAPI {
		t.Fatal("forwarded record not committed with the public manager", err)
	}
	// The exact duplicate returns its retained outcome without a second effect.
	if status, again := send(write); status != http.StatusOK || again.Status != out.Status || !bytes.Equal(canonicalOutcome(t, again.Body), canonicalOutcome(t, out.Body)) {
		t.Fatal("dedup replay changed the retained outcome", status)
	}
	// A clean public refusal is retained as the exact relayed result, with no effect.
	status, refused := send(command("stale-match", api.ForwardedObject{Operation: api.ForwardedWrite, Kind: "DNSRecordSet", Namespace: "default", Name: "www", IfMatch: "1", Document: document(record)}))
	var relayed api.Error
	if status != http.StatusOK || refused.Status != http.StatusPreconditionFailed || json.Unmarshal(refused.Body, &relayed) != nil || relayed.Body.Key == "" {
		t.Fatal("precondition refusal not relayed exactly", status, refused.Status)
	}
	status, refused = send(command("create-again", api.ForwardedObject{Operation: api.ForwardedWrite, Kind: "DNSRecordSet", Namespace: "default", Name: "www", CreateOnly: true, Document: document(record)}))
	if status != http.StatusOK || refused.Status != http.StatusConflict {
		t.Fatal("create-only conflict not relayed", status, refused.Status)
	}
	for name, bad := range map[string]clusterreplica.Command{
		"unsupported kind": command("unsupported", api.ForwardedObject{Operation: api.ForwardedWrite, Kind: "APIToken", Name: "x", Document: document(record)}),
		"operation payload mismatch": func() clusterreplica.Command {
			c := command("mismatch", api.ForwardedObject{Operation: api.ForwardedWrite, Kind: "DNSRecordSet", Namespace: "default", Name: "www", Document: document(record)})
			c.Operation = memberObjectDelete
			return c
		}(),
		"forged subject": func() clusterreplica.Command {
			c := command("forged", api.ForwardedObject{Operation: api.ForwardedDelete, Kind: "DNSRecordSet", Namespace: "default", Name: "www"})
			c.Subject.ID = "someone-else"
			return c
		}(),
		"invalid raw credential": func() clusterreplica.Command {
			c := command("invalid-raw", api.ForwardedObject{Operation: api.ForwardedDelete, Kind: "DNSRecordSet", Namespace: "default", Name: "www"})
			c.Credential, err = clusterreplica.NewCredentialProof(servingauth.Credential{Kind: servingauth.APIToken, Value: "invalid-original-token"})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}(),
	} {
		// Scoped refusals abort the exchange without any acknowledgment bytes.
		if status, out := send(bad); *h.sendErr == nil && (status == http.StatusOK && out.Status < 400) {
			t.Fatal("invalid forwarded command succeeded:", name)
		}
	}
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(after)
	if err != nil || bytes.Contains(encoded, []byte(f.token)) {
		t.Fatal("raw proof entered the durable image", err)
	}
	var current public.DNSRecordSet
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "www"}, &current); err != nil || current.ResourceVersion != stored.ResourceVersion {
		t.Fatal("replay or refusals changed the forwarded record", err)
	}
	audits, err := f.s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "DNSRecordSet", Namespace: "default", Name: "www"})
	if err != nil {
		t.Fatal(err)
	}
	attributed := 0
	for _, entry := range audits {
		if entry.RequestID == "create-record" && entry.Actor == "User:root-command" && entry.Authz != "" {
			attributed++
		}
	}
	if attributed != 1 {
		t.Fatal("forwarded write lacks exactly one attributed audit event", attributed)
	}
	status, out = send(command("delete-record", api.ForwardedObject{Operation: api.ForwardedDelete, Kind: "DNSRecordSet", Namespace: "default", Name: "www"}))
	if status != http.StatusOK || out.Status != http.StatusOK || json.Unmarshal(out.Body, &result) != nil || result.Result != "deleted" {
		t.Fatal("forwarded delete refused", status, out.Status, string(out.Body))
	}
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "www"}, &stored); err == nil {
		t.Fatal("forwarded delete left the record")
	}
	// A revoked original credential cannot reacquire even its retained result.
	writer, err := f.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.opts.Store.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.Delete(ctx, store.Key{Kind: "APIToken", Name: reference}, store.DeleteOptions{})
	}); err != nil {
		t.Fatal(err)
	}
	if status, out := send(write); status == http.StatusOK && out.Status == http.StatusOK {
		t.Fatal("revoked credential recovered a forwarded outcome")
	}
}

func canonicalOutcome(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Zone convenience commands run through the primary's own route function, with
// the plan executed inside the member command transaction.
func TestRootMemberZoneCommandActualHTTP(t *testing.T) {
	h := newRootMemberHarness(t)
	zoneDoc, err := json.Marshal(map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "editor"},
		"spec": map[string]any{"zoneName": "editor.test.", "type": "primary", "nameservers": []any{"ns1.editor.test."},
			"soa": map[string]any{"mname": "ns1.editor.test.", "rname": "hostmaster.editor.test."}}})
	if err != nil {
		t.Fatal(err)
	}
	if status, out := h.send(h.command("zone", memberObjectWrite, api.ForwardedObject{Operation: api.ForwardedWrite, Kind: "DNSZone", Name: "editor", Document: zoneDoc})); status != http.StatusOK || out.Status != http.StatusOK {
		t.Fatal("zone fixture refused", status, out.Status)
	}
	input := func(v any) json.RawMessage {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	upsert := api.ForwardedZone{Action: api.ZoneCommandUpsertRecord, Zone: "editor",
		Input: input(map[string]any{"Zone": "editor", "RecordSet": "", "Namespace": "", "Body": map[string]any{"dnsName": "www", "recordType": "A", "targets": []string{"192.0.2.20"}}})}
	status, out := h.send(h.command("upsert", memberZoneCommand, upsert))
	var result map[string]any
	if status != http.StatusOK || out.Status != http.StatusOK || json.Unmarshal(out.Body, &result) != nil || result["Status"] != float64(http.StatusCreated) {
		t.Fatal("forwarded zone upsert refused", status, out.Status, string(out.Body))
	}
	var stored public.DNSRecordSet
	if err := h.f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "api-editor"}, &stored); err != nil || len(stored.Spec.Endpoints) != 1 {
		t.Fatal("forwarded upsert not committed", err)
	}
	missing := api.ForwardedZone{Action: api.ZoneCommandUpsertRecord, Zone: "absent",
		Input: input(map[string]any{"Zone": "absent", "RecordSet": "", "Namespace": "", "Body": map[string]any{"dnsName": "www", "recordType": "A", "targets": []string{"192.0.2.21"}}})}
	if status, out := h.send(h.command("missing", memberZoneCommand, missing)); status != http.StatusOK || out.Status != http.StatusNotFound {
		t.Fatal("missing zone refusal not relayed exactly", status, out.Status)
	}
	for name, bad := range map[string]api.ForwardedZone{
		"input zone mismatch": {Action: api.ZoneCommandUpsertRecord, Zone: "editor", Input: input(map[string]any{"Zone": "absent", "RecordSet": "", "Namespace": "", "Body": map[string]any{"dnsName": "x", "recordType": "A", "targets": []string{"192.0.2.22"}}})},
		"unknown action":      {Action: "rename_zone", Zone: "editor", Input: input(map[string]any{"Zone": "editor"})},
		"unknown input field": {Action: api.ZoneCommandDeleteRecord, Zone: "editor", Input: input(map[string]any{"Zone": "editor", "Name": "www", "Type": "A", "Extra": true})},
	} {
		if status, out := h.send(h.command("bad-"+name, memberZoneCommand, bad)); *h.sendErr == nil && status == http.StatusOK && out.Status < 400 {
			t.Fatal("invalid zone command succeeded:", name)
		}
	}
	remove := api.ForwardedZone{Action: api.ZoneCommandDeleteRecord, Zone: "editor", Input: input(map[string]any{"Zone": "editor", "Name": "www", "Type": "A", "RecordSet": "", "SetIdentifier": ""})}
	if status, out := h.send(h.command("delete", memberZoneCommand, remove)); status != http.StatusOK || out.Status != http.StatusOK {
		t.Fatal("forwarded record delete refused", status, out.Status, string(out.Body))
	}
	if err := h.f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "api-editor"}, &stored); err == nil && len(stored.Spec.Endpoints) != 0 {
		t.Fatal("forwarded delete left the record")
	}
}

// A forwarded local login runs the primary's shared admission and enrolls the
// follower-prepared session; no password or CSRF secret becomes durable.
func TestRootMemberLoginCommandActualHTTP(t *testing.T) {
	h := newRootMemberHarness(t)
	hash, err := users.NewHasher(1).Hash(t.Context(), "forwarded-login-password-1")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := h.f.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.f.s.opts.Store.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
			return err
		}
		user.Spec.PasswordHash = hash
		_, err := tx.Apply(ctx, &user, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.f.s.publisher.Wake()
	login := func(id, password string, mutate func(*clusterreplica.Command)) (int, memberLoginResult, string) {
		t.Helper()
		_, prepared := security.NewForwardedEnrollment()
		// #nosec G117 -- disposable fixture password in a test-only member command
		payload, err := json.Marshal(memberLogin{Username: "root-command", Password: password, ClientKey: "192.0.2.200", SessionID: prepared.ID, CSRFHash: prepared.CSRFHash,
			Nonce: "nonce-that-is-long-enough-for-the-forwarded-login-check"})
		if err != nil {
			t.Fatal(err)
		}
		c := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 1, ID: id, ExpectedTerm: h.f.s.opts.Store.Authority().Term,
			Hops: 1, IssuedAt: h.f.now, NotAfter: h.f.now.Add(time.Minute), Operation: memberLocalLogin,
			Subject: clusterreplica.SubjectRef{Kind: auth.KindUser, ID: "local:root-command", Provider: "local", CredentialKind: "session", CredentialID: prepared.ID, IssuedAt: h.f.now},
			Payload: payload}
		if mutate != nil {
			mutate(&c)
		}
		status, out := h.send(c)
		var result memberLoginResult
		if status == http.StatusOK && json.Unmarshal(out.Body, &result) != nil {
			t.Fatal("undecodable login result")
		}
		return status, result, prepared.ID
	}
	// Login may need the fresh password hash in a published view; retry only
	// this fixture's first distinct attempt until the hash is current.
	var status int
	var result memberLoginResult
	var session string
	if !kudurrutestEventually(10*time.Second, func() bool {
		status, result, session = login(uuid.NewString(), "forwarded-login-password-1", nil)
		return status == http.StatusOK && result.Committed
	}) {
		t.Fatal("forwarded login refused", status, result)
	}
	if result.SessionID != session {
		t.Fatal("primary did not enroll the follower-prepared session")
	}
	image, err := h.f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(image)
	if err != nil || !bytes.Contains(encoded, []byte(session)) || bytes.Contains(encoded, []byte("forwarded-login-password-1")) {
		t.Fatal("session not durable, or the password entered the image", err)
	}
	// A wrong password is a retained refusal; its counters commit.
	status, refused, _ := login("wrong-password", "not-the-password-2", nil)
	if status != http.StatusOK || refused.Committed || refused.Status != http.StatusUnauthorized || !refused.Credentials {
		t.Fatal("credential refusal not retained", status, refused)
	}
	for name, mutate := range map[string]func(*clusterreplica.Command){
		"subject mismatch": func(c *clusterreplica.Command) { c.Subject.ID = "local:someone-else" },
		"session mismatch": func(c *clusterreplica.Command) { c.Subject.CredentialID = "other" },
		"version 2":        func(c *clusterreplica.Command) { c.Version = 2 },
		"wrong operation":  func(c *clusterreplica.Command) { c.Operation = memberBlockingToggle },
	} {
		if status, out, _ := login("bad-"+name, "forwarded-login-password-1", mutate); *h.sendErr == nil && status == http.StatusOK && out.Committed {
			t.Fatal("invalid forwarded login succeeded:", name)
		}
	}
}

func kudurrutestEventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

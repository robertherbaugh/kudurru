package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Enrollment setup is explicit fixture administration. The HTTP server below
// mounts the actual root executor; production listener/operator activation is
// separate and is not claimed by this test.
func enrollServingMemberFixture(t *testing.T, s *servingRuntime) (*memberidentity.Identity, *memberidentity.Identity) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	local, err := memberidentity.Create(filepath.Join(t.TempDir(), "local.json"), s.opts.Store.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	_, cut, err := s.opts.Store.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write := func(fn func(context.Context, *sqlite.CommandTx) error) {
		token, err := s.opts.Store.WriterToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.opts.Store.Command(sqlite.WithWriterToken(ctx, token), token.Term(), fn); err != nil {
			t.Fatal(err)
		}
	}
	write(func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.StageEnrollmentPrimary(ctx, cut, local, "primary", "127.0.0.1:8082")
	})
	remote, err := memberidentity.Create(filepath.Join(t.TempDir(), "remote.json"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	change := func(fn func(context.Context, *sqlite.CommandTx, *persistence.Original) error) {
		writer, err := s.opts.Store.WriterToken()
		if err != nil {
			t.Fatal(err)
		}
		originalContext := sqlite.WithWriterToken(ctx, writer)
		image, err := s.opts.Store.ReadGeneration(originalContext)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := accepted.DecodeRows(image.Payload.Membership, image.Term.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		original, err := persistence.Capture(originalContext, owner, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := s.opts.Store.Command(originalContext, writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error { return fn(ctx, tx, original) }); err != nil {
			t.Fatal(err)
		}
	}
	var invitation enrollment.Invitation
	change(func(ctx context.Context, tx *sqlite.CommandTx, original *persistence.Original) error {
		var err error
		_, invitation, err = persistence.Issue(ctx, tx, original, "peer", "127.0.0.2:8082", time.Now().Add(time.Minute))
		return err
	})
	proposal, err := invitation.Propose(remote)
	if err != nil {
		t.Fatal(err)
	}
	change(func(ctx context.Context, tx *sqlite.CommandTx, original *persistence.Original) error {
		_, err := persistence.Accept(ctx, tx, original, proposal)
		return err
	})
	return local, remote
}

func TestRootMemberBlockingCommandActualHTTP(t *testing.T) {
	for _, useHTTP2 := range []bool{false, true} {
		name := "http1"
		if useHTTP2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			var local, remote *memberidentity.Identity
			f := newServingCommandFixture(t, func(s *servingRuntime) { local, remote = enrollServingMemberFixture(t, s) })
			credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{37}, 32), NodeName: "root-member-command"})
			if err != nil {
				t.Fatal(err)
			}
			server, err := clusterreplica.NewMemberServer(clusterreplica.MemberServerOptions{Store: f.s.opts.Store, Credentials: credentials, ExecuteScoped: f.s.executeMemberCommand, Timeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			serverTLS, err := local.ServerTLS([]memberidentity.Trust{remote.Trust()})
			if err != nil {
				t.Fatal(err)
			}
			live := httptest.NewUnstartedServer(server.Handler())
			live.EnableHTTP2, live.TLS = useHTTP2, serverTLS
			live.StartTLS()
			defer live.Close()
			clientTLS, err := remote.ClientTLS(local.Trust())
			if err != nil {
				t.Fatal(err)
			}
			transport := &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: useHTTP2}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 6 * time.Second}
			send := func(command clusterreplica.Command) (*http.Response, []byte, error) {
				body, err := json.Marshal(command)
				if err != nil {
					return nil, nil, err
				}
				r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, live.URL+clusterreplica.PathWrite, bytes.NewReader(body))
				if err != nil {
					return nil, nil, err
				}
				r.Header.Set("Authorization", "Bearer "+credentials.Token())
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set(clusterreplica.HeaderCluster, command.ExpectedTerm.ClusterID)
				r.Header.Set(clusterreplica.HeaderMember, remote.Trust().MemberID())
				response, err := client.Do(r)
				if err != nil {
					return response, nil, err
				}
				defer func() { _ = response.Body.Close() }()
				out, err := io.ReadAll(io.LimitReader(response.Body, 8192))
				return response, out, err
			}
			var settings public.Kudurru
			if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
				t.Fatal(err)
			}
			lease, err := f.s.publisher.CaptureHTTP(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			value, ok := lease.Bundle().Component("auth")
			if !ok {
				t.Fatal("missing real root auth publication")
			}
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
			until := f.now.Add(time.Minute)
			payload, _ := json.Marshal(map[string]any{"blocking": false, "until": until})
			command := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 2, ID: "root-blocking-original", ExpectedTerm: f.s.opts.Store.Authority().Term,
				Hops: 1, IssuedAt: f.now, NotAfter: f.now.Add(time.Minute), Operation: memberBlockingToggle, Credential: proof,
				Subject:       clusterreplica.SubjectRef{Kind: principal.Kind, ID: principal.ID, Provider: principal.Provider, CredentialKind: "api-token", CredentialID: reference, IssuedAt: principal.CredentialIssuedAt},
				Preconditions: []clusterreplica.Precondition{{Kind: "Kudurru", Name: "default", ResourceVersion: settings.ResourceVersion}}, Payload: payload}
			response, body, err := send(command)
			if err != nil || response.StatusCode != http.StatusOK || response.ProtoMajor != map[bool]int{false: 1, true: 2}[useHTTP2] {
				t.Fatal("actual root member command refused", err)
			}
			var outcome clusterreplica.Outcome
			var receipt blockingReceipt
			if json.Unmarshal(body, &outcome) != nil || json.Unmarshal(outcome.Body, &receipt) != nil || !receipt.Committed || !receipt.Disabled || receipt.DisabledUntil == nil || !receipt.DisabledUntil.Equal(until) || receipt.Publication != "pending" {
				t.Fatal("root response lost actual bounded intent or overstated publication")
			}
			image, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil || !settings.Status.Blocking.Disabled || settings.Status.Blocking.DisabledUntil == nil || !settings.Status.Blocking.DisabledUntil.Time.Equal(until) {
				t.Fatal("actual canonical blocking status missing", err)
			}
			// Explicit duplicate request checks current authorization before
			// dedup; its already-consumed original version must not rerun effects.
			response, again, err := send(command)
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatal("authorized exact dedup replay failed", err)
			}
			// Durable JSON canonicalization may reorder fields. The typed
			// status and complete receipt must retain precisely the same values.
			var replay clusterreplica.Outcome
			var retained blockingReceipt
			if json.Unmarshal(again, &replay) != nil || json.Unmarshal(replay.Body, &retained) != nil || replay.Status != outcome.Status || !reflect.DeepEqual(retained, receipt) {
				t.Fatal("retained outcome changed actual command receipt")
			}
			for _, mode := range []string{"stale-version", "unknown-operation", "relative-timer", "missing-intent", "forged-reference", "invalid-raw"} {
				t.Run(mode, func(t *testing.T) {
					bad := command
					bad.ID = mode
					switch mode {
					case "unknown-operation":
						bad.Operation = "objects.apply"
					case "relative-timer":
						bad.Payload = json.RawMessage(`{"blocking":false,"timer":60}`)
					case "missing-intent":
						bad.Payload = json.RawMessage(`{}`)
					case "forged-reference":
						bad.Subject.CredentialID = "forged-reference"
					case "invalid-raw":
						bad.Credential, err = clusterreplica.NewCredentialProof(servingauth.Credential{Kind: servingauth.APIToken, Value: "invalid-original-token"})
						if err != nil {
							t.Fatal(err)
						}
					}
					refused, _, err := send(bad)
					if err == nil && refused.StatusCode == http.StatusOK {
						t.Fatal("invalid original command succeeded")
					}
				})
			}
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !after.Generation().Same(image.Generation()) {
				t.Fatal("replay or refusals changed state", err)
			}
			stored, err := json.Marshal(after)
			if err != nil || bytes.Contains(stored, []byte(f.token)) {
				t.Fatal("raw proof entered durable image", err)
			}
			audits, err := f.s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "Kudurru", Name: "default"})
			if err != nil {
				t.Fatal(err)
			}
			attributed := false
			for _, entry := range audits {
				if entry.RequestID == command.ID {
					attributed = entry.Actor == "User:root-command" && entry.Authz != "" && entry.RequestPath == clusterreplica.PathWrite
				}
			}
			auditBytes, err := json.Marshal(audits)
			if err != nil || !attributed || bytes.Contains(auditBytes, []byte(f.token)) {
				t.Fatal("root mutation lost verified audit attribution or exposed proof", err)
			}
			writer, err := f.s.opts.Store.WriterToken()
			if err != nil {
				t.Fatal(err)
			}
			if err := f.s.opts.Store.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
				return tx.Delete(ctx, store.Key{Kind: "APIToken", Name: reference}, store.DeleteOptions{})
			}); err != nil {
				t.Fatal(err)
			}
			beforeRevoked, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			refused, _, err := send(command)
			if err == nil && refused.StatusCode == http.StatusOK {
				t.Fatal("revoked original proof recovered dedup acknowledgment")
			}
			afterRevoked, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !beforeRevoked.Generation().Same(afterRevoked.Generation()) {
				t.Fatal("revoked replay changed state", err)
			}
		})
	}
}

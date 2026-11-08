package providercoordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// The domain checks the actual received raw credential but its mutation is a
// fixture. Production root domain policy and listener assembly are separate.
type scopedHTTPDomain struct {
	deps           servingauth.Dependencies
	authorizations *atomic.Int32
}

func (d scopedHTTPDomain) Authorize(ctx context.Context, tx replica.CommandTx, command replica.Command) (context.Context, error) {
	d.authorizations.Add(1)
	raw, err := command.Credential.Raw()
	if err != nil {
		return nil, err
	}
	p, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx.State().(*sqlite.CommandTx), d.deps, raw, rbac.Attributes{Resource: "stats", Verb: "read"}, time.Now)
	if err != nil {
		return nil, err
	}
	if p.Kind != command.Subject.Kind || p.ID != command.Subject.ID || p.Provider != command.Subject.Provider || p.TokenID != command.Subject.CredentialID || !p.CredentialIssuedAt.Equal(command.Subject.IssuedAt) {
		return nil, replica.ErrForbidden
	}
	return ctx, nil
}

func (scopedHTTPDomain) Execute(ctx context.Context, tx replica.CommandTx, _ replica.Command) (replica.Outcome, error) {
	err := tx.State().(*sqlite.CommandTx).PutSection(ctx, "dedup", replication.Entry{Key: "coordinator-http", Data: []byte(`{"changed":true}`)})
	return replica.Outcome{Status: 200, Body: json.RawMessage(`{"changed":true}`)}, err
}

func TestMemberCoordinatorActualScopedHTTPFlush(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
			f := newMemberOutputFixture(t)
			original, target := writer(t, f.store)
			writerToken, _ := sqlite.WriterTokenFromContext(original)
			credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{29}, 32), NodeName: "coordinator-http"})
			if err != nil {
				t.Fatal(err)
			}
			var authorizations atomic.Int32
			server, err := replica.NewMemberServer(replica.MemberServerOptions{Store: f.store, Credentials: credentials, Timeout: 4 * time.Second,
				ExecuteScoped: func(ctx context.Context, request *sqlite.MemberRequest, command replica.Command, emit func(context.Context, replica.Outcome) error) error {
					ctx = sqlite.WithWriterToken(ctx, writerToken)
					var out replica.Outcome
					_, err := f.coordinator.WithMemberCommandOutput(ctx, target, request, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
						service := replica.CommandService{Domain: scopedHTTPDomain{deps: deps, authorizations: &authorizations}}
						var err error
						out, err = service.StageSQLite(ctx, tx, command)
						return err
					}, func(scope context.Context) error { return emit(scope, out) })
					return err
				}})
			if err != nil {
				t.Fatal(err)
			}
			serverTLS, err := f.localIdentity.ServerTLS([]memberidentity.Trust{f.remoteIdentity.Trust()})
			if err != nil {
				t.Fatal(err)
			}
			release, returned := make(chan struct{}), make(chan struct{})
			var once, returnedOnce sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			live := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				server.Handler().ServeHTTP(w, r)
				returnedOnce.Do(func() { close(returned) })
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			live.EnableHTTP2, live.TLS = http2, serverTLS
			live.StartTLS()
			defer live.Close()
			// Unblock the outer handler before closing the live server on failure.
			defer finish()
			clientTLS, err := f.remoteIdentity.ClientTLS(f.localIdentity.Trust())
			if err != nil {
				t.Fatal(err)
			}
			transport := &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: http2}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			now := time.Now().UTC()
			raw := servingauth.Credential{Kind: servingauth.APIToken, Value: f.wire}
			principal, err := f.view(t).Authenticate(t.Context(), raw, now)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := replica.NewCredentialProof(raw)
			if err != nil {
				t.Fatal(err)
			}
			command := replica.Command{Format: replica.CommandFormat, Version: 2, ID: "coordinator-http", ExpectedTerm: target.Term, Hops: 1, IssuedAt: now, NotAfter: now.Add(time.Minute), Operation: "fixture.change", Subject: replica.SubjectRef{Kind: auth.KindServiceAccount, ID: principal.ID, Provider: principal.Provider, CredentialKind: "api-token", CredentialID: principal.TokenID, IssuedAt: principal.CredentialIssuedAt}, Credential: proof, Payload: json.RawMessage(`{"change":true}`)}
			body, _ := json.Marshal(command)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, live.URL+replica.PathWrite, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+credentials.Token())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(replica.HeaderCluster, target.Term.ClusterID)
			req.Header.Set(replica.HeaderMember, f.remote)
			response, err := client.Do(req)
			if err != nil {
				t.Fatal("composed command/output refused", err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusOK || response.ProtoMajor != map[bool]int{false: 1, true: 2}[http2] {
				t.Fatal("wrong actual response", response.StatusCode, response.Proto)
			}
			want, _ := json.Marshal(replica.Outcome{Status: 200, Body: json.RawMessage(`{"changed":true}`)})
			got := make([]byte, len(want))
			if _, err := io.ReadFull(response.Body, got); err != nil || !bytes.Equal(got, want) {
				t.Fatal("protected small response not flushed", err)
			}
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("coordinator did not release after flush")
			}
			finish()
			if tail, err := io.ReadAll(response.Body); err != nil || len(tail) != 0 {
				t.Fatal("scoped framing failed", err)
			}
			image, err := f.store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range image.Payload.Dedup {
				if row.Key == "coordinator-http" {
					found = true
				}
			}
			if !found {
				t.Fatal("response had no durable domain effect")
			}
			stored, err := json.Marshal(image)
			if err != nil || bytes.Contains(stored, []byte(f.wire)) {
				t.Fatal("raw credential entered persisted image or dedup", err)
			}
			for _, mode := range []string{"invalid-raw", "forged-subject", "missing-proof", "legacy-with-proof"} {
				t.Run(mode, func(t *testing.T) {
					bad := command
					bad.ID = mode
					switch mode {
					case "invalid-raw":
						bad.Credential, err = replica.NewCredentialProof(servingauth.Credential{Kind: servingauth.APIToken, Value: "invalid-original-token"})
						if err != nil {
							t.Fatal(err)
						}
					case "forged-subject":
						bad.Subject.ID = "local:admin"
					case "missing-proof":
						bad.Credential = nil
					case "legacy-with-proof":
						bad.Version = 1
					}
					encoded, err := json.Marshal(bad)
					if err != nil {
						t.Fatal(err)
					}
					request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, live.URL+replica.PathWrite, bytes.NewReader(encoded))
					if err != nil {
						t.Fatal(err)
					}
					request.Header = req.Header.Clone()
					refused, err := client.Do(request)
					if refused != nil {
						_ = refused.Body.Close()
						if refused.StatusCode == http.StatusOK || refused.Header.Get(replica.HeaderTerm) != "" {
							t.Fatal("invalid user proof received an authority acknowledgment")
						}
					} else if err == nil {
						t.Fatal("missing refusal")
					}
					after, err := f.store.ReadGeneration(t.Context())
					if err != nil || !after.Generation().Same(image.Generation()) {
						t.Fatal("invalid user proof changed domain or dedup", err)
					}
				})
			}
			// A retained successful outcome never bypasses current raw proof.
			ctx, writeTarget := writer(t, f.store)
			if err := f.coordinator.WithCommand(ctx, writeTarget, func(ctx context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies) error {
				return tx.Delete(ctx, store.Key{Kind: "APIToken", Name: principal.TokenID}, store.DeleteOptions{})
			}); err != nil {
				t.Fatal(err)
			}
			before, err := f.store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			attempts := authorizations.Load()
			replay, err := http.NewRequestWithContext(t.Context(), http.MethodPost, live.URL+replica.PathWrite, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			replay.Header = req.Header.Clone()
			refused, err := client.Do(replay)
			if refused != nil {
				_ = refused.Body.Close()
			}
			if err == nil || authorizations.Load() != attempts+1 {
				t.Fatal("revoked dedup did not reach current proof refusal")
			}
			after, err := f.store.ReadGeneration(t.Context())
			if err != nil || !after.Generation().Same(before.Generation()) {
				t.Fatal("revoked replay mutated state", err)
			}
		})
	}
}

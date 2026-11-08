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
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// The domain checks an actual current raw credential but its mutation is a
// fixture. Production forwarded proof encoding and domain policy are separate.
type scopedHTTPDomain struct {
	f    memberOutputFixture
	deps servingauth.Dependencies
}

func (d scopedHTTPDomain) Authorize(ctx context.Context, tx replica.CommandTx, _ replica.Command) (context.Context, error) {
	return ctx, d.f.authorize(ctx, tx.State().(*sqlite.CommandTx), d.deps)
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
			writer, _ := sqlite.WriterTokenFromContext(original)
			credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{29}, 32), NodeName: "coordinator-http"})
			if err != nil {
				t.Fatal(err)
			}
			server, err := replica.NewMemberServer(replica.MemberServerOptions{Store: f.store, Credentials: credentials, Timeout: 4 * time.Second,
				ExecuteScoped: func(ctx context.Context, request *sqlite.MemberRequest, command replica.Command, emit func(context.Context, replica.Outcome) error) error {
					ctx = sqlite.WithWriterToken(ctx, writer)
					var out replica.Outcome
					_, err := f.coordinator.WithMemberCommandOutput(ctx, target, request, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
						service := replica.CommandService{Domain: scopedHTTPDomain{f: f, deps: deps}}
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
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			live := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				server.Handler().ServeHTTP(w, r)
				close(returned)
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
			command := replica.Command{Format: replica.CommandFormat, Version: 1, ID: "coordinator-http", ExpectedTerm: target.Term, Hops: 1, IssuedAt: now, NotAfter: now.Add(time.Minute), Operation: "fixture.change", Subject: replica.SubjectRef{Kind: auth.KindServiceAccount, ID: "sa:apps/ci", Provider: "local", CredentialKind: "api-token", CredentialID: "fixture", IssuedAt: now.Add(-time.Minute)}, Payload: json.RawMessage(`{"change":true}`)}
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
		})
	}
}

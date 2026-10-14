package replica

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var sharedValidator = sync.OnceValue(func() *validator.Validator {
	v, e := validator.New()
	if e != nil {
		panic(e)
	}
	return v
})

func openStore(t *testing.T, mode replication.Mode) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Validator: sharedValidator(), InitialMode: mode, Identity: "test-node"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func zoneObject(name string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.DNSZoneSpec{ZoneName: name + ".example.", Type: v1alpha1.ZoneTypePrimary}}
}
func imageOf(t *testing.T, s *sqlite.Store) *replication.Image {
	t.Helper()
	i, e := s.ReadGeneration(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	return i
}
func mirrorOf(t *testing.T, p *sqlite.Store) *sqlite.Store {
	t.Helper()
	s := openStore(t, replication.Follower)
	ctx := context.Background()
	if e := s.Enroll(ctx, p.Authority().Term); e != nil {
		t.Fatal(e)
	}
	i := imageOf(t, p)
	v, e := s.ValidateImage(ctx, *i)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := s.InstallReplica(ctx, v, replication.InstallOptions{ExpectedTerm: i.Term, ExpectedInstalled: s.Authority().Installed}); e != nil {
		t.Fatal(e)
	}
	return s
}
func credentials(t *testing.T, key byte) *internalapi.Credentials {
	t.Helper()
	c, e := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{key}, 32), NodeName: "test-node"})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func tlsServer(t *testing.T, creds *internalapi.Credentials, h http.Handler) string {
	t.Helper()
	s := httptest.NewUnstartedServer(h)
	s.TLS = creds.ServerTLS()
	s.EnableHTTP2 = true
	s.StartTLS()
	t.Cleanup(s.Close)
	return strings.TrimPrefix(s.URL, "https://")
}
func clientFor(t *testing.T, s *sqlite.Store, creds *internalapi.Credentials, edit ...func(*ClientOptions)) *Client {
	t.Helper()
	o := ClientOptions{Credentials: creds, Observer: s, Self: func() Peer { a := s.Authority(); return Peer{MemberID: a.MemberID, ClusterID: a.Term.ClusterID} }}
	for _, f := range edit {
		f(&o)
	}
	c, e := NewClient(o)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func sourceFor(t *testing.T, c *Client, peer Peer, p *sqlite.Store, edit ...func(*SourceOptions)) *Source {
	t.Helper()
	i := imageOf(t, p)
	o := SourceOptions{Resolve: func(context.Context) (Peer, error) { return peer, nil }, Validation: replication.Validation{SchemaVersion: store.SchemaVersion, Kinds: i.KindManifest}}
	for _, f := range edit {
		f(&o)
	}
	s, e := NewSource(c, o)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func serverFor(t *testing.T, s *sqlite.Store, creds *internalapi.Credentials, member string, service *CommandService) *Server {
	t.Helper()
	server, e := NewServer(ServerOptions{Store: s, Credentials: creds, Commands: service, AuthorizePeer: func(_ context.Context, p Peer, _ *http.Request) error {
		if p.MemberID != member {
			return ErrUnauthorized
		}
		return nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	return server
}

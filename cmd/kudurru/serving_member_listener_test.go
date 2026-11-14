package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func serveRootMemberListener(t *testing.T, options internalapi.ServerOptions) string {
	t.Helper()
	server, err := internalapi.NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("internal member listener did not stop")
		}
	})
	return "https://" + listener.Addr().String()
}

func TestRootMemberListenerStartupIdentityRefusals(t *testing.T) {
	s := newServingRuntimeFixture(t)
	s.opts.Config.Node.DataDir = t.TempDir()
	path := filepath.Join(s.opts.Config.Node.DataDir, memberIdentityFile)
	if listener, err := s.prepareMemberListener(t.Context(), nil); err != nil || listener != nil {
		t.Fatal("dormant setup should leave the legacy listener unchanged", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("dormant startup created a private identity")
	}
	local, _ := enrollServingMemberFixture(t, s, path)
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{39}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if listener, err := s.prepareMemberListener(t.Context(), nil); err == nil || listener != nil {
		t.Fatal("enrolled startup accepted absent internal credentials")
	}
	if listener, err := s.prepareMemberListener(t.Context(), creds); err != nil || listener == nil {
		t.Fatal("exact retained identity refused", err)
	}
	retained := path + ".retained"
	if err := os.Rename(path, retained); err != nil {
		t.Fatal(err)
	}
	if listener, err := s.prepareMemberListener(t.Context(), creds); err == nil || listener != nil {
		t.Fatal("enrolled startup repaired missing identity")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing identity was recreated")
	}
	// A valid new CA for the SAME UUID is still not the enrolled local CA.
	if _, err := memberidentity.Create(path, local.Trust().MemberID()); err != nil {
		t.Fatal(err)
	}
	if listener, err := s.prepareMemberListener(t.Context(), creds); err == nil || listener != nil {
		t.Fatal("same UUID substituted a new private CA")
	}
	if err := os.Rename(path, path+".foreign"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(retained, path); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- deliberate invalid permissions on a temporary test key to prove startup refusal
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if listener, err := s.prepareMemberListener(t.Context(), creds); err == nil || listener != nil {
		t.Fatal("permissive identity file accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if listener, err := s.prepareMemberListener(t.Context(), creds); err != nil || listener == nil {
		t.Fatal("original retained identity no longer loads", err)
	}
}

func TestRootMemberListenerTLSSeparationAndLiveRevocation(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		name := "http1"
		if http2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			s := newServingRuntimeFixture(t)
			s.opts.Config.Node.DataDir = t.TempDir()
			local, peer := enrollServingMemberFixture(t, s, filepath.Join(s.opts.Config.Node.DataDir, memberIdentityFile))
			creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{38}, 32)})
			if err != nil {
				t.Fatal(err)
			}
			enrolled, err := s.prepareMemberListener(t.Context(), creds)
			if err != nil {
				t.Fatal(err)
			}
			url := serveRootMemberListener(t, internalapi.ServerOptions{Credentials: creds, Enrolled: enrolled})
			newClient := func(config *tls.Config) *http.Client {
				transport := &http.Transport{TLSClientConfig: config, ForceAttemptHTTP2: http2}
				t.Cleanup(transport.CloseIdleConnections)
				return &http.Client{Transport: transport, Timeout: 5 * time.Second}
			}
			clientTLS, err := peer.ClientTLS(local.Trust())
			if err != nil {
				t.Fatal(err)
			}
			client := newClient(clientTLS)
			legacy := newClient(creds.ClientTLS())
			request := func(client *http.Client, path, host string) (int, bool, error) {
				var reused bool
				ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})
				r, err := http.NewRequestWithContext(ctx, http.MethodGet, url+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				r.Host = host
				r.Header.Set("Authorization", "Bearer "+creds.Token())
				r.Header.Set(clusterreplica.HeaderCluster, s.opts.Store.Authority().Term.ClusterID)
				r.Header.Set(clusterreplica.HeaderMember, peer.Trust().MemberID())
				response, err := client.Do(r)
				if err != nil {
					return 0, reused, err
				}
				defer func() { _ = response.Body.Close() }()
				_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<20))
				if response.ProtoMajor != map[bool]int{false: 1, true: 2}[http2] {
					t.Fatal("wrong actual HTTP protocol")
				}
				return response.StatusCode, reused, err
			}
			if code, _, err := request(client, clusterreplica.PathImage, "legacy.invalid"); err != nil || code != http.StatusOK {
				t.Fatal("original enrolled image refused", code, err)
			}
			if code, _, err := request(client, internalapi.PathStatus, "legacy.invalid"); err != nil || code != http.StatusNotFound {
				t.Fatal("enrolled connection entered legacy routes", code, err)
			}
			if code, _, err := request(legacy, internalapi.PathStatus, local.Trust().MemberID()+".member.kudurru.invalid"); err != nil || code != http.StatusOK {
				t.Fatal("legacy status compatibility lost", code, err)
			}
			if code, _, err := request(legacy, clusterreplica.PathImage, local.Trust().MemberID()+".member.kudurru.invalid"); err != nil || code != http.StatusNotFound {
				t.Fatal("legacy connection or forged Host entered member routes", code, err)
			}
			missing := clientTLS.Clone()
			missing.GetClientCertificate = nil
			if _, _, err := request(newClient(missing), clusterreplica.PathImage, "member.invalid"); err == nil {
				t.Fatal("missing peer certificate accepted")
			}
			unknown := clientTLS.Clone()
			unknown.ServerName = uuid.NewString() + ".member.kudurru.invalid"
			if _, _, err := request(newClient(unknown), clusterreplica.PathImage, "member.invalid"); err == nil {
				t.Fatal("unknown member SNI accepted")
			}
			foreign, err := memberidentity.Create(filepath.Join(t.TempDir(), "foreign.json"), peer.Trust().MemberID())
			if err != nil {
				t.Fatal(err)
			}
			foreignTLS, err := foreign.ClientTLS(local.Trust())
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := request(newClient(foreignTLS), clusterreplica.PathImage, "member.invalid"); err == nil {
				t.Fatal("same peer UUID with foreign CA accepted")
			}

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			writer, err := s.opts.Store.WriterToken()
			if err != nil {
				t.Fatal(err)
			}
			ctx = sqlite.WithWriterToken(ctx, writer)
			image, err := s.opts.Store.ReadGeneration(ctx)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := accepted.DecodeRows(image.Payload.Membership, image.Term.ClusterID)
			if err != nil {
				t.Fatal(err)
			}
			original, err := persistence.Capture(ctx, owner, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := s.opts.Store.Command(ctx, writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
				_, err := persistence.Revoke(ctx, tx, original, peer.Trust().MemberID())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if code, reused, err := request(client, clusterreplica.PathImage, "member.invalid"); err != nil || code != http.StatusUnauthorized || !reused {
				t.Fatal("revocation did not refuse on the original established connection", code, reused, err)
			}
			// With no remaining peers, the listener keeps its original identity
			// but refuses new handshakes instead of admitting shared credentials.
			if _, _, err := request(newClient(clientTLS.Clone()), clusterreplica.PathImage, "member.invalid"); err == nil {
				t.Fatal("revoked peer opened a new TLS connection")
			}
			if code, _, err := request(legacy, internalapi.PathStatus, "legacy.invalid"); err != nil || code != http.StatusOK {
				t.Fatal("revocation broke legacy status", code, err)
			}
		})
	}
}

// Actual runServe loads the persisted original identity and installs both route
// sets. Enrollment is explicit stopped-store fixture administration, not an
// operator API or proof that public forwarding/join/promotion is implemented.
func TestRunServeEnrolledMemberListenerRestart(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	st, closeStore := tsigMountedStore(t, cfg)
	path := filepath.Join(cfg.Node.DataDir, memberIdentityFile)
	local, peer := enrollServingMemberFixture(t, &servingRuntime{opts: servingRuntimeOptions{Store: st, Config: cfg}}, path)
	// #nosec G304 -- path is the fixture-created private key under t.TempDir, used only for byte comparison
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a, owner, err := st.ReadMemberEnrollment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	closeStore()
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{8}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		stop := startStaticMount(t, cfg)
		clientTLS, err := peer.ClientTLS(local.Trust())
		if err != nil {
			t.Fatal(err)
		}
		transport := &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: true}
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+cfg.Listeners.Internal.Address+clusterreplica.PathImage, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+creds.Token())
		r.Header.Set(clusterreplica.HeaderCluster, a.Term.ClusterID)
		r.Header.Set(clusterreplica.HeaderMember, peer.Trust().MemberID())
		response, err := client.Do(r)
		if err != nil {
			transport.CloseIdleConnections()
			t.Fatal("production enrolled listener request failed", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		closeErr := response.Body.Close()
		transport.CloseIdleConnections()
		var image replication.Image
		if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || response.ProtoMajor != 2 || json.Unmarshal(body, &image) != nil || !image.Term.Same(a.Term) {
			t.Fatal("production enrolled image refused or lost original term", response.StatusCode, readErr, closeErr)
		}
		installed, err := accepted.DecodeRows(image.Payload.Membership, image.Term.ClusterID)
		if err != nil || installed.Digest() != owner.Digest() {
			t.Fatal("restart changed original enrollment", err)
		}
		// #nosec G304 -- the same fixture-created path under t.TempDir; no key bytes are logged or exported
		retained, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(retained, original) {
			t.Fatal("startup replaced private identity", err)
		}
		stop()
	}
}

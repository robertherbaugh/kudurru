package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This peer implements first-party pinned SDK wire fixtures over verified TLS.
// The registered production constructor keeps its canonical Cloudflare origin;
// only this test's transport dials the disposable local peer for that origin.
type integrationCloudflarePeer struct {
	unreadable   bool
	mu           sync.Mutex
	rows         map[string]map[string]any
	next, writes int
	ambiguous    bool
	// requests counts every request the peer served; deleted lists the IDs
	// named by delete effects, in order.
	requests int
	deleted  []string
	// hold, when set, holds the next batch before it is applied: the peer
	// closes entered, waits until hold is closed (without its lock), and then
	// applies and answers the batch. Each hold is used once.
	hold, entered chan struct{}
	// holdRead, when set, holds the next record listing the same way, before
	// it is answered. Each holdRead is used once. readCanceledAt, when
	// readCanceled is set, is len(tokens) at the moment the client canceled
	// a held listing (its connection closed while the peer held it).
	holdRead, readEntered chan struct{}
	readCanceled          bool
	readCanceledAt        int
	// accept, when set, lists the bearer credentials the peer accepts
	// (default test-only-private-credential); tokens lists the credential of
	// every request it served, in order, and contents the record contents
	// every batch wrote.
	accept   []string
	tokens   []string
	contents []string
	// zones names further zone IDs the peer serves (ID to zone name). Rows
	// are kept per zone; the default zone is integrationCloudflareZone.
	zones map[string]string
}

const integrationCloudflareZone = "11111111111111111111111111111111"
const integrationCloudflareAccount = "22222222222222222222222222222222"

func integrationCloudflareTLS(t *testing.T) *integrationCloudflarePeer {
	t.Helper()
	peer := &integrationCloudflarePeer{rows: map[string]map[string]any{}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		accept := peer.accept
		if accept == nil {
			accept = []string{"test-only-private-credential"}
		}
		accepted, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Host != "api.cloudflare.com" || !bearer || !slices.Contains(accept, accepted) {
			t.Error("original Cloudflare origin or credential missing")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		peer.requests++
		peer.tokens = append(peer.tokens, accepted)
		w.Header().Set("Content-Type", "application/json")
		zone, zoneName, path := integrationCloudflareZone, "example", r.URL.Path
		for id, name := range peer.zones {
			if rest, ok := strings.CutPrefix(path, "/client/v4/zones/"+id); ok {
				zone, zoneName, path = id, name, "/client/v4/zones/"+integrationCloudflareZone+rest
			}
		}
		inZone := func(row map[string]any) bool {
			id, _ := row["zone_id"].(string)
			return id == zone || id == "" && zone == integrationCloudflareZone
		}
		success := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result})
		}
		switch path {
		case "/client/v4/user/tokens/verify":
			success(map[string]any{"id": integrationCloudflareAccount, "status": "active"})
		case "/client/v4/zones/" + integrationCloudflareZone:
			success(map[string]any{"id": zone, "name": zoneName, "account": map[string]any{"id": integrationCloudflareAccount}, "plan": map[string]any{"name": "Free Website"}})
		case "/client/v4/zones/" + integrationCloudflareZone + "/dns_records":
			if peer.holdRead != nil {
				hold, entered := peer.holdRead, peer.readEntered
				peer.holdRead, peer.readEntered = nil, nil
				peer.mu.Unlock()
				stop := context.AfterFunc(r.Context(), func() {
					peer.mu.Lock()
					defer peer.mu.Unlock()
					peer.readCanceled, peer.readCanceledAt = true, len(peer.tokens)
				})
				close(entered)
				<-hold
				stop()
				peer.mu.Lock()
			}
			if peer.unreadable {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			size, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			if page != 1 || size != 500 {
				t.Error("unexpected bounded SDK pagination")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			rows := []any{}
			for _, row := range peer.rows {
				if inZone(row) {
					rows = append(rows, row)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": rows, "result_info": map[string]any{"page": 1, "per_page": 500, "count": len(rows), "total_count": len(rows), "total_pages": 1}})
		case "/client/v4/zones/" + integrationCloudflareZone + "/dns_records/batch":
			if peer.hold != nil {
				hold, entered := peer.hold, peer.entered
				peer.hold, peer.entered = nil, nil
				peer.mu.Unlock()
				close(entered)
				<-hold
				peer.mu.Lock()
			}
			if r.Method != http.MethodPost {
				t.Error("unexpected effect method")
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var batch map[string][]map[string]any
			if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&batch) != nil {
				t.Error("malformed SDK batch")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			peer.writes++
			result := map[string]any{}
			for _, kind := range []string{"deletes", "patches", "puts", "posts"} {
				list := []any{}
				for _, body := range batch[kind] {
					id, _ := body["id"].(string)
					if kind == "deletes" {
						peer.deleted = append(peer.deleted, id)
						delete(peer.rows, id)
						list = append(list, map[string]any{"id": id})
						continue
					}
					if kind == "posts" {
						peer.next++
						id = fmt.Sprintf("%032x", peer.next)
					}
					row := map[string]any{}
					if kind == "patches" {
						for k, v := range peer.rows[id] {
							row[k] = v
						}
					}
					for k, v := range body {
						row[k] = v
					}
					if content, ok := body["content"].(string); ok {
						peer.contents = append(peer.contents, content)
					}
					row["id"] = id
					if zone != integrationCloudflareZone {
						row["zone_id"] = zone
					}
					peer.rows[id] = row
					list = append(list, row)
				}
				result[kind] = list
			}
			if peer.ambiguous {
				peer.unreadable = true
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			success(result)
		default:
			t.Error("unexpected Cloudflare SDK operation")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"api.cloudflare.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	transport.Proxy = nil
	trust := x509.NewCertPool()
	trust.AddCert(parsed)
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: trust}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "api.cloudflare.com:443" {
			return nil, errors.New("fixture refuses foreign origin")
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original; transport.CloseIdleConnections(); server.Close() })
	return peer
}

func integrationAdminSettled(t *testing.T, f *integrationOperationFixture) {
	t.Helper()
	awaitServingRuntime(t, f.s, func() bool {
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		base, current := lease.Bundle().Identity().Base, f.s.opts.Store.Authority().Installed
		return base.Term.Same(current.Term) && base.Revision == current.Revision
	})
}

func TestIntegrationAdminActualCloudflareTLSPublication(t *testing.T) {
	peer := integrationCloudflareTLS(t)
	f := &integrationOperationFixture{}
	f.s, f.path = integrationFixture(t, nil, nil)
	integrationOperationCaller(t, f)
	record := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "published", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80"}}}}}
	if _, err := f.s.opts.Store.Apply(t.Context(), record, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	d := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "publisher", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "cloudflare", Resync: metav1.Duration{Duration: time.Hour}, Credentials: map[string]public.CredentialReference{"apiToken": {File: f.path}}, Cloudflare: &public.CloudflareIntegration{Zones: []public.CloudflareZone{{Zone: "example", ZoneID: integrationCloudflareZone}}}}}
	integrationOperationStart(t, f, d)
	router := integrationAdminRouter(t, f)
	path := "/integrations/default/publisher"
	// Remote drift requires a new operator preview, without a source publication
	// or a background wake that could repair it before this explicit operation.
	peer.mu.Lock()
	peer.rows = map[string]map[string]any{}
	before := peer.writes
	peer.mu.Unlock()
	review := api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion}
	w := integrationAdminCall(t, router, f, http.MethodPost, path+"/preview", review, f.token)
	if w.Code != http.StatusOK {
		t.Fatal("Cloudflare preview refused", w.Code)
	}
	if strings.Contains(w.Body.String(), integrationCloudflareZone) || strings.Contains(w.Body.String(), integrationCloudflareAccount) {
		t.Fatal("preview disclosed private destination IDs")
	}
	var result api.IntegrationExecutionResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Preview == nil || len(result.Preview.Operations) == 0 {
		t.Fatal("remote drift did not produce reviewed operations")
	}
	integrationAdminSettled(t, f)
	review.Mode = "publish"
	originalDigest := result.Preview.Digest
	review.PreviewDigest = strings.Repeat("a", 64)
	w = integrationAdminCall(t, router, f, http.MethodPost, path+"/sync", review, f.token)
	peer.mu.Lock()
	refusedWrites := peer.writes
	peer.mu.Unlock()
	if w.Code != http.StatusConflict || refusedWrites != before {
		t.Fatal("stale preview reached effect", w.Code)
	}
	integrationAdminSettled(t, f)
	review.PreviewDigest = originalDigest
	w = integrationAdminCall(t, router, f, http.MethodPost, path+"/sync", review, f.token)
	if w.Code != http.StatusOK {
		t.Fatal("Cloudflare reviewed sync refused", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	peer.mu.Lock()
	writes, count := peer.writes, len(peer.rows)
	peer.mu.Unlock()
	if !result.Completed || result.Uncertain || result.Applied == 0 || writes <= before || count == 0 {
		t.Fatal("real Cloudflare effects were not acknowledged")
	}
	// A distinct operator review starts a new operation after acknowledged status.
	integrationAdminSettled(t, f)
	w = integrationAdminCall(t, router, f, http.MethodGet, path, nil, f.token)
	var summary api.IntegrationSummary
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &summary) != nil {
		t.Fatal("new review unavailable")
	}
	review = api.IntegrationReview{UID: summary.UID, Revision: summary.Revision}
	awaitServingRuntime(t, f.s, func() bool {
		_, err := f.s.integrations.host.CaptureCommand(t.Context(), integrationOwner(f.s, f.d), review.Revision)
		return err == nil
	})
	peer.mu.Lock()
	peer.rows = map[string]map[string]any{}
	peer.ambiguous = true
	before = peer.writes
	peer.mu.Unlock()
	w = integrationAdminCall(t, router, f, http.MethodPost, path+"/preview", review, f.token)
	result = api.IntegrationExecutionResult{}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Preview == nil {
		t.Fatal("new drift preview unavailable", w.Code)
	}
	integrationAdminSettled(t, f)
	review.Mode = "publish"
	review.PreviewDigest = result.Preview.Digest
	w = integrationAdminCall(t, router, f, http.MethodPost, path+"/sync", review, f.token)
	var refusal struct {
		Error struct {
			Key  string                         `json:"key"`
			Hint api.IntegrationExecutionResult `json:"hint"`
		} `json:"error"`
	}
	if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Error.Key != "integration_uncertain" || !refusal.Error.Hint.Uncertain || refusal.Error.Hint.Pending == 0 {
		t.Fatal("ambiguous effect lost pending refusal", w.Code)
	}
	peer.mu.Lock()
	writes = peer.writes
	count = len(peer.rows)
	peer.mu.Unlock()
	if writes != before+1 || count == 0 {
		t.Fatal("ambiguous actual effect was replayed or missing")
	}
}

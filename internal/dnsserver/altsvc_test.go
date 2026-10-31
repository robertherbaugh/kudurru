package dnsserver

import (
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go/http3"

	"github.com/robertherbaugh/kudurru/internal/dnsserver/internal/testcert"
)

func alternativeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestDoHAdvertisesMatchingLiveHTTP3(t *testing.T) {
	address := alternativeAddress(t)
	f := startFixture(t, fixtureOpts{specs: []Spec{
		{Protocol: ProtoDoH, Address: address, ClientIDs: true},
		{Protocol: ProtoDoH3, Address: address, ClientIDs: true},
	}})
	_, port, _ := net.SplitHostPort(address)
	want := `h3=":` + port + `"; ma=60`
	q := query("example.com")
	q.Id = 0
	wire, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	url := "https://" + address + "/dns-query/phone?dns=" + base64.RawURLEncoding.EncodeToString(wire)
	for _, h2 := range []bool{false, true} {
		client := dohClient(t, f, h2)
		res, err := client.Get(url)
		if err != nil {
			t.Fatal("actual HTTPS query failed")
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK || res.Header.Get("Alt-Svc") != want || (res.ProtoMajor == 2) != h2 {
			t.Fatal("matching HTTPS endpoint did not advertise its active HTTP/3 alternative")
		}
		for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Port", "X-Forwarded-Proto"} {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
			req.Header.Set(header, "untrusted")
			res, err := client.Do(req)
			if err != nil {
				t.Fatal("proxied HTTPS query failed")
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK || res.Header.Get("Alt-Svc") != "" {
				t.Fatal("forwarding headers must suppress native alternative discovery")
			}
		}
	}
	// Follow the advertised same-authority endpoint with an actual verifying
	// HTTP/3 client, preserving the path and client policy selector.
	tr := &http3.Transport{TLSClientConfig: (&tlsPool{f.cert}).client()}
	t.Cleanup(func() { _ = tr.Close() })
	res, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		t.Fatal("advertised HTTP/3 endpoint is not reachable")
	}
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	answer := new(dns.Msg)
	if err != nil || res.StatusCode != http.StatusOK || res.ProtoMajor != 3 || answer.Unpack(body) != nil {
		t.Fatal("advertised HTTP/3 endpoint did not answer DNS")
	}
	checkA(t, answer)
	clientIdentity, _ := f.rec.last(t)
	if clientIdentity.ClientID != "phone" {
		t.Fatal("HTTP/3 alternative changed client policy identity")
	}
	doh := f.reg.Lookup(ProtoDoH)[0].(*dohListener)
	plain := httptest.NewRequest(http.MethodGet, "http://"+address+"/dns-query", nil)
	if doh.handler.altSvc(plain) != "" {
		t.Fatal("plaintext request advertised an HTTPS alternative")
	}
	h3 := f.reg.Lookup(ProtoDoH3)[0].(*doh3Listener)
	_ = h3.Close()
	deadline := time.Now().Add(time.Second)
	for h3.Ready() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h3.Ready() {
		t.Fatal("closed HTTP/3 listener remains ready")
	}
	secure := httptest.NewRequest(http.MethodGet, url, nil)
	if doh.handler.altSvc(secure) != "" {
		t.Fatal("closed HTTP/3 listener remains advertised")
	}
}

func TestDoHDoesNotAdvertiseDifferentEndpoint(t *testing.T) {
	for _, mismatch := range []string{"missing", "port", "path", "client policy", "certificate"} {
		t.Run(mismatch, func(t *testing.T) {
			address := alternativeAddress(t)
			specs := []Spec{{Protocol: ProtoDoH, Address: address}, {Protocol: ProtoDoH3, Address: address}}
			switch mismatch {
			case "missing":
				specs = specs[:1]
			case "port":
				specs[1].Address = "127.0.0.1:0"
			case "path":
				specs[1].Path = "/other"
			case "client policy":
				specs[1].ClientIDs = true
			case "certificate":
				pair, err := testcert.Write(t.TempDir(), "other")
				if err != nil {
					t.Fatal(err)
				}
				specs[1].CertFile, specs[1].KeyFile = pair.CertFile, pair.KeyFile
			}
			f := startFixture(t, fixtureOpts{specs: specs})
			if f.reg.Lookup(ProtoDoH)[0].(*dohListener).handler.altSvc != nil {
				t.Fatal("a different endpoint was linked as an HTTP/3 alternative")
			}
		})
	}
}

func TestHTTP3AlternativePreservesHTTPSAuthorityPort(t *testing.T) {
	for _, tc := range []struct {
		host, port string
		want       bool
	}{
		{"dns.example", "443", true},
		{"dns.example", "8443", false},
		{"dns.example:8443", "8443", true},
		{"dns.example:443", "8443", false},
		{"[::1]", "443", true},
		{"[::1]:8443", "8443", true},
		{"", "443", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "https://dns.example/dns-query", nil)
		r.Host = tc.host
		if directHTTPSAuthority(r, tc.port) != tc.want {
			t.Fatal("alternative did not preserve the original HTTPS authority port")
		}
	}
}

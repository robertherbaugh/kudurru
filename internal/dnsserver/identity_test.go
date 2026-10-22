package dnsserver

import (
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/internal/testcert"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type identityAnswer struct{}

func (identityAnswer) Name() string { return "local" }
func (identityAnswer) Serve(r *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	code := dns.RcodeRefused
	if slices.Equal(r.Client.GroupIDs, []string{"phone"}) {
		code = dns.RcodeSuccess
	}
	return pipeline.Reply(r, code, pipeline.StatusLocalRecord), nil
}

func TestTransportClientIDsMakeEquivalentPolicyDecisions(t *testing.T) {
	cert, err := testcert.Write(t.TempDir(), "", "*.dot.example.com")
	if err != nil {
		t.Fatal(err)
	}
	ix := policy.Build(policy.Input{ClientGroups: []*v1alpha1.ClientGroup{
		{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "0.0.0.0/0"}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "phone"}, Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{ClientID: "phone"}}}},
	}}, policy.Options{})
	f := startFixture(t, fixtureOpts{cert: cert, specs: []Spec{
		{Protocol: ProtoDoT, ClientIDDomain: "dot.example.com"}, {Protocol: ProtoDoQ, ClientIDDomain: "dot.example.com"},
		{Protocol: ProtoDoH, ClientIDs: true}, {Protocol: ProtoDoH3, ClientIDs: true},
	}, stages: []pipeline.Stage{identify.New(policy.Static{Idx: ix}, nil), identityAnswer{}}})
	for _, id := range []string{"phone", "other"} {
		want := dns.RcodeRefused
		if id == "phone" {
			want = dns.RcodeSuccess
		}
		tlsConfig := &tls.Config{RootCAs: cert.Pool, ServerName: id + ".dot.example.com", MinVersion: tls.VersionTLS12}
		client := &dns.Client{Net: "tcp-tls", TLSConfig: tlsConfig, Timeout: time.Second}
		answer, _, err := client.ExchangeContext(t.Context(), query("example.com"), f.addr(ProtoDoT))
		if err != nil || answer.Rcode != want {
			t.Fatalf("DoT %s: %v %v", id, answer, err)
		}
		tlsConfig = tlsConfig.Clone()
		tlsConfig.NextProtos = []string{"doq"}
		conn, err := quic.DialAddr(t.Context(), f.addr(ProtoDoQ), tlsConfig, &quic.Config{})
		if err != nil {
			t.Fatal(err)
		}
		q := query("example.com")
		q.Id = 0
		answer, err = doqExchange(conn, q)
		closeQUIC(conn)
		if err != nil || answer.Rcode != want {
			t.Fatalf("DoQ %s: %v %v", id, answer, err)
		}
		wire, _ := q.Pack()
		for _, proto := range []Protocol{ProtoDoH, ProtoDoH3} {
			var transport http.RoundTripper
			if proto == ProtoDoH {
				tr := &http.Transport{TLSClientConfig: (&tlsPool{cert}).client(), ForceAttemptHTTP2: true}
				t.Cleanup(tr.CloseIdleConnections)
				transport = tr
			} else {
				tr := &http3.Transport{TLSClientConfig: (&tlsPool{cert}).client()}
				t.Cleanup(func() { _ = tr.Close() })
				transport = tr
			}
			httpClient := &http.Client{Transport: transport, Timeout: time.Second}
			resp, err := httpClient.Post("https://"+f.addr(proto)+"/dns-query/"+id, dohMIME, bytes.NewReader(wire))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			answer = new(dns.Msg)
			if err := answer.Unpack(body); err != nil || answer.Rcode != want || !strings.HasPrefix(resp.Header.Get("Cache-Control"), "private, ") {
				t.Fatalf("%s %s: %s %v %v", proto, id, resp.Status, answer, err)
			}
		}
	}
	c := dohClient(t, f, true)
	for _, path := range []string{"/dns-query/", "/dns-query/phone/extra", "/dns-query/ph%6fne", "/dns-query/phone%2Fextra"} {
		resp, err := c.Post("https://"+f.addr(ProtoDoH)+path, dohMIME, bytes.NewReader([]byte{0}))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("path %s accepted: %s", path, resp.Status)
		}
	}
}

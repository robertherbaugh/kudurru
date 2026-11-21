package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestRunServeSQLiteRPZRestartCompileWindow(t *testing.T) {
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	const entries = 50_000
	const listed = "unsigned.rpzlab.net."
	const downloaded = "rule-49999.download.example."
	var body strings.Builder
	for index := range entries {
		fmt.Fprintf(&body, "rule-%05d.download.example\n", index)
	}
	listBody := body.String()
	var downloads atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, listBody)
	}))
	t.Cleanup(origin.Close)
	primary := kubeRPZOpenPrimary(t)
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.DNSSEC.Enabled = false
	cfg.Blocking.Enabled = true
	cfg.Blocking.AllowDegraded = true
	cfg.Blocking.AllowLocalListURLs = true
	cfg.Blocking.DefaultResponse = "nxdomain"
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{kubeRPZUpstream(t)}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
	st, closeStore := tsigMountedStore(t, cfg)
	for _, object := range []store.Object{
		&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "feed", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block",
			Sources: []public.ListSource{{Name: "native", Format: "rpz", RPZ: &public.RPZSource{ZoneName: "open.test.", Primaries: []string{primary.address}}}}}},
		&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "download", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block",
			Sources: []public.ListSource{{Name: "remote", Format: "domains", URL: origin.URL}}}},
		&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: public.PolicySpec{BlockListRefs: []string{"feed", "download"}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	closeStore()
	stop := tsigMountedStart(t, cfg)
	tsigMountedAwaitRcode(t, cfg, listed, dns.RcodeNameError, true)
	tsigMountedAwaitRcode(t, cfg, downloaded, dns.RcodeNameError, true)
	if got := kubeRPZRcode(cfg, "unlisted.example.", "tcp", nil); got != dns.RcodeSuccess || downloads.Load() == 0 || primary.transfers.Load() == 0 {
		t.Fatalf("fixture did not prime both lists against a working upstream: unlisted rcode=%d downloads=%d transfers=%d", got, downloads.Load(), primary.transfers.Load())
	}
	stop()
	primary.available.Store(false)
	origin.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	query := new(dns.Msg)
	query.SetQuestion(listed, dns.TypeA)
	client := &dns.Client{Net: "tcp", Timeout: 50 * time.Millisecond}
	probe := func() (*dns.Msg, error) {
		answer, _, err := client.ExchangeContext(ctx, query, cfg.Listeners.DNS[0].Address)
		return answer, err
	}
	if answer, err := probe(); err == nil || answer != nil {
		t.Fatalf("old DNS listener still answered before restart: %v", answer)
	}
	began := time.Now()
	stop = tsigMountedStart(t, cfg)
	defer stop()
	var firstResponse, firstFiltered time.Time
	var probes, answers, servfails int
	for ctx.Err() == nil {
		probes++
		answer, err := probe()
		if err == nil && answer != nil {
			at := time.Now()
			answers++
			if firstResponse.IsZero() {
				firstResponse = at
			}
			switch answer.Rcode {
			case dns.RcodeNameError:
				if firstFiltered.IsZero() {
					firstFiltered = at
				}
			case dns.RcodeServerFailure:
				servfails++
			default:
				t.Fatalf("listed RPZ name answered %s at %s (probe %d)", dns.RcodeToString[answer.Rcode], at.Sub(began), probes)
			}
			if !firstFiltered.IsZero() && offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK &&
				kubeRPZRcode(cfg, downloaded, "tcp", nil) == dns.RcodeNameError {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatalf("restart never became ready with both RPZ and the persisted %d-entry download enforced: probes=%d answers=%d SERVFAIL=%d", entries, probes, answers, servfails)
	}
	t.Logf("SQLite restart, %d persisted downloaded entries: first DNS response %s after restart; first response to first RPZ NXDOMAIN %s; %d probes, %d answers, %d SERVFAIL, no NOERROR", entries, firstResponse.Sub(began), firstFiltered.Sub(firstResponse), probes, answers, servfails)
}

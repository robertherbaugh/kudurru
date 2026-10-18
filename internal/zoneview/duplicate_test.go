package zoneview

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

func TestDuplicateApexIsolationAndRecovery(t *testing.T) {
	for _, coldStart := range []bool{false, true} {
		name := "last good"
		if coldStart {
			name = "cold start"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			st := memory.New()
			t.Cleanup(func() { _ = st.Close() })
			apply := func(obj store.Object) {
				t.Helper()
				if _, err := st.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					t.Fatal(err)
				}
			}
			primary := func(name, apex string) *v1alpha1.DNSZone {
				return &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.DNSZoneSpec{ZoneName: apex, Type: v1alpha1.ZoneTypePrimary}}
			}
			records := func(name, zone, owner, ip string) *v1alpha1.DNSRecordSet {
				return &v1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"}, Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: zone}, Endpoints: []v1alpha1.Endpoint{{DNSName: owner, RecordType: "A", Targets: []string{ip}}}}}
			}
			apply(primary("original", "Home.Example."))
			apply(records("home", "original", "nas.home.example.", "192.0.2.1"))
			apply(primary("healthy", "healthy.example."))
			apply(records("healthy", "healthy", "nas.healthy.example.", "192.0.2.10"))
			// Sorts before original; a live conflict must not replace last good.
			conflict := primary("aaa-conflict", "home.example.")
			conflictingRecords := records("conflict", "aaa-conflict", "nas.home.example.", "192.0.2.99")
			if coldStart {
				apply(conflict)
				apply(conflictingRecords)
			}
			zc, err := zonecache.New(st, zonecache.Options{Debounce: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if err := zc.Register(Builder(nil)); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); _ = zc.Start(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			stage := authoritative.New(Source{Cache: zc})
			answerIP := func(owner string) string {
				m := new(dns.Msg)
				m.SetQuestion(owner, dns.TypeA)
				forwarded := false
				res, err := stage.Serve(&pipeline.Request{Msg: m}, func(*pipeline.Request) (*pipeline.Response, error) { forwarded = true; return nil, nil })
				if forwarded || err != nil || res == nil || res.Msg == nil || res.Msg.Rcode != dns.RcodeSuccess || len(res.Msg.Answer) != 1 {
					return ""
				}
				a, ok := res.Msg.Answer[0].(*dns.A)
				if !ok || !res.Msg.Authoritative {
					return ""
				}
				return a.A.String()
			}
			waitIP := func(owner, ip string) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for answerIP(owner) != ip {
					if time.Now().After(deadline) {
						t.Fatalf("%s never served %s (got %s)", owner, ip, answerIP(owner))
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			wantHome := "192.0.2.1"
			if coldStart {
				wantHome = "192.0.2.99"
			}
			waitIP("nas.home.example.", wantHome)
			waitIP("nas.healthy.example.", "192.0.2.10")
			if !coldStart {
				apply(conflict)
				apply(conflictingRecords)
			}
			// Both a new unrelated zone and an update must publish despite conflict.
			apply(primary("new-zone", "new.example."))
			apply(records("new", "new-zone", "nas.new.example.", "192.0.2.20"))
			apply(records("healthy", "healthy", "nas.healthy.example.", "192.0.2.11"))
			apply(records("home", "original", "nas.home.example.", "192.0.2.2"))
			waitIP("nas.new.example.", "192.0.2.20")
			waitIP("nas.healthy.example.", "192.0.2.11")
			if got := answerIP("nas.home.example."); got != wantHome {
				t.Fatalf("conflict replaced last-good answer: %s, want %s", got, wantHome)
			}
			if err := st.Delete(ctx, store.Key{Kind: "DNSZone", Name: "aaa-conflict"}, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
				t.Fatal(err)
			}
			waitIP("nas.home.example.", "192.0.2.2")
		})
	}
}

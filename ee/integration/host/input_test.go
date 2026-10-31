package host

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func captureInput(t testing.TB, definitions ...*public.Integration) Input {
	t.Helper()
	st := memory.New()
	for _, d := range definitions {
		if _, err := st.Apply(context.Background(), d, store.ApplyOptions{Manager: "test", CreateOnly: true}); err != nil {
			t.Fatal(err)
		}
	}
	cache, err := zonecache.New(st, zonecache.Options{Kinds: []store.Kind{"Integration"}, Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	published := make(chan zonecache.ObjectSet, 1)
	cache.OnPublish(func(s *zonecache.Snapshot) {
		if s.Objects.Count("Integration") == len(definitions) {
			select {
			case published <- s.Objects:
			default:
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- cache.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	var objects zonecache.ObjectSet
	select {
	case objects = <-published:
	case <-ctx.Done():
		t.Fatal("capture cache startup timed out")
	}
	return Input{Objects: objects, Term: replication.Term{ClusterID: "cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "promotion"}, BundleID: "bundle-1", Complete: true}
}
func cloudflare(name string) *public.Integration {
	return &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "dns", UID: "uid-" + metav1.NamespaceDefault}, Spec: public.IntegrationSpec{Type: "cloudflare", Cloudflare: &public.CloudflareIntegration{Zones: []public.CloudflareZone{{Zone: "example.com"}}}, Credentials: map[string]public.CredentialReference{"apiToken": {Env: "TEST_TOKEN"}}}}
}
func TestCapturedDefinitionCredentialsAndRotation(t *testing.T) {
	d := cloudflare("public")
	in := captureInput(t, d)
	token := "private-token-A"
	r, err := credentials.New(credentials.Options{Environment: []string{"TEST_TOKEN"}, FingerprintKey: []byte(strings.Repeat("k", 32)), Getenv: func(name string) (string, bool) { return token, name == "TEST_TOKEN" }})
	if err != nil {
		t.Fatal(err)
	}
	in.Credentials = r
	instances, err := CaptureInstances(context.Background(), in)
	if err != nil || len(instances) != 1 {
		t.Fatal(instances, err)
	}
	i := instances[0]
	g, err := i.Guard("lease-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := i.Secrets().Resolve(context.Background(), "apiToken")
	if err != nil || got != token {
		t.Fatal("named credential not delegated")
	}
	for _, ref := range []string{"TEST_TOKEN", "env:TEST_TOKEN", "/etc/passwd", "secret:other/key"} {
		if _, err := i.Secrets().Resolve(context.Background(), ref); !errors.Is(err, credentials.ErrDenied) {
			t.Fatal("unlisted credential reference resolved", ref, err)
		}
	}
	if strings.Contains(string(i.ProviderSpec()), token) || strings.Contains(g.CredentialDigest, token) || strings.Contains(i.CredentialFingerprints()["apiToken"], token) {
		t.Fatal("credential value escaped")
	}
	definitionCopy := i.Definition()
	definitionCopy.Spec.Cloudflare.Zones[0].Zone = "mutated"
	fingerprints := i.CredentialFingerprints()
	fingerprints["apiToken"] = "mutated"
	spec := i.ProviderSpec()
	spec[0] = '!'
	if i.Definition().Spec.Cloudflare.Zones[0].Zone != "example.com" || i.CredentialFingerprints()["apiToken"] == "mutated" || i.ProviderSpec()[0] == '!' {
		t.Fatal("mutable capture escaped")
	}
	token = "private-token-B"
	next, err := CaptureInstances(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	newGuard, err := next[0].Guard("lease-2")
	if err != nil {
		t.Fatal(err)
	}
	if g.CredentialDigest == newGuard.CredentialDigest || g.ConfigDigest != newGuard.ConfigDigest || g.Owner != newGuard.Owner {
		t.Fatal("rotation lost stable UID/spec or fingerprint")
	}
	old, err := i.Secrets().Resolve(context.Background(), "apiToken")
	if err != nil || old != "private-token-A" {
		t.Fatal("old capture substituted live credentials")
	}
}
func TestCredentialFailureIsolatesInstanceAndNeverReusesValues(t *testing.T) {
	good := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "good", Namespace: "dns"}, Spec: public.IntegrationSpec{Type: "webhook", Webhook: &public.WebhookIntegration{Endpoint: "https://provider.example"}}}
	bad := cloudflare("bad")
	in := captureInput(t, good, bad)
	instances, err := CaptureInstances(context.Background(), in)
	if err != nil || len(instances) != 2 {
		t.Fatal(err)
	}
	if instances[0].Owner().Name != "bad" || !errors.Is(instances[0].CredentialError(), ErrCredentials) || instances[1].CredentialError() != nil {
		t.Fatal("failure leaked across independent instances")
	}
	if _, err := instances[0].Guard("lease"); !errors.Is(err, ErrCredentials) {
		t.Fatal("unresolved credentials produced execution guard")
	}
	if _, err := instances[0].Secrets().Resolve(context.Background(), "apiToken"); !errors.Is(err, ErrCredentials) {
		t.Fatal(err)
	}
}
func TestCaptureRejectsIncompleteUnsupportedAndCanceledInput(t *testing.T) {
	in := captureInput(t)
	in.Complete = false
	if _, err := CaptureInstances(context.Background(), in); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	in.Complete = true
	in.Term.Epoch = 0
	if _, err := CaptureInstances(context.Background(), in); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	bad := cloudflare("bad")
	bad.Spec.Webhook = &public.WebhookIntegration{Endpoint: "https://provider.example"}
	in = captureInput(t, bad)
	if _, err := CaptureInstances(context.Background(), in); !errors.Is(err, ErrInvalid) {
		t.Fatal("provider one-of accepted", err)
	}
	in = captureInput(t, cloudflare("valid"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CaptureInstances(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCaptureRejectsMalformedPerInstanceSourceDigest(t *testing.T) {
	in := captureInput(t, webhook("source"))
	in.ReconcileDigests = map[InstanceID]string{{ClusterID: "cluster", Namespace: "dns", Name: "source", UID: "original"}: "not-a-digest"}
	if _, err := CaptureInstances(t.Context(), in); err != ErrInvalid {
		t.Fatal("malformed source digest accepted")
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
)

type kubeRotationProvider struct {
	*integrationLifecycleSignals
	oldCredential bool
}

type integrationLifecycleSignals struct {
	oldStarted, stopping, allowStop, stopped chan struct{}
	newProvisioned                           chan bool
	startedOnce, stoppingOnce, stoppedOnce   sync.Once
}

func (*kubeRotationProvider) Manifest() integration.Manifest {
	return integration.Manifest{Type: "cloudflare"}
}

func (p *kubeRotationProvider) Provision(ctx context.Context, _ integration.Host, _ json.RawMessage, credentials integration.SecretResolver) error {
	value, err := credentials.Resolve(ctx, "apiToken")
	if err != nil {
		return err
	}
	switch value {
	case "rotation-old":
		p.oldCredential = true
	case "rotation-new":
		select {
		case <-p.stopped:
			p.newProvisioned <- true
		default:
			p.newProvisioned <- false
		}
	default:
		return errors.New("fixture did not receive the expected credential version")
	}
	return nil
}

func (*kubeRotationProvider) Validate(context.Context) error { return nil }

func (*kubeRotationProvider) Check(context.Context) (integration.Status, error) {
	return integration.Status{Authenticated: true, Reachable: true}, nil
}

func (p *kubeRotationProvider) Start(ctx context.Context) error {
	if p.oldCredential {
		p.startedOnce.Do(func() { close(p.oldStarted) })
	}
	<-ctx.Done()
	return nil
}

func (p *kubeRotationProvider) Stop(ctx context.Context) error {
	if !p.oldCredential {
		return nil
	}
	p.stoppingOnce.Do(func() { close(p.stopping) })
	select {
	case <-p.allowStop:
		p.stoppedOnce.Do(func() { close(p.stopped) })
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRunServeKubeCredentialRotationJoinsPreviousStop(t *testing.T) {
	env := newKubeIntegrationEnv(t)
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte("rotation-old"), 0o600); err != nil {
		t.Fatal(err)
	}
	signals := &integrationLifecycleSignals{oldStarted: make(chan struct{}), stopping: make(chan struct{}), allowStop: make(chan struct{}), stopped: make(chan struct{}), newProvisioned: make(chan bool, 8)}
	release := sync.OnceFunc(func() { close(signals.allowStop) })
	gate := newPartitionGate(t, env.rest)
	cfg := env.replica(t, "rotation", gate, credential)
	_ = kubeIntegrationMountWithFactory(t, cfg, func(string) (integration.Integration, error) {
		return &kubeRotationProvider{integrationLifecycleSignals: signals}, nil
	})
	t.Cleanup(release)
	definition := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "rotation", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "cloudflare", Credentials: map[string]public.CredentialReference{"apiToken": {File: credential}}, Cloudflare: &public.CloudflareIntegration{Zones: []public.CloudflareZone{{Zone: "example", ZoneID: integrationCloudflareZone}}}}}
	if err := env.admin.Create(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signals.oldStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the first provider never started")
	}
	replacement := credential + ".new"
	if err := os.WriteFile(replacement, []byte("rotation-new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, credential); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signals.stopping:
	case <-time.After(30 * time.Second):
		t.Fatal("rotation did not stop the old provider")
	}
	select {
	case <-signals.newProvisioned:
		t.Fatal("the replacement provisioned while the previous Stop was blocked")
	case <-time.After(time.Second):
	}
	release()
	select {
	case joined := <-signals.newProvisioned:
		if !joined {
			t.Fatal("the replacement provisioned before the previous Stop returned")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the replacement never provisioned after Stop returned")
	}
}

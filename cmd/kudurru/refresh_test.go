package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

type staticObjects []*v1alpha1.BlockList

func (o staticObjects) BlockLists() []*v1alpha1.BlockList { return listfetcher.EnabledBlockLists(o) }
func (staticObjects) Subscribe() <-chan struct{}          { return make(chan struct{}) }
func (staticObjects) Initialized() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (staticObjects) Live() bool { return true }

type replica struct{ leader string }

func (r replica) Leader(context.Context) (cluster.Peer, error) {
	return cluster.Peer{Name: r.leader}, nil
}
func (r replica) Peers(context.Context) ([]cluster.Peer, error) { return nil, nil }

func TestListRefresher(t *testing.T) {
	cache, err := listfetcher.OpenDiskCache(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	objs := staticObjects{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ads"}, Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{URL: "https://lists.example/ads"}}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "off", Annotations: map[string]string{v1alpha1.EnabledAnnotation: "false"}},
			Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{URL: "https://lists.example/off"}}}},
	}
	f, err := listfetcher.New(listfetcher.Options{Store: memory.New(), Objects: objs, Cache: cache,
		Downloader: listfetcher.NewDownloader(cache, listfetcher.DownloadOptions{}), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := listRefresher(cluster.Self{Name: "a"}, f).RefreshList(ctx, "default", "ads"); err != nil {
		t.Fatalf("leader: %v", err)
	}
	status := func(err error) int {
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("not an API error: %v", err)
		}
		return apiErr.Status
	}
	if got := status(listRefresher(cluster.Self{Name: "a"}, f).RefreshList(ctx, "default", "off")); got != http.StatusConflict {
		t.Fatalf("disabled list: %d", got)
	}
	if got := status(listRefresher(replica{leader: "b"}, f).RefreshList(ctx, "default", "ads")); got != http.StatusConflict {
		t.Fatalf("replica: %d", got)
	}
	if got := status(listRefresher(replica{}, f).RefreshList(ctx, "default", "ads")); got != http.StatusServiceUnavailable {
		t.Fatalf("no leader: %d", got)
	}
	if querylogPurger(nil) != nil {
		t.Fatal("no sqlite writer: DELETE /querylog stays 501")
	}
}

func TestLeaderResolverFallsBackToTheName(t *testing.T) {
	byName := func(_ context.Context, pod string) (string, error) { return pod + ".svc:8082", nil }
	for _, headless := range []string{"", "kudurru-headless.kudurru.svc.cluster.local"} {
		r, source, err := leaderResolver(nil, headless, "8082", byName)
		if err != nil {
			t.Fatal(err)
		}
		if source != nil {
			t.Fatal("no cluster config: unexpected membership source")
		}
		if got, _ := r(context.Background(), "kudurru-0"); got != "kudurru-0.svc:8082" {
			t.Fatalf("%q: %q", headless, got)
		}
	}
	// With a cluster config the EndpointSlice client is built (no request
	// is made until the leader is looked up).
	r, source, err := leaderResolver(&rest.Config{Host: "https://127.0.0.1:1"}, "kudurru-headless.kudurru.svc.cluster.local", "8082", byName)
	if err != nil || r == nil || source == nil || source.Name() != "endpointslice:kudurru/kudurru-headless" {
		t.Fatalf("endpointslice resolver: %v", err)
	}
	// The apiserver is unreachable: the DNS name is the fallback.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got, err := r(ctx, "kudurru-0"); err != nil || got != "kudurru-0.svc:8082" {
		t.Fatalf("fallback: %q %v", got, err)
	}
}

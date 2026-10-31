package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestProviderDependencyCaptureRequiresSameHostInstallation(t *testing.T) {
	for _, scenario := range []string{"current", "different-host", "different-expected", "missing-provider-inventory", "provider-error", "base-error", "stale-base", "stale-resource"} {
		t.Run(scenario, func(t *testing.T) {
			var baseReleased, providerReleased atomic.Int64
			var baseCurrent, resourceCurrent atomic.Bool
			baseCurrent.Store(scenario != "stale-base")
			resourceCurrent.Store(scenario != "stale-resource")
			image := &replication.Image{Revision: 17}
			calls := 0
			failed := errors.New("capture failed")
			capture, err := providerDependencyCapture(func(_ context.Context, got *replication.Image) (servingbuild.Dependencies, error) {
				if got != image {
					t.Fatal("base image changed")
				}
				d := servingbuild.Dependencies{Identity: "base/v1", Auth: servingauth.Dependencies{Identity: "host/v1", ExpectedIdentity: "host/v1"}, Current: baseCurrent.Load, Release: func() { baseReleased.Add(1) }}
				if scenario == "base-error" {
					return d, failed
				}
				if scenario == "different-expected" {
					d.Auth.ExpectedIdentity = "host/v2"
				}
				return d, nil
			}, func(_ context.Context, got *replication.Image) (providercoordinator.Capture, error) {
				calls++
				if got != image {
					t.Fatal("provider image changed")
				}
				p := providercoordinator.Capture{Identity: "provider/v1", HostIdentity: "host/v1", ConfigProviders: []servingauth.ProviderSource{}, ProviderResources: []servingauth.ProviderResource{}, Current: resourceCurrent.Load, Release: func() { providerReleased.Add(1) }}
				if scenario == "different-host" {
					p.HostIdentity = "host/v2"
				}
				if scenario == "missing-provider-inventory" {
					p.ProviderResources = nil
				}
				if scenario == "provider-error" {
					return p, failed
				}
				return p, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			d, err := capture(t.Context(), image)
			if d.Release == nil {
				t.Fatal("capture lost ownership on return")
			}
			if scenario != "current" {
				d.Release()
				if scenario != "base-error" {
					d.Release()
				}
			}
			if scenario == "base-error" {
				if calls != 0 || baseReleased.Load() != 1 || providerReleased.Load() != 0 || !errors.Is(err, failed) {
					t.Fatal("base failure acquired providers or leaked ownership", err)
				}
				return
			}
			if calls != 1 || scenario != "current" && (baseReleased.Load() != 1 || providerReleased.Load() != 1) {
				t.Fatal("partial/complete ownership leaked or released twice")
			}
			if scenario != "current" {
				if err == nil || d.Current() {
					t.Fatal("mismatched/failed cuts combined", err)
				}
				return
			}
			if err != nil || !d.Current() || d.Identity == "base/v1" || d.Auth.Identity != "provider/v1" || d.Auth.ExpectedIdentity != d.Auth.Identity || d.Auth.ConfigProviders == nil || d.Auth.ProviderResources == nil {
				t.Fatal("complete provider cut not propagated", err)
			}
			baseCurrent.Store(false)
			if d.Current() {
				t.Fatal("base invalidation lost")
			}
			baseCurrent.Store(true)
			resourceCurrent.Store(false)
			if d.Current() {
				t.Fatal("provider invalidation lost")
			}
			resourceCurrent.Store(true)
			d.Release()
			d.Release()
			if d.Current() || baseReleased.Load() != 1 || providerReleased.Load() != 1 {
				t.Fatal("release retained current authority or leaked/doubled references")
			}
		})
	}
	if _, err := providerDependencyCapture(nil, nil); !errors.Is(err, serving.ErrComponents) {
		t.Fatal(err)
	}
}

package main

import (
	"context"
	"io"
	"net/http"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/robertherbaugh/kudurru/ee/edge/vip"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// serviceVIPObservation mounts explicit host-selected observation only. It
// copies the installed REST configuration, preserving its credentials, trust,
// impersonation and transport wrappers. No supervisor or writer is started.
func serviceVIPObservation(cfg *config.Config, installed *rest.Config) (func() api.VIPObservationSnapshot, func(context.Context) error, error) {
	settings := cfg.VIP.Observation
	if settings.Provider == "none" || settings.Provider == "" {
		return nil, nil, nil
	}
	unavailable := func() api.VIPObservationSnapshot {
		result := api.VIPObservationSnapshot{Provider: settings.Provider, VIPs: []api.VIPOwnerObservation{}}
		for _, b := range settings.Services {
			result.VIPs = append(result.VIPs, api.VIPOwnerObservation{Name: b.Name, Address: b.Address, Reason: "source_unavailable"})
		}
		return result
	}
	if installed == nil {
		var err error
		installed, err = ctrlconfig.GetConfig()
		if err != nil {
			return unavailable, nil, err
		}
	}
	captured := rest.CopyConfig(installed)
	if captured.Timeout <= 0 || captured.Timeout > settings.Timeout.Std() {
		captured.Timeout = settings.Timeout.Std()
	}
	captured.Wrap(func(inner http.RoundTripper) http.RoundTripper { return vipObservationTransport{inner: inner} })
	client, err := dynamic.NewForConfig(captured)
	if err != nil {
		return unavailable, nil, err
	}
	options := vip.ServiceObserverConfig{Kind: settings.Provider, AnnouncerNamespace: settings.AnnouncerNamespace, Interval: settings.Interval.Std(), Timeout: settings.Timeout.Std(), MaxAge: settings.MaxAge.Std()}
	for _, b := range settings.Services {
		options.Bindings = append(options.Bindings, vip.ServiceBinding{Name: b.Name, Address: b.Address, Namespace: b.Namespace, Service: b.Service})
	}
	observer, err := vip.NewServiceObserver(client, options)
	if err != nil {
		return unavailable, nil, err
	}
	read := func() api.VIPObservationSnapshot { return apiVIPObservation(observer.Snapshot()) }
	return read, observer.Run, nil
}

// Bound every observation response without replacing the installed transport
// or its authorization headers. The enclosing acquisition has a total deadline.
type vipObservationTransport struct{ inner http.RoundTripper }

func (t vipObservationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	response.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.LimitReader(response.Body, 2<<20), Closer: response.Body}
	return response, nil
}

func apiVIPObservation(snapshot vip.Observation) api.VIPObservationSnapshot {
	result := api.VIPObservationSnapshot{Provider: snapshot.Provider, VIPs: make([]api.VIPOwnerObservation, 0, len(snapshot.VIPs))}
	for _, item := range snapshot.VIPs {
		value := api.VIPOwnerObservation{Name: item.Name, Address: item.Address, Owner: item.Owner, Known: item.Known, Reason: item.Reason, CheckedAt: item.CheckedAt, ExpiresAt: item.ExpiresAt, ServiceUID: item.ServiceUID}
		if e := item.Evidence; e != nil {
			value.Evidence = &api.VIPAnnouncerEvidence{Kind: e.Kind, UID: e.UID, ResourceVersion: e.ResourceVersion, CreatedAt: e.CreatedAt, RenewedAt: e.RenewedAt, LivenessAt: e.LivenessAt}
		}
		result.VIPs = append(result.VIPs, value)
	}
	return result
}

package network

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// Send makes one bounded round using the previously admitted private attempt.
// Every failure retains durable intent. An already-started network packet can
// arrive after invalidation; current acknowledgment, not recall, is guaranteed.
func (s *Sender) Send(ctx context.Context, m pr.Member, z *zone.Zone) error {
	a, err := s.bound(ctx, m, z)
	if err != nil {
		return err
	}
	if !a.state.CompareAndSwap(attemptAdmitted, attemptSending) {
		return pr.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	targets, err := s.targets(ctx, a)
	if err != nil {
		return err
	}
	jobs := make(chan netip.AddrPort, len(targets))
	for _, target := range targets {
		jobs <- target
	}
	close(jobs)
	var mu sync.Mutex
	var failures []error
	var workers sync.WaitGroup
	for range min(s.options.Parallelism, len(targets)) {
		workers.Go(func() {
			for target := range jobs {
				if err := a.current(ctx); err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
					return
				}
				err := zone.SendNotify(ctx, zone.Notify{Zone: m.Zone.Apex, Serial: a.serial, SOA: a.soa, Target: target, Source: a.policy.source, Timeout: 3 * time.Second, KeyName: a.key.Key, Algorithm: a.key.Algorithm, TSIG: dnsserver.TSIGProvider(a.cut.Keys)})
				if err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
				}
			}
		})
	}
	workers.Wait()
	if err := errors.Join(failures...); err != nil {
		return err
	}
	if err := a.current(ctx); err != nil {
		return err
	}
	if !a.state.CompareAndSwap(attemptSending, attemptSent) {
		return pr.ErrUnavailable
	}
	return nil
}

func (s *Sender) targets(ctx context.Context, a *attempt) ([]netip.AddrPort, error) {
	var out []netip.AddrPort
	add := func(ap netip.AddrPort) error {
		if !ap.IsValid() || ap.Port() == 0 || ap.Addr().Zone() != "" {
			return pr.ErrInvalid
		}
		ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		if !slices.Contains(out, ap) {
			if len(out) == s.options.MaxTargets {
				return pr.ErrCapacity
			}
			out = append(out, ap)
		}
		return nil
	}
	if a.policy.mode == public.NotifyNone {
		return nil, nil
	}
	if a.policy.mode == public.NotifySpecified || a.policy.mode == public.NotifyBoth {
		for _, ap := range a.policy.targets {
			if err := add(ap); err != nil {
				return nil, err
			}
		}
	}
	if a.policy.mode == public.NotifyZoneNameServers || a.policy.mode == public.NotifyBoth {
		for _, ns := range a.names {
			if zone.KeyName(ns.Host) == zone.KeyName(a.soa.Ns) {
				continue
			}
			if err := a.current(ctx); err != nil {
				return nil, err
			}
			addrs := ns.Addrs
			if len(addrs) == 0 {
				var err error
				addrs, err = s.options.Resolver.LookupNetIP(ctx, "ip", ns.Host)
				if err != nil {
					return nil, err
				}
			}
			if len(addrs) == 0 {
				return nil, pr.ErrUnavailable
			}
			if len(addrs) > s.options.MaxAddresses {
				return nil, pr.ErrCapacity
			}
			for _, addr := range addrs {
				if err := add(netip.AddrPortFrom(addr, zone.DefaultNotifyPort)); err != nil {
					return nil, err
				}
			}
		}
	}
	return normalizedTargets(out), nil
}

// Package servingzones composes one candidate's committed primary routing with
// strict nonprimary configuration and credential-bound node-local runtime.
package servingzones

import (
	"fmt"
	"strings"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/stub"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// Options comes from root's ONE validated candidate and immutable dependency
// cut. Objects is its prepared ObjectSet, never an independently live cache.
// Current/publication fencing remains outer root responsibility.
type Options struct {
	Base               replication.Generation
	Objects            zonecache.ObjectSet
	Primary            *primaryruntime.PreparedImage
	Secondary          *secondary.Snapshot
	Stub               *stub.Snapshot
	RPZ                *rpz.Snapshot
	Keys               *dnsserver.StaticKeyring
	Now                time.Time
	MaxZones, MaxFeeds int
}

// Prepared owns detached configurations and bound runtime, with no live source
// or provider lookup on the serving path. It does not acknowledge publication.
type Prepared struct {
	base        replication.Generation
	primary     *primaryruntime.PreparedImage
	nonprimary  *zone.Snapshot
	secondary   *secondary.BoundSnapshot
	stub        *stub.Snapshot
	coordinated map[string]bool
	coverage    map[string]*zone.Zone
	rpz         *rpz.BoundSnapshot
	lists       map[string]*public.BlockList
}

// Base is the complete candidate identity shared with the primary part.
func (p *Prepared) Base() replication.Generation { return p.base }

// HasList reports exact canonical raw BlockList names (namespace/name, or name
// for a namespace-less object). Root may filter synthetic names ONLY when its
// same captured catalogue confirms them; unknown real names must fail closed.
func (p *Prepared) HasList(name string) bool { return p.lists[name] != nil }

// Zones evaluates only this prepared immutable routing at the supplied time.
func (p *Prepared) Zones(now time.Time) zone.Set {
	return composed{primary: p.primary.Cut(now), nonprimary: p.nonprimary, secondary: p.secondary, stub: p.stub, prepared: p.primary, coordinated: p.coordinated, coverage: p.coverage, now: now}
}

// SelectRPZ preserves ordered names/sources using detached raw BlockLists. Only
// reserved service:* catalogue labels are ignored; root validates those labels.
// Other synthetic names may be filtered via HasList and root's captured catalogue.
// Ambiguous/unqualified, unknown and duplicate names are errors, never an empty
// successful substitute for required policy. Expiry remains evaluated by RPZ.
func (p *Prepared) SelectRPZ(names []string) (*rpz.Selection, error) {
	if len(names) > 4096 {
		return nil, fmt.Errorf("servingzones: selection exceeds bounds")
	}
	lists := make([]*public.BlockList, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" || len(name) > 512 || strings.ContainsRune(name, 0) || seen[name] {
			return nil, fmt.Errorf("servingzones: invalid or duplicate list selection")
		}
		seen[name] = true
		list := p.lists[name]
		if list != nil {
			lists = append(lists, list)
		} else if !strings.HasPrefix(name, "service:") || name == "service:" {
			return nil, fmt.Errorf("servingzones: unknown canonical list name")
		}
	}
	return p.rpz.Select(lists, true), nil
}

// Prepare builds only NONPRIMARY desired configuration, then binds captured
// copies to this candidate's exact identities and immutable provider. Nil runtime
// is valid unavailable coverage; nil keyring is invalid even for unsigned data.
func Prepare(o Options) (*Prepared, error) {
	if o.Primary == nil || o.Keys == nil || o.Now.IsZero() || o.Base.Validate() != nil || !o.Base.Same(o.Primary.Base()) {
		return nil, fmt.Errorf("servingzones: missing/mismatched candidate or immutable provider")
	}
	if o.MaxZones <= 0 {
		o.MaxZones = 10000
	}
	if o.MaxFeeds <= 0 {
		o.MaxFeeds = 10000
	}
	if o.MaxZones > 100000 || o.MaxFeeds > 100000 || len(o.Objects.Kinds()) == 0 || len(o.Objects.Zones()) > o.MaxZones || len(o.Objects.BlockLists()) > o.MaxFeeds {
		return nil, fmt.Errorf("servingzones: missing object registry or exceeded bounds")
	}
	configs := make([]*public.DNSZone, 0, len(o.Objects.Zones()))
	models := make([]*zone.Zone, 0, len(configs))
	coordinated := make(map[string]bool)
	coverage := make(map[string]*zone.Zone)
	cut := o.Primary.Cut(o.Now)
	seen := map[string]bool{}
	for _, desired := range o.Objects.Zones() {
		if desired == nil || desired.UID == "" {
			return nil, fmt.Errorf("servingzones: missing desired zone identity")
		}
		config := desired.DeepCopy()
		apex := dns.CanonicalName(config.Spec.ZoneName)
		if seen[apex] {
			return nil, fmt.Errorf("servingzones: duplicate configured apex")
		}
		seen[apex] = true
		configs = append(configs, config)
		if config.Spec.Type == public.ZoneTypePrimary {
			continue
		}
		var model *zone.Zone
		var err error
		switch config.Spec.Type {
		case public.ZoneTypeSecondary:
			model, err = zone.BuildSecondaryPlaceholder(config)
		case public.ZoneTypeStub:
			model, err = zone.BuildStubPlaceholder(config)
		default:
			sets := o.Objects.RecordSetsForZone(config.Name)
			owned := make([]*public.DNSRecordSet, 0, len(sets))
			for _, rs := range sets {
				if rs == nil {
					return nil, fmt.Errorf("servingzones: nil record set")
				}
				owned = append(owned, rs.DeepCopy())
			}
			var errors zone.Errors
			model, errors = zone.Build(config, zone.ServedRecordSets(config, owned), zone.BuildOptions{Now: o.Now})
			if !errors.Empty() {
				err = fmt.Errorf("servingzones: invalid nonprimary records/policy")
			}
		}
		if err != nil {
			return nil, fmt.Errorf("servingzones: nonprimary build failed: %w", err)
		}
		if model == nil {
			return nil, fmt.Errorf("servingzones: nonprimary build returned no model")
		}
		closed := config.DeepCopy()
		closed.Spec.Type = public.ZoneTypePrimary
		coverage[apex], err = zone.BuildCommittedUnavailable(closed)
		if err != nil {
			return nil, err
		}
		eligible, applies := membership(o.Primary, cut, config, o.Now)
		if !eligible {
			model = coverage[apex]
		} else {
			coordinated[apex] = applies
		}
		models = append(models, model)
	}
	nonprimary, err := zone.NewSnapshot(o.Base.Revision, true, models...)
	if err != nil {
		return nil, err
	}
	bound, err := o.Secondary.Bind(configs, o.Keys)
	if err != nil {
		return nil, err
	}
	lists := make(map[string]*public.BlockList, len(o.Objects.BlockLists()))
	ordered := make([]*public.BlockList, 0, len(lists))
	feeds := 0
	for _, raw := range o.Objects.BlockLists() {
		if raw == nil || raw.Name == "" || raw.UID == "" {
			return nil, fmt.Errorf("servingzones: missing list identity")
		}
		list := raw.DeepCopy()
		name := list.Name
		if list.Namespace != "" {
			name = list.Namespace + "/" + list.Name
		}
		if lists[name] != nil {
			return nil, fmt.Errorf("servingzones: duplicate canonical list name")
		}
		for _, source := range list.Spec.Sources {
			if source.RPZ != nil || source.Format == "rpz" {
				feeds++
				if feeds > o.MaxFeeds {
					return nil, fmt.Errorf("servingzones: source count exceeds bounds")
				}
			}
		}
		lists[name] = list
		ordered = append(ordered, list)
	}
	rpzBound, err := o.RPZ.Bind(ordered, o.Keys)
	if err != nil {
		return nil, err
	}
	return &Prepared{base: o.Base, primary: o.Primary, nonprimary: nonprimary, secondary: bound, stub: o.Stub, coordinated: coordinated, coverage: coverage, rpz: rpzBound, lists: lists}, nil
}

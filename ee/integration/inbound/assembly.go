package inbound

import (
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
)

// SourceAdmission comes from ONE current complete object/primary-runtime cut.
// Ready acknowledges this exact source root and ALL original target receipts.
// This pure value is not a transaction mutation capability or fixture admission.
type SourceAdmission struct {
	Owner        kinds.Owner
	ConfigDigest string
	RootDigest   string
	Ready        bool
	Targets      []TargetEvidence
}

// ImageInput includes durable arbitration state, not a reconstructed client list.
// Groups are original public objects. Prepare never mutates their specs/status.
type ImageInput struct {
	// ClusterID is the actual complete-image cluster, never taken from a source.
	ClusterID  string
	Sources    []SourceImage
	Admissions []SourceAdmission
	Inventory  inventory.State
	Groups     []*public.ClientGroup
	At         time.Time
}

// PreparedSource keeps original desired/evidence even when live authority closes.
// Eligible never renews a record's expiry; primary lifecycle owns DNS retirement.
type PreparedSource struct {
	Head     *kinds.IntegrationSourceHead
	Document Document
	Eligible bool
}

// PreparedImage contains detached serving inputs. Root publishes inventory,
// merged groups and exact target receipts together in its complete DNS bundle.
type PreparedImage struct {
	Inventory inventory.State
	Groups    []*public.ClientGroup
	Sources   []PreparedSource
}

// Prepare assembles private MAC contributions under current single-ref grants.
// Missing/changed source/target/UID/receipt closes enrichment, preserving original
// durable tombstones and finite desired DNS evidence for root reconciliation.
func Prepare(in ImageInput) (*PreparedImage, error) {
	invalid := func() (*PreparedImage, error) { return nil, errors.New("inbound: invalid/incomplete serving image") }
	if !boundedText(in.ClusterID, 256) || in.At.IsZero() || len(in.Sources) > kinds.MaxSourceCount || len(in.Admissions) > kinds.MaxSourceCount || len(in.Groups) > kinds.MaxSourceCount*kinds.MaxSourceGroups {
		return invalid()
	}
	raw, err := inventory.EncodeState(in.Inventory)
	if err != nil {
		return nil, err
	}
	state, err := inventory.DecodeState(raw)
	if err != nil {
		return nil, err
	}
	holder, err := inventory.RestoreState(state, func() time.Time { return in.At })
	if err != nil {
		return nil, err
	}
	admissions := map[kinds.Owner]SourceAdmission{}
	currentNames := map[string]bool{}
	for _, a := range in.Admissions {
		if a.Owner.ClusterID != in.ClusterID || a.Owner.Validate() != nil || !kinds.ValidDigest(a.ConfigDigest) || !kinds.ValidDigest(a.RootDigest) || len(a.Targets) > kinds.MaxSourceZones+1 || admissions[a.Owner].Owner.UID != "" {
			return invalid()
		}
		for _, t := range a.Targets {
			if !targetValid(t) {
				return invalid()
			}
		}
		key := a.Owner.ClusterID + "\x00" + a.Owner.Namespace + "\x00" + a.Owner.Name
		if currentNames[key] {
			return invalid()
		}
		currentNames[key] = true
		admissions[a.Owner] = a
	}
	sourceBatches := map[string]inventory.SourceState{}
	for _, s := range state.Sources {
		sourceBatches[s.ID] = s
	}
	out := &PreparedImage{Inventory: state}
	eligible := map[string]bool{}
	sourceOwners := map[kinds.Owner]bool{}
	for _, image := range in.Sources {
		doc, err := DecodeSource(image)
		if err != nil {
			return nil, err
		}
		h := image.Head.Spec
		if h.Owner.ClusterID != in.ClusterID || h.AcceptedAt.After(in.At) || h.WithdrawnAt.After(in.At) {
			return invalid()
		}
		if sourceOwners[h.Owner] {
			return invalid()
		}
		sourceOwners[h.Owner] = true
		a, ok := admissions[h.Owner]
		live := ok && a.Ready && h.State == "active" && h.ConfigDigest == a.ConfigDigest && h.RootDigest == a.RootDigest
		if live {
			// Exact target tokens are acknowledgments, never replacement RVs exchanged
			// into an original request. Every receipt is required; no partial assembly.
			targets := map[string]TargetEvidence{}
			for _, t := range a.Targets {
				if targets[t.Object.Name].Object.UID != "" {
					return invalid()
				}
				targets[t.Object.Name] = t
			}
			if len(targets) != len(doc.Targets) {
				live = false
			}
			for _, t := range doc.Targets {
				if targets[t.Object.Name] != t {
					live = false
				}
			}
		}
		id := host.SourceID(h.Owner)
		if live {
			b, exists := sourceBatches[id]
			if !exists || b.Revision != h.Sequence {
				return invalid()
			}
			verifier := inventory.New(func() time.Time { return h.AcceptedAt })
			if verifier.Replace(id, h.Sequence, doc.Snapshot.Clients) != nil {
				return invalid()
			}
			canonical := verifier.Snapshot().Clients()
			stored := slices.Clone(b.Clients)
			slices.SortFunc(stored, func(a, b inventory.Client) int { return strings.Compare(a.ID, b.ID) })
			x, _ := json.Marshal(canonical)
			y, _ := json.Marshal(stored)
			if string(x) != string(y) {
				return invalid()
			}
		}
		eligible[id] = live
		out.Sources = append(out.Sources, PreparedSource{Head: image.Head.DeepCopy(), Document: doc, Eligible: live})
	}
	// Unknown Integration inventory rows cannot acquire authority merely because
	// the private source kind was omitted. Non-Integration producers are preserved.
	for i, w := range out.Inventory.Watermarks {
		if strings.HasPrefix(w.Source, "integration-") && !eligible[w.Source] {
			out.Inventory.Watermarks[i].Live = false
		}
	}
	liveMAC := map[string]map[string]bool{}
	for _, c := range holder.Snapshot().Clients() {
		if !eligible[c.Source] || !in.At.Before(c.ExpiresAt) {
			continue
		}
		active := false
		for _, ip := range c.IPs {
			winner := holder.Snapshot().Lookup(ip, in.At)
			if winner != nil && winner.Source == c.Source && winner.ID == c.ID {
				active = true
				break
			}
		}
		if !active {
			continue
		}
		if liveMAC[c.Source] == nil {
			liveMAC[c.Source] = map[string]bool{}
		}
		for _, m := range c.MACs {
			liveMAC[c.Source][m] = true
		}
	}
	groupSources := map[string][]struct {
		owner kinds.Owner
		macs  []string
		uid   string
	}{}
	for _, s := range out.Sources {
		if !s.Eligible {
			continue
		}
		for _, g := range s.Document.Groups {
			key := g.Group.Namespace + "\x00" + g.Group.Name
			groupSources[key] = append(groupSources[key], struct {
				owner kinds.Owner
				macs  []string
				uid   string
			}{s.Head.Spec.Owner, g.MACs, g.Group.UID})
		}
	}
	seenGroups := map[string]bool{}
	for _, original := range in.Groups {
		if original == nil || original.UID == "" || len(original.Spec.Clients) > kinds.MaxEffectiveMembers {
			return invalid()
		}
		key := original.Namespace + "\x00" + original.Name
		if seenGroups[key] {
			return invalid()
		}
		seenGroups[key] = true
		group := original.DeepCopy()
		already := map[string]bool{}
		for _, m := range group.Spec.Clients {
			if mac, err := net.ParseMAC(m.MAC); err == nil {
				already[mac.String()] = true
			}
		}
		var add []string
		if group.Spec.Sources != nil && group.Spec.Sources.IntegrationRef != nil {
			ref := group.Spec.Sources.IntegrationRef
			for _, c := range groupSources[key] {
				if c.uid != string(group.UID) || c.owner.Namespace != group.Namespace || c.owner.Name != ref.Name || ref.Namespace != "" && ref.Namespace != group.Namespace {
					continue
				}
				for _, mac := range c.macs {
					if !already[mac] && liveMAC[host.SourceID(c.owner)][mac] {
						already[mac] = true
						add = append(add, mac)
					}
				}
			}
		}
		slices.Sort(add)
		if len(group.Spec.Clients)+len(add) > kinds.MaxEffectiveMembers {
			return invalid()
		}
		for _, mac := range add {
			group.Spec.Clients = append(group.Spec.Clients, public.ClientMatcher{MAC: mac})
		}
		out.Groups = append(out.Groups, group)
	}
	slices.SortFunc(out.Groups, func(a, b *public.ClientGroup) int {
		return strings.Compare(a.Namespace+"\x00"+a.Name, b.Namespace+"\x00"+b.Name)
	})
	slices.SortFunc(out.Sources, func(a, b PreparedSource) int {
		return strings.Compare(host.SourceID(a.Head.Spec.Owner), host.SourceID(b.Head.Spec.Owner))
	})
	return out, nil
}

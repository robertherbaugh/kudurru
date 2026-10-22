package host

import (
	"context"
	"strings"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// SourceHost lends a background observer a copy of its captured destination.
// It never reads live objects or rebinds a request to a newer configuration.
type SourceHost interface {
	OwnerHost
	SourceZone(context.Context, public.ObjectRef) (*public.DNSZone, error)
}

// SourceZone uses only the original operation's guarded complete object cut.
// Root's publication adapter still checks target UID/RV and write permission
// at its admission boundary; this read does not grant mutation authority.
func (p providerHost) SourceZone(ctx context.Context, ref public.ObjectRef) (*public.DNSZone, error) {
	if ref.Namespace != "" || len(validation.IsDNS1123Subdomain(ref.Name)) != 0 {
		return nil, ErrInvalid
	}
	g, err := p.guard(ctx)
	if err != nil {
		return nil, err
	}
	p.h.mu.RLock()
	defer p.h.mu.RUnlock()
	j := p.j
	if p.h.jobs[j.key] != j || !j.active || j.ctx.Err() != nil || j.leaseCtx.Err() != nil || !p.h.opts.Allowed() || j.guard != g.Ledger || j.frame.input.BundleID != g.BundleID || !j.frame.input.Complete {
		return nil, ErrFenced
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	z, ok := j.frame.input.Objects.Zone(ref.Name)
	if !ok || z == nil || z.Spec.Type != public.ZoneTypePrimary || public.IsDisabled(z) || !z.DeletionTimestamp.IsZero() || z.UID == "" || z.ResourceVersion == "" {
		return nil, ErrUnavailable
	}
	apex := dns.CanonicalName(z.Spec.ZoneName)
	if _, ok := dns.IsDomainName(apex); !ok || apex == "." || strings.ContainsAny(apex, " \\*\t\r\n\x00") {
		return nil, ErrInvalid
	}
	return z.DeepCopy(), nil
}

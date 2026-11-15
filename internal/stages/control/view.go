package control

import (
	"fmt"
	"net/netip"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// View owns immutable global rules for one complete publication or legacy reload.
type View struct{ opts Options }

// NewView validates typed options and takes private copies of both lists.
func NewView(opts Options) (*View, error) {
	if len(opts.AllowedNetworks) > 1024 || len(opts.BlockedQTypes) > 256 {
		return nil, fmt.Errorf("control: rule limit exceeded")
	}
	for _, p := range opts.AllowedNetworks {
		if !p.IsValid() || p.Addr().Is4In6() && p.Bits() < 96 {
			return nil, fmt.Errorf("control: invalid network")
		}
	}
	for _, typ := range opts.BlockedQTypes {
		if typ == 0 {
			return nil, fmt.Errorf("control: invalid query type")
		}
	}
	return viewOf(opts), nil
}

// ViewFromConfig compiles all settings before returning an adoptable view.
func ViewFromConfig(c config.Control) (*View, error) {
	rules, err := c.Rules()
	if err != nil {
		return nil, err
	}
	return NewView(Options{AllowedNetworks: rules.AllowedNetworks, BlockedQTypes: rules.BlockedQTypes})
}

func viewOf(opts Options) *View {
	nets := make([]netip.Prefix, 0, len(opts.AllowedNetworks))
	for _, p := range opts.AllowedNetworks {
		if !p.IsValid() {
			continue
		}
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		nets = append(nets, p.Masked())
	}
	opts.AllowedNetworks = nets
	opts.BlockedQTypes = slices.Clone(opts.BlockedQTypes)
	return &View{opts: opts}
}

// Replace atomically adopts a previously validated immutable view.
func (s *Stage) Replace(v *View) error {
	if v == nil {
		return fmt.Errorf("control: missing view")
	}
	s.live.Store(v)
	return nil
}

func (s *Stage) pin(req *pipeline.Request) (*View, error) {
	if s.Capture != nil {
		v, err := s.Capture(req)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, pipeline.ErrServingCapture
		}
		return v, nil
	}
	if req.Serving != nil {
		return nil, pipeline.ErrServingCapture
	}
	v := s.live.Load()
	if v == nil {
		return nil, pipeline.ErrServingCapture
	}
	return v, nil
}

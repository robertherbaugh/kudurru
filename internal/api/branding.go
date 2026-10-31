package api

import (
	"context"
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/ee/branding"
	"github.com/robertherbaugh/kudurru/internal/license"
)

// BrandingInfo is the public, effective organization presentation. Enabled
// means the Enterprise entitlement permits customization, even with no values.
type BrandingInfo branding.Info

// BrandingReadSource reads the original request's settings and entitlement.
// Complete serving mounts must supply it; partial mounts never use live state.
type BrandingReadSource func(context.Context) (BrandingInfo, error)

type brandingOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         BrandingInfo
}

func (s *Server) registerBranding() {
	register(s, operation(http.MethodGet, "/branding", "getBranding", "Public Enterprise branding, or the built-in presentation", "meta"), perm{public: true}, func(ctx context.Context, _ *struct{}) (*brandingOutput, error) {
		info := BrandingInfo(branding.Default())
		if s.opts.BrandingReads != nil {
			var err error
			info, err = s.opts.BrandingReads(ctx)
			if err != nil {
				return nil, errUnavailable("captured branding unavailable")
			}
		} else if s.opts.CanonicalSettings != nil {
			return nil, errUnavailable("captured branding unavailable")
		} else if s.opts.License != nil {
			status := s.opts.License.Status()
			var until time.Time
			if status.GraceEndsAt != nil {
				until = *status.GraceEndsAt
			}
			info = BrandingInfo(branding.Project(s.config().Branding, status.Allows(license.FeatureBranding), until, s.opts.Now()))
		}
		return &brandingOutput{CacheControl: "no-store", Body: info}, nil
	})
}

func brandingLicenseRequired() error {
	return newError(http.StatusPaymentRequired, KeyLicense, "custom branding requires an Enterprise license", "install an Enterprise key that includes branding; saved branding is retained and DNS keeps working")
}

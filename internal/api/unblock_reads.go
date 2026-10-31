package api

import (
	"context"
	"net/http"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/unblock"
)

type unblockReadSource interface {
	UnblockSnapshot(context.Context) ([]*public.UnblockRequest, replication.Generation, error)
}

type unblockReadEntitlement interface {
	UnblockRequestsAllowed(context.Context, time.Time) (bool, error)
}

func (s *Server) requireCapturedRequests(ctx context.Context) error {
	r, ok := s.opts.ObjectReads.(unblockReadEntitlement)
	if !ok || ctx == nil || ctx.Err() != nil {
		return objectReadUnavailable()
	}
	allowed, err := r.UnblockRequestsAllowed(ctx, s.opts.Now())
	if err != nil || ctx.Err() != nil {
		return objectReadUnavailable()
	}
	if !allowed {
		return newError(http.StatusPaymentRequired, KeyLicense, "unblock requests needs a license key", "add a license key; DNS keeps working meanwhile")
	}
	return nil
}

func (s *Server) capturedUnblockImage(ctx context.Context) ([]*public.UnblockRequest, serving.Identity, time.Duration, error) {
	id, err := s.convenienceIdentity(ctx)
	if err != nil {
		return nil, id, 0, err
	}
	if s.opts.CanonicalSettings == nil {
		return nil, id, 0, objectReadUnavailable()
	}
	if _, _, err := s.capturedObjects(ctx); err != nil {
		return nil, id, 0, err
	}
	reader, ok := s.opts.ObjectReads.(unblockReadSource)
	if !ok {
		return nil, id, 0, objectReadUnavailable()
	}
	objects, base, err := reader.UnblockSnapshot(ctx)
	if err != nil || !base.Same(id.Base) {
		return nil, id, 0, objectReadUnavailable()
	}
	cfg, err := s.opts.CanonicalSettings.Settings(ctx)
	if err != nil || cfg == nil || ctx.Err() != nil {
		return nil, id, 0, objectReadUnavailable()
	}
	expiry := cfg.UnblockRequests.PendingExpiry.Std()
	if expiry <= 0 {
		return nil, id, 0, objectReadUnavailable()
	}
	return objects, id, expiry, nil
}

func (s *Server) getCapturedUnblock(ctx context.Context, in *nsObjectInput) (*unblockOutput, error) {
	objects, _, expiry, err := s.capturedUnblockImage(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireCapturedRequests(ctx); err != nil {
		return nil, err
	}
	var r *public.UnblockRequest
	for _, object := range objects {
		if object == nil {
			return nil, objectReadUnavailable()
		}
		if object.Namespace == in.Namespace && object.Name == in.Name {
			r = object
		}
	}
	var v UnblockRequest
	if r != nil {
		v = unblockViewWithExpiry(r, expiry)
	}
	if err := s.requireCapturedRequests(ctx); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errNotFound("UnblockRequest " + in.Namespace + "/" + in.Name)
	}
	return &unblockOutput{Body: &v}, nil
}

func (s *Server) listCapturedUnblock(ctx context.Context, in *unblockListInput) (*unblockListOutput, error) {
	objects, cut, expiry, err := s.capturedUnblockImage(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireCapturedRequests(ctx); err != nil {
		return nil, err
	}
	query := convenienceQuery(struct {
		Resource, State string
		Limit           int
	}{"unblockrequests", in.State, clampLimit(in.Limit, 100, 500)})
	start, err := conveniencePageStart(in.Continue, cut, query)
	if err != nil {
		return nil, err
	}
	all := objects
	for _, r := range all {
		if r == nil || ctx.Err() != nil {
			return nil, objectReadUnavailable()
		}
	}
	unblock.SortNewestFirst(all)
	out := unblockPage(all, in, start, expiry, func(offset int) string { return conveniencePageNext(cut, query, offset) })
	// Evaluate the same original finite entitlement after projection too. A
	// held request cannot cross its deadline and adopt a renewed live license.
	if err := s.requireCapturedRequests(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

package servingbuild

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
)

// CatalogueReads reads the catalog from the original HTTP request's blocking
// owner. It has no holder, publisher or store and cannot capture a fresh lease.
type CatalogueReads struct{}

// NewCatalogueReads constructs the stateless original-request catalog reader.
func NewCatalogueReads() *CatalogueReads { return &CatalogueReads{} }

// Catalogue returns a borrowed immutable catalog valid within the handler.
func (r *CatalogueReads) Catalogue(ctx context.Context) (*catalogue.Catalogue, error) {
	if r == nil || ctx == nil {
		return nil, serving.ErrComponents
	}
	if _, err := servinghttp.Identity(ctx); err != nil {
		return nil, err
	}
	view, err := servinghttp.Component[*blockingstage.View](ctx, "blocking")
	if err != nil {
		return nil, err
	}
	c := view.Catalogue()
	if c == nil {
		return nil, serving.ErrComponents
	}
	return c, nil
}

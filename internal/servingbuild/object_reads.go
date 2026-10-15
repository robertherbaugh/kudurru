package servingbuild

import (
	"context"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// ObjectReads reads public desired objects from the original HTTP request cut.
// It has no publisher or store and cannot acquire a replacement lease.
type ObjectReads struct{}

// NewObjectReads constructs the stateless original-request object reader.
func NewObjectReads() *ObjectReads { return &ObjectReads{} }

// ObjectSnapshot returns immutable objects valid only within the handler. An
// empty object roster is valid; a missing registry or original lease is not.
func (r *ObjectReads) ObjectSnapshot(ctx context.Context) (zonecache.ObjectSet, replication.Generation, error) {
	if r == nil || ctx == nil {
		return zonecache.ObjectSet{}, replication.Generation{}, serving.ErrComponents
	}
	id, err := servinghttp.Identity(ctx)
	if err != nil {
		return zonecache.ObjectSet{}, replication.Generation{}, err
	}
	objects, err := servinghttp.Component[*zonecache.Snapshot](ctx, "objects")
	if err != nil {
		return zonecache.ObjectSet{}, replication.Generation{}, err
	}
	if id.Base.Validate() != nil || objects.Generation != id.Base.Revision || !slices.Equal(objects.Objects.Kinds(), zonecache.DefaultKinds()) {
		return zonecache.ObjectSet{}, replication.Generation{}, serving.ErrComponents
	}
	return objects.Objects, id.Base, nil
}

package servinghttp

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/serving"
)

// CommittedPast reports whether the Store's installed revision is now past
// the revision of the publication that this request's current lease holds,
// in the same installed term. A current command is admitted only while its
// lease is the current publication, so after a command of the request
// committed, a later revision means that this commit, or one after it,
// advanced the generation. That change already makes the publisher prepare
// a candidate read after it, so the command need not Wake the publisher: a
// Wake that arrives after that preparation read its candidate only makes the
// publisher prepare and publish the same revision once more, which
// supersedes every lease captured on the first publication (D407). It
// reads only memory and grants nothing. It is false without a held lease.
func CommittedPast(ctx context.Context, publisher *serving.Publisher) bool {
	if ctx == nil || publisher == nil {
		return false
	}
	lease, ok := requestLease(ctx)
	if !ok {
		return false
	}
	token, err := lease.SourceToken()
	if err != nil {
		return false
	}
	held := token.Authority().Installed
	now := publisher.Status().Authority.Installed
	return held.Term.Validate() == nil && now.Term.Same(held.Term) && now.Revision > held.Revision
}

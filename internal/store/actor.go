package store

import "context"

type actorKey struct{}

// WithActor returns a context whose writes are audited as actor ("<kind>:
// <name>", for example "User:alice"). The API layer sets it per request;
// the sqlite driver and the audit store decorator read it with ActorFrom.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom returns the actor set by WithActor, or "".
func ActorFrom(ctx context.Context) string {
	a, _ := ctx.Value(actorKey{}).(string)
	return a
}

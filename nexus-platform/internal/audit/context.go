package audit

import "context"

// actorCtxKey is the unexported context key under which the verified actor user id travels
// from the HTTP layer down to the admin service layer. Using a private struct type avoids
// collisions with any other package's context keys.
type actorCtxKey struct{}

// WithActor returns a child context carrying the verified actor user id. The HTTP layer
// (admin handlers) calls this so admin service mutations (entity/field/role) can attribute
// the audit row to the caller WITHOUT threading a userID parameter through every method —
// keeping the large existing call-site surface unchanged. An empty id is still stored; the
// reader treats "" as an unknown actor.
func WithActor(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, userID)
}

// ActorFromContext extracts the actor user id placed by WithActor. It returns "" when no
// actor was set (e.g. an internal call path), so a caller can decide whether to skip the
// audit or record an unknown actor.
func ActorFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorCtxKey{}).(string); ok {
		return v
	}
	return ""
}

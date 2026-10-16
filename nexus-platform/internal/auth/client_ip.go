package auth

import (
	"context"
	"errors"
)

// ErrAPIKeyThrottled is returned by the API-key verifier when the presenting client IP has spent its
// budget of failed key lookups and the presented string is not a key already known to be live
// (specs/045 FR-9, queue #356). Distinct from ErrAPIKeyInvalid on purpose: the auth middleware answers
// it 429 RATE_LIMITED, not 401, and no lookup was made.
var ErrAPIKeyThrottled = errors.New("auth: api key verification throttled")

type clientIPKey struct{}

// WithClientIP returns ctx carrying the request's client IP (as the per-IP limiters see it: the direct
// peer, or the real client behind a trusted proxy). The auth middleware sets it; the API-key verifier
// reads it. It lives here, not in middleware/services, because both sides import auth and neither
// imports the other.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIPFrom returns the client IP WithClientIP put on ctx, or "" (no per-IP decision possible).
func ClientIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}

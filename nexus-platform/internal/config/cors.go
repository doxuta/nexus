package config

import (
	"time"

	"github.com/gin-contrib/cors"
)

// BuildCORSConfig constructs a gin-contrib/cors.Config from the allowed-origins list.
// Rules (RM-04):
//   - AllowCredentials is always true — NEVER combined with AllowAllOrigins (that would
//     let any origin read credentialed responses, violating the CORS spec).
//   - AllowHeaders: Authorization, Content-Type (the minimum the FE needs).
//   - AllowMethods: GET, POST, PUT, PATCH, DELETE, OPTIONS (covers all dynamic-record methods).
//   - An empty origins slice → fail-closed: AllowOriginFunc returns false for every origin.
//     gin-contrib/cors panics when AllowOrigins is empty AND AllowOriginFunc is nil, so
//     we always supply AllowOriginFunc; when allowedOrigins is non-empty it is used as a
//     fast exact-match set, otherwise it always returns false (deny all).
func BuildCORSConfig(allowedOrigins []string) cors.Config {
	// Build a lookup set for O(1) exact-match checks.
	originSet := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		originSet[o] = struct{}{}
	}

	cfg := cors.Config{
		// AllowOriginFunc is the single origin-check path. An explicit list with
		// AllowOrigins would also work, but supplying AllowOriginFunc avoids the
		// library's panic on an empty AllowOrigins slice.
		AllowOriginFunc: func(origin string) bool {
			_, ok := originSet[origin]
			return ok
		},
		AllowMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders: []string{"Authorization", "Content-Type", "X-Nexus-Draft"}, // X-Nexus-Draft: specs/063 builder preview of the draft overlay
		// Retry-After is set on 429s (auth rate-limit RM-15, AI budget-guard E9 M4);
		// without exposing it, cross-origin FE code cannot read the header and has to
		// render the seconds-free message variant.
		ExposeHeaders:    []string{"Retry-After"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
		// AllowAllOrigins must never be true with AllowCredentials=true.
		AllowAllOrigins: false,
	}
	return cfg
}

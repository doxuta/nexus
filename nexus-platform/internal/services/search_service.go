// SEARCH-UNIFIED (Wave 3.5): one tenant+ACL-scoped fan-out search behind GET /search?q=. The
// ⌘K palette previously fanned out client-side over only the first ~5 entities, so a record in
// the 6th+ entity was unfindable. This runs the fan-out SERVER-side across EVERY entity the
// caller may view, reusing the exact ACL + row-scope + field-ACL path a normal GET /:entity?q=
// takes (ACLService.Authorize sets the access level; RecordService.List applies A5 + RM-42), so
// search can never surface a record — or match on a field — the caller couldn't already browse.
package services

import (
	"context"
	"sort"
	"strings"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// searchEntityLister is the metadata slice SearchService needs (satisfied by *metadata.Repository).
type searchEntityLister interface {
	ListEntities(tenantID string) ([]metadata.EntityDefinition, error)
}

// searchACL is the per-entity decision (satisfied by *services.ACLService): Authorize yields the
// (allowed, access_level) pair the row scope needs — the SAME call middleware.RequireACL makes.
type searchACL interface {
	Authorize(ctx context.Context, tenantID, roleID, entity, action string) (bool, string, error)
}

// SearchService fans a substring query out over the caller's viewable entities.
type SearchService struct {
	meta searchEntityLister
	acl  searchACL
	rec  *RecordService
}

func NewSearchService(meta searchEntityLister, acl searchACL, rec *RecordService) *SearchService {
	return &SearchService{meta: meta, acl: acl, rec: rec}
}

// SearchHit is one matched record: enough for the palette to render a row + navigate.
type SearchHit struct {
	ID   string         `json:"id"`
	Data map[string]any `json:"data"`
}

// SearchGroup is the hits from one entity.
type SearchGroup struct {
	Entity string      `json:"entity"`
	Label  string      `json:"label"`
	Hits   []SearchHit `json:"hits"`
}

const (
	searchMaxEntities = 40 // fan-out ceiling — a tenant with more entities searches the first N by name
	searchPerEntity   = 5  // hits kept per entity (the palette shows a few per group)
	searchMinChars    = 2
)

// Search runs the fan-out. Empty/short term → no groups (the caller shows recents instead). An
// entity that errors (transient) contributes nothing rather than failing the whole search.
func (s *SearchService) Search(ctx context.Context, scope RecordScope, tenantID, term string) ([]SearchGroup, error) {
	term = strings.TrimSpace(term)
	if len([]rune(term)) < searchMinChars {
		return []SearchGroup{}, nil
	}
	ents, err := s.meta.ListEntities(tenantID)
	if err != nil {
		return nil, err
	}
	// Stable order (by name) so the "first N" cut is deterministic, not DB-order roulette.
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name < ents[j].Name })

	groups := make([]SearchGroup, 0, len(ents))
	scanned := 0
	for _, en := range ents {
		if scanned >= searchMaxEntities {
			break
		}
		scanned++

		// ACL first — identical decision to a GET /:entity request. Admin (nil-safe) and the
		// resolved access level flow into the row scope; denied entities are skipped silently
		// (no existence leak, mirrors the assistant's fail-closed listing).
		// Fail CLOSED on an unwired resolver. This started as `allowed, level := true, "all"`, which
		// meant a nil ACL granted every non-adminall-row access to every entity in the tenant — the
		// only surface in the codebase whose nil-dependency default widened visibility instead of
		// removing it (files.scopeFor, approvals and the record list all deny). Production wires the
		// ACL, so this was latent; a default that is safe only because of wiring elsewhere is the
		// kind that survives a refactor and stops being safe without anyone noticing.
		allowed, level := scope.IsAdmin, string(dynrepo.AccessAll)
		if !scope.IsAdmin {
			if s.acl == nil {
				continue
			}
			ok, lvl, aerr := s.acl.Authorize(ctx, tenantID, scope.RoleID, en.Name, "view")
			if aerr != nil || !ok {
				continue
			}
			allowed, level = ok, lvl
		}
		if !allowed {
			continue
		}

		entScope := scope
		if !scope.IsAdmin {
			entScope.AccessLevel = level
		}
		// A hit is a record the caller can OPEN, and opening one honours manual shares (specs/051).
		entScope.IncludeShares = true
		res, lerr := s.rec.List(ctx, entScope, tenantID, en.Name, ListQuery{
			Search: term, Limit: searchPerEntity, Sort: "updated_at", Order: "desc",
		})
		if lerr != nil || len(res.Records) == 0 {
			continue
		}
		hits := make([]SearchHit, 0, len(res.Records))
		for _, r := range res.Records {
			hits = append(hits, SearchHit{ID: r.ID, Data: r.Data})
		}
		groups = append(groups, SearchGroup{Entity: en.Name, Label: en.Label, Hits: hits})
	}
	return groups, nil
}

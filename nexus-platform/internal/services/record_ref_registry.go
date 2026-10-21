package services

// ENG-SAMPLE-REF — cross-request "@ref" resolution for POST /:entity/bulk (record_bulk.go).
//
// Why this exists: the AI Builder's propose_records tool used to refuse sample data for an
// entity with a REQUIRED relation field ("field relation cần id record THẬT — không biết id thì
// BỎ TRỐNG"), because at propose time the model never knows the real id of a parent row it is
// ALSO proposing in the same turn. vertical_pack (pack.go/install.go) already solves the identical
// problem for packs via a symbolic "@ref" resolved against a same-Install() in-memory map
// (refMap) before each sample insert. This file provides the SAME symbolic-ref idea for
// POST /:entity/bulk, generalized to survive ACROSS SEPARATE HTTP requests: the AI Builder's FE
// (frozen — not touched by this change) applies each entity's "records" draft as its OWN bulk-
// create call, sequentially, in the order the model proposed them (AiBuilder.tsx applyAll +
// draftApplyRank tie-break on original index) — so "propose Dự án, then propose Công việc
// referencing it" becomes "POST /obj_du_an/bulk, then POST /obj_cong_viec/bulk" as two
// independent requests. A single in-process refMap (like pack's) cannot bridge that gap; this
// registry can, at the cost of being in-memory (see below).
//
// Scope, deliberately narrower than vertical_pack's resolveRefs:
//   - Only `relation`-typed field VALUES, and the `id` inside a `polymorphic_relation` {type, id}
//     value (queue #140), are treated as references (a string starting with "@"). Every other
//     field type is left alone — neither can legitimately hold an id that starts with "@"
//     (real values are server-minted UUIDs, and the existence probe rejects anything else), so
//     this can never misinterpret a human's ordinary text/email/... value. That is what makes
//     this change SAFE to land on the shared bulk-create path every entity/tenant uses.
//   - A row DECLARES itself via the reserved key "$ref" (a string that never collides with a real
//     field name — field names are SanitizeIdentifier'd to `^[a-z][a-z0-9_]{0,25}$`, which can
//     never contain "$"). record_bulk.go strips "$ref" from the payload BEFORE ValidateRecord runs
//     (so it is never treated as RECORD_UNKNOWN_FIELD) and registers {tenant,ref}->{entity,id}
//     ONLY after the row is actually, successfully persisted — never before. This means a ref can
//     ONLY ever resolve to a REAL, committed row; there is no pre-assignment of ids anywhere.
//   - Consequence: a row can NEVER reference a sibling row's ref from the SAME CreateBulk call
//     (neither is registered yet when Phase 1 runs the resolution pass below) — only a ref
//     registered by an EARLIER, already-completed CreateBulk call resolves. This is intentional,
//     not an oversight: cha-con "trong cùng lô đề xuất" means "trong cùng lượt chat" (propose the
//     parent's records first, then the child's, in two propose_records calls — ai/tools.go mangles
//     the ref token so it matches across calls) — NOT "trong cùng một mảng rows". Same-call
//     self-reference and dependency-cycle detection are handled at PROPOSE time instead
//     (ai/tools.go), where the whole graph is visible in one process BEFORE any draft is even
//     shown to the human — see ai/tools.go's cycle check for why that is the only place a cycle
//     can be told apart from an ordinary dangling reference.
//
// Why in-memory (not a new table/Redis) is an ACCEPTED trade-off here, not a shortcut: Redis is
// provisioned in docker-compose but has ZERO Go wiring in this codebase today (grepped — see
// middleware/auth_ratelimit.go's own note on the same point), so reaching for it would be new
// infrastructure for one feature, not reuse. This mirrors the RM-15 auth rate-limiter's own
// documented trade-off (in-memory, per-process, TTL+janitor) for a comparable "cheap, bounded,
// honest-degrade" feature: a lost entry (process restart, multi-instance, TTL expiry) degrades to
// a clean 422 RECORD_REF_UNRESOLVED — never wrong data — and even a STALE/misattributed hit is
// still caught by the existing tenant-scoped relation-existence probe
// (applyBulkRelationValidation), because resolution always runs strictly BEFORE that check.
// ai/tools.go additionally mints the ref token as "@<turnID>:<name>" (turnID = the agent run's own
// uuid) so two unrelated conversations can never collide on a human-chosen name like "@du_an_1".

import (
	"sync"
	"time"
)

// recordRefTTL bounds how long a registered ref stays resolvable — long enough for a human to
// review and click Apply on a few draft cards in one sitting, short enough to bound memory and
// avoid a very-stale ref quietly resolving to an unrelated record days later.
const recordRefTTL = 30 * time.Minute

// recordRefEntry is what a "@ref" token resolves to: the entity it was created in (informational
// — resolution itself only checks the caller's OWN relation field target, via the existing
// applyBulkRelationValidation probe) and the real, persisted record id.
type recordRefEntry struct {
	entity    string
	id        string
	createdAt time.Time
}

// recordRefRegistry is a small, bounded, thread-safe map from (tenantID, token) to the record it
// names. Every method is nil-receiver-safe: a *RecordService built via a bare struct literal
// (common in this package's unit tests) leaves refRegistry nil, and a nil registry simply resolves
// nothing (fail-closed — every "@ref" reports unresolved, never a panic, never wrong data). This
// keeps every existing test — none of which ever sends a "$ref" key or an "@"-prefixed relation
// value — completely unaffected.
type recordRefRegistry struct {
	mu      sync.Mutex
	entries map[string]recordRefEntry
}

// newRecordRefRegistry constructs an empty registry and starts its janitor goroutine (mirrors
// middleware.newMemRateLimiter's shape — auth_ratelimit.go).
func newRecordRefRegistry() *recordRefRegistry {
	reg := &recordRefRegistry{entries: make(map[string]recordRefEntry)}
	go reg.janitor()
	return reg
}

func recordRefKey(tenantID, token string) string { return tenantID + "\x00" + token }

// put registers token (already tenant-unique — mangled by ai/tools.go at propose time) as
// resolving to (entity, id). Called only AFTER the row is actually, successfully persisted.
func (reg *recordRefRegistry) put(tenantID, token, entity, id string) {
	if reg == nil || token == "" {
		return
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.entries[recordRefKey(tenantID, token)] = recordRefEntry{entity: entity, id: id, createdAt: time.Now()}
}

// get resolves token for tenantID. ok is false for: no registry, unknown token, or a token whose
// TTL has lapsed (treated as absent rather than actively pruned on read — the janitor sweeps it).
func (reg *recordRefRegistry) get(tenantID, token string) (recordRefEntry, bool) {
	if reg == nil {
		return recordRefEntry{}, false
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	e, ok := reg.entries[recordRefKey(tenantID, token)]
	if !ok || time.Since(e.createdAt) > recordRefTTL {
		return recordRefEntry{}, false
	}
	return e, true
}

// RefEntity says which entity a "@ref" token's record was created in, when an earlier CreateBulk of
// this process registered it and its TTL has not lapsed: the lookup Apply's resolveBulkRefs runs.
// propose_records asks it for a parent card the builder already applied (issue #42: an MCP thread no
// longer replays applied cards, and the registry is where an applied "$ref" lives).
func (s *RecordService) RefEntity(tenantID, token string) (string, bool) {
	e, ok := s.refRegistry.get(tenantID, token)
	return e.entity, ok
}

// janitor evicts every entry older than recordRefTTL so the map cannot grow without bound across
// the life of the process (mirrors memRateLimiter.janitor — auth_ratelimit.go).
func (reg *recordRefRegistry) janitor() {
	ticker := time.NewTicker(recordRefTTL)
	defer ticker.Stop()
	for range ticker.C {
		reg.mu.Lock()
		cutoff := time.Now().Add(-recordRefTTL)
		for k, e := range reg.entries {
			if e.createdAt.Before(cutoff) {
				delete(reg.entries, k)
			}
		}
		reg.mu.Unlock()
	}
}

// KHUON-SONG-P1 (../specs/011-pack-provenance/spec.md §4.1-§4.2): per-item pack provenance.
//
// At install time we record, for every metadata item the pack ships, the item's content EXACTLY
// as the pack declared it — the non-backfillable "ancestor" the future pack-upgrade 3-way merge
// and today's read-only diff report both compare against. The collector walks the PACK struct
// (not the install result): provenance answers "what did khuôn version V expect this item to look
// like", which is defined for skipped items too. Write semantics differ though (vet BLOCKER 3):
// an item this install actually WROTE upserts its row; a SKIPPED item (pre-existing) only
// insert-if-absents so it can never overwrite the ancestor recorded by the install that DID write.
package vertical_pack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// Provenance item types (../specs/011-pack-provenance/spec.md §4.2). VARCHAR(24) in sys_pack_items.
const (
	ItemEntity        = "entity"
	ItemDeferredField = "deferred_field"
	ItemLayout        = "layout"
	ItemRole          = "role"
	ItemNN            = "nn_relationship"
	ItemWorkflow      = "workflow"
	ItemReport        = "report"
	ItemDashboard     = "dashboard"
	ItemTeam          = "team"
	ItemWorkQueue     = "work_queue"
	ItemApproval      = "approval"
	// ItemFieldACL is one field restriction, key "<role>:<entity>.<field>" (specs/069 L3).
	ItemFieldACL = "field_acl"
	// ItemInboundHook is one inbound webhook receiver, key = its name (specs/069 L5). Its signing
	// key is never part of the content: it is minted per tenant and never carried.
	ItemInboundHook = "inbound_hook"
)

// PackItem is one provenance row to persist. Content is the canonical JSON of the pack-side
// struct. Wrote selects the persistence semantics (upsert vs insert-if-absent); LiveID anchors
// the created live record when the create path returned an id (else empty → stored NULL).
type PackItem struct {
	Type    string
	Key     string
	Content json.RawMessage
	Wrote   bool
	LiveID  string
}

// itemStore persists provenance rows. Optional dependency like installStore: when nil, install
// skips provenance (no-op). Satisfied by *packinstall.Repository.
type itemStore interface {
	// SaveItems persists the batch under (tenantID, slug, version): Wrote items upsert,
	// non-Wrote items insert-if-absent. Implementations must be tenant-scoped.
	SaveItems(ctx context.Context, tenantID, slug, version string, items []PackItem) error
	// ListItems returns the tenant's provenance rows for one pack slug.
	ListItems(ctx context.Context, tenantID, slug string) ([]StoredPackItem, error)
}

// StoredPackItem is one persisted provenance row as read back for the diff report.
type StoredPackItem struct {
	Type        string
	Key         string
	PackVersion string
	Content     json.RawMessage
	// LiveID anchors the live record the create path returned ("" = unknown); Protected is the
	// reserved "Luôn giữ" flag. Both feed the 3-way upgrade plan (specs/069).
	LiveID    string
	Protected bool
}

// OtherPackItem is one provenance row of ANOTHER pack in the same tenant (FR-015 "dùng chung").
type OtherPackItem struct{ Slug, Type, Key string }

// otherSlugLister is an OPTIONAL capability of the item store (type-asserted, like the adapter
// readiness check in plan.md D12) so existing fakes of itemStore need no new method.
type otherSlugLister interface {
	ListItemKeysOtherSlugs(ctx context.Context, tenantID, slug string) ([]OtherPackItem, error)
}

// WithItemStore wires the optional provenance store (mirrors the installStore pattern).
func (s *Service) WithItemStore(st itemStore) *Service {
	s.items = st
	return s
}

// marshalItem serializes one pack-side struct as the provenance content. Marshal of these structs
// is infallible in practice (plain data, json tags); an error is surfaced so the caller can warn.
func marshalItem(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.marshalItem: %w", err)
	}
	return b, nil
}

// hasWriters marks which OPTIONAL install passes are wired on the service. A pack section whose
// pass never ran (nil writer) must NOT be recorded as provenance — the khuôn "expected" it but
// the system never installed it, and recording it would make the diff report a false "missing".
type hasWriters struct {
	Workflows, Reports, Dashboards, Teams, WorkQueues, Approvals, FieldACLs, InboundHooks bool
}

func (s *Service) writerAvailability() hasWriters {
	return hasWriters{
		Workflows:  s.workflowWr != nil,
		Reports:    s.reportWr != nil,
		Dashboards: s.dashboardWr != nil,
		Teams:      s.teamWr != nil,
		WorkQueues: s.workQueueWr != nil,
		Approvals:  s.approvalWr != nil,
		// Install needs the writer, the live re-export the reader: without both, one side of the
		// 3-way compare would be empty and every restriction would read as added/removed.
		FieldACLs:    s.fieldACLWr != nil && s.fieldACLRd != nil,
		InboundHooks: s.hookWr != nil,
	}
}

// provenanceItems walks the pack and derives one PackItem per tracked metadata item, using the
// install plan's decision sets to mark wrote-vs-skipped (../specs/011-pack-provenance/spec.md §4.1). createdIDs
// maps "<type>/<key>" → live id for the passes whose create path returns one; missing keys → "".
// Samples and Links are deliberately untracked (§4.1: customer data from day one).
func provenanceItems(pack *Pack, plan *InstallPlan, createdIDs map[string]string, has hasWriters) []PackItem {
	var out []PackItem
	add := func(typ, key string, content json.RawMessage, wrote bool) {
		key = strings.TrimSpace(key)
		if key == "" || content == nil {
			return
		}
		out = append(out, PackItem{
			Type: typ, Key: key, Content: content, Wrote: wrote,
			LiveID: createdIDs[typ+"/"+key],
		})
	}

	for _, e := range pack.Entities {
		// Canonical entity shape = the EXPORT shape: rollup + polymorphic_relation fields live in
		// DeferredFields, never inline (export.go moves them out; a hand-authored/builtin pack may
		// inline them — the CRM pack does for polymorphic). Split here so provenance content and a
		// live re-export are structurally comparable (the fresh-install-unchanged gate caught this).
		entWrote := !plan.entitySkip[strings.TrimSpace(e.Name)]
		inline := e
		inline.Fields = nil
		for _, f := range e.Fields {
			if f.FieldType == "rollup" || f.FieldType == "polymorphic_relation" {
				key := strings.TrimSpace(e.Name) + "." + strings.TrimSpace(f.Name)
				if c, err := marshalItem(PackDeferredField{EntityName: e.Name, Field: f}); err == nil {
					// Installed inline with its entity → shares the entity's wrote semantics.
					add(ItemDeferredField, key, c, entWrote)
				}
				continue
			}
			inline.Fields = append(inline.Fields, f)
		}
		if c, err := marshalItem(inline); err == nil {
			add(ItemEntity, e.Name, c, entWrote)
		}
	}
	for _, df := range pack.DeferredFields {
		key := strings.TrimSpace(df.EntityName) + "." + strings.TrimSpace(df.Field.Name)
		if c, err := marshalItem(df); err == nil {
			add(ItemDeferredField, key, c, !plan.fieldSkip[key])
		}
	}
	for _, l := range pack.Layouts {
		// Layouts are overwrite-by-design on install (Pass 5) → always "wrote".
		key := strings.TrimSpace(l.EntityName) + ":" + strings.TrimSpace(l.ViewType)
		if c, err := marshalItem(l); err == nil {
			add(ItemLayout, key, c, true)
		}
	}
	for _, r := range pack.Roles {
		if c, err := marshalItem(r); err == nil {
			add(ItemRole, r.Name, c, !plan.roleSkip[strings.TrimSpace(r.Name)])
		}
	}
	if has.FieldACLs {
		for _, fa := range pack.FieldACLs {
			rn := strings.TrimSpace(fa.RoleName)
			// Full access is the default: PutGrants deletes such a row, so a live re-export never
			// carries it — recording it would read as "removed" on the very next plan.
			if fa.CanView && fa.CanEdit {
				continue
			}
			// Install skips a restriction whose role resolves nowhere; it must not be tracked either.
			if plan.roleIDByName != nil && createdIDs[ItemRole+"/"+rn] == "" && plan.roleIDByName[rn] == "" {
				continue
			}
			if c, err := marshalItem(fa); err == nil {
				add(ItemFieldACL, fieldACLKey(fa), c, true)
			}
		}
	}
	for _, nn := range pack.NNRelationships {
		if c, err := marshalItem(nn); err == nil {
			add(ItemNN, nn.Name, c, !plan.nnSkip[strings.TrimSpace(nn.Name)])
		}
	}
	if has.Workflows {
		for _, w := range pack.Workflows {
			// sys_pack_items.content is a SECOND persisted copy of the pack — in the TARGET
			// tenant, and readable through GET /admin/verticals/installs/:slug/diff. An
			// outbound_webhook's HMAC secret / Authorization header must not land there either,
			// including when the pack arrived from an older export that still carried them.
			sw, _ := stripWorkflowCredentials(w)
			if c, err := marshalItem(sw); err == nil {
				add(ItemWorkflow, w.Name, c, !plan.wfSkip[strings.TrimSpace(w.Name)])
			}
		}
	}
	if has.Reports {
		for _, r := range pack.Reports {
			if c, err := marshalItem(r); err == nil {
				add(ItemReport, r.Name, c, !plan.reportSkip[strings.TrimSpace(r.Name)])
			}
		}
	}
	if has.Dashboards {
		for _, d := range pack.Dashboards {
			if c, err := marshalItem(d); err == nil {
				add(ItemDashboard, d.Name, c, !plan.dashSkip[strings.TrimSpace(d.Name)])
			}
		}
	}
	if has.WorkQueues {
		for _, q := range pack.WorkQueues {
			if c, err := marshalItem(q); err == nil {
				add(ItemWorkQueue, q.Name, c, !plan.wqSkip[strings.TrimSpace(q.Name)])
			}
		}
	}
	if has.Approvals {
		for _, pa := range pack.Approvals {
			// Key mirrors the install pass's idempotency pair (entity, name) in its display shape
			// "<entity>:<name>" — an approval NAME alone is not unique across entities.
			e, n := strings.TrimSpace(pa.EntityName), strings.TrimSpace(pa.Name)
			if c, err := marshalItem(pa); err == nil {
				add(ItemApproval, e+":"+n, c, !plan.apSkip[approvalDupKey(e, n)])
			}
		}
	}
	if has.InboundHooks {
		for _, h := range pack.InboundHooks {
			if c, err := marshalItem(h); err == nil {
				add(ItemInboundHook, h.Name, c, !plan.hookSkip[strings.TrimSpace(h.Name)])
			}
		}
	}
	if has.Teams {
		for _, t := range pack.Teams {
			if c, err := marshalItem(t); err == nil {
				add(ItemTeam, t.Name, c, !plan.teamSkip[strings.TrimSpace(t.Name)])
			}
		}
	}

	// Duplicate keys within one pack (workflow names are NOT DB-unique — §4.2): last-wins,
	// mirroring the install passes' own list-then-skip behavior.
	seen := make(map[string]int, len(out))
	dedup := out[:0]
	for _, it := range out {
		k := it.Type + "/" + it.Key
		if idx, dup := seen[k]; dup {
			dedup[idx] = it
			continue
		}
		seen[k] = len(dedup)
		dedup = append(dedup, it)
	}
	return dedup
}

// recordProvenance persists the provenance batch after a successful install. Fail-open like the
// ledger (a provenance failure never fails the install) but NEVER silent: every skip reason lands
// in res.Warnings (../specs/011-pack-provenance/spec.md §4.1).
func (s *Service) recordProvenance(ctx context.Context, tenantID string, pack *Pack, plan *InstallPlan, res *InstallResult, createdIDs map[string]string) {
	if s.items == nil {
		return
	}
	if pack.Manifest == nil || strings.TrimSpace(pack.Manifest.Slug) == "" {
		// Empty slug would collide every unnamed pack onto pack_slug='' and cross-contaminate
		// provenance (vet). Honest skip: warn in BOTH the result and the log (review SHOULD-FIX 2).
		res.Warnings = append(res.Warnings, "pack không có slug — không track được provenance (Khuôn Sống)")
		s.logger.Warn("vertical_pack.recordProvenance: slug-less pack — provenance skipped",
			zap.String("tenant_id", tenantID))
		return
	}
	slug := strings.TrimSpace(pack.Manifest.Slug)
	version := strings.TrimSpace(pack.Manifest.Version)
	items := provenanceItems(pack, plan, createdIDs, s.writerAvailability())
	if len(items) == 0 {
		return
	}
	if err := s.items.SaveItems(ctx, tenantID, slug, version, items); err != nil {
		res.Warnings = append(res.Warnings, "không ghi được provenance khuôn: "+err.Error())
		s.logger.Warn("vertical_pack.recordProvenance: save failed (install succeeded)",
			zap.String("tenant_id", tenantID), zap.String("slug", slug), zap.Error(err))
	}
}

// fieldACLKey is the provenance key of one field restriction: <role>:<entity>.<field>.
func fieldACLKey(fa PackFieldACL) string {
	return strings.TrimSpace(fa.RoleName) + ":" + strings.TrimSpace(fa.EntityName) + "." + strings.TrimSpace(fa.FieldName)
}

// Pack upgrade, slice L7 (../specs/069-pack-apply-upgrade/plan.md §10): upgrading a DELIVERED
// project. The builder, inside the project's build session, applies the same plan; what happens to
// each item depends on how the draft layer treats its document type (metaversion.ClassOf — the ONE
// class table, no second one here): through a draft (customers see it on Publish), at once with a
// version entry, at once without, or "new and hidden until Publish".
package vertical_pack

import (
	"context"
	"fmt"
	"strings"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
)

// applyMode wire strings (plan.md §10). Four values, mapped from metaversion.ClassOf.
const (
	ApplyModeDraft     = "draft"
	ApplyModeTraced    = "immediate_traced"
	ApplyModeUntraced  = "immediate_untraced"
	ApplyModeHidden    = "immediate_hidden"
	StatusDraftPending = "draft_pending"
	OutcomeDrafted     = "drafted"
	ReasonDraftPending = "DRAFT_PENDING"
)

// itemDocType joins a provenance item type to the draft layer's document type (a NAME join only;
// the behaviour comes from metaversion.ClassOf). A child item points at its parent's document:
// a role's grant is the role, a deferred field is a field.
var itemDocType = map[string]string{
	ItemEntity: "entity", ItemField: "field", ItemDeferredField: "field", ItemNN: "relationship",
	ItemLayout: "layout", ItemRole: "role", ItemRoleGrant: "role", ItemFieldACL: "field_acl",
	ItemTeam: "team", ItemDashboard: "dashboard", ItemWorkflow: "workflow", ItemReport: "report",
	ItemWorkQueue: "work_queue", ItemApproval: "approval", ItemInboundHook: "inbound_hook",
}

// applyModeOf maps the class of the item's document to the wire string.
func applyModeOf(it *UpgradeItem) string {
	cl := metaversion.ClassOf(itemDocType[it.ItemType])
	isNew := it.Status == StatusAdded
	switch cl.Kind {
	case metaversion.Draft:
		if isNew && cl.NewHidden {
			return ApplyModeHidden
		}
		// EditField splits options.indexed off and applies it at once (S7-8): a field change that is
		// only that toggle never reaches a draft.
		if it.ItemType == ItemField && indexedOnly(it.TheirsPaths) {
			return ApplyModeUntraced
		}
		// S7-9: a table is drafted only as "delete"; an upgrade edit of one writes at once, untraced.
		// ponytail: a pack-driven delete of a table is also drafted but is asked for the tick anyway (harmless over-ask).
		if it.ItemType == ItemEntity {
			return ApplyModeUntraced
		}
		return ApplyModeDraft
	case metaversion.DirectTraced:
		return ApplyModeTraced
	default:
		if isNew && cl.NewHidden {
			return ApplyModeHidden
		}
		return ApplyModeUntraced
	}
}

func indexedOnly(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if p != "options.indexed" {
			return false
		}
	}
	return true
}

type confirmImmediateKey struct{}

// WithConfirmImmediate carries the builder's "Áp ngay cho khách" tick (set by the handler from the
// request body; never from anywhere else).
func WithConfirmImmediate(ctx context.Context) context.Context {
	return context.WithValue(ctx, confirmImmediateKey{}, true)
}

// ImmediateConfirmed reports WithConfirmImmediate.
func ImmediateConfirmed(ctx context.Context) bool {
	v, _ := ctx.Value(confirmImmediateKey{}).(bool)
	return v
}

// draftLister is the read side of the draft layer (satisfied by *metaversion.DraftRepo). The engine
// never reads a draft to RUN anything; the plan only asks "is there one for this item".
type draftLister interface {
	ListRead(ctx context.Context, tenantID string) ([]metaversion.DraftRow, error)
}

// WithDraftReader arms draft_pending detection on a delivered project. Chainable.
func (s *Service) WithDraftReader(d draftLister) *Service {
	s.drafts = d
	return s
}

// draftIndex answers "does this item have a pending draft" without a query per item.
type draftIndex struct {
	keys   map[string]bool // docType/docKey (workflow/report/dashboard: live id; layout: entity:view)
	labels map[string]bool // docType/name — a draft-only document has no live id yet
	// fieldEntities: entities with ≥1 field draft. A field draft carries no field name, so any field
	// item of such an entity stays "Giữ" (conservative; ponytail: resolve by field id if it hurts).
	fieldEntities map[string]bool
}

func (s *Service) loadDraftIndex(ctx context.Context, tenantID string) (*draftIndex, error) {
	x := &draftIndex{keys: map[string]bool{}, labels: map[string]bool{}, fieldEntities: map[string]bool{}}
	if s.drafts == nil {
		return x, nil
	}
	rows, err := s.drafts.ListRead(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.loadDraftIndex: %w", err)
	}
	for _, r := range rows {
		x.keys[r.DocType+"/"+r.DocKey] = true
		x.labels[r.DocType+"/"+strings.TrimSpace(r.Label)] = true
		if r.DocType == "field" && r.EntityName != nil {
			x.fieldEntities[*r.EntityName] = true
		}
	}
	return x, nil
}

func (x *draftIndex) pending(it *UpgradeItem) bool {
	if x == nil {
		return false
	}
	switch it.ItemType {
	case ItemWorkflow, ItemReport, ItemDashboard:
		doc := itemDocType[it.ItemType]
		return (it.LiveID != "" && x.keys[doc+"/"+it.LiveID]) || x.labels[doc+"/"+it.ItemKey]
	case ItemLayout:
		return x.keys["layout/"+it.ItemKey]
	case ItemField, ItemDeferredField:
		ent, _, _ := strings.Cut(it.ItemKey, ".")
		return x.fieldEntities[ent]
	}
	return false
}

// overlayDelivered stamps applyMode on every item and turns an item that already has a draft into
// draft_pending (only "Giữ": the builder's draft is never overwritten — FR-104). Runs inside
// computePlan BEFORE the hash, so a draft appearing between plan and apply makes the plan stale.
func overlayDelivered(items []UpgradeItem, drafts *draftIndex) {
	for i := range items {
		it := &items[i]
		if it.Status != StatusUnchanged && it.Status != StatusInSync && drafts.pending(it) {
			it.Status, it.Actions, it.DefaultAction, it.Adopt, it.Reason = StatusDraftPending, []string{ActionKeep}, ActionKeep, false, ReasonDraftPending
		}
		it.ApplyMode = applyModeOf(it)
	}
}

// checkDeliveredCaller is the gate for applying into a delivered project: only the builder acting
// on their OWN project (the build session's tenant) may. A builder reaching it through
// ?targetTenant= gets the actionable 409; everyone else keeps the install-style refusal.
func checkDeliveredCaller(ctx context.Context, tenantID string) error {
	c, ok := metaversion.CallerFrom(ctx)
	switch {
	case ok && c.IsBuilder && c.TenantID == tenantID:
		return nil
	case ok && c.IsBuilder:
		return &services.ValidationError{Code: "PACK_UPGRADE_OPEN_WORKSPACE",
			Msg: "Mở phiên dựng của dự án để nâng cấp", Details: map[string]any{}}
	default:
		return &services.ValidationError{Code: "PROJECT_DELIVERED_INSTALL_BLOCKED",
			Msg: "Dự án đã giao: chưa nâng cấp gói vào bản nháp được", Details: map[string]any{}}
	}
}

// needsImmediateConfirm lists the chosen writes that reach the customer at once with no trace.
func needsImmediateConfirm(work []upgradeTodo) []map[string]string {
	var out []map[string]string
	for _, w := range work {
		if w.it.ApplyMode == ApplyModeUntraced && (w.action == ActionApply || w.action == ActionRestore || w.action == ActionDelete) {
			out = append(out, map[string]string{"itemType": w.it.ItemType, "itemKey": w.it.ItemKey})
		}
	}
	return out
}

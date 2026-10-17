package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/workflow"
)

// Rollback (specs/063 US8, plan §9): put a delivered workspace back to the state of an earlier
// numbered version. The plan is computed from the version history against the LIVE documents (never
// from another version's after_json), and the write goes through the Publish path with kind='rollback'.

// RollbackChange is one line of the Modal's change list.
type RollbackChange struct {
	DocType string `json:"docType"`
	DocKey  string `json:"docKey"`
	Label   string `json:"label"`
	Op      string `json:"op"` // create | update | delete, relative to the LIVE document
}

// RollbackSecretRestore names a rule whose webhook signing key goes back to an older value.
type RollbackSecretRestore struct {
	DocType string `json:"docType"`
	DocKey  string `json:"docKey"`
	Label   string `json:"label"`
}

// RollbackReactivate names a rule that is off now and on in the target; Outbound: it sends a webhook or email.
type RollbackReactivate struct {
	DocKey   string `json:"docKey"`
	Label    string `json:"label"`
	Outbound bool   `json:"outbound"`
}

// RollbackOverride names a document whose latest change after the target was an applied-at-once edit by someone else.
type RollbackOverride struct {
	DocType string    `json:"docType"`
	DocKey  string    `json:"docKey"`
	Label   string    `json:"label"`
	ByLabel string    `json:"byLabel"`
	At      time.Time `json:"at"`
}

// RollbackPlanView is GET /admin/publish/versions/:id/rollback-plan. No document body, so no secret.
type RollbackPlanView struct {
	TargetVersionNo uint                    `json:"targetVersionNo"`
	HeadVersionNo   uint                    `json:"headVersionNo"`
	Changes         []RollbackChange        `json:"changes"`
	UntouchedTypes  []string                `json:"untouchedTypes"`
	SecretRestores  []RollbackSecretRestore `json:"secretRestores"`
	Reactivates     []RollbackReactivate    `json:"reactivates"`
	OverridesDirect []RollbackOverride      `json:"overridesDirect"`
	// KeptAsIs lists documents the target cannot restore (a direct entry that is redacted or partial):
	// the live document stays as it is.
	KeptAsIs []RollbackKept `json:"keptAsIs"`
	// SchemaRestores lists the fields and tables the rollback brings back from a published delete
	// (specs/063 S7-10); a table carries the number of records that come back with it.
	SchemaRestores []RollbackSchemaRestore `json:"schemaRestores"`
	// Rehides lists the tables, fields and N-N relations a publish after the target released: the rollback
	// takes the target's schema cutoff, so the customer stops seeing them (definition and data stay).
	Rehides []RollbackKept `json:"rehides"`
	// PlanToken fingerprints everything the builder must see before confirming; Rollback re-derives it
	// under the tenant lock and answers 409 PLAN_CHANGED when it moved (FR-051).
	PlanToken string `json:"planToken"`
}

// RollbackSchemaRestore names a field or table a rollback un-deletes. SweptCount (tables only) is how many
// records the delete trashed and this rollback brings back.
type RollbackSchemaRestore struct {
	DocType    string `json:"docType"`
	DocKey     string `json:"docKey"`
	Label      string `json:"label"`
	SweptCount *int64 `json:"sweptCount,omitempty"`
}

// RollbackKept names a document the rollback leaves untouched although the history has an entry for it.
type RollbackKept struct {
	DocType string `json:"docType"`
	DocKey  string `json:"docKey"`
	Label   string `json:"label"`
}

// rollbackSecretTypes store a REDACTED body in a "direct" entry (TraceLive / DisableNow), so that body
// can never be written back over a live document or used to rebuild a deleted one.
var rollbackSecretTypes = map[string]bool{DocTypeWorkflow: true}

func (v *RollbackPlanView) token() string {
	b, _ := json.Marshal([]any{v.UntouchedTypes, v.SecretRestores, v.Reactivates, v.OverridesDirect, v.KeptAsIs, v.SchemaRestores, v.Rehides}) // plain structs: cannot fail
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:16])
}

// RollbackRequest is POST /admin/publish/versions/:id/rollback.
type RollbackRequest struct {
	ExpectedHeadVersion uint
	Note                string
	AcceptUntouched     bool
	// ExpectedPlanToken is RollbackPlanView.PlanToken as the builder saw it; empty skips the check.
	ExpectedPlanToken string
}

// rollbackInput is everything the planner reads: history rows and a live reader. No DB in here.
type rollbackInput struct {
	Types    []string // registered types a rollback restores
	Covered  []string // CoveredTypesUpTo(seq of the target)
	Target   []metaversion.HistoryDoc
	After    []metaversion.HistoryDoc // seq > target, oldest first, no snapshots
	CallerID string
	Live     func(docType, key string) (json.RawMessage, bool, error)
	Redact   func(docType string, doc json.RawMessage) json.RawMessage
	// RestoreMeta returns the Meta the newest published delete of a field / table recorded (S7-10);
	// nil or found=false means that document cannot be brought back.
	RestoreMeta func(docType, key string) (json.RawMessage, bool, error)
	// TargetAt is when the target version was written: a restored screen takes it as its updated_at so
	// a field born after that stays in "Mới thêm" exactly as it did then.
	TargetAt time.Time
}

type rollbackPlan struct {
	Changes []metaversion.Change
	View    RollbackPlanView
}

type rbKey struct{ docType, key string }

// planRollback derives the changes that turn the live documents into the state at the target:
// per (type, key) the newest history entry up to the target (direct entries included, so a customer
// admin's edit made before it is kept) - or "absent" when the document first appeared after it.
// Types the history never captured at the target are listed in UntouchedTypes and left alone.
func planRollback(in rollbackInput) (*rollbackPlan, error) {
	covered := map[string]bool{}
	for _, t := range in.Covered {
		covered[t] = true
	}
	known := map[string]bool{}
	for _, t := range in.Types {
		known[t] = true
	}
	target := map[rbKey]metaversion.HistoryDoc{}
	for _, d := range in.Target {
		if known[d.DocType] && covered[d.DocType] {
			target[rbKey{d.DocType, d.DocKey}] = d
		}
	}
	after := map[rbKey][]metaversion.HistoryDoc{}
	untouched := map[string]bool{}
	for _, d := range in.After {
		if !known[d.DocType] {
			continue
		}
		if !covered[d.DocType] {
			untouched[d.DocType] = true
			continue
		}
		after[rbKey{d.DocType, d.DocKey}] = append(after[rbKey{d.DocType, d.DocKey}], d)
	}
	keys := make([]rbKey, 0, len(target)+len(after))
	seen := map[rbKey]bool{}
	for k := range target {
		keys, seen[k] = append(keys, k), true
	}
	for k := range after {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].docType != keys[j].docType {
			return keys[i].docType < keys[j].docType
		}
		return keys[i].key < keys[j].key
	})

	plan := &rollbackPlan{View: RollbackPlanView{Changes: []RollbackChange{}, UntouchedTypes: []string{}, SecretRestores: []RollbackSecretRestore{},
		Reactivates: []RollbackReactivate{}, OverridesDirect: []RollbackOverride{}, KeptAsIs: []RollbackKept{}, SchemaRestores: []RollbackSchemaRestore{},
		Rehides: []RollbackKept{}}}
	for _, k := range keys {
		t, hasT := target[k]
		as := after[k]
		var desired json.RawMessage
		present := false
		switch {
		case hasT:
			present, desired = t.Op != "delete" && len(t.AfterJSON) > 0, t.AfterJSON
		case len(as) > 0 && as[0].Op == "create":
			// first seen after the target as a new document: it did not exist then
		case hasDelete(as) && (k.docType == DocTypeField || k.docType == DocTypeEntity):
			// A field / table is created live and released by whatever publish comes next, so no history doc
			// says it existed at the target (an update between birth and delete does not change that); the
			// delete that removed it recorded its definition (S7-10).
			doc, born, berr := schemaBornBeforeTarget(in, k)
			if berr != nil {
				return nil, berr
			}
			if !born {
				continue
			}
			present, desired = true, doc
		default:
			continue // the history says nothing about this document at the target: leave it alone
		}
		live, exists, err := in.Live(k.docType, k.key)
		if err != nil {
			return nil, fmt.Errorf("services.planRollback: live %s/%s: %w", k.docType, k.key, err)
		}
		if k.docType == DocTypeBranding && !present { // a tenant always has a branding document: back to the empty one
			present, desired = true, json.RawMessage(`{}`)
		}
		if present && hasT && t.Kind == metaversion.KindDirect {
			var skip bool
			if desired, skip, err = directTargetBody(in, k, t, live, exists); err != nil {
				return nil, err
			}
			if skip {
				plan.View.KeptAsIs = append(plan.View.KeptAsIs, RollbackKept{DocType: k.docType, DocKey: k.key, Label: t.Label})
				continue
			}
		}
		op, err := rollbackOp(present, exists, desired, live)
		if err != nil {
			return nil, fmt.Errorf("services.planRollback: %s/%s: %w", k.docType, k.key, err)
		}
		if op == "" {
			continue
		}
		var restore json.RawMessage
		var restoreView *RollbackSchemaRestore
		if k.docType == DocTypeField || k.docType == DocTypeEntity {
			// ponytail: a field's / table's attribute edits are not rolled back (S7-10 brings back what a
			// published delete removed); upgrade path: drop this skip, FieldDocAdapter.ApplyTx already updates.
			if op == "update" {
				continue
			}
			if op == "delete" && !hasT {
				// Born after the target (its first entry is the publish that released it): the rollback takes the
				// target's cutoff, which hides it from the customer again (Rehides). A rollback never deletes a
				// definition - and sweeps its records - that the target's history did not delete.
				continue
			}
			if op == "create" {
				raw, found, merr := restoreMetaOf(in, k)
				if merr != nil {
					return nil, merr
				}
				if !found {
					plan.View.KeptAsIs = append(plan.View.KeptAsIs, RollbackKept{DocType: k.docType, DocKey: k.key, Label: schemaLabel(t, as)})
					continue
				}
				restore = raw
				restoreView = &RollbackSchemaRestore{DocType: k.docType, DocKey: k.key, Label: schemaLabel(t, as)}
				if m, _ := parseRestoreMeta(raw); m != nil && k.docType == DocTypeEntity {
					restoreView.SweptCount = m.SweptCount
				}
			}
		}
		label, entity := t.Label, t.EntityName
		if !hasT && len(as) > 0 {
			label, entity = as[0].Label, as[0].EntityName
		}
		c := metaversion.Change{DocType: k.docType, DocKey: k.key, Op: metaversion.OpUpsert, Label: label, EntityName: entity,
			UserID: in.CallerID, DraftUpdatedAt: in.TargetAt}
		if exists {
			c.Before = live
		}
		c.Restore = restore
		if restoreView != nil {
			plan.View.SchemaRestores = append(plan.View.SchemaRestores, *restoreView)
		}
		if present {
			c.After = desired
		} else {
			c.Op = metaversion.OpDelete
		}
		plan.Changes = append(plan.Changes, c)
		plan.View.Changes = append(plan.View.Changes, RollbackChange{DocType: k.docType, DocKey: k.key, Label: label, Op: op})
		if last := lastOf(as); last != nil && last.Kind == metaversion.KindDirect && last.CreatedBy != in.CallerID {
			plan.View.OverridesDirect = append(plan.View.OverridesDirect, RollbackOverride{DocType: k.docType, DocKey: k.key, Label: label, ByLabel: last.ByLabel, At: last.At})
		}
		if k.docType == DocTypeWorkflow && present {
			workflowWarnings(plan, k.key, label, desired, live, exists)
		}
	}
	for t := range untouched {
		plan.View.UntouchedTypes = append(plan.View.UntouchedTypes, t)
	}
	sort.Strings(plan.View.UntouchedTypes)
	return plan, nil
}

// restoreMetaOf reads the recorded delete Meta of a field / table and checks it can drive a restore.
func restoreMetaOf(in rollbackInput, k rbKey) (json.RawMessage, bool, error) {
	if in.RestoreMeta == nil {
		return nil, false, nil
	}
	raw, found, err := in.RestoreMeta(k.docType, k.key)
	if err != nil {
		return nil, false, fmt.Errorf("services.planRollback: restore meta %s/%s: %w", k.docType, k.key, err)
	}
	if !found {
		return nil, false, nil
	}
	m, perr := parseRestoreMeta(raw)
	if perr != nil || !m.restorable(k.docType) {
		return nil, false, nil // a delete recorded without its definition cannot be undone: reported in keptAsIs
	}
	return raw, true, nil
}

// hasDelete: some version after the target deleted the document.
func hasDelete(as []metaversion.HistoryDoc) bool {
	for _, d := range as {
		if d.Op == "delete" {
			return true
		}
	}
	return false
}

// schemaBornBeforeTarget reports whether a field / table that only has a delete in the history existed
// when the target version was written (its created_at is not after it) and returns its canonical
// document as of the delete. ponytail: created_at has second precision, so a definition created within
// the same second as the target counts as existing then; the schema cutoff has the same resolution.
func schemaBornBeforeTarget(in rollbackInput, k rbKey) (json.RawMessage, bool, error) {
	raw, found, err := restoreMetaOf(in, k)
	if err != nil || !found {
		return nil, false, err
	}
	m, _ := parseRestoreMeta(raw) // restoreMetaOf parsed it already
	def := m.FieldDef
	if k.docType == DocTypeEntity {
		def = m.EntityDef
	}
	created, ok := parseDefTime(def["created_at"])
	if !ok || created.After(in.TargetAt) {
		return nil, false, nil
	}
	doc, err := schemaDocFromDef(k.docType, def)
	if err != nil {
		return nil, false, fmt.Errorf("services.planRollback: %s/%s: %w", k.docType, k.key, err)
	}
	return doc, true, nil
}

// parseDefTime reads a timestamp column that went through a generic scan and JSON (RFC 3339, or the
// "2006-01-02 15:04:05" the driver gives without parseTime).
func parseDefTime(v any) (time.Time, bool) {
	s, _ := v.(string)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// schemaDocFromDef builds the canonical document of a deleted field / table from the definition row its
// delete recorded - the same shape FieldDocAdapter / EntityDocAdapter.LoadLive produce.
func schemaDocFromDef(docType string, def map[string]any) (json.RawMessage, error) {
	str := func(k string) string { s, _ := def[k].(string); return s }
	if docType == DocTypeEntity {
		b, err := json.Marshal(entityDoc{Name: str("name"), Label: str("label"), Description: str("description")})
		if err != nil {
			return nil, fmt.Errorf("services.schemaDocFromDef: %w", err)
		}
		return b, nil
	}
	var opts *string
	if s, ok := def["options"].(string); ok {
		opts = &s
	}
	od, err := optionsWithoutIndexed(opts)
	if err != nil {
		return nil, err
	}
	order := 0
	switch n := def["field_order"].(type) {
	case float64:
		order = int(n)
	case int64:
		order = int(n)
	}
	return fieldDoc{EntityID: str("entity_id"), Label: str("label"), IsRequired: truthy(def["is_required"]), DefaultValue: str("default_val"),
		Options: od, FieldOrder: order, Description: str("description")}.marshal()
}

// schemaLabel is the name shown for a restored field / table: the target's, or the delete's when the
// target holds nothing (the definition was born after the baseline).
func schemaLabel(t metaversion.HistoryDoc, after []metaversion.HistoryDoc) string {
	if t.Label == "" && len(after) > 0 {
		return after[0].Label
	}
	return t.Label
}

func lastOf(as []metaversion.HistoryDoc) *metaversion.HistoryDoc {
	if len(as) == 0 {
		return nil
	}
	return &as[len(as)-1]
}

// rollbackOp is "create" / "update" / "delete" when live must change to reach the target, "" when it already is.
func rollbackOp(present, exists bool, desired, live json.RawMessage) (string, error) {
	switch {
	case !present && !exists:
		return "", nil
	case !present:
		return "delete", nil
	case !exists:
		return "create", nil
	}
	a, err := metaversion.DocHash(desired)
	if err != nil {
		return "", err
	}
	b, err := metaversion.DocHash(live)
	if err != nil {
		return "", err
	}
	if a == b {
		return "", nil
	}
	return "update", nil
}

// directTargetBody handles a target that is a "direct" entry. Two shapes are not a full canonical
// document: an app created at once stores only its properties (no menu), and every traced write of a
// type with secrets stores the REDACTED body. Neither may be written over a live document blindly.
func directTargetBody(in rollbackInput, k rbKey, t metaversion.HistoryDoc, live json.RawMessage, exists bool) (json.RawMessage, bool, error) {
	if k.docType == DocTypeApp && t.Op == "create" {
		if exists {
			return nil, true, nil // keep the live app and its menu: the entry cannot say what the menu was
		}
		var d appDoc
		if err := json.Unmarshal(t.AfterJSON, &d); err != nil {
			return nil, false, fmt.Errorf("services.planRollback: decode direct app %s: %w", k.key, err)
		}
		b, err := d.marshal()
		if err != nil {
			return nil, false, err
		}
		return b, false, nil
	}
	if rollbackSecretTypes[k.docType] {
		// ponytail: the body is redacted (no signing key, masked headers/url); writing it would destroy
		// the live secrets or rebuild a deleted rule without them. Leave the document and report it in
		// KeptAsIs. Upgrade path: store the sealed body in the direct entry.
		return nil, true, nil
	}
	return t.AfterJSON, false, nil
}

// workflowSecrets lists the webhook signing keys of a rule document.
func workflowSecrets(doc json.RawMessage) []string {
	var d workflowDoc
	if json.Unmarshal(doc, &d) != nil {
		return nil
	}
	var acts []map[string]any
	if json.Unmarshal(d.Actions, &acts) != nil {
		return nil
	}
	var out []string
	for _, a := range acts {
		for k, v := range a {
			if s, ok := v.(string); ok && s != "" && strings.EqualFold(k, "secret") {
				out = append(out, s)
			}
		}
	}
	return out
}

// workflowWarnings fills secretRestores and reactivates for a rule the plan writes.
func workflowWarnings(plan *rollbackPlan, key, label string, desired, live json.RawMessage, exists bool) {
	var want workflowDoc
	if json.Unmarshal(desired, &want) != nil {
		return
	}
	var liveSecrets []string
	liveActive := false
	if exists {
		liveSecrets = workflowSecrets(live)
		var l workflowDoc
		if json.Unmarshal(live, &l) == nil {
			liveActive = l.IsActive
		}
	}
	have := map[string]bool{}
	for _, s := range liveSecrets {
		have[s] = true
	}
	for _, s := range workflowSecrets(desired) {
		if !have[s] {
			plan.View.SecretRestores = append(plan.View.SecretRestores, RollbackSecretRestore{DocType: DocTypeWorkflow, DocKey: key, Label: label})
			break
		}
	}
	if want.IsActive && !liveActive {
		plan.View.Reactivates = append(plan.View.Reactivates, RollbackReactivate{DocKey: key, Label: label, Outbound: hasOutboundAction(want.Actions)})
	}
}

func hasOutboundAction(actions json.RawMessage) bool {
	var acts []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(actions, &acts) != nil {
		return false
	}
	for _, a := range acts {
		if a.Type == workflow.ActionWebhook || a.Type == workflow.ActionEmailAlert {
			return true
		}
	}
	return false
}

// rollbackTypes are the registered types a rollback restores: all of them.
func (s *PublishService) rollbackTypes() []string { return s.reg.Types() }

// rollbackTarget finds the numbered version, pinned to the tenant. A malformed id, an unknown id and
// another tenant's id are the same VERSION_NOT_FOUND (FR-081); a "direct" entry is hoàn tác, not quay lại.
func (s *PublishService) rollbackTarget(ctx context.Context, tenantID, id string) (*metaversion.Version, error) {
	notFound := invalid("VERSION_NOT_FOUND", "Không tìm thấy phiên bản này")
	if _, err := uuid.Parse(id); err != nil {
		return nil, notFound
	}
	v, err := s.versions.GetVersion(ctx, tenantID, id)
	if errors.Is(err, metaversion.ErrVersionNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, fmt.Errorf("load version: %w", err)
	}
	if v.VersionNo == nil || v.Kind == metaversion.KindDirect {
		return nil, invalid("ROLLBACK_USE_REVERT", "Đây là thay đổi áp ngay: dùng Hoàn tác, không dùng Quay lại")
	}
	return v, nil
}

// buildRollbackPlan reads the history and the live documents on q (the pool for the preview, the open
// tx for the write) and plans.
func (s *PublishService) buildRollbackPlan(ctx context.Context, q sqlx.QueryerContext, tenantID string, target *metaversion.Version, head *metaversion.Version, caller metaversion.Caller, lock bool) (*rollbackPlan, error) {
	types := s.rollbackTypes()
	covered, err := s.versions.CoveredTypesUpTo(ctx, q, tenantID, target.Seq)
	if err != nil {
		return nil, err
	}
	tdocs, err := s.versions.TargetDocs(ctx, q, tenantID, target.Seq, types)
	if err != nil {
		return nil, err
	}
	adocs, err := s.versions.DocsAfter(ctx, q, tenantID, target.Seq, types)
	if err != nil {
		return nil, err
	}
	in := rollbackInput{Types: types, Covered: covered, Target: tdocs, After: adocs, CallerID: caller.UserID, TargetAt: target.CreatedAt,
		Live: func(docType, key string) (json.RawMessage, bool, error) {
			ad, err := s.reg.Must(docType)
			if err != nil {
				return nil, false, err
			}
			return ad.LoadLive(ctx, q, tenantID, key, lock)
		},
		RestoreMeta: func(docType, key string) (json.RawMessage, bool, error) {
			return s.versions.LatestDeleteMeta(ctx, q, tenantID, docType, key)
		},
		Redact: func(docType string, doc json.RawMessage) json.RawMessage {
			if ad, ok := s.reg.Get(docType); ok {
				return ad.Redact(doc)
			}
			return nil
		}}
	p, err := planRollback(in)
	if err != nil {
		return nil, err
	}
	if tc, hc := target.SchemaCutoffAt, head.SchemaCutoffAt; tc != nil && hc != nil && hc.After(*tc) {
		born, err := schemaBornBetween(ctx, q, tenantID, *tc, hc)
		if err != nil {
			return nil, err
		}
		for _, r := range born {
			p.View.Rehides = append(p.View.Rehides, RollbackKept{DocType: r.DocType, DocKey: r.ID, Label: r.Label})
		}
	}
	p.View.TargetVersionNo, p.View.HeadVersionNo = *target.VersionNo, *head.VersionNo
	p.View.PlanToken = p.View.token()
	return p, nil
}

// RollbackPlan previews a rollback: what would change, and what the builder must know first.
func (s *PublishService) RollbackPlan(ctx context.Context, tenantID, id string) (*RollbackPlanView, error) {
	caller, err := s.builder(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	target, err := s.rollbackTarget(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	head, err := s.versions.HeadVersionRead(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.RollbackPlan: %w", err)
	}
	if head == nil || head.VersionNo == nil {
		return nil, invalid("NOT_DELIVERED", "Dự án chưa giao: không có phiên bản để quay lại")
	}
	p, err := s.buildRollbackPlan(ctx, s.db, tenantID, target, head, caller, false)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.RollbackPlan: %w", err)
	}
	return &p.View, nil
}

// Rollback applies the plan through the Publish path: one tx under the tenant lock, kind='rollback',
// rollback_of = the target's number, the schema cutoff of the target (what a later publish released is
// hidden again, never deleted). Refused while anything is pending - a draft, or a table / field / relation
// born after the last publish (it would be built on a state about to vanish, or released by a cutoff the
// builder never published) - when the head moved since the Modal opened, and when a type the target never
// captured would be silently skipped without acceptUntouched.
func (s *PublishService) Rollback(ctx context.Context, tenantID, id string, req RollbackRequest) (*PublishResult, error) {
	caller, err := s.builder(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if err := s.validateNote(req.Note); err != nil {
		return nil, err
	}
	if req.ExpectedHeadVersion == 0 {
		return nil, errInvalidBody("expectedHeadVersion là bắt buộc")
	}
	target, err := s.rollbackTarget(ctx, tenantID, id)
	if err != nil {
		return nil, s.fail("Rollback", err)
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.Rollback: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockTenant(ctx, tx, tenantID); err != nil {
		return nil, s.fail("Rollback", err)
	}
	head, err := s.versions.HeadVersion(ctx, tx, tenantID)
	if err != nil {
		return nil, s.fail("Rollback", err)
	}
	if head == nil || head.VersionNo == nil {
		return nil, invalid("NOT_DELIVERED", "Dự án chưa giao: không có phiên bản để quay lại")
	}
	if *head.VersionNo != req.ExpectedHeadVersion {
		return nil, &ValidationError{Code: "HEAD_MOVED", Msg: "Có phiên bản mới hơn vừa được xuất bản, hãy tải lại",
			Details: map[string]any{"headVersion": *head.VersionNo}}
	}
	drafts, err := s.drafts.List(ctx, tx, tenantID, true)
	if err != nil {
		return nil, s.fail("Rollback", err)
	}
	pending, err := s.pendingRows(ctx, tx, tenantID, drafts)
	if err != nil {
		return nil, s.fail("Rollback", err)
	}
	if len(pending) > 0 {
		return nil, &ValidationError{Code: "DRAFTS_PENDING", Msg: "Còn thay đổi chưa xuất bản: xuất bản hoặc bỏ chúng trước khi quay lại",
			Details: map[string]any{"pendingCount": len(pending)}}
	}
	p, err := s.buildRollbackPlan(ctx, tx, tenantID, target, head, caller, true)
	if err != nil {
		return nil, s.fail("Rollback", err)
	}
	if req.ExpectedPlanToken != "" && req.ExpectedPlanToken != p.View.PlanToken {
		return nil, &ValidationError{Code: "PLAN_CHANGED", Msg: "Kế hoạch quay lại đã thay đổi từ lúc bạn xem (có thay đổi áp ngay mới), hãy tải lại",
			Details: map[string]any{"headVersion": *head.VersionNo}}
	}
	if len(p.View.UntouchedTypes) > 0 && !req.AcceptUntouched {
		return nil, &ValidationError{Code: "ROLLBACK_NO_SNAPSHOT", Msg: "Phiên bản này chưa chụp một số loại tài liệu: chúng sẽ được giữ nguyên",
			Details: map[string]any{"docTypes": p.View.UntouchedTypes}}
	}
	if len(p.Changes) == 0 && len(p.View.Rehides) == 0 {
		return nil, invalid("NOTHING_TO_PUBLISH", "Bản đang chạy đã giống phiên bản này")
	}
	var cutoff time.Time
	switch { // a legacy version without a cutoff hides nothing: fall back to the head's, then to a fresh one
	case target.SchemaCutoffAt != nil:
		cutoff = *target.SchemaCutoffAt
	case head.SchemaCutoffAt != nil:
		cutoff = *head.SchemaCutoffAt
	default:
		if cutoff, err = s.releaseCutoff(ctx, tx, tenantID, nil); err != nil {
			return nil, s.fail("Rollback", err)
		}
	}
	of := *target.VersionNo
	out, err := s.commitChanges(ctx, tx, tenantID, p.Changes, commitOpts{Caller: caller, Kind: metaversion.KindRollback, Note: req.Note, RollbackOf: &of, Cutoff: cutoff})
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 { // a restored app / rule / report took a name someone else holds now
			return nil, &ValidationError{Code: "ROLLBACK_NAME_TAKEN", Msg: "Không quay lại được: một tên đã bị dùng cho tài liệu khác, đổi tên tài liệu đó rồi thử lại"}
		}
		return nil, s.fail("Rollback", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, s.fail("Rollback", err)
	}
	return s.finishCommit(ctx, caller, tenantID, out, "rollback"), nil
}

// Pack upgrade adapters for tables, fields, deferred fields and N-N relations, plus the app-menu
// step (specs/069 L4, plan.md §5-§6).
//
// A table the tenant lacks is one whole plan row (the planner only splits a LIVE table into
// parent + field rows), so it is created exactly like Install does. A live table is edited in
// place: label/description on the parent, one AddField/EditField per field row. Nothing here
// removes anything: a field or table the pack dropped is "keep" only (obj_* tables are shared
// across tenants, HARD_DELETE_KIND), a type/uniqueness change is `unsupported` in the plan.
package vertical_pack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// entityUpdater / fieldEditor are the update halves of the entity and field admin services (D12:
// "ready" is a type assertion, so a build without them offers "keep" instead of a hard failure).
type entityUpdater interface {
	UpdateEntity(ctx context.Context, tenantID, entityName string, in services.UpdateEntityInput) (*metadata.EntityDefinition, error)
}

type fieldEditor interface {
	EditField(ctx context.Context, tenantID string, in services.EditFieldInput) (*metadata.FieldDefinition, error)
}

var (
	_ entityUpdater = (*services.EntityService)(nil)
	_ fieldEditor   = (*services.FieldService)(nil)
)

func init() {
	upgradeAdapters[ItemEntity] = upgradeAdapter{
		ready: func(s *Service) bool {
			_, ok := s.entityWr.(entityUpdater)
			return ok && s.entityRd != nil
		},
		write: writeEntity,
	}
	fieldReady := func(s *Service) bool {
		_, ok := s.fieldWr.(fieldEditor)
		return ok && s.entityRd != nil
	}
	upgradeAdapters[ItemField] = upgradeAdapter{ready: fieldReady, write: writeField}
	upgradeAdapters[ItemDeferredField] = upgradeAdapter{ready: fieldReady, write: writeDeferredField}
	upgradeAdapters[ItemNN] = upgradeAdapter{
		ready: func(s *Service) bool { return s.relationWr != nil },
		write: writeNN,
	}
}

func isCreateStatus(st string) bool {
	return st == StatusAdded || st == StatusDeletedByBuilder || st == StatusConflictDeleted
}

// ── tables ──────────────────────────────────────────────────────────────────────────────────

func writeEntity(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	name := it.ItemKey
	id, err := e.writeEntity(ctx, it)
	if e.entRuns == nil {
		e.entRuns = map[string]error{}
	}
	e.entRuns[name] = err
	if err == nil {
		if isCreateStatus(it.Status) {
			e.created = append(e.created, name)
		}
	}
	return id, err
}

func (e *applyEnv) writeEntity(ctx context.Context, it *UpgradeItem) (string, error) {
	raw, ok := e.theirs[ItemEntity+"/"+it.ItemKey]
	if !ok {
		return "", invalid("UPGRADE_PACK_ITEM_MISSING", "Bản mới của gói không còn bảng này")
	}
	var pe PackEntity
	if err := json.Unmarshal(raw, &pe); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.writeEntity: decode %q: %w", it.ItemKey, err)
	}
	if isCreateStatus(it.Status) {
		in := services.CreateEntityInput{Name: pe.Name, Label: pe.Label, Description: pe.Description}
		for _, f := range pe.Fields {
			in.Fields = append(in.Fields, toFieldInput(f))
		}
		ent, _, err := e.s.entityWr.CreateEntity(ctx, e.tenantID, in)
		if err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.createEntity: %w", err)
		}
		if ent == nil {
			return "", nil
		}
		return ent.ID, nil
	}
	u, _ := e.s.entityWr.(entityUpdater)
	label, desc := pe.Label, pe.Description
	if _, err := u.UpdateEntity(ctx, e.tenantID, it.ItemKey, services.UpdateEntityInput{Label: &label, Description: &desc}); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.updateEntity: %w", err)
	}
	return it.LiveID, nil
}

// ── fields ──────────────────────────────────────────────────────────────────────────────────

func writeField(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	var pf PackField
	if len(it.Theirs) == 0 || json.Unmarshal(it.Theirs, &pf) != nil {
		return "", invalid("UPGRADE_PACK_ITEM_MISSING", "Bản mới của gói không còn trường này")
	}
	return e.writeOneField(ctx, it.ParentKey, pf, isCreateStatus(it.Status), it.Live)
}

func writeDeferredField(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	raw, ok := e.theirs[ItemDeferredField+"/"+it.ItemKey]
	if !ok {
		return "", invalid("UPGRADE_PACK_ITEM_MISSING", "Bản mới của gói không còn trường này")
	}
	var df PackDeferredField
	if err := json.Unmarshal(raw, &df); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.writeDeferredField: decode %q: %w", it.ItemKey, err)
	}
	var live json.RawMessage
	if len(it.Live) > 0 {
		var l PackDeferredField
		if json.Unmarshal(it.Live, &l) == nil {
			live, _ = json.Marshal(l.Field) // ponytail: re-marshal of a struct we just decoded cannot fail
		}
	}
	return e.writeOneField(ctx, df.EntityName, df.Field, isCreateStatus(it.Status), live)
}

// writeOneField adds the field (item absent in the tenant) or edits label / required / options of
// the live one. Options are the live ones overlaid with the pack's, so a key the builder added by
// hand survives (ceiling: a key the pack REMOVED is not removed).
func (e *applyEnv) writeOneField(ctx context.Context, entity string, pf PackField, create bool, liveRaw json.RawMessage) (string, error) {
	if create {
		fd, err := e.s.fieldWr.AddField(ctx, e.tenantID, services.AddFieldInput{EntityName: entity, Field: toFieldInput(pf)})
		if err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.addField: %w", err)
		}
		if fd == nil {
			return "", nil
		}
		return fd.ID, nil
	}
	id, err := e.fieldID(entity, pf.Name)
	if err != nil {
		return "", err
	}
	ed, _ := e.s.fieldWr.(fieldEditor)
	label, req := pf.Label, pf.IsRequired
	in := services.EditFieldInput{FieldID: id, Label: &label, IsRequired: &req}
	if len(pf.Options) > 0 {
		opts := map[string]any{}
		var lf PackField
		if len(liveRaw) > 0 && json.Unmarshal(liveRaw, &lf) == nil {
			for k, v := range lf.Options {
				opts[k] = v
			}
		}
		for k, v := range pf.Options {
			opts[k] = v
		}
		in.Options = opts
	}
	if _, err := ed.EditField(ctx, e.tenantID, in); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.editField: %w", err)
	}
	return id, nil
}

// fieldID resolves a field name to its id through the entity registry (tenant-scoped reads), cached.
func (e *applyEnv) fieldID(entity, field string) (string, error) {
	if e.fieldIDs == nil {
		e.fieldIDs = map[string]map[string]string{}
	}
	m, ok := e.fieldIDs[entity]
	if !ok {
		ents, err := e.s.entityRd.ListEntities(e.tenantID)
		if err != nil {
			return "", fmt.Errorf("vertical_pack.applyEnv.fieldID: list entities: %w", err)
		}
		m = map[string]string{}
		for _, en := range ents {
			if en.Name != entity {
				continue
			}
			fs, err := e.s.entityRd.ListFields(e.run, e.tenantID, en.ID)
			if err != nil {
				return "", fmt.Errorf("vertical_pack.applyEnv.fieldID: list fields of %q: %w", entity, err)
			}
			for _, f := range fs {
				m[f.Name] = f.ID
			}
		}
		e.fieldIDs[entity] = m
	}
	id := m[field]
	if id == "" {
		return "", invalid("FIELD_NOT_FOUND", "Trường %s.%s không còn trong workspace", entity, field)
	}
	return id, nil
}

// ── N-N ─────────────────────────────────────────────────────────────────────────────────────

func writeNN(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	raw, ok := e.theirs[ItemNN+"/"+it.ItemKey]
	if !ok {
		return "", invalid("UPGRADE_PACK_ITEM_MISSING", "Bản mới của gói không còn quan hệ này")
	}
	var nn PackNNRelationship
	if err := json.Unmarshal(raw, &nn); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.writeNN: decode %q: %w", it.ItemKey, err)
	}
	_, err := e.s.relationWr.RegisterRelationship(ctx, e.tenantID, services.RegisterRelationshipInput{Name: nn.Name, EntityA: nn.EntityA, EntityB: nn.EntityB})
	if err != nil && !isRelAlreadyExists(err) {
		return "", fmt.Errorf("vertical_pack.upgrade.registerRelationship: %w", err)
	}
	return "", nil
}

// ── blocked-by ──────────────────────────────────────────────────────────────────────────────

// itemEntities: the tables a plan row depends on (empty ⇒ none we can name).
func (e *applyEnv) itemEntities(it *UpgradeItem) []string {
	switch it.ItemType {
	case ItemField:
		return []string{it.ParentKey}
	case ItemDeferredField:
		n, _, _ := strings.Cut(it.ItemKey, ".")
		return []string{n}
	case ItemRoleGrant:
		if p := strings.Split(it.ItemKey, "/"); len(p) == 3 {
			return []string{p[1]}
		}
	case ItemFieldACL:
		if _, ent, _, ok := splitFieldACLKey(it.ItemKey); ok {
			return []string{ent}
		}
	case ItemLayout:
		n, _, _ := strings.Cut(it.ItemKey, ":")
		return []string{n}
	case ItemNN:
		var nn PackNNRelationship
		if json.Unmarshal(e.theirs[ItemNN+"/"+it.ItemKey], &nn) == nil {
			return []string{nn.EntityA, nn.EntityB}
		}
	case ItemWorkflow, ItemReport, ItemWorkQueue, ItemInboundHook:
		var x struct {
			EntityName string `json:"entityName"`
		}
		if json.Unmarshal(e.theirs[it.ItemType+"/"+it.ItemKey], &x) == nil && x.EntityName != "" {
			return []string{x.EntityName}
		}
	case ItemApproval:
		n, _, _ := strings.Cut(it.ItemKey, ":")
		return []string{n}
	}
	return nil
}

// blockedBy: non-empty when the item needs a table this run was supposed to create and did not
// (kept by the builder, or its create failed). plan.md §6: such an item is `blocked`, not guessed.
func (e *applyEnv) blockedBy(it *UpgradeItem) string {
	for _, name := range e.itemEntities(it) {
		p := e.plan[ItemEntity+"/"+name]
		if p == nil || !isCreateStatus(p.Status) {
			continue
		}
		if err, tried := e.entRuns[name]; !tried || err != nil {
			return fmt.Sprintf("Bảng %q chưa được tạo ở lần này nên mục này chưa áp", name)
		}
	}
	return ""
}

// ── entity baseline ─────────────────────────────────────────────────────────────────────────

// isEntityFamily: a table row that was split into field rows, or one of those field rows. Its
// baseline is composed from the children's outcomes instead of copied from the pack (plan.md §7).
func (e *applyEnv) isEntityFamily(it *UpgradeItem) bool {
	switch it.ItemType {
	case ItemField:
		return true
	case ItemEntity:
		return len(e.entityKids(it.ItemKey)) > 0
	}
	return false
}

func (e *applyEnv) entityKids(name string) []*UpgradeItem {
	var out []*UpgradeItem
	for _, it := range e.plan {
		if it.ItemType == ItemField && it.ParentKey == name {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ItemKey < out[j].ItemKey })
	return out
}

// markDone records that a member of a table family reached the pack's content (applied, restored
// or adopted) and re-saves the family's baseline.
func (e *applyEnv) markDone(it *UpgradeItem) {
	name := it.ItemKey
	if it.ItemType == ItemField {
		name = it.ParentKey
	}
	if e.done == nil {
		e.done = map[string]bool{}
	}
	e.done[it.ItemType+"/"+it.ItemKey] = true
	content := e.composeEntityBaseline(name)
	if content == nil {
		return
	}
	liveID := ""
	if p := e.plan[ItemEntity+"/"+name]; p != nil {
		liveID = p.LiveID
	}
	bctx, cancel := bookCtx(e.book)
	defer cancel()
	err := e.s.items.SaveItems(bctx, e.tenantID, e.slug, e.toVersion, []PackItem{{
		Type: ItemEntity, Key: name, Content: content, Wrote: true, LiveID: liveID,
	}})
	if err != nil {
		e.s.logger.Error("vertical_pack.markDone: save entity baseline", zap.String("tenant_id", e.tenantID),
			zap.String("slug", e.slug), zap.String("entity", name), zap.Error(err))
	}
}

// composeEntityBaseline: a member that reached the pack takes the pack's content, any other keeps
// its OLD baseline content — a field the builder kept must stay "different from the pack".
func (e *applyEnv) composeEntityBaseline(name string) json.RawMessage {
	traw, ok := e.theirs[ItemEntity+"/"+name]
	if !ok {
		return nil
	}
	tParent, tKids, err := splitParent(ItemEntity, name, traw)
	if err != nil {
		return traw
	}
	oldParent, oldKids, hasOld := json.RawMessage(nil), map[string]json.RawMessage{}, false
	if old, ok := e.baseRow(ItemEntity + "/" + name); ok {
		if p, k, perr := splitParent(ItemEntity, name, old.Content); perr == nil {
			oldParent, oldKids, hasOld = p, k, true
		}
	}
	parent := tParent
	if hasOld && !e.done[ItemEntity+"/"+name] {
		parent = oldParent
	}
	keys := map[string]bool{}
	for k := range tKids {
		keys[k] = true
	}
	for k := range oldKids {
		keys[k] = true
	}
	elems := []json.RawMessage{}
	for _, k := range sortedKeys(keys) {
		if e.done[ItemField+"/"+k] {
			if tk, ok := tKids[k]; ok {
				elems = append(elems, tk)
			}
		} else if ok, has := oldKids[k]; has {
			elems = append(elems, ok)
		}
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(parent, &m) != nil {
		return traw
	}
	m["fields"], _ = json.Marshal(elems) // ponytail: marshalling decoded raw messages cannot fail
	out, err := json.Marshal(m)
	if err != nil {
		return traw
	}
	return out
}

// ── app menu ────────────────────────────────────────────────────────────────────────────────

// appendNav puts the tables created in this run at the END of the pack's app menu (plan.md §5):
// GetApp → existing items untouched, in order → new ones appended → ReplaceNav. Never reorders,
// never removes. ok=false ⇒ nothing to report.
func (s *Service) appendNav(ctx context.Context, e *applyEnv) (UpgradeResult, bool) {
	if len(e.created) == 0 || s.appMint == nil {
		return UpgradeResult{}, false
	}
	slug := strings.ReplaceAll(e.slug, "-", "_")
	if r := []rune(slug); len(r) > 60 {
		slug = string(r[:60])
	}
	res := UpgradeResult{ItemType: "app_nav", ItemKey: slug, Action: ActionApply}
	app := s.findAppBySlug(ctx, e.tenantID, slug)
	if app == nil {
		res.Outcome, res.Code, res.Message = OutcomeKept, "APP_NOT_FOUND", "Không thấy app của gói trong workspace — bảng mới chưa có trên menu, tự gắn trong Quản lý apps"
		return res, true
	}
	fail := func(err error) (UpgradeResult, bool) {
		res.Outcome, res.Code, res.Message = OutcomeFailed, "NAV_APPEND_FAILED", "Đã tạo bảng nhưng chưa gắn được vào menu app"
		s.logger.Warn("vertical_pack.appendNav", zap.String("tenant_id", e.tenantID), zap.String("slug", e.slug), zap.Error(err))
		return res, true
	}
	_, nav, err := s.appMint.GetApp(ctx, e.tenantID, app.ID)
	if err != nil {
		return fail(err)
	}
	ents, err := s.entityRd.ListEntities(e.tenantID)
	if err != nil {
		return fail(err)
	}
	idByName := make(map[string]string, len(ents))
	for _, en := range ents {
		idByName[en.Name] = en.ID
	}
	items := make([]services.AppNavItemInput, 0, len(nav)+len(e.created))
	seen := map[string]bool{}
	for _, n := range nav {
		in := services.AppNavItemInput{ItemType: n.ItemType, RefID: n.RefID}
		if n.Label != nil {
			in.Label = *n.Label
		}
		items = append(items, in)
		seen[n.ItemType+":"+n.RefID] = true
	}
	added := 0
	for _, name := range e.created {
		id, ok := idByName[name]
		if !ok || seen[appmeta.ItemTypeEntity+":"+id] {
			continue
		}
		items = append(items, services.AppNavItemInput{ItemType: appmeta.ItemTypeEntity, RefID: id})
		added++
	}
	if added == 0 {
		return UpgradeResult{}, false
	}
	if _, err := s.appMint.ReplaceNav(ctx, e.tenantID, app.ID, items); err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			res.Outcome, res.Code, res.Message = OutcomeFailed, ve.Code, ve.Msg
			return res, true
		}
		return fail(err)
	}
	res.Outcome = OutcomeApplied
	return res, true
}

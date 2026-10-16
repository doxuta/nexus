// Pack upgrade adapters for roles, role grants and field restrictions (specs/069 L3, plan.md §5-§7).
//
// A role is written as ONE unit: UpdateRole REPLACES the whole grant set, so every role_grant child
// and the role's label are folded into a single call whose base is the LIVE grant set read right
// before (a grant the builder widened or added by hand survives; a '*' from the pack never enters a
// role that already exists). The first member of a role to be processed does the write for all of
// them and records the role's baseline row composed from the children's outcomes; the rest find the
// cached result. The customer-owner role (auth.OwnerRoleName) is never reached: the plan marks it
// protected/keep-only and the guards here refuse it again.
package vertical_pack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// Compile-time pin of the ports the adapters rely on (a service signature change breaks the build).
var (
	_ roleWriter     = (*services.RoleService)(nil)
	_ fieldACLWriter = (*services.FieldACLAdminService)(nil)
)

func isRoleFamily(typ string) bool { return typ == ItemRole || typ == ItemRoleGrant }

// roleNameOf: the role a role / role_grant plan row belongs to.
func roleNameOf(it *UpgradeItem) string {
	if it.ItemType == ItemRoleGrant {
		return it.ParentKey
	}
	return it.ItemKey
}

// roleRun is the cached result of writing one role in this run.
type roleRun struct {
	id  string
	err error
}

func init() {
	upgradeAdapters[ItemRole] = upgradeAdapter{
		ready: func(s *Service) bool { return s.roleWr != nil },
		write: writeRoleFamily,
	}
	upgradeAdapters[ItemRoleGrant] = upgradeAdapter{
		ready: func(s *Service) bool { return s.roleWr != nil },
		write: writeRoleFamily,
		remove: func(ctx context.Context, e *applyEnv, it *UpgradeItem) error {
			_, err := writeRoleFamily(ctx, e, it)
			return err
		},
	}
	upgradeAdapters[ItemFieldACL] = upgradeAdapter{
		ready:  func(s *Service) bool { return s.fieldACLWr != nil },
		write:  writeFieldACL,
		remove: removeFieldACL,
	}
}

func writeRoleFamily(_ context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	return e.ensureRole(roleNameOf(it))
}

// bind hands the apply loop's decisions to the adapters that fold several plan rows into one write.
func (e *applyEnv) bind(book, run context.Context, items []UpgradeItem, eff []UpgradeDecision) {
	e.book, e.run = book, run
	e.plan = make(map[string]*UpgradeItem, len(items))
	e.act = make(map[string]string, len(eff))
	for i := range items {
		e.plan[items[i].ItemType+"/"+items[i].ItemKey] = &items[i]
	}
	for _, d := range eff {
		e.act[d.ItemType+"/"+d.ItemKey] = d.Action
	}
}

// roleIDByName: role name → id, read once; roles created in this run are added by ensureRole.
func (e *applyEnv) roleIDByName(ctx context.Context) (map[string]string, error) {
	if e.roleIDs != nil {
		return e.roleIDs, nil
	}
	rs, err := e.s.roleWr.ListRoles(ctx, e.tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.applyEnv.roleIDByName: %w", err)
	}
	e.roleIDs = make(map[string]string, len(rs))
	for _, r := range rs {
		e.roleIDs[strings.TrimSpace(r.Name)] = r.ID
	}
	return e.roleIDs, nil
}

func (e *applyEnv) ensureRole(name string) (string, error) {
	if r, ok := e.roleRuns[name]; ok {
		return r.id, r.err
	}
	if e.roleRuns == nil {
		e.roleRuns = map[string]*roleRun{}
	}
	id, err := e.writeRole(name)
	e.roleRuns[name] = &roleRun{id: id, err: err}
	return id, err
}

func (e *applyEnv) writeRole(name string) (string, error) {
	if strings.EqualFold(name, auth.OwnerRoleName) {
		return "", invalid("ROLE_NAME_RESERVED", "Vai này do hệ thống quản lý — nâng cấp gói không đụng vào")
	}
	parent := e.plan[ItemRole+"/"+name]
	if parent == nil {
		return "", invalid("UPGRADE_PACK_ITEM_MISSING", "Bản mới của gói không còn vai này")
	}
	traw, hasT := e.theirs[ItemRole+"/"+name]
	ctx := e.run
	rw := e.s.roleWr
	if !hasT {
		// The pack dropped the role. Compose from the OLD baseline; if the tenant already deleted
		// it too, there is nothing to write — just retire the baseline row.
		old, hasOld := e.baseRow(ItemRole + "/" + name)
		if !hasOld {
			return "", invalid("UPGRADE_PACK_ITEM_MISSING", "Bản mới của gói không còn vai này")
		}
		live := parent.LiveID
		if live == "" {
			ids, err := e.roleIDByName(ctx)
			if err != nil {
				return "", err
			}
			live = ids[name]
		}
		if live == "" {
			bctx, cancel := bookCtx(e.book)
			defer cancel()
			if err := e.s.upgradeStore().DeleteItems(bctx, e.tenantID, e.slug, [][2]string{{ItemRole, name}}); err != nil {
				return "", fmt.Errorf("vertical_pack.upgrade.writeRole: drop baseline %q: %w", name, err)
			}
			return "", nil
		}
		traw = old.Content
	}

	// Role absent from the tenant: create it whole (a new role may carry '*' like at install).
	if hasT && (parent.Status == StatusAdded || parent.Status == StatusDeletedByBuilder || parent.Status == StatusConflictDeleted) {
		var pr PackRole
		if err := json.Unmarshal(traw, &pr); err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.writeRole: decode %q: %w", name, err)
		}
		in := services.CreateRoleInput{Name: pr.Name, Label: pr.Label}
		for _, a := range pr.Actions {
			in.Actions = append(in.Actions, services.RoleActionInput{EntityName: a.EntityName, Action: a.Action, AccessLevel: a.AccessLevel})
		}
		rv, err := rw.CreateRole(ctx, e.tenantID, in)
		if err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.createRole: %w", err)
		}
		id := ""
		if rv != nil {
			id = rv.ID
			if ids, ierr := e.roleIDByName(ctx); ierr == nil {
				ids[name] = id
			}
		}
		e.saveRoleBaseline(name, id, traw)
		return id, nil
	}

	id := parent.LiveID
	if id == "" {
		ids, err := e.roleIDByName(ctx)
		if err != nil {
			return "", err
		}
		id = ids[name]
	}
	if id == "" {
		return "", invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết vai nào trong workspace ứng với mục này")
	}

	kids := e.roleKids(name)
	for _, k := range kids {
		// A grant on a table this run failed to create is left out of the write (and of the new
		// baseline: it counts as kept), its own row reports `blocked`.
		if e.blockedBy(k) != "" {
			e.act[ItemRoleGrant+"/"+k.ItemKey] = ActionKeep
		}
	}
	write := e.act[ItemRole+"/"+name] == ActionApply || e.act[ItemRole+"/"+name] == ActionRestore
	for _, k := range kids {
		switch e.act[ItemRoleGrant+"/"+k.ItemKey] {
		case ActionApply, ActionRestore, ActionDelete:
			write = true
		}
	}
	if write {
		// Read the grant set right before the write: UpdateRole replaces it (plan.md §6).
		cur, err := rw.GetRole(ctx, e.tenantID, id)
		if err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.writeRole: read live %q: %w", name, err)
		}
		if cur == nil {
			return "", invalid("ROLE_NOT_FOUND", "Vai %q không còn trong workspace", name)
		}
		final := map[string]services.RoleActionInput{}
		for _, a := range cur.Actions {
			final[a.EntityName+"/"+a.Action] = services.RoleActionInput{EntityName: a.EntityName, Action: a.Action, AccessLevel: a.AccessLevel}
		}
		for _, k := range kids {
			sub := strings.TrimPrefix(k.ItemKey, name+"/")
			switch e.act[ItemRoleGrant+"/"+k.ItemKey] {
			case ActionApply, ActionRestore:
				var a PackRoleAction
				if len(k.Theirs) == 0 || json.Unmarshal(k.Theirs, &a) != nil {
					return "", invalid("UPGRADE_PACK_ITEM_MISSING", "Bản mới của gói không còn quyền này")
				}
				if strings.TrimSpace(a.EntityName) == wildcardEntityName {
					return "", invalid(ReasonRoleWildcard, "Quyền '*' của gói không được gắn vào vai có sẵn")
				}
				final[sub] = services.RoleActionInput{EntityName: a.EntityName, Action: a.Action, AccessLevel: a.AccessLevel}
			case ActionDelete:
				delete(final, sub)
			}
		}
		in := services.UpdateRoleInput{Actions: make([]services.RoleActionInput, 0, len(final))}
		for _, k := range sortedKeys(final) {
			in.Actions = append(in.Actions, final[k])
		}
		if a := e.act[ItemRole+"/"+name]; a == ActionApply || a == ActionRestore {
			var pr PackRole
			if err := json.Unmarshal(traw, &pr); err == nil && pr.Label != "" && pr.Label != cur.Label {
				in.Label = &pr.Label
			}
		}
		if _, err := rw.UpdateRole(ctx, e.tenantID, id, in); err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.updateRole: %w", err)
		}
	}
	e.saveRoleBaseline(name, id, e.composeRoleBaseline(name, traw))
	return id, nil
}

// roleKids returns the role_grant plan rows of one role, in key order.
func (e *applyEnv) roleKids(name string) []*UpgradeItem {
	var out []*UpgradeItem
	for _, it := range e.plan {
		if it.ItemType == ItemRoleGrant && it.ParentKey == name {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ItemKey < out[j].ItemKey })
	return out
}

// composeRoleBaseline builds the role's new baseline from the old one and each child's outcome
// (plan.md §7): applied/restored/adopted ⇒ the pack's element, deleted ⇒ dropped, kept ⇒ the old one.
// A child the builder kept must stay "different from the pack" next time, so it is NOT overwritten.
func (e *applyEnv) composeRoleBaseline(name string, traw json.RawMessage) json.RawMessage {
	tParent, tKids, err := splitParent(ItemRole, name, traw)
	if err != nil {
		return traw
	}
	oldParent, oldKids := json.RawMessage(nil), map[string]json.RawMessage{}
	hasOld := false
	if old, ok := e.baseRow(ItemRole + "/" + name); ok {
		if p, k, perr := splitParent(ItemRole, name, old.Content); perr == nil {
			oldParent, oldKids, hasOld = p, k, true
		}
	}
	parent := tParent
	if a := e.act[ItemRole+"/"+name]; hasOld && a != ActionApply && a != ActionRestore && a != "adopt" {
		parent = oldParent
	}
	keys := map[string]bool{}
	for k := range tKids {
		keys[k] = true
	}
	for k := range oldKids {
		keys[k] = true
	}
	var elems []json.RawMessage
	for _, k := range sortedKeys(keys) {
		tk, hasTK := tKids[k]
		ok, hasOK := oldKids[k]
		switch e.act[ItemRoleGrant+"/"+k] {
		case ActionApply, ActionRestore, "adopt":
			if hasTK {
				elems = append(elems, tk)
			}
		case ActionDelete:
		default:
			if hasOK {
				elems = append(elems, ok)
			}
		}
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(parent, &m) != nil {
		return traw
	}
	if elems == nil {
		elems = []json.RawMessage{}
	}
	m["actions"], _ = json.Marshal(elems)
	out, err := json.Marshal(m)
	if err != nil {
		return traw
	}
	return out
}

// baseRow returns the stored baseline row of "type/key", reading the pack's rows once per run.
func (e *applyEnv) baseRow(k string) (StoredPackItem, bool) {
	if e.baseRows == nil {
		e.baseRows = map[string]StoredPackItem{}
		rows, err := e.s.items.ListItems(e.book, e.tenantID, e.slug)
		if err != nil {
			e.s.logger.Error("vertical_pack.baseRow", zap.String("tenant_id", e.tenantID), zap.String("slug", e.slug), zap.Error(err))
		}
		for _, r := range rows {
			e.baseRows[r.Type+"/"+r.Key] = r
		}
	}
	r, ok := e.baseRows[k]
	return r, ok
}

// saveRoleBaseline records the role's baseline right after its write (a crash never leaves an
// applied grant looking like a customisation). Failure is logged: the next plan adopts it.
func (e *applyEnv) saveRoleBaseline(name, liveID string, content json.RawMessage) {
	bctx, cancel := bookCtx(e.book)
	defer cancel()
	err := e.s.items.SaveItems(bctx, e.tenantID, e.slug, e.toVersion, []PackItem{{
		Type: ItemRole, Key: name, Content: content, Wrote: true, LiveID: liveID,
	}})
	if err != nil {
		e.s.logger.Error("vertical_pack.saveRoleBaseline", zap.String("tenant_id", e.tenantID),
			zap.String("slug", e.slug), zap.String("role", name), zap.Error(err))
	}
}

// ── field restrictions ──────────────────────────────────────────────────────────────────────

// splitFieldACLKey: "<role>:<entity>.<field>".
func splitFieldACLKey(key string) (role, entity, field string, ok bool) {
	role, rest, ok1 := strings.Cut(key, ":")
	entity, field, ok2 := strings.Cut(rest, ".")
	return role, entity, field, ok1 && ok2 && role != "" && entity != "" && field != ""
}

// aclRoleID resolves the role of a restriction, refusing the customer-owner role and a role whose
// own write failed in this run (BLOCKED_BY).
func (e *applyEnv) aclRoleID(role string) (string, error) {
	if strings.EqualFold(role, auth.OwnerRoleName) {
		return "", invalid("ROLE_NAME_RESERVED", "Vai này do hệ thống quản lý — nâng cấp gói không đụng vào")
	}
	if r, ok := e.roleRuns[role]; ok && r.err != nil {
		return "", invalid("BLOCKED_BY", "Vai %q chưa ghi được ở lần này nên quyền theo trường của nó chưa áp", role)
	}
	ids, err := e.roleIDByName(e.run)
	if err != nil {
		return "", err
	}
	id := ids[role]
	if id == "" {
		return "", invalid("UPGRADE_ROLE_UNRESOLVED", "Vai %q không có trong workspace", role)
	}
	return id, nil
}

func writeFieldACL(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	raw, ok := e.theirs[ItemFieldACL+"/"+it.ItemKey]
	if !ok {
		return "", invalid("UPGRADE_PACK_ITEM_MISSING", "Bản mới của gói không còn quy tắc này")
	}
	var fa PackFieldACL
	if err := json.Unmarshal(raw, &fa); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.writeFieldACL: decode %q: %w", it.ItemKey, err)
	}
	roleID, err := e.aclRoleID(strings.TrimSpace(fa.RoleName))
	if err != nil {
		return "", err
	}
	if _, err := e.s.fieldACLWr.PutGrants(ctx, e.tenantID, []services.FieldACLGrantInput{{
		RoleID: roleID, EntityName: fa.EntityName, FieldName: fa.FieldName, CanView: fa.CanView, CanEdit: fa.CanEdit,
	}}); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.writeFieldACL: %w", err)
	}
	return "", nil
}

// removeFieldACL lifts the restriction: the full-access grant the screen would write. Only ever a
// hand-picked action (FIELD_ACL_OPENING is a confirm warning).
func removeFieldACL(ctx context.Context, e *applyEnv, it *UpgradeItem) error {
	role, entity, field, ok := splitFieldACLKey(it.ItemKey)
	if !ok {
		return invalid("UPGRADE_ITEM_KEY_INVALID", "Khoá quy tắc không hợp lệ")
	}
	roleID, err := e.aclRoleID(role)
	if err != nil {
		return err
	}
	if _, err := e.s.fieldACLWr.PutGrants(ctx, e.tenantID, []services.FieldACLGrantInput{{
		RoleID: roleID, EntityName: entity, FieldName: field, CanView: true, CanEdit: true,
	}}); err != nil {
		return fmt.Errorf("vertical_pack.upgrade.removeFieldACL: %w", err)
	}
	return nil
}

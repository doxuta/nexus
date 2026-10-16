package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/metaversion"
)

// DocTypeApp is the draft-layer document type of specs/063 S7-4: ONE document per app holding its
// properties AND its menu, so a publish moves them together and a menu item can never outlive the
// thing it points at. The key is the app id. Creating and deleting an app are not draftable ("Áp ngay").
const DocTypeApp = "app"

// appDocNav is one menu item of the canonical app document.
type appDocNav struct {
	ItemType string `json:"itemType"`
	RefID    string `json:"refId"`
	Label    string `json:"label"` // "" = use the referenced object's own label (SQL NULL)
}

// appDoc is the canonical form of an app: EXACTLY what appmeta.UpdateExec + ReplaceNavExec write (no
// timestamps, no actor). A blank description/icon/color is SQL NULL, so "" is its one canonical form.
// HomeDashboardID carries the one key of sys_apps.options that exists today (#289).
type appDoc struct {
	Name            string      `json:"name"`
	Label           string      `json:"label"`
	Description     string      `json:"description"`
	Icon            string      `json:"icon"`
	Color           string      `json:"color"`
	IsDefault       bool        `json:"isDefault"`
	SortOrder       int         `json:"sortOrder"`
	HomeDashboardID string      `json:"homeDashboardId"`
	HomeQueueIDs    []string    `json:"homeQueueIds,omitempty"`
	DefaultRoleIDs  []string    `json:"defaultRoleIds,omitempty"`
	Nav             []appDocNav `json:"nav"`
}

func (d appDoc) marshal() (json.RawMessage, error) {
	if d.Nav == nil {
		d.Nav = []appDocNav{} // "[]", never "null": one canonical form for an empty menu
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("services.appDoc.marshal: %w", err)
	}
	return b, nil
}

func strOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func appDocOf(a *appmeta.App, nav []appmeta.NavItem) appDoc {
	d := appDoc{Name: a.Name, Label: a.Label, Description: strOf(a.Description), Icon: strOf(a.Icon), Color: strOf(a.Color),
		IsDefault: a.IsDefault, SortOrder: a.SortOrder, HomeDashboardID: a.HomeDashboardID, Nav: make([]appDocNav, 0, len(nav))}
	d.HomeQueueIDs, d.DefaultRoleIDs = nonEmpty(a.HomeQueueIDs), nonEmpty(a.DefaultRoleIDs)
	for _, n := range nav {
		d.Nav = append(d.Nav, appDocNav{ItemType: n.ItemType, RefID: n.RefID, Label: strOf(n.Label)})
	}
	return d
}

func (d appDoc) updateInput(actor string) appmeta.UpdateInput {
	home := d.HomeDashboardID // always explicit: the document is the full state, a stale pointer must not survive
	queues, roles := append([]string{}, d.HomeQueueIDs...), append([]string{}, d.DefaultRoleIDs...)
	return appmeta.UpdateInput{Label: d.Label, Description: d.Description, Icon: d.Icon, Color: d.Color,
		IsDefault: d.IsDefault, SortOrder: d.SortOrder, UpdatedBy: actor, HomeDashboardID: &home,
		HomeQueueIDs: &queues, DefaultRoleIDs: &roles}
}

// nonEmpty maps an empty list to nil so the canonical document has ONE form for "none" (omitempty).
func nonEmpty(l []string) []string {
	if len(l) == 0 {
		return nil
	}
	return l
}

func (d appDoc) navInputs() []appmeta.NavItemInput {
	out := make([]appmeta.NavItemInput, len(d.Nav))
	for i, n := range d.Nav {
		out[i] = appmeta.NavItemInput{ItemType: n.ItemType, RefID: n.RefID, Label: n.Label}
	}
	return out
}

// withoutDead drops the menu items (and the home-page pointer) for which dead reports true.
func (d appDoc) withoutDead(dead func(itemType, refID string) bool) (appDoc, bool) {
	changed := false
	nav := make([]appDocNav, 0, len(d.Nav))
	for _, n := range d.Nav {
		if dead(n.ItemType, n.RefID) {
			changed = true
			continue
		}
		nav = append(nav, n)
	}
	d.Nav = nav
	if d.HomeDashboardID != "" && dead(appmeta.ItemTypeDashboard, d.HomeDashboardID) {
		d.HomeDashboardID, changed = "", true
	}
	return d, changed
}

func (d appDoc) withoutDashboards(gone map[string]bool) (appDoc, bool) {
	return d.withoutDead(func(t, id string) bool { return t == appmeta.ItemTypeDashboard && gone[id] })
}

// AppDocAdapter binds the "app" document type to sys_apps + sys_app_nav_items.
type AppDocAdapter struct{}

// NewAppDocAdapter returns the adapter (stateless).
func NewAppDocAdapter() *AppDocAdapter { return &AppDocAdapter{} }

var (
	_ metaversion.DocAdapter = (*AppDocAdapter)(nil)
	_ metaversion.Deriver    = (*AppDocAdapter)(nil)
)

// Type implements metaversion.DocAdapter.
func (*AppDocAdapter) Type() string { return DocTypeApp }

// LoadLive implements metaversion.DocAdapter.
func (*AppDocAdapter) LoadLive(ctx context.Context, q sqlx.QueryerContext, tenantID, key string, forUpdate bool) (json.RawMessage, bool, error) {
	query := `SELECT name, label, description, icon, color, is_default, sort_order, options FROM sys_apps
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var row struct {
		Name        string  `db:"name"`
		Label       string  `db:"label"`
		Description *string `db:"description"`
		Icon        *string `db:"icon"`
		Color       *string `db:"color"`
		IsDefault   bool    `db:"is_default"`
		SortOrder   int     `db:"sort_order"`
		Options     *string `db:"options"`
	}
	if err := sqlx.GetContext(ctx, q, &row, query, tenantID, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("services.AppDocAdapter.LoadLive: %w", err)
	}
	var navRows []struct {
		ItemType string  `db:"item_type"`
		RefID    string  `db:"ref_id"`
		Label    *string `db:"label"`
	}
	if err := sqlx.SelectContext(ctx, q, &navRows, `SELECT item_type, ref_id, label FROM sys_app_nav_items
		WHERE tenant_id = ? AND app_id = ? ORDER BY sort_order ASC, created_at ASC, id ASC`, tenantID, key); err != nil {
		return nil, false, fmt.Errorf("services.AppDocAdapter.LoadLive: nav: %w", err)
	}
	d := appDoc{Name: row.Name, Label: row.Label, Description: strOf(row.Description), Icon: strOf(row.Icon), Color: strOf(row.Color),
		IsDefault: row.IsDefault, SortOrder: row.SortOrder, Nav: make([]appDocNav, 0, len(navRows))}
	if row.Options != nil { // a malformed/foreign options blob yields "" — never an error source (same as appmeta.fillHome)
		var o struct {
			HomeDashboardID string   `json:"homeDashboardId"`
			HomeQueueIDs    []string `json:"homeQueueIds"`
			DefaultRoleIDs  []string `json:"defaultRoleIds"`
		}
		if json.Unmarshal([]byte(*row.Options), &o) == nil {
			d.HomeDashboardID = o.HomeDashboardID
			d.HomeQueueIDs, d.DefaultRoleIDs = nonEmpty(o.HomeQueueIDs), nonEmpty(o.DefaultRoleIDs)
		}
	}
	for _, n := range navRows {
		d.Nav = append(d.Nav, appDocNav{ItemType: n.ItemType, RefID: n.RefID, Label: strOf(n.Label)})
	}
	b, err := d.marshal()
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// ListLiveKeys implements metaversion.DocAdapter.
func (*AppDocAdapter) ListLiveKeys(ctx context.Context, q sqlx.QueryerContext, tenantID string) ([]metaversion.DocRef, error) {
	var rows []struct {
		ID    string `db:"id"`
		Label string `db:"label"`
	}
	if err := sqlx.SelectContext(ctx, q, &rows, `SELECT id, label FROM sys_apps
		WHERE tenant_id = ? AND deleted_at IS NULL ORDER BY sort_order, created_at, id`, tenantID); err != nil {
		return nil, fmt.Errorf("services.AppDocAdapter.ListLiveKeys: %w", err)
	}
	out := make([]metaversion.DocRef, len(rows))
	for i, r := range rows {
		out[i] = metaversion.DocRef{Key: r.ID, Label: r.Label}
	}
	return out, nil
}

// Derive implements metaversion.Deriver: a dashboard deleted by this publish takes its menu items and
// home-page pointer with it (FR-025) — and that is a change to the APP document, so it is recorded as
// one (version_docs, before/after) instead of vanishing inside the dashboard's delete. An app that
// already has a draft in this publish is rewritten in place; any other live app that points at a
// deleted dashboard gets a synthesized change.
func (a *AppDocAdapter) Derive(ctx context.Context, q sqlx.QueryerContext, tenantID string, changes []metaversion.Change) ([]metaversion.Change, error) {
	gone, upserted := map[string]bool{}, map[string]bool{}
	actor := ""
	for _, c := range changes {
		if c.DocType != DocTypeDashboard {
			continue
		}
		if c.Op == metaversion.OpDelete {
			gone[c.DocKey], actor = true, c.UserID
		} else {
			upserted[c.DocKey] = true
		}
	}
	// A table is deleted live (not drafted yet, S7-9) and a legacy admin may delete a dashboard directly,
	// so an app draft can point at something already gone: prune it here instead of blocking the publish.
	exists := func(table, id string) bool {
		var n int
		return sqlx.GetContext(ctx, q, &n, "SELECT COUNT(*) FROM "+table+" WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL", tenantID, id) == nil && n > 0
	}
	deadRef := func(t, id string) bool {
		switch t {
		case appmeta.ItemTypeEntity:
			return !exists("sys_entities", id)
		case appmeta.ItemTypeDashboard:
			return gone[id] || (!upserted[id] && !exists("sys_dashboards", id))
		}
		return false
	}
	for i, c := range changes {
		if c.DocType != DocTypeApp || c.Op != metaversion.OpUpsert {
			continue
		}
		var d appDoc
		if json.Unmarshal(c.After, &d) != nil {
			continue
		}
		if nd, changed := d.withoutDead(deadRef); changed {
			b, err := nd.marshal()
			if err != nil {
				return nil, err
			}
			changes[i].After = b
		}
	}
	if len(gone) == 0 {
		return changes, nil
	}
	have := map[string]bool{}
	for i, c := range changes {
		if c.DocType != DocTypeApp || c.Op != metaversion.OpUpsert {
			continue
		}
		have[c.DocKey] = true
		var d appDoc
		if err := json.Unmarshal(c.After, &d); err != nil {
			continue // ValidateFinal reports an unreadable draft
		}
		if nd, changed := d.withoutDashboards(gone); changed {
			b, err := nd.marshal()
			if err != nil {
				return nil, err
			}
			changes[i].After = b
		}
	}
	ids := make([]string, 0, len(gone))
	for id := range gone {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	query, args, err := sqlx.In(`SELECT DISTINCT n.app_id FROM sys_app_nav_items n
		JOIN sys_apps a ON a.id = n.app_id AND a.tenant_id = n.tenant_id AND a.deleted_at IS NULL
		WHERE n.tenant_id = ? AND n.item_type = 'dashboard' AND n.ref_id IN (?)`, tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("services.AppDocAdapter.Derive: %w", err)
	}
	var navApps []string
	if err := sqlx.SelectContext(ctx, q, &navApps, query, args...); err != nil {
		return nil, fmt.Errorf("services.AppDocAdapter.Derive: nav apps: %w", err)
	}
	query, args, err = sqlx.In(`SELECT id FROM sys_apps WHERE tenant_id = ? AND deleted_at IS NULL AND options IS NOT NULL
		AND JSON_UNQUOTE(JSON_EXTRACT(options, '$.homeDashboardId')) IN (?)`, tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("services.AppDocAdapter.Derive: %w", err)
	}
	var homeApps []string
	if err := sqlx.SelectContext(ctx, q, &homeApps, query, args...); err != nil {
		return nil, fmt.Errorf("services.AppDocAdapter.Derive: home apps: %w", err)
	}
	affected := map[string]bool{}
	for _, id := range append(navApps, homeApps...) {
		if !have[id] {
			affected[id] = true
		}
	}
	keys := make([]string, 0, len(affected))
	for id := range affected {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		before, ok, err := a.LoadLive(ctx, q, tenantID, id, true)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var d appDoc
		if err := json.Unmarshal(before, &d); err != nil {
			return nil, fmt.Errorf("services.AppDocAdapter.Derive: decode live %s: %w", id, err)
		}
		nd, changed := d.withoutDashboards(gone)
		if !changed {
			continue
		}
		after, err := nd.marshal()
		if err != nil {
			return nil, err
		}
		changes = append(changes, metaversion.Change{DocType: DocTypeApp, DocKey: id, Op: metaversion.OpUpsert, Before: before, After: after,
			Label: d.Label, UserID: actor})
	}
	return changes, nil
}

// ApplyTx implements metaversion.DocAdapter: the app's properties and its menu, through the same *Exec
// statements the admin repository uses, on the publish tx. Creating/deleting an app is never a draft.
func (*AppDocAdapter) ApplyTx(ctx context.Context, tx *sqlx.Tx, tenantID string, c metaversion.Change) error {
	if c.Op == metaversion.OpDelete { // only a rollback deletes an app (one created after the target version)
		if err := appmeta.SoftDeleteExec(ctx, tx, tenantID, c.DocKey, time.Now()); err != nil && !errors.Is(err, appmeta.ErrAppNotFound) {
			return fmt.Errorf("services.AppDocAdapter.ApplyTx: delete %s: %w", c.DocKey, err)
		}
		return nil
	}
	if c.Op != metaversion.OpUpsert {
		return fmt.Errorf("services.AppDocAdapter.ApplyTx: unsupported op %q for app %s", c.Op, c.DocKey)
	}
	var d appDoc
	if err := json.Unmarshal(c.After, &d); err != nil {
		return fmt.Errorf("services.AppDocAdapter.ApplyTx: decode draft %s: %w", c.DocKey, err)
	}
	in := d.updateInput(c.UserID)
	if d.IsDefault {
		if err := appmeta.ClearDefaultExec(ctx, tx, tenantID, c.DocKey); err != nil {
			return fmt.Errorf("services.AppDocAdapter.ApplyTx: %w", err)
		}
	}
	if len(c.Before) > 0 { // live app exists
		if err := appmeta.UpdateExec(ctx, tx, tenantID, c.DocKey, in); err != nil {
			return fmt.Errorf("services.AppDocAdapter.ApplyTx: update %s: %w", c.DocKey, err)
		}
	} else { // forced publish over an app the customer admin deleted
		restored, err := appmeta.RestoreExec(ctx, tx, tenantID, c.DocKey, d.Name, in)
		if err != nil {
			return fmt.Errorf("services.AppDocAdapter.ApplyTx: restore %s: %w", c.DocKey, err)
		}
		if !restored {
			return nil // gone for good: nothing to configure
		}
		// RestoreExec leaves options alone; write (or clear) the home pointer the same way an update does.
		if err := appmeta.ApplyOptionsExec(ctx, tx, tenantID, c.DocKey, in); err != nil {
			return fmt.Errorf("services.AppDocAdapter.ApplyTx: restore home %s: %w", c.DocKey, err)
		}
	}
	if err := appmeta.ReplaceNavExec(ctx, tx, tenantID, c.DocKey, d.navInputs(), time.Now()); err != nil {
		return fmt.Errorf("services.AppDocAdapter.ApplyTx: nav %s: %w", c.DocKey, err)
	}
	return nil
}

// ValidateFinal implements metaversion.DocAdapter: every menu item and the home page of an app that will
// be live points at something that exists once this publish is applied (read on the publish tx).
func (*AppDocAdapter) ValidateFinal(ctx context.Context, f metaversion.FinalState) []metaversion.Problem {
	willExist, willGo := map[string]bool{}, map[string]bool{} // dashboards upserted / deleted by this publish
	for _, c := range f.Changes {
		if c.DocType != DocTypeDashboard {
			continue
		}
		if c.Op == metaversion.OpUpsert {
			willExist[c.DocKey] = true
		} else {
			willGo[c.DocKey] = true
		}
	}
	count := func(q string, id string) bool {
		var n int
		return sqlx.GetContext(ctx, f.Q, &n, q, f.TenantID, id) == nil && n > 0
	}
	dashboardOK := func(id string) bool {
		if willGo[id] { // Derive strips these; a menu that still lists one here would write a dead tab back after the delete
			return false
		}
		return willExist[id] || count(`SELECT COUNT(*) FROM sys_dashboards WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, id)
	}
	entityOK := func(id string) bool {
		return count(`SELECT COUNT(*) FROM sys_entities WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, id)
	}
	var problems []metaversion.Problem
	for _, c := range f.Changes {
		if c.DocType != DocTypeApp || c.Op != metaversion.OpUpsert {
			continue
		}
		var d appDoc
		if err := json.Unmarshal(c.After, &d); err != nil {
			problems = append(problems, metaversion.Problem{DocType: DocTypeApp, DocKey: c.DocKey, Code: "APP_DRAFT_UNREADABLE", Message: "bản nháp của app này không đọc được"})
			continue
		}
		for _, n := range d.Nav {
			ok := false
			switch n.ItemType {
			case appmeta.ItemTypeDashboard:
				ok = dashboardOK(n.RefID)
			case appmeta.ItemTypeEntity:
				ok = entityOK(n.RefID)
			}
			if !ok {
				problems = append(problems, metaversion.Problem{DocType: DocTypeApp, DocKey: c.DocKey, Code: "APP_NAV_REF_INVALID",
					Message: fmt.Sprintf("menu của app %q có mục %q trỏ tới thứ không còn tồn tại (%s %s)", d.Label, n.Label, n.ItemType, n.RefID)})
			}
		}
		if d.HomeDashboardID != "" && !dashboardOK(d.HomeDashboardID) {
			problems = append(problems, metaversion.Problem{DocType: DocTypeApp, DocKey: c.DocKey, Code: "APP_HOME_DASHBOARD_INVALID",
				Message: fmt.Sprintf("trang chủ của app %q trỏ tới dashboard không còn tồn tại", d.Label)})
		}
	}
	return problems
}

// Redact implements metaversion.DocAdapter: an app holds no secret.
func (*AppDocAdapter) Redact(doc json.RawMessage) json.RawMessage { return doc }

// Entities implements metaversion.DocAdapter: apps have no engine cache to drop.
func (*AppDocAdapter) Entities(json.RawMessage) []string { return nil }

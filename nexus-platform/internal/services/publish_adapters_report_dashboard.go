package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/report"
)

// Draft-layer document types of specs/063 S7-2; the key is the row id.
const (
	DocTypeReport    = "report"
	DocTypeDashboard = "dashboard"
)

// ---------------------------------------------------------------------------------------------
// report
// ---------------------------------------------------------------------------------------------

// reportDoc is the canonical form of a report: EXACTLY the columns report.UpdateExec writes (no
// timestamps, no actor), so a draft saved identical to the live report hashes identical to it.
// Config is compared through metaversion.Canonical, so MySQL's JSON key re-ordering never matters.
type reportDoc struct {
	Name       string          `json:"name"`
	EntityName string          `json:"entityName"`
	Type       string          `json:"type"`
	Config     json.RawMessage `json:"config"`
	IsActive   bool            `json:"isActive"`
}

func (d reportDoc) marshal() (json.RawMessage, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("services.reportDoc.marshal: %w", err)
	}
	return b, nil
}

func reportDocOfRecord(r *report.Record) reportDoc {
	return reportDoc{Name: r.Name, EntityName: r.EntityName, Type: r.Type, Config: rawOrNull(r.Config), IsActive: r.IsActive}
}

func (d reportDoc) updateInput(actor string) report.UpdateInput {
	return report.UpdateInput{Name: d.Name, EntityName: d.EntityName, Type: report.Type(d.Type), Config: []byte(d.Config), IsActive: d.IsActive, UpdatedBy: actor}
}

// ReportDocAdapter binds the "report" document type to sys_reports.
type ReportDocAdapter struct{}

// NewReportDocAdapter returns the adapter (stateless: every call takes the querier it works on).
func NewReportDocAdapter() *ReportDocAdapter { return &ReportDocAdapter{} }

// Type implements metaversion.DocAdapter.
func (*ReportDocAdapter) Type() string { return DocTypeReport }

// LoadLive implements metaversion.DocAdapter.
func (*ReportDocAdapter) LoadLive(ctx context.Context, q sqlx.QueryerContext, tenantID, key string, forUpdate bool) (json.RawMessage, bool, error) {
	query := "SELECT name, entity_name, type, config, is_active FROM sys_reports WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL"
	if forUpdate {
		query += " FOR UPDATE"
	}
	var row struct {
		Name       string          `db:"name"`
		EntityName string          `db:"entity_name"`
		Type       string          `db:"type"`
		Config     json.RawMessage `db:"config"`
		IsActive   bool            `db:"is_active"`
	}
	if err := sqlx.GetContext(ctx, q, &row, query, tenantID, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("services.ReportDocAdapter.LoadLive: %w", err)
	}
	b, err := reportDoc{Name: row.Name, EntityName: row.EntityName, Type: row.Type, Config: rawOrNull(row.Config), IsActive: row.IsActive}.marshal()
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// ListLiveKeys implements metaversion.DocAdapter.
func (*ReportDocAdapter) ListLiveKeys(ctx context.Context, q sqlx.QueryerContext, tenantID string) ([]metaversion.DocRef, error) {
	var rows []struct {
		ID         string `db:"id"`
		Name       string `db:"name"`
		EntityName string `db:"entity_name"`
	}
	if err := sqlx.SelectContext(ctx, q, &rows, `SELECT id, name, entity_name FROM sys_reports
		WHERE tenant_id = ? AND deleted_at IS NULL ORDER BY created_at, id`, tenantID); err != nil {
		return nil, fmt.Errorf("services.ReportDocAdapter.ListLiveKeys: %w", err)
	}
	out := make([]metaversion.DocRef, len(rows))
	for i, r := range rows {
		out[i] = metaversion.DocRef{Key: r.ID, Label: r.Name, EntityName: r.EntityName}
	}
	return out, nil
}

// ApplyTx implements metaversion.DocAdapter: ONE change through the same *Exec statements the admin
// repository uses, on the publish transaction.
func (*ReportDocAdapter) ApplyTx(ctx context.Context, tx *sqlx.Tx, tenantID string, c metaversion.Change) error {
	switch c.Op {
	case metaversion.OpDelete:
		err := report.SoftDeleteExec(ctx, tx, tenantID, c.DocKey, time.Now())
		if err != nil && !errors.Is(err, report.ErrReportNotFound) { // already gone (forced publish): the intent is met
			return fmt.Errorf("services.ReportDocAdapter.ApplyTx: delete %s: %w", c.DocKey, err)
		}
		return nil
	case metaversion.OpUpsert:
		var d reportDoc
		if err := json.Unmarshal(c.After, &d); err != nil {
			return fmt.Errorf("services.ReportDocAdapter.ApplyTx: decode draft %s: %w", c.DocKey, err)
		}
		in := d.updateInput(c.UserID)
		if len(c.Before) > 0 { // live row exists
			if err := report.UpdateExec(ctx, tx, tenantID, c.DocKey, in); err != nil {
				return fmt.Errorf("services.ReportDocAdapter.ApplyTx: update %s: %w", c.DocKey, err)
			}
			return nil
		}
		restored, err := report.RestoreExec(ctx, tx, tenantID, c.DocKey, in) // forced publish over a customer-deleted report
		if err != nil {
			return fmt.Errorf("services.ReportDocAdapter.ApplyTx: restore %s: %w", c.DocKey, err)
		}
		if restored {
			return nil
		}
		if err := report.InsertWithIDExec(ctx, tx, c.DocKey, report.CreateInput{TenantID: tenantID, Name: in.Name, EntityName: in.EntityName,
			Type: in.Type, Config: in.Config, IsActive: in.IsActive, CreatedBy: c.UserID}, time.Now()); err != nil {
			return fmt.Errorf("services.ReportDocAdapter.ApplyTx: insert %s: %w", c.DocKey, err)
		}
		return nil
	}
	return fmt.Errorf("services.ReportDocAdapter.ApplyTx: unknown op %q", c.Op)
}

// ValidateFinal implements metaversion.DocAdapter: every report that will be live points at an entity
// that exists in the tenant (read on the publish tx after the tenant lock). Shape checks already ran
// when each draft was saved.
func (*ReportDocAdapter) ValidateFinal(ctx context.Context, f metaversion.FinalState) []metaversion.Problem {
	var problems []metaversion.Problem
	checked := map[string]bool{}
	for _, c := range f.Changes {
		if c.DocType != DocTypeReport || c.Op != metaversion.OpUpsert {
			continue
		}
		var d reportDoc
		if err := json.Unmarshal(c.After, &d); err != nil {
			problems = append(problems, metaversion.Problem{DocType: DocTypeReport, DocKey: c.DocKey, Code: "REPORT_DRAFT_UNREADABLE", Message: "bản nháp của báo cáo này không đọc được"})
			continue
		}
		if checked[d.EntityName] {
			continue
		}
		checked[d.EntityName] = true
		if !entityExistsTx(ctx, f, d.EntityName) {
			problems = append(problems, metaversion.Problem{DocType: DocTypeReport, DocKey: c.DocKey, Code: "ENTITY_NOT_FOUND",
				Message: fmt.Sprintf("báo cáo %q trỏ tới bảng %q không còn tồn tại", d.Name, d.EntityName)})
		}
	}
	return problems
}

// Redact implements metaversion.DocAdapter: a report holds no secret.
func (*ReportDocAdapter) Redact(doc json.RawMessage) json.RawMessage { return doc }

// Entities implements metaversion.DocAdapter: reports have no engine cache to drop.
func (*ReportDocAdapter) Entities(json.RawMessage) []string { return nil }

func entityExistsTx(ctx context.Context, f metaversion.FinalState, name string) bool {
	var n int
	err := sqlx.GetContext(ctx, f.Q, &n, `SELECT COUNT(*) FROM sys_entities WHERE tenant_id = ? AND name = ? AND deleted_at IS NULL`, f.TenantID, name)
	return err == nil && n > 0
}

// ---------------------------------------------------------------------------------------------
// dashboard
// ---------------------------------------------------------------------------------------------

// dashboardDoc is the canonical form of a dashboard: the columns dashboard.UpdateExec writes. A blank
// description is stored as SQL NULL, so "" is its one canonical form.
type dashboardDoc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Widgets     json.RawMessage `json:"widgets"`
}

func (d dashboardDoc) marshal() (json.RawMessage, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("services.dashboardDoc.marshal: %w", err)
	}
	return b, nil
}

func dashboardDocOfRecord(r *dashboard.Record) dashboardDoc {
	d := dashboardDoc{Name: r.Name, Widgets: rawOrNull(r.Widgets)}
	if r.Description != nil {
		d.Description = *r.Description
	}
	return d
}

func (d dashboardDoc) updateInput() dashboard.UpdateInput {
	return dashboard.UpdateInput{Name: d.Name, Description: d.Description, Widgets: []byte(d.Widgets)}
}

// DashboardDocAdapter binds the "dashboard" document type to sys_dashboards.
type DashboardDocAdapter struct{}

// NewDashboardDocAdapter returns the adapter (stateless).
func NewDashboardDocAdapter() *DashboardDocAdapter { return &DashboardDocAdapter{} }

// Type implements metaversion.DocAdapter.
func (*DashboardDocAdapter) Type() string { return DocTypeDashboard }

// LoadLive implements metaversion.DocAdapter.
func (*DashboardDocAdapter) LoadLive(ctx context.Context, q sqlx.QueryerContext, tenantID, key string, forUpdate bool) (json.RawMessage, bool, error) {
	query := "SELECT name, description, widgets FROM sys_dashboards WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL"
	if forUpdate {
		query += " FOR UPDATE"
	}
	var row struct {
		Name        string          `db:"name"`
		Description *string         `db:"description"`
		Widgets     json.RawMessage `db:"widgets"`
	}
	if err := sqlx.GetContext(ctx, q, &row, query, tenantID, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("services.DashboardDocAdapter.LoadLive: %w", err)
	}
	d := dashboardDoc{Name: row.Name, Widgets: rawOrNull(row.Widgets)}
	if row.Description != nil {
		d.Description = *row.Description
	}
	b, err := d.marshal()
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// ListLiveKeys implements metaversion.DocAdapter.
func (*DashboardDocAdapter) ListLiveKeys(ctx context.Context, q sqlx.QueryerContext, tenantID string) ([]metaversion.DocRef, error) {
	var rows []struct {
		ID   string `db:"id"`
		Name string `db:"name"`
	}
	if err := sqlx.SelectContext(ctx, q, &rows, `SELECT id, name FROM sys_dashboards
		WHERE tenant_id = ? AND deleted_at IS NULL ORDER BY created_at, id`, tenantID); err != nil {
		return nil, fmt.Errorf("services.DashboardDocAdapter.ListLiveKeys: %w", err)
	}
	out := make([]metaversion.DocRef, len(rows))
	for i, r := range rows {
		out[i] = metaversion.DocRef{Key: r.ID, Label: r.Name}
	}
	return out, nil
}

// ApplyTx implements metaversion.DocAdapter. A delete also removes the dashboard's app-nav rows and
// home-page pointer IN THE SAME TX (dashboard.SoftDeleteExec → appmeta.PurgeNavForRef, FR-025): no
// dead tab survives a publish. S7-4 lifts this into the combined app change.
func (*DashboardDocAdapter) ApplyTx(ctx context.Context, tx *sqlx.Tx, tenantID string, c metaversion.Change) error {
	switch c.Op {
	case metaversion.OpDelete:
		err := dashboard.SoftDeleteExec(ctx, tx, tenantID, c.DocKey, time.Now())
		if err != nil && !errors.Is(err, dashboard.ErrDashboardNotFound) {
			return fmt.Errorf("services.DashboardDocAdapter.ApplyTx: delete %s: %w", c.DocKey, err)
		}
		return nil
	case metaversion.OpUpsert:
		var d dashboardDoc
		if err := json.Unmarshal(c.After, &d); err != nil {
			return fmt.Errorf("services.DashboardDocAdapter.ApplyTx: decode draft %s: %w", c.DocKey, err)
		}
		in := d.updateInput()
		if len(c.Before) > 0 {
			if err := dashboard.UpdateExec(ctx, tx, tenantID, c.DocKey, in); err != nil {
				return fmt.Errorf("services.DashboardDocAdapter.ApplyTx: update %s: %w", c.DocKey, err)
			}
			return nil
		}
		restored, err := dashboard.RestoreExec(ctx, tx, tenantID, c.DocKey, in)
		if err != nil {
			return fmt.Errorf("services.DashboardDocAdapter.ApplyTx: restore %s: %w", c.DocKey, err)
		}
		if restored {
			return nil
		}
		if err := dashboard.InsertWithIDExec(ctx, tx, c.DocKey, dashboard.CreateInput{TenantID: tenantID, Name: in.Name,
			Description: in.Description, Widgets: in.Widgets, CreatedBy: c.UserID}, time.Now()); err != nil {
			return fmt.Errorf("services.DashboardDocAdapter.ApplyTx: insert %s: %w", c.DocKey, err)
		}
		return nil
	}
	return fmt.Errorf("services.DashboardDocAdapter.ApplyTx: unknown op %q", c.Op)
}

// ValidateFinal implements metaversion.DocAdapter: every entity a widget of a live-to-be dashboard
// reads still exists.
func (*DashboardDocAdapter) ValidateFinal(ctx context.Context, f metaversion.FinalState) []metaversion.Problem {
	var problems []metaversion.Problem
	checked := map[string]bool{}
	for _, c := range f.Changes {
		if c.DocType != DocTypeDashboard || c.Op != metaversion.OpUpsert {
			continue
		}
		var d dashboardDoc
		if err := json.Unmarshal(c.After, &d); err != nil {
			problems = append(problems, metaversion.Problem{DocType: DocTypeDashboard, DocKey: c.DocKey, Code: "DASHBOARD_DRAFT_UNREADABLE", Message: "bản nháp của dashboard này không đọc được"})
			continue
		}
		for _, e := range dashboardEntities(d.Widgets) {
			if checked[e] {
				continue
			}
			checked[e] = true
			if !entityExistsTx(ctx, f, e) {
				problems = append(problems, metaversion.Problem{DocType: DocTypeDashboard, DocKey: c.DocKey, Code: "ENTITY_NOT_FOUND",
					Message: fmt.Sprintf("dashboard %q có widget trỏ tới bảng %q không còn tồn tại", d.Name, e)})
			}
		}
	}
	return problems
}

// Redact implements metaversion.DocAdapter: a dashboard holds no secret.
func (*DashboardDocAdapter) Redact(doc json.RawMessage) json.RawMessage { return doc }

// Entities implements metaversion.DocAdapter: dashboards have no engine cache to drop.
func (*DashboardDocAdapter) Entities(json.RawMessage) []string { return nil }

// dashboardEntities lists the distinct entity names of a widgets array (unreadable ⇒ none).
func dashboardEntities(widgets json.RawMessage) []string {
	var ws []struct {
		Entity string `json:"entity"`
	}
	if json.Unmarshal(widgets, &ws) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range ws {
		if w.Entity != "" && !seen[w.Entity] {
			seen[w.Entity] = true
			out = append(out, w.Entity)
		}
	}
	return out
}

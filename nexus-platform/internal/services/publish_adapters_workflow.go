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

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/workflow"
)

// DocTypeWorkflow is the draft-layer type of an automation rule; its key is the rule id.
const DocTypeWorkflow = "workflow"

// workflowDoc is the canonical form of a rule: EXACTLY the seven columns workflow.UpdateExec writes
// (no timestamps, no actor), so a draft saved identical to the live rule hashes identical to it and
// publish writes back precisely what the builder saw.
type workflowDoc struct {
	Name            string          `json:"name"`
	EntityName      string          `json:"entityName"`
	Trigger         string          `json:"trigger"`
	Condition       json.RawMessage `json:"condition"`
	Actions         json.RawMessage `json:"actions"`
	IsActive        bool            `json:"isActive"`
	IntervalSeconds int             `json:"intervalSeconds"`
}

func rawOrNull(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

func workflowDocOfUpdate(in workflow.UpdateInput) workflowDoc {
	return workflowDoc{Name: in.Name, EntityName: in.EntityName, Trigger: string(in.Trigger),
		Condition: rawOrNull(in.Condition), Actions: rawOrNull(in.Actions), IsActive: in.IsActive, IntervalSeconds: in.IntervalSeconds}
}

func workflowDocOfCreate(in workflow.CreateInput) workflowDoc {
	return workflowDoc{Name: in.Name, EntityName: in.EntityName, Trigger: string(in.Trigger),
		Condition: rawOrNull(in.Condition), Actions: rawOrNull(in.Actions), IsActive: in.IsActive, IntervalSeconds: in.IntervalSeconds}
}

func workflowDocOfRecord(r *workflow.WorkflowRecord) workflowDoc {
	return workflowDoc{Name: r.Name, EntityName: r.EntityName, Trigger: r.Trigger,
		Condition: rawOrNull(r.Condition), Actions: rawOrNull(r.Actions), IsActive: r.IsActive, IntervalSeconds: r.IntervalSeconds}
}

func (d workflowDoc) marshal() (json.RawMessage, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("services.workflowDoc.marshal: %w", err)
	}
	return b, nil
}

// updateInput maps a canonical doc back to the columns UpdateExec / InsertWithIDExec write. A JSON
// null condition is stored as SQL NULL (match-all), exactly as the service stores it.
func (d workflowDoc) updateInput(actor string) workflow.UpdateInput {
	var cond []byte
	if c := string(d.Condition); c != "" && c != "null" {
		cond = []byte(d.Condition)
	}
	return workflow.UpdateInput{Name: d.Name, EntityName: d.EntityName, Trigger: workflow.Trigger(d.Trigger),
		Condition: cond, Actions: []byte(d.Actions), IsActive: d.IsActive, IntervalSeconds: d.IntervalSeconds, UpdatedBy: actor}
}

// WorkflowDocAdapter binds the "workflow" document type to sys_workflows.
type WorkflowDocAdapter struct{}

// NewWorkflowDocAdapter returns the adapter (stateless: every call takes the querier it works on).
func NewWorkflowDocAdapter() *WorkflowDocAdapter { return &WorkflowDocAdapter{} }

// Type implements metaversion.DocAdapter.
func (*WorkflowDocAdapter) Type() string { return DocTypeWorkflow }

type liveWorkflowRow struct {
	Name            string           `db:"name"`
	EntityName      string           `db:"entity_name"`
	Trigger         string           `db:"trigger_event"`
	Condition       *json.RawMessage `db:"condition"`
	Actions         json.RawMessage  `db:"actions"`
	IsActive        bool             `db:"is_active"`
	IntervalSeconds int              `db:"interval_seconds"`
}

// LoadLive implements metaversion.DocAdapter.
func (*WorkflowDocAdapter) LoadLive(ctx context.Context, q sqlx.QueryerContext, tenantID, key string, forUpdate bool) (json.RawMessage, bool, error) {
	query := "SELECT name, entity_name, trigger_event, `condition`, actions, is_active, interval_seconds FROM sys_workflows" +
		" WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL"
	if forUpdate {
		query += " FOR UPDATE"
	}
	var row liveWorkflowRow
	if err := sqlx.GetContext(ctx, q, &row, query, tenantID, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("services.WorkflowDocAdapter.LoadLive: %w", err)
	}
	d := workflowDoc{Name: row.Name, EntityName: row.EntityName, Trigger: row.Trigger, Actions: rawOrNull(row.Actions),
		IsActive: row.IsActive, IntervalSeconds: row.IntervalSeconds, Condition: json.RawMessage("null")}
	if row.Condition != nil && len(*row.Condition) > 0 {
		d.Condition = *row.Condition
	}
	b, err := d.marshal()
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// ListLiveKeys implements metaversion.DocAdapter.
func (*WorkflowDocAdapter) ListLiveKeys(ctx context.Context, q sqlx.QueryerContext, tenantID string) ([]metaversion.DocRef, error) {
	var rows []struct {
		ID         string `db:"id"`
		Name       string `db:"name"`
		EntityName string `db:"entity_name"`
	}
	if err := sqlx.SelectContext(ctx, q, &rows, `SELECT id, name, entity_name FROM sys_workflows
		WHERE tenant_id = ? AND deleted_at IS NULL ORDER BY created_at, id`, tenantID); err != nil {
		return nil, fmt.Errorf("services.WorkflowDocAdapter.ListLiveKeys: %w", err)
	}
	out := make([]metaversion.DocRef, len(rows))
	for i, r := range rows {
		out[i] = metaversion.DocRef{Key: r.ID, Label: r.Name, EntityName: r.EntityName}
	}
	return out, nil
}

// ApplyTx implements metaversion.DocAdapter: it writes ONE change through the same *Exec statements
// the admin repository uses, on the publish transaction.
func (*WorkflowDocAdapter) ApplyTx(ctx context.Context, tx *sqlx.Tx, tenantID string, c metaversion.Change) error {
	switch c.Op {
	case metaversion.OpDelete:
		err := workflow.SoftDeleteExec(ctx, tx, tenantID, c.DocKey, time.Now())
		if err != nil && !errors.Is(err, workflow.ErrWorkflowNotFound) { // already gone (forced publish): the intent is met
			return fmt.Errorf("services.WorkflowDocAdapter.ApplyTx: delete %s: %w", c.DocKey, err)
		}
		return nil
	case metaversion.OpUpsert:
		var d workflowDoc
		if err := json.Unmarshal(c.After, &d); err != nil {
			return fmt.Errorf("services.WorkflowDocAdapter.ApplyTx: decode draft %s: %w", c.DocKey, err)
		}
		in := d.updateInput(c.UserID)
		if len(c.Before) > 0 { // live row exists
			if err := workflow.UpdateExec(ctx, tx, tenantID, c.DocKey, in); err != nil {
				return fmt.Errorf("services.WorkflowDocAdapter.ApplyTx: update %s: %w", c.DocKey, err)
			}
			return nil
		}
		// No live row: either a brand-new rule, or (forced publish) one the customer admin deleted.
		restored, err := workflow.RestoreExec(ctx, tx, tenantID, c.DocKey, in)
		if err != nil {
			return fmt.Errorf("services.WorkflowDocAdapter.ApplyTx: restore %s: %w", c.DocKey, err)
		}
		if restored {
			return nil
		}
		if err := workflow.InsertWithIDExec(ctx, tx, c.DocKey, workflow.CreateInput{
			TenantID: tenantID, Name: in.Name, EntityName: in.EntityName, Trigger: in.Trigger, Condition: in.Condition,
			Actions: in.Actions, IsActive: in.IsActive, IntervalSeconds: in.IntervalSeconds, CreatedBy: c.UserID,
		}, time.Now()); err != nil {
			return fmt.Errorf("services.WorkflowDocAdapter.ApplyTx: insert %s: %w", c.DocKey, err)
		}
		return nil
	}
	return fmt.Errorf("services.WorkflowDocAdapter.ApplyTx: unknown op %q", c.Op)
}

// ValidateFinal implements metaversion.DocAdapter: the cross-document checks only the FINAL state
// can answer, read on the publish tx after the tenant lock — (1) every rule that will be live points
// at an entity that exists, (2) the per-(entity, trigger) cap on active before rules still holds.
// Per-document shape checks already ran when each draft was saved (the service body).
func (*WorkflowDocAdapter) ValidateFinal(ctx context.Context, f metaversion.FinalState) []metaversion.Problem {
	var problems []metaversion.Problem
	changedIDs := make([]string, 0, len(f.Changes))
	type bucket struct{ entity, trigger string }
	activeAfter := map[bucket]int{}
	entityChecked := map[string]bool{}
	for _, c := range f.Changes {
		if c.DocType != DocTypeWorkflow {
			continue
		}
		changedIDs = append(changedIDs, c.DocKey)
		if c.Op != metaversion.OpUpsert {
			continue
		}
		var d workflowDoc
		if err := json.Unmarshal(c.After, &d); err != nil {
			problems = append(problems, metaversion.Problem{DocType: DocTypeWorkflow, DocKey: c.DocKey, Code: "WORKFLOW_DRAFT_UNREADABLE", Message: "bản nháp của luật này không đọc được"})
			continue
		}
		if !entityChecked[d.EntityName] {
			var n int
			if err := sqlx.GetContext(ctx, f.Q, &n, `SELECT COUNT(*) FROM sys_entities WHERE tenant_id = ? AND name = ? AND deleted_at IS NULL`,
				f.TenantID, d.EntityName); err != nil || n == 0 {
				problems = append(problems, metaversion.Problem{DocType: DocTypeWorkflow, DocKey: c.DocKey, Code: "ENTITY_NOT_FOUND",
					Message: fmt.Sprintf("luật %q trỏ tới bảng %q không còn tồn tại", d.Name, d.EntityName)})
			}
			entityChecked[d.EntityName] = true
		}
		if d.IsActive && workflow.IsBeforeTrigger(workflow.Trigger(d.Trigger)) {
			activeAfter[bucket{d.EntityName, d.Trigger}]++
		}
	}
	buckets := make([]bucket, 0, len(activeAfter))
	for b := range activeAfter {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].entity != buckets[j].entity {
			return buckets[i].entity < buckets[j].entity
		}
		return buckets[i].trigger < buckets[j].trigger
	})
	for _, b := range buckets {
		// Live rules of the bucket that this publish does not touch, plus the changed ones that stay active there.
		q, args := `SELECT COUNT(*) FROM sys_workflows WHERE tenant_id = ? AND entity_name = ? AND trigger_event = ? AND is_active = 1 AND deleted_at IS NULL`,
			[]any{f.TenantID, b.entity, b.trigger}
		if len(changedIDs) > 0 {
			in, inArgs, err := sqlx.In(" AND id NOT IN (?)", changedIDs)
			if err != nil {
				problems = append(problems, metaversion.Problem{DocType: DocTypeWorkflow, Code: "PUBLISH_CHECK_FAILED", Message: "không kiểm được trần luật trước khi lưu"})
				continue
			}
			q += in
			args = append(args, inArgs...)
		}
		var untouched int
		if err := sqlx.GetContext(ctx, f.Q, &untouched, q, args...); err != nil {
			problems = append(problems, metaversion.Problem{DocType: DocTypeWorkflow, Code: "PUBLISH_CHECK_FAILED", Message: "không kiểm được trần luật trước khi lưu"})
			continue
		}
		if total := untouched + activeAfter[b]; total > maxActiveBeforeRules {
			problems = append(problems, metaversion.Problem{DocType: DocTypeWorkflow, Code: "WORKFLOW_BEFORE_RULE_LIMIT",
				Message: fmt.Sprintf("bảng %q sẽ có %d luật %s đang bật sau khi xuất bản (tối đa %d)", b.entity, total, b.trigger, maxActiveBeforeRules)})
		}
	}
	return problems
}

// Redact implements metaversion.DocAdapter: the webhook key material of the rule's actions is masked
// before the document leaves through the changes / versions APIs.
func (*WorkflowDocAdapter) Redact(doc json.RawMessage) json.RawMessage {
	var d workflowDoc
	if err := json.Unmarshal(doc, &d); err != nil {
		return json.RawMessage("null") // never echo a body this function cannot read: it may hold a key
	}
	d.Actions = workflow.RedactActionsJSON(d.Actions)
	b, err := d.marshal()
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// Entities implements metaversion.DocAdapter.
func (*WorkflowDocAdapter) Entities(doc json.RawMessage) []string {
	var d workflowDoc
	if err := json.Unmarshal(doc, &d); err != nil || d.EntityName == "" {
		return nil
	}
	return []string{d.EntityName}
}

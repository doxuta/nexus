package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nexus/platform/internal/metadiff"
	"nexus/platform/internal/workflow"
)

// changePathsMax is how many changed places one card lists; the rest become MoreChanged.
const changePathsMax = 5

// DraftChangeTarget names what the proposal touches.
type DraftChangeTarget struct {
	Entity string `json:"entity,omitempty"`
	Field  string `json:"field,omitempty"`
	View   string `json:"view,omitempty"`
	Name   string `json:"name,omitempty"`
}

// DraftChange is the preview envelope of one proposal (specs/067 plan §7): create or update or
// delete, with Before/After projections (already secret-redacted) and where they differ.
// DocType/DocKey follow the metaversion registry of specs/063-draft-publish.
type DraftChange struct {
	DocType      string            `json:"docType"`
	DocKey       string            `json:"docKey,omitempty"`
	Target       DraftChangeTarget `json:"target"`
	Op           string            `json:"op"` // create | update | delete
	Label        string            `json:"label"`
	Before       json.RawMessage   `json:"before,omitempty"`
	After        json.RawMessage   `json:"after,omitempty"`
	ChangedPaths []string          `json:"changedPaths,omitempty"`
	MoreChanged  int               `json:"moreChanged,omitempty"`
}

// newChange builds the envelope; before/after are projections (any JSON-able value, nil = absent).
func newChange(docType, docKey, op, label string, target DraftChangeTarget, before, after any) *DraftChange {
	c := &DraftChange{DocType: docType, DocKey: docKey, Op: op, Label: label, Target: target}
	c.Before, c.After = marshalProjection(before), marshalProjection(after)
	if op == "update" {
		c.ChangedPaths, c.MoreChanged = changedPaths(docType, before, after)
	}
	return c
}

func marshalProjection(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// changedPaths = Limit(Diff(Canonicalize(before), Canonicalize(after))), via a JSON round trip so
// both sides go through the same decode as the stored documents do.
func changedPaths(docType string, before, after any) ([]string, int) {
	norm := func(v any) any {
		var out any
		if b, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(b, &out)
		}
		metadiff.Canonicalize(docType, out)
		return out
	}
	return metadiff.Limit(metadiff.Diff(norm(before), norm(after)), changePathsMax)
}

// attachChanges gives every item appended since `from` that has no envelope its default one and
// returns the last item's envelope. "app" gets none (L10-4b).
func (e *toolExecutor) attachChanges(from int) *DraftChange {
	var last *DraftChange
	for i := from; i < len(e.draft); i++ {
		if e.draft[i].Change == nil {
			e.draft[i].Change = simpleChange(&e.draft[i])
		}
		last = e.draft[i].Change
	}
	return last
}

// simpleChange covers the kinds whose Before (if any) is already in the draft payload.
func simpleChange(it *DraftItem) *DraftChange {
	switch it.Kind {
	case "entity":
		d := it.Entity
		return newChange("entity", "", "create", fmt.Sprintf("Bảng %q", d.Name), DraftChangeTarget{Entity: d.Name}, nil, d)
	case "field":
		d := it.Field
		return newChange("field", "", "create", fmt.Sprintf("Trường %q", d.Name), DraftChangeTarget{Entity: d.Entity, Field: d.Name}, nil, d)
	case "relationship":
		d := it.Relationship
		return newChange("relationship", "", "create", fmt.Sprintf("Quan hệ %q", d.Name), DraftChangeTarget{Name: d.Name}, nil, d)
	case "approval":
		d := it.Approval
		return newChange("approval", "", "create", fmt.Sprintf("Quy trình duyệt %q", d.Name), DraftChangeTarget{Name: d.Name}, nil, d)
	case "dashboard":
		d := it.Dashboard
		return newChange("dashboard", "", "create", fmt.Sprintf("Bảng điều khiển %q", d.Name), DraftChangeTarget{Name: d.Name}, nil, d)
	case "workqueue":
		d := it.WorkQueue
		return newChange("workqueue", "", "create", fmt.Sprintf("Hàng đợi việc %q", d.Name), DraftChangeTarget{Name: d.Name}, nil, d)
	case "records":
		d := it.Records
		return newChange("records", "", "create", fmt.Sprintf("Dữ liệu mẫu cho %q", d.Entity), DraftChangeTarget{Entity: d.Entity},
			nil, map[string]any{"entity": d.Entity, "rowCount": len(d.Rows)})
	case "edit_field":
		d := it.EditField
		return newChange("field", d.FieldID, "update", fmt.Sprintf("Trường %q", d.Field), DraftChangeTarget{Entity: d.Entity, Field: d.Field},
			fieldProjection(d.Before), fieldProjection(d.After))
	case "delete_field":
		d := it.DeleteField
		return newChange("field", d.FieldID, "delete", fmt.Sprintf("Trường %q", d.Field), DraftChangeTarget{Entity: d.Entity, Field: d.Field},
			map[string]any{"label": d.Label, "fieldType": d.FieldType}, nil)
	case "delete_entity":
		d := it.DeleteEntity
		return newChange("entity", "", "delete", fmt.Sprintf("Bảng %q", d.Entity), DraftChangeTarget{Entity: d.Entity},
			map[string]any{"label": d.Label, "fieldCount": d.FieldCount}, nil)
	}
	return nil
}

// fieldProjection is the plan-§7 field projection {label,isRequired,defaultValue,options,description}.
func fieldProjection(s DraftFieldState) map[string]any {
	return map[string]any{"label": s.Label, "isRequired": s.IsRequired, "defaultValue": s.DefaultValue, "options": s.Options, "description": s.Description}
}

// executeWithChange runs a tool and, for every proposal it buffered, stamps the change envelope
// and adds {op, changedPaths} next to "buffered" in the result so the model answers truthfully.
func (e *toolExecutor) executeWithChange(ctx context.Context, name, argsJSON string) string {
	from := len(e.draft)
	res := e.dispatch(ctx, name, argsJSON)
	if len(e.draft) == from {
		return res
	}
	last := e.attachChanges(from)
	if last == nil {
		return res
	}
	var env struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if json.Unmarshal([]byte(res), &env) != nil || !env.OK || env.Result == nil {
		return res
	}
	env.Result["op"] = last.Op
	if len(last.ChangedPaths) > 0 {
		env.Result["changedPaths"] = last.ChangedPaths
	}
	return toolOK(env.Result)
}

func previewFailed(err error) string {
	return toolErr("PREVIEW_READ_FAILED: không đọc được bản đang có để so sánh (%v) — thử lại", err)
}

// layoutChange reads the layout currently SERVED for (entity, view) and builds the envelope.
func (e *toolExecutor) layoutChange(ctx context.Context, d *DraftLayout) (*DraftChange, string) {
	if e.docs == nil || e.docs.Layouts == nil {
		return nil, docUnavailable()
	}
	res, err := e.docs.Layouts.GetLayout(ctx, e.tenantID, d.Entity, d.ViewType)
	if err != nil {
		return nil, previewFailed(err)
	}
	v := res.Views[d.ViewType]
	if v == nil {
		return nil, previewFailed(fmt.Errorf("không có màn %s", d.ViewType))
	}
	label, key := fmt.Sprintf("Bố cục %s của %q", d.ViewType, d.Entity), d.Entity+":"+d.ViewType
	if v.IsDefault {
		label += " (mặc định)"
		key = ""
	}
	proj := func(l map[string]any) map[string]any {
		return map[string]any{"entityName": d.Entity, "viewType": d.ViewType, "layout": l}
	}
	return newChange("layout", key, "update", label, DraftChangeTarget{Entity: d.Entity, View: d.ViewType}, proj(v.LayoutData), proj(d.LayoutJSON)), ""
}

func workflowProjection(name, entity, trigger string, cond, actions any, active bool, interval int) map[string]any {
	return map[string]any{"name": name, "entityName": entity, "trigger": trigger, "condition": cond, "actions": actions, "isActive": active, "intervalSeconds": interval}
}

func rawJSON(r json.RawMessage) any {
	if len(strings.TrimSpace(string(r))) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(r, &v) != nil {
		return nil
	}
	return v
}

// workflowChange finds a same-named rule (trimmed, as CreateWorkflow does) and builds the envelope.
func (e *toolExecutor) workflowChange(ctx context.Context, d *DraftWorkflow) (*DraftChange, string) {
	if e.docs == nil || e.docs.Workflows == nil {
		return nil, docUnavailable()
	}
	recs, err := e.docs.Workflows.ListWorkflows(ctx, e.tenantID)
	if err != nil {
		return nil, previewFailed(err)
	}
	after := workflowProjection(strings.TrimSpace(d.Name), d.EntityName, d.Trigger, d.Condition, d.Actions, d.IsActive, 0)
	label, target := fmt.Sprintf("Luật %q", d.Name), DraftChangeTarget{Entity: d.EntityName, Name: d.Name}
	for _, r := range recs {
		if strings.TrimSpace(r.Name) != strings.TrimSpace(d.Name) {
			continue
		}
		if t := workflow.Trigger(r.Trigger); t == workflow.TriggerBeforeSave || t == workflow.TriggerBeforeDelete {
			return nil, toolErr("WORKFLOW_NAME_TAKEN: đã có luật chặn khi lưu/xoá tên %q — đổi tên luật mới (không đề xuất ghi đè luật chặn)", d.Name)
		}
		before := workflowProjection(r.Name, r.EntityName, r.Trigger, rawJSON(r.Condition),
			rawJSON(workflow.RedactActionsForDisplay(r.Actions)), r.IsActive, r.IntervalSeconds)
		return newChange("workflow", r.ID, "update", label, target, before, after), ""
	}
	return newChange("workflow", "", "create", label, target, nil, after), ""
}

// reportChange: same-named report (trimmed) ⇒ update.
func (e *toolExecutor) reportChange(ctx context.Context, d *DraftReport) (*DraftChange, string) {
	if e.docs == nil || e.docs.Reports == nil {
		return nil, docUnavailable()
	}
	recs, err := e.docs.Reports.ListReports(ctx, e.tenantID)
	if err != nil {
		return nil, previewFailed(err)
	}
	proj := func(name, entity, typ string, cfg any, active bool) map[string]any {
		return map[string]any{"name": name, "entityName": entity, "type": typ, "config": cfg, "isActive": active}
	}
	after := proj(strings.TrimSpace(d.Name), d.EntityName, d.Type, d.Config, d.IsActive)
	label, target := fmt.Sprintf("Báo cáo %q", d.Name), DraftChangeTarget{Entity: d.EntityName, Name: d.Name}
	for _, r := range recs {
		if strings.TrimSpace(r.Name) == strings.TrimSpace(d.Name) {
			return newChange("report", r.ID, "update", label, target, proj(r.Name, r.EntityName, r.Type, rawJSON(r.Config), r.IsActive), after), ""
		}
	}
	return newChange("report", "", "create", label, target, nil, after), ""
}

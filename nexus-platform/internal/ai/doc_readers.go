package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// Read-tool limits (specs/067 D7): at most docListCap items and docByteCap bytes per answer, so a
// big tenant cannot flood the model's context.
const (
	docListCap = 50
	docByteCap = 8 * 1024
)

// The four readers are satisfied by the real services with no adapter (precedent: AccessProbes) —
// so once specs/063 wraps the services' repos, the model reads the draft overlay by construction.
type layoutReader interface {
	GetLayout(ctx context.Context, tenantID, entityName, viewFilter string) (*services.GetLayoutResult, error)
}
type workflowReader interface {
	ListWorkflows(ctx context.Context, tenantID string) ([]workflow.WorkflowRecord, error)
}
type reportReader interface {
	ListReports(ctx context.Context, tenantID string) ([]report.Record, error)
}

// DocReaders is what get_layout / list_workflows / list_reports / get_app read through.
// nil (or a nil member) ⇒ the tool answers DOC_READ_UNAVAILABLE.
type DocReaders struct {
	Layouts   layoutReader
	Workflows workflowReader
	Reports   reportReader
	Apps      appNavReader
}

func docUnavailable() string {
	return toolErr("DOC_READ_UNAVAILABLE: server chưa nối bộ đọc tài liệu cho AI Builder — soi thủ công ở màn Studio")
}

// resolveEntity returns the canonical name of a tenant-scoped entity ("" + a tool error otherwise).
func (e *toolExecutor) resolveEntity(ctx context.Context, name string) (string, string) {
	ent, err := e.reader.GetEntityByName(ctx, e.tenantID, name)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return "", toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại trong tenant hiện tại", name)
		}
		return "", toolErr("DOC_READ_FAILED: %v", err)
	}
	return ent.Name, ""
}

func (e *toolExecutor) getLayout(ctx context.Context, argsJSON string) string {
	if e.docs == nil || e.docs.Layouts == nil {
		return docUnavailable()
	}
	var a struct {
		Entity   string `json:"entity"`
		ViewType string `json:"viewType"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	name, errStr := e.resolveEntity(ctx, a.Entity)
	if errStr != "" {
		return errStr
	}
	res, err := e.docs.Layouts.GetLayout(ctx, e.tenantID, name, a.ViewType)
	if err != nil {
		return toolErr("DOC_READ_FAILED: %v", err)
	}
	if a.ViewType == "" {
		views := []map[string]any{}
		for _, vt := range []string{"edit", "detail", "list"} {
			if v := res.Views[vt]; v != nil {
				views = append(views, map[string]any{"viewType": vt, "isCustom": !v.IsDefault})
			}
		}
		return toolOK(map[string]any{"entity": name, "views": views})
	}
	v := res.Views[a.ViewType]
	if v == nil {
		return toolErr("INVALID_ARGS: viewType phải là edit|detail|list")
	}
	out := map[string]any{"entity": name, "viewType": a.ViewType, "isCustom": !v.IsDefault, "layout": v.LayoutData}
	if b, _ := json.Marshal(out); len(b) > docByteCap {
		out = map[string]any{"entity": name, "viewType": a.ViewType, "isCustom": !v.IsDefault, "truncated": true}
	}
	return toolOK(out)
}

// docList caps items at docListCap and docByteCap bytes, flagging more/truncated honestly.
func docList(key string, items []map[string]any) string {
	more := len(items) > docListCap
	if more {
		items = items[:docListCap]
	}
	truncated := false
	for {
		out := map[string]any{key: items}
		if more {
			out["more"] = true
		}
		if truncated {
			out["truncated"] = true
		}
		b, _ := json.Marshal(out)
		if len(b) <= docByteCap || len(items) == 0 {
			return toolOK(out)
		}
		items, truncated, more = items[:len(items)-1], true, true
	}
}

func rawOrNil(r json.RawMessage) any {
	if len(strings.TrimSpace(string(r))) == 0 {
		return nil
	}
	return r
}

func (e *toolExecutor) listWorkflows(ctx context.Context, argsJSON string) string {
	if e.docs == nil || e.docs.Workflows == nil {
		return docUnavailable()
	}
	entity, errStr := e.docEntityFilter(ctx, argsJSON)
	if errStr != "" {
		return errStr
	}
	recs, err := e.docs.Workflows.ListWorkflows(ctx, e.tenantID)
	if err != nil {
		return toolErr("DOC_READ_FAILED: %v", err)
	}
	items := []map[string]any{}
	for _, r := range recs {
		if entity != "" && r.EntityName != entity {
			continue
		}
		items = append(items, map[string]any{
			"name": r.Name, "entity": r.EntityName, "trigger": r.Trigger, "isActive": r.IsActive,
			"condition": rawOrNil(r.Condition), "actions": rawOrNil(workflow.RedactActionsForDisplay(r.Actions)),
		})
	}
	return docList("workflows", items)
}

func (e *toolExecutor) listReports(ctx context.Context, argsJSON string) string {
	if e.docs == nil || e.docs.Reports == nil {
		return docUnavailable()
	}
	entity, errStr := e.docEntityFilter(ctx, argsJSON)
	if errStr != "" {
		return errStr
	}
	recs, err := e.docs.Reports.ListReports(ctx, e.tenantID)
	if err != nil {
		return toolErr("DOC_READ_FAILED: %v", err)
	}
	items := []map[string]any{}
	for _, r := range recs {
		if entity != "" && r.EntityName != entity {
			continue
		}
		items = append(items, map[string]any{
			"name": r.Name, "entity": r.EntityName, "type": r.Type, "isActive": r.IsActive, "config": rawOrNil(r.Config),
		})
	}
	return docList("reports", items)
}

// docEntityFilter reads the optional `entity` argument and resolves it tenant-scoped.
func (e *toolExecutor) docEntityFilter(ctx context.Context, argsJSON string) (string, string) {
	var a struct {
		Entity string `json:"entity"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", toolErr("INVALID_ARGS: %v", err)
	}
	if a.Entity == "" {
		return "", ""
	}
	return e.resolveEntity(ctx, a.Entity)
}

func (e *toolExecutor) getApp(ctx context.Context, argsJSON string) string {
	if e.docs == nil || e.docs.Apps == nil {
		return docUnavailable()
	}
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	// IsAdmin: the builder is an admin-gated surface; the same scope the context resolver uses.
	app, nav, err := e.docs.Apps.GetNav(ctx, services.RecordScope{IsAdmin: true}, e.tenantID, a.Name)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			return toolErr("%s: %s", ve.Code, ve.Msg)
		}
		return toolErr("DOC_READ_FAILED: %v", err)
	}
	items := make([]map[string]any, 0, len(nav))
	for _, it := range nav {
		items = append(items, map[string]any{"type": it.Type, "label": it.Label, "name": it.RefName})
	}
	return toolOK(map[string]any{"name": app.Name, "label": app.Label, "nav": items})
}

func docToolDefinitions() []Tool {
	entityArg := func(desc string) map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{
			"entity": map[string]any{"type": "string", "description": desc},
		}}
	}
	layoutParams := map[string]any{"type": "object", "properties": map[string]any{
		"entity":   map[string]any{"type": "string", "description": "Tên entity (vd 'patients')."},
		"viewType": map[string]any{"type": "string", "enum": []string{"edit", "detail", "list"}, "description": "Bỏ trống = chỉ liệt kê 3 màn và màn nào đã tuỳ biến (isCustom); có = trả bố cục của màn đó."},
	}, "required": []string{"entity"}}
	appParams := map[string]any{"type": "object", "properties": map[string]any{
		"name": map[string]any{"type": "string", "description": "Tên (slug) app, vd 'crm'."},
	}, "required": []string{"name"}}
	def := func(name, desc string, params map[string]any) Tool {
		return Tool{Type: "function", Function: ToolFunction{Name: name, Description: desc, Parameters: params}}
	}
	return []Tool{
		def("get_layout", "ĐỌC bố cục form/chi tiết/danh sách ĐANG CÓ của một entity (isCustom=false là bản mặc định tự sinh). Soi TRƯỚC propose_layout để biết sẽ đè lên gì. Tối đa 8 KB.", layoutParams),
		def("list_workflows", "ĐỌC các luật tự động hoá đang có (tên, bảng, trigger, điều kiện, hành động). Khoá bí mật của webhook đã bị che. Lọc theo `entity` nếu có; tối đa 50 mục / 8 KB. Soi TRƯỚC khi đề xuất luật mới hay sửa luật cũ.", entityArg("Chỉ lấy luật của bảng này (tuỳ chọn).")),
		def("list_reports", "ĐỌC các báo cáo đang có (tên, bảng, loại, cấu hình). Lọc theo `entity` nếu có; tối đa 50 mục / 8 KB.", entityArg("Chỉ lấy báo cáo của bảng này (tuỳ chọn).")),
		def("get_app", "ĐỌC menu (nav) đang có của một app, đúng thứ tự hiển thị.", appParams),
	}
}

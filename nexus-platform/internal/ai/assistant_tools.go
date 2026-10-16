// assistant_tools.go — the ⌘K end-user data assistant's OWN tool surface: read + HITL-draft,
// ZERO direct writes.
//
// This registry is deliberately SEPARATE from the builder's ToolDefinitions() (trap #3 of the
// slice design): the assistant advertises six READ tools plus two HITL RECORD-DRAFT tools
// (propose_record_create / propose_record_update — validate-and-buffer only, the actual write
// happens later from the FE via POST /:entity / PUT /:entity/:id under the caller's own token)
// and structurally cannot reach the builder's metadata propose_*/snapshot_model/admin surfaces.
// A "filter the builder registry by mode" approach would silently regress the moment a new
// builder tool is added — a separate explicit list cannot.
//
// ACL model (trap #1/#2/#4): every record read goes through RecordReader, whose production
// implementation (RecordReadAdapter) wraps services.RecordService + services.ACLService — so
// tenant pinning, entity-level ACL, A5 row-level scope AND field-level ACL are enforced by the
// SAME engine the normal browse endpoints use. The caller's services.RecordScope is built by the
// HANDLER (scopeOf) and threaded down here VERBATIM — never rebuilt from the thin ai.Scope.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

// Row-count bounds for query_records: the model reads the rows back as prompt tokens, so the
// page must stay small. 50 mirrors the task cap; 10 is a sane default for "show me…" questions.
const (
	assistantMaxRows     = 50
	assistantDefaultRows = 10
)

// RecordReader is the READ-ONLY, ACL-enforced data surface the assistant tools consume. It is
// declared consumer-side (production: *RecordReadAdapter over RecordService + ACLService +
// metadata repo) so unit tests inject a fake that records the scope it receives — proving the
// caller's RecordScope is threaded verbatim, never substituted.
//
// Every method takes the caller's scope + the request tenant as its scoping arguments; entity
// names come from model tool-calls and are validated/authorized inside the implementation.
type RecordReader interface {
	// QueryRecords lists records of one entity the caller may view (tenant + entity-ACL + A5
	// row scope + field-ACL enforced downstream). An entity that does not exist OR that the
	// caller may not view surfaces as services.ErrEntityNotFound (no existence leak).
	QueryRecords(ctx context.Context, scope services.RecordScope, tenantID, entity string, q services.ListQuery) (services.ListResult, error)
	// AggregateRecords computes count/sum/avg/min/max (optionally grouped/bucketed) over the
	// rows the caller may read — same scoping contract as QueryRecords.
	AggregateRecords(ctx context.Context, scope services.RecordScope, tenantID, entity string, q services.AggregateQuery) (services.AggregateResult, error)
	// ViewableEntities returns ONLY the entities the caller's role may "view" (trap #2: the
	// builder's list_entities is admin-wide; the assistant's must be ACL-filtered). Admins see
	// every entity of the tenant.
	ViewableEntities(ctx context.Context, scope services.RecordScope, tenantID string) ([]metadata.EntityDefinition, error)
	// Fields returns the field definitions of one entity — only when the caller may view that
	// entity; otherwise services.ErrEntityNotFound (same no-leak contract as QueryRecords) — and
	// only the fields the caller's role may view (field-ACL hidden ones are absent, not flagged).
	Fields(ctx context.Context, scope services.RecordScope, tenantID, entity string) ([]metadata.FieldDefinition, error)
	// ListReports returns the tenant's saved reports (A3 semantic layer) — the same catalog the
	// non-admin GET /reports route already serves every authenticated user.
	ListReports(ctx context.Context, scope services.RecordScope, tenantID string) ([]report.Record, error)
	// RunReport executes one saved report under the caller's scope; the service resolves the
	// per-entity "view" level itself (fail-closed AuthorizeView — same decision as browsing).
	RunReport(ctx context.Context, scope services.RecordScope, tenantID, id string) (services.ReportRunResult, error)
	// GetRecord fetches one record by id under the caller's scope — the propose_record_update
	// existence probe. Same scoping contract as QueryRecords: forbidden/nonexistent entity →
	// services.ErrEntityNotFound; a record outside the caller's row scope (or absent) →
	// services.ErrRecordNotFound (no cross-team/cross-tenant leak — same engine as GET /:entity/:id).
	GetRecord(ctx context.Context, scope services.RecordScope, tenantID, entity, id string) (*dynrepo.Record, error)
}

// AssistantToolDefinitions returns the assistant's eight tools: six READ tools + two HITL
// record-DRAFT tools (⌘K-can-act, EASE Nước 7). CRITICAL invariants (pinned by tests): no schema
// declares a tenant parameter (tenant comes from the verified JWT, one-way, exactly like the
// builder), and no tool is a builder metadata/admin surface — the only propose_* names here are
// the two record drafts, which validate-and-buffer under the caller's own ACL and never write.
func AssistantToolDefinitions() []Tool {
	return []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "list_entities",
				Description: "Liệt kê các entity (name + label) NGƯỜI DÙNG HIỆN TẠI được xem. Gọi trước khi truy vấn nếu chưa chắc tên entity.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "list_fields",
				Description: "Liệt kê field của một entity người dùng được xem (name, label, fieldType, required) — để biết lọc/sắp xếp/tính toán trên field nào.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity": map[string]any{"type": "string", "description": "Tên entity cần xem field (vd 'opportunities')."},
					},
					"required": []string{"entity"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name: "query_records",
				Description: "ĐỌC danh sách record của một entity người dùng được xem — có lọc, sắp xếp, giới hạn dòng (tối đa 50). " +
					"Dùng cho câu hỏi 'liệt kê / cho xem / top N'. Chỉ đọc — không sửa được gì.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity":  map[string]any{"type": "string", "description": "Tên entity cần đọc (vd 'opportunities')."},
						"filters": assistantFilterArraySchema(),
						"limit":   map[string]any{"type": "integer", "description": "Số record tối đa trả về (1..50, mặc định 10)."},
						"sort":    map[string]any{"type": "string", "description": "Field sắp xếp — phải là field có index (unique/relation/indexed) hoặc cột hệ thống id/created_at/updated_at."},
						"order":   map[string]any{"type": "string", "enum": []string{"asc", "desc"}, "description": "Hướng sắp xếp (mặc định desc)."},
					},
					"required": []string{"entity"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name: "list_reports",
				Description: "Liệt kê BÁO CÁO ĐÃ LƯU của tổ chức (định nghĩa chỉ số đã được công ty duyệt: doanh thu, pipeline, đếm theo trạng thái…). " +
					"Gọi ĐẦU TIÊN khi câu hỏi nghe giống một chỉ số chuẩn — ưu tiên chạy báo cáo có sẵn thay vì tự dựng phép tính.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name: "run_report",
				Description: "Chạy một báo cáo đã lưu theo ĐÚNG định nghĩa công ty (lấy id từ list_reports). " +
					"Kết quả tôn trọng quyền dữ liệu của người hỏi. Dùng khi câu hỏi khớp một báo cáo có sẵn — số sẽ nhất quán với màn Báo cáo. " +
					"Báo cáo nhóm nhiều cấp trả cây (groups lồng children), bảng chéo trả rows/columns/cells (cells trỏ theo chỉ số r/c); mỗi nhóm có label để gọi tên. having (nếu có) chỉ lọc từng ô / nhóm cấp sâu nhất; total và tổng hàng/cấp cha KHÔNG bị lọc; ô vắng = không có dữ liệu hoặc không đạt ngưỡng. Nếu có truncated:true thì mới đọc được phần đầu — nói rõ với người hỏi.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{"type": "string", "description": "ID báo cáo (từ list_reports)."},
					},
					"required": []string{"id"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name: "aggregate_records",
				Description: "TÍNH TOÁN trên các record người dùng được xem: count/sum/avg/min/max, tuỳ chọn gom nhóm (groupBy) và bucket thời gian. " +
					"Dùng cho câu hỏi 'bao nhiêu / tổng / trung bình / lớn nhất / nhỏ nhất / theo tháng'.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity":  map[string]any{"type": "string", "description": "Tên entity nguồn (vd 'opportunities')."},
						"metric":  map[string]any{"type": "string", "enum": []string{"count", "sum", "avg", "min", "max"}, "description": "Phép tính."},
						"field":   map[string]any{"type": "string", "description": "Field số (number/currency/rating, hoặc formula ra số/tiền) khi metric là sum/avg/min/max — bỏ trống với count."},
						"groupBy": map[string]any{"type": "string", "description": "Tuỳ chọn: field gom nhóm — phải có index (unique/relation/indexed) hoặc là cột hệ thống created_at/updated_at/id."},
						"bucket":  map[string]any{"type": "string", "enum": []string{"day", "week", "month", "quarter", "year"}, "description": "Tuỳ chọn: gom theo thời gian — chỉ khi groupBy là field date/datetime hoặc created_at/updated_at."},
						"filters": assistantFilterArraySchema(),
					},
					"required": []string{"entity", "metric"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name: "propose_record_create",
				Description: "Đề xuất TẠO 1 record mới cho một entity người dùng được xem — chỉ tạo BẢN NHÁP để người dùng bấm Xác nhận trong giao diện, KHÔNG tự ghi. " +
					"Gọi list_fields TRƯỚC để map đúng tên field và giá trị: picklist đúng values, số trong min/max, ngày YYYY-MM-DD, datetime RFC3339, currency {amount, currencyCode}. " +
					"KHÔNG set field computed (formula/lookup/rollup/auto_number — engine tự tính). Field relation cần id record THẬT (tra bằng query_records) — không biết id thì bỏ trống.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity": map[string]any{"type": "string", "description": "Tên entity nhận record mới (vd 'tickets')."},
						"data":   map[string]any{"type": "object", "description": "Payload record: {tên_field: giá trị} đúng field của entity."},
					},
					"required": []string{"entity", "data"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name: "propose_record_update",
				Description: "Đề xuất SỬA 1 record đang có — chỉ tạo BẢN NHÁP để người dùng bấm Xác nhận trong giao diện, KHÔNG tự sửa. " +
					"Tìm recordId bằng query_records trước (lọc theo tên/mã người dùng nhắc tới). data CHỈ chứa những field cần đổi (partial update); " +
					"giá trị null = xoá giá trị của field không bắt buộc. Gọi list_fields nếu chưa chắc tên field/giá trị hợp lệ.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity":   map[string]any{"type": "string", "description": "Tên entity của record (vd 'orders')."},
						"recordId": map[string]any{"type": "string", "description": "Id record cần sửa (lấy từ query_records)."},
						"data":     map[string]any{"type": "object", "description": "Các field cần đổi: {tên_field: giá trị mới}."},
					},
					"required": []string{"entity", "recordId", "data"},
				},
			},
		},
	}
}

// DraftRecordCreate is a proposed NEW record for one entity — the ⌘K assistant's HITL create
// draft. Data is the RAW proposed payload (what the model suggested, not the normalized/defaulted
// store shape): the FE renders it as a confirm card and applies via POST /:entity under the
// CALLER's own token, whose engine re-runs the SAME services.ValidateRecord plus the DB-dependent
// checks (relation existence, unique, edit-ACL) that propose cannot see.
type DraftRecordCreate struct {
	Entity string         `json:"entity"`
	Data   map[string]any `json:"data"`
}

// DraftRecordUpdate is a proposed PARTIAL update of one existing record — the ⌘K assistant's HITL
// update draft. Data holds ONLY the fields to change (PUT partial-merge semantics; an explicit
// null clears an optional field). Before echoes the CURRENT values of exactly those fields (as
// visible to the caller — field-ACL already applied by the read engine) so the FE confirm card
// can render an honest old→new diff. Applied via PUT /:entity/:id under the caller's own token.
type DraftRecordUpdate struct {
	Entity   string         `json:"entity"`
	RecordID string         `json:"recordId"`
	Data     map[string]any `json:"data"`
	Before   map[string]any `json:"before"`
}

// assistantFilterArraySchema is the filters[] fragment for query_records/aggregate_records — the
// SAME field/op/values dialect a live ?field[op]=value query (and the builder's propose_report)
// accepts, so there is exactly one filter vocabulary across the platform.
func assistantFilterArraySchema() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "Tuỳ chọn: điều kiện lọc (nối AND).",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"field": map[string]any{"type": "string", "description": "Field để lọc (phải tồn tại trong entity)."},
				"op":    map[string]any{"type": "string", "enum": []string{"eq", "neq", "gt", "gte", "lt", "lte", "contains", "in", "between", "isnull", "notnull"}, "description": "Toán tử; bỏ trống = eq."},
				"values": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Toán hạng (chuỗi thô): 1 phần tử cho eq/neq/gt/…, nhiều cho in, đúng 2 cho between, rỗng cho isnull/notnull."},
			},
			"required": []string{"field"},
		},
	}
}

// assistantFilter is the model's wire shape for one filter predicate.
type assistantFilter struct {
	Field  string   `json:"field"`
	Op     string   `json:"op"`
	Values []string `json:"values"`
}

func toFilterInputs(filters []assistantFilter) []services.FilterInput {
	out := make([]services.FilterInput, 0, len(filters))
	for _, f := range filters {
		out = append(out, services.FilterInput{
			Field:    f.Field,
			Operator: dynrepo.FilterOperator(f.Op), // "" == eq, validated by the service
			Values:   f.Values,
		})
	}
	return out
}

// assistantExecutor runs one assistant tool call. tenantID is set ONCE from the request's
// verified JWT (never a model argument — no schema carries a tenant), and scope is the caller's
// services.RecordScope built by the handler via scopeOf and threaded VERBATIM (trap #1): the
// executor never mutates it and never rebuilds it from ai.Scope. draft accumulates the turn's
// HITL record proposals (record_create/record_update ONLY — never a metadata kind); NOTHING in
// it is applied here — the user confirms in the palette and the FE writes through the normal
// gated record routes.
type assistantExecutor struct {
	reader   RecordReader
	tenantID string
	scope    services.RecordScope
	draft    []DraftItem
}

// execute dispatches the named tool. Like the builder executor it NEVER returns a Go error: any
// failure is surfaced inside the content as {"ok":false,"error":"<CODE>: ..."} so the model can
// read the machine code and self-correct on the next step.
func (e *assistantExecutor) execute(ctx context.Context, name, argsJSON string) string {
	switch name {
	case "list_entities":
		return e.listEntities(ctx)
	case "list_fields":
		return e.listFields(ctx, argsJSON)
	case "query_records":
		return e.queryRecords(ctx, argsJSON)
	case "aggregate_records":
		return e.aggregateRecords(ctx, argsJSON)
	case "list_reports":
		return e.listReports(ctx)
	case "run_report":
		return e.runReport(ctx, argsJSON)
	case "propose_record_create":
		return e.proposeRecordCreate(ctx, argsJSON)
	case "propose_record_update":
		return e.proposeRecordUpdate(ctx, argsJSON)
	default:
		return toolErr("UNKNOWN_TOOL: tool %q không tồn tại", name)
	}
}

// recordSpecs resolves the validation specs of one entity under the caller's "view" ACL: Fields()
// is the ACL-gated front door (forbidden/nonexistent → services.ErrEntityNotFound, no existence
// leak — reader.scopeFor semantics), then services.FieldSpecsOf runs the SAME conversion the
// record engine's resolve() runs before every ValidateRecord. Returns ("", nil, toolErr-json) on
// failure.
func (e *assistantExecutor) recordSpecs(ctx context.Context, op, entity string) ([]services.FieldSpec, string) {
	fields, err := e.reader.Fields(ctx, e.scope, e.tenantID, entity)
	if err != nil {
		return nil, assistantErr(op, err)
	}
	specs, err := services.FieldSpecsOf(fields)
	if err != nil {
		// Corrupt stored options JSON — an infra fault, not a model mistake; keep the raw error
		// out of the model context (same hygiene as assistantErr's default branch).
		return nil, toolErr("%s_FAILED: lỗi hệ thống khi đọc metadata field, thử lại", op)
	}
	return specs, ""
}

// proposeRecordCreate validates a proposed NEW record with config-time PARITY to the apply path
// (POST /:entity → RecordService.Create, which runs services.ValidateRecord — the SAME single
// body, over specs from services.FieldSpecsOf) and buffers it as DraftItem{Kind:"record_create"}.
// NOTHING is written — the user confirms in the palette and the FE applies via POST /:entity
// under the caller's own token (HITL). ACL: the entity resolves through the reader's "view" gate
// (forbidden → ENTITY_NOT_FOUND, no leak); create/edit-level ACL is enforced by the apply path —
// the same trust boundary every manual create goes through.
//
// NOT mirrored (DB-dependent, surfaces at APPLY exactly like the builder's propose_records):
// relation/polymorphic existence probes, unique collisions, field-ACL write guards on VISIBLE
// locked fields. Hidden fields are absent from the specs (reader.Fields), so a hidden required
// field is not demanded here — POST /:entity refuses it nameless (RECORD_REQUIRED_LOCKED). Data is
// buffered RAW — the apply path recomputes clean/normalized payloads itself.
func (e *assistantExecutor) proposeRecordCreate(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity string         `json:"entity"`
		Data   map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	specs, res := e.recordSpecs(ctx, "PROPOSE_RECORD_CREATE", a.Entity)
	if res != "" {
		return res
	}
	if _, err := services.ValidateRecord(specs, a.Data); err != nil {
		code, msg, _ := validationOf(err)
		return toolErr("%s: %s", code, msg)
	}

	e.draft = append(e.draft, DraftItem{
		Kind:         "record_create",
		RecordCreate: &DraftRecordCreate{Entity: a.Entity, Data: a.Data},
	})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("bản nháp record mới cho %q — chờ người dùng xác nhận", a.Entity)})
}

// proposeRecordUpdate validates a proposed PARTIAL update with config-time PARITY to the apply
// path (PUT /:entity/:id → RecordService.Update, which runs services.ValidatePartialRecord — the
// SAME single body, same partial-merge/explicit-null semantics) and buffers it as
// DraftItem{Kind:"record_update"}. The record's existence is probed through the reader's
// GetRecord (the same engine as GET /:entity/:id): absent / out of the caller's row scope →
// RECORD_NOT_FOUND, indistinguishable by construction (no leak). The probe also feeds the draft's
// Before map (current values of exactly the touched fields) so the FE renders an honest old→new
// confirm card. NOTHING is written — the FE applies via PUT /:entity/:id under the caller's own
// token (HITL; edit-ACL + version checks enforced there).
func (e *assistantExecutor) proposeRecordUpdate(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity   string         `json:"entity"`
		RecordID string         `json:"recordId"`
		Data     map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	if strings.TrimSpace(a.RecordID) == "" {
		return toolErr("RECORD_ID_REQUIRED: thiếu recordId — tìm id record bằng query_records trước")
	}
	if len(a.Data) == 0 {
		return toolErr("RECORD_UPDATE_EMPTY: data không có field nào để đổi — truyền ít nhất một field")
	}

	specs, res := e.recordSpecs(ctx, "PROPOSE_RECORD_UPDATE", a.Entity)
	if res != "" {
		return res
	}
	clean, err := services.ValidatePartialRecord(specs, a.Data)
	if err != nil {
		code, msg, _ := validationOf(err)
		return toolErr("%s: %s", code, msg)
	}

	rec, err := e.reader.GetRecord(ctx, e.scope, e.tenantID, a.Entity, a.RecordID)
	if err != nil {
		if errors.Is(err, services.ErrRecordNotFound) {
			return toolErr("RECORD_NOT_FOUND: record %q không tồn tại trong %q hoặc bạn không có quyền xem — tra lại id bằng query_records", a.RecordID, a.Entity)
		}
		return assistantErr("PROPOSE_RECORD_UPDATE", err)
	}

	// specs/052 (queue #230): the dependent-picklist pair, judged on the stored record exactly as
	// PUT /:entity/:id does - a wrong pair is refused here instead of as a 422 at Apply.
	if derr := services.CheckDependentPicklists(specs, clean, rec.Data); derr != nil {
		code, msg, _ := validationOf(derr)
		return toolErr("%s: %s", code, msg)
	}

	// Before = the CURRENT values of exactly the fields this draft touches (field-ACL already
	// applied by the read engine — a hidden field simply reads back as null).
	before := make(map[string]any, len(a.Data))
	for k := range a.Data {
		before[k] = rec.Data[k]
	}

	e.draft = append(e.draft, DraftItem{
		Kind:         "record_update",
		RecordUpdate: &DraftRecordUpdate{Entity: a.Entity, RecordID: rec.ID, Data: a.Data, Before: before},
	})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("bản nháp sửa record %q trên %q — chờ người dùng xác nhận", rec.ID, a.Entity)})
}

// listReports surfaces the tenant's saved-report catalog (A3): id + name + entity + type so the
// model can match the user's question against a REVIEWED metric definition before hand-rolling
// an aggregate. Inactive reports are surfaced with isActive=false — run_report refuses them with
// REPORT_INACTIVE, so the model can explain instead of silently skipping.
func (e *assistantExecutor) listReports(ctx context.Context) string {
	rows, err := e.reader.ListReports(ctx, e.scope, e.tenantID)
	if err != nil {
		return assistantErr("LIST_REPORTS_FAILED", err)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"id": r.ID, "name": r.Name, "entity": r.EntityName, "type": r.Type, "isActive": r.IsActive,
		})
	}
	return toolOK(map[string]any{"reports": out})
}

// runReport executes one saved report and projects the result compactly. Tabular rows are capped
// at assistantMaxRows (the model reads them back as prompt tokens); grouped/scalar aggregates use
// the SAME shape aggregate_records emits so the model needs one mental model for numbers.
func (e *assistantExecutor) runReport(ctx context.Context, argsJSON string) string {
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	res, err := e.reader.RunReport(ctx, e.scope, e.tenantID, a.ID)
	if err != nil {
		return assistantErr("RUN_REPORT_FAILED", err)
	}
	if res.Tabular != nil {
		rows := res.Tabular.Records
		truncated := false
		if len(rows) > assistantMaxRows {
			rows, truncated = rows[:assistantMaxRows], true
		}
		outRows := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			// Project each record's data map to the report's chosen columns only — the saved
			// definition IS the projection; the full map would just burn prompt tokens.
			proj := make(map[string]any, len(res.Tabular.Columns))
			for _, col := range res.Tabular.Columns {
				if v, ok := r.Data[col]; ok {
					proj[col] = v
				}
			}
			outRows = append(outRows, map[string]any{"id": r.ID, "data": proj})
		}
		out := map[string]any{"columns": res.Tabular.Columns, "rows": outRows, "total": res.Tabular.Total}
		if truncated {
			out["truncated"] = true
		}
		return toolOK(out)
	}
	if res.Aggregate != nil {
		ag := res.Aggregate
		if ag.Grouped {
			groups := make([]map[string]any, 0, len(ag.Groups))
			for _, g := range ag.Groups {
				groups = append(groups, map[string]any{"key": strPtrOrNil(g.Key), "value": strPtrOrNil(g.Value), "count": g.Count})
			}
			out := map[string]any{"groups": groups}
			if ag.CurrencyCode != "" {
				out["currencyCode"] = ag.CurrencyCode
			}
			return toolOK(out)
		}
		out := map[string]any{"value": strPtrOrNil(ag.Scalar.Value), "count": ag.Scalar.Count}
		if ag.CurrencyCode != "" {
			out["currencyCode"] = ag.CurrencyCode
		}
		return toolOK(out)
	}
	if res.Grid != nil {
		return toolOK(gridForModel(res))
	}
	// Defensive: RunReport always fills exactly one branch for a valid type.
	return toolErr("RUN_REPORT_FAILED: báo cáo trả kết quả rỗng, thử hỏi khác")
}

func (e *assistantExecutor) listEntities(ctx context.Context) string {
	ents, err := e.reader.ViewableEntities(ctx, e.scope, e.tenantID)
	if err != nil {
		return assistantErr("LIST_ENTITIES_FAILED", err)
	}
	out := make([]map[string]any, 0, len(ents))
	for _, en := range ents {
		out = append(out, map[string]any{"name": en.Name, "label": en.Label})
	}
	return toolOK(map[string]any{"entities": out})
}

func (e *assistantExecutor) listFields(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity string `json:"entity"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	fields, err := e.reader.Fields(ctx, e.scope, e.tenantID, a.Entity)
	if err != nil {
		return assistantErr("LIST_FIELDS_FAILED", err)
	}
	out := make([]map[string]any, 0, len(fields))
	for _, f := range fields {
		fm := map[string]any{
			"name": f.Name, "label": f.Label, "fieldType": f.FieldType, "required": f.IsRequired,
		}
		// For a picklist/multi_select, surface the ALLOWED values so the model filters/answers with
		// the stored value (e.g. "open"), not the user's Vietnamese phrasing ("đang mở") — otherwise
		// a wrong-but-valid value silently yields 0 rows and a confidently-wrong "you have none"
		// (reviewer MED-3). These option values are the same metadata every user already sees in the
		// entry form, so this is not a disclosure. Fields() already gated the entity by "view" ACL.
		if (f.FieldType == "picklist" || f.FieldType == "multi_select") && f.Options != nil {
			var opt struct {
				Values []string `json:"values"`
			}
			if json.Unmarshal([]byte(*f.Options), &opt) == nil && len(opt.Values) > 0 {
				fm["values"] = opt.Values
			}
		}
		out = append(out, fm)
	}
	return toolOK(map[string]any{"entity": a.Entity, "fields": out})
}

func (e *assistantExecutor) queryRecords(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity  string            `json:"entity"`
		Filters []assistantFilter `json:"filters"`
		Limit   int               `json:"limit"`
		Sort    string            `json:"sort"`
		Order   string            `json:"order"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	limit := a.Limit
	if limit <= 0 {
		limit = assistantDefaultRows
	}
	if limit > assistantMaxRows {
		limit = assistantMaxRows
	}

	res, err := e.reader.QueryRecords(ctx, e.scope, e.tenantID, a.Entity, services.ListQuery{
		Page:    1, // the assistant reads one bounded page; "more" means a narrower filter
		Limit:   limit,
		Sort:    a.Sort,
		Order:   a.Order,
		Filters: toFilterInputs(a.Filters),
	})
	if err != nil {
		return assistantErr("QUERY_RECORDS_FAILED", err)
	}

	// Compact projection: id + data only (field-ACL already stripped hidden values downstream).
	// The full dynrepo.Record envelope (version/timestamps) would just burn prompt tokens.
	rows := make([]map[string]any, 0, len(res.Records))
	for _, r := range res.Records {
		rows = append(rows, map[string]any{"id": r.ID, "data": r.Data})
	}
	return toolOK(map[string]any{"records": rows, "total": res.Total})
}

func (e *assistantExecutor) aggregateRecords(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity  string            `json:"entity"`
		Metric  string            `json:"metric"`
		Field   string            `json:"field"`
		GroupBy string            `json:"groupBy"`
		Bucket  string            `json:"bucket"`
		Filters []assistantFilter `json:"filters"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	res, err := e.reader.AggregateRecords(ctx, e.scope, e.tenantID, a.Entity, services.AggregateQuery{
		Metric:  services.AggregateMetric(a.Metric),
		Field:   a.Field,
		GroupBy: a.GroupBy,
		Bucket:  a.Bucket,
		Filters: toFilterInputs(a.Filters),
	})
	if err != nil {
		return assistantErr("AGGREGATE_RECORDS_FAILED", err)
	}

	if res.Grouped {
		groups := make([]map[string]any, 0, len(res.Groups))
		for _, g := range res.Groups {
			groups = append(groups, map[string]any{"key": strPtrOrNil(g.Key), "value": strPtrOrNil(g.Value), "count": g.Count})
		}
		out := map[string]any{"groups": groups}
		if res.CurrencyCode != "" {
			out["currencyCode"] = res.CurrencyCode
		}
		return toolOK(out)
	}
	out := map[string]any{"value": strPtrOrNil(res.Scalar.Value), "count": res.Scalar.Count}
	if res.CurrencyCode != "" {
		out["currencyCode"] = res.CurrencyCode
	}
	return toolOK(out)
}

// assistantErr maps a RecordReader error to the tool-error envelope, surfacing the REAL machine
// code so the model can self-correct: services.ErrEntityNotFound (which the adapter also returns
// for an entity the caller may not view — no existence leak) → ENTITY_NOT_FOUND; a
// *services.ValidationError (LIST_UNKNOWN_FILTER, AGGREGATE_FIELD_NOT_NUMERIC, SORT_NOT_ALLOWED,
// FIELD_FORBIDDEN, …) → its own code; anything else → the given operation code.
func assistantErr(op string, err error) string {
	if errors.Is(err, services.ErrEntityNotFound) {
		return toolErr("ENTITY_NOT_FOUND: entity không tồn tại hoặc bạn không có quyền xem — gọi list_entities để biết các entity được phép")
	}
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		return toolErr("%s: %s", ve.Code, ve.Msg)
	}
	// Non-sentinel infra fault (wrapped SQL/driver error, etc.): surface ONLY the operation code,
	// never the raw error — its text can carry obj_* table / generated-column / driver strings that
	// would flow into the model context (and possibly the user's answer). The real error is already
	// logged server-side by RecordService/dynrepo. Mirrors DynamicRecordHandler.respondError hygiene.
	return toolErr("%s_FAILED: lỗi hệ thống khi truy vấn dữ liệu, thử hỏi khác", op)
}

// strPtrOrNil unwraps a *string for JSON marshaling: nil stays JSON null, non-nil becomes the
// string value.
func strPtrOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Budgets for a tree / cross-tab read back by run_report (specs/049 S7b): the model reads the result
// as prompt tokens, so a big grid is cut and flagged `truncated` (for the model only — the report
// screen never truncates).
const (
	assistantMaxTreeGroups = 50
	assistantMaxGridRows   = 30
	assistantMaxGridCols   = 20
)

// gridForModel projects a tree / cross-tab run into the compact {key,label?,value,count} shape the
// other run_report branches use. The numbers were already scoped to the caller by RunReport.
func gridForModel(res services.ReportRunResult) map[string]any {
	g := res.Grid
	truncated := false
	group := func(a services.AggregateGroupResult) map[string]any {
		m := map[string]any{"key": strPtrOrNil(a.Key), "value": strPtrOrNil(a.Value), "count": a.Count}
		if a.Label != nil {
			m["label"] = *a.Label
		}
		return m
	}
	groups := func(gs []services.AggregateGroupResult, limit int) []map[string]any {
		if len(gs) > limit {
			gs, truncated = gs[:limit], true
		}
		out := make([]map[string]any, 0, len(gs))
		for _, x := range gs {
			out = append(out, group(x))
		}
		return out
	}
	axes := make([]map[string]any, 0, len(res.Axes))
	for _, ax := range res.Axes {
		m := map[string]any{"field": ax.Field}
		if ax.Bucket != "" {
			m["bucket"] = ax.Bucket
		}
		axes = append(axes, m)
	}
	out := map[string]any{"type": string(res.Type), "total": map[string]any{"value": strPtrOrNil(g.Total.Value), "count": g.Total.Count}}
	if g.CurrencyCode != "" {
		out["currencyCode"] = g.CurrencyCode
	}
	if res.Having != nil {
		out["having"] = res.Having
	}
	if g.Kind == services.GridKindTree {
		var walk func(ns []services.GridNode) []map[string]any
		walk = func(ns []services.GridNode) []map[string]any {
			if len(ns) > assistantMaxTreeGroups {
				ns, truncated = ns[:assistantMaxTreeGroups], true
			}
			out := make([]map[string]any, 0, len(ns))
			for _, n := range ns {
				m := group(n.AggregateGroupResult)
				if len(n.Children) > 0 {
					m["children"] = walk(n.Children)
				}
				out = append(out, m)
			}
			return out
		}
		out["levels"] = axes
		out["groups"] = walk(g.Groups)
	} else {
		out["rowAxis"], out["columnAxis"] = nil, nil
		if len(axes) > 0 {
			out["rowAxis"] = axes[0]
		}
		if len(axes) > 1 {
			out["columnAxis"] = axes[1]
		}
		rows, cols := groups(g.Rows, assistantMaxGridRows), groups(g.Columns, assistantMaxGridCols)
		cells := make([]map[string]any, 0, len(g.Cells))
		for _, c := range g.Cells {
			// Cells address rows/columns by index; the cut keeps a prefix, so indexes stay valid.
			if c.R >= len(rows) || c.C >= len(cols) {
				continue
			}
			cells = append(cells, map[string]any{"r": c.R, "c": c.C, "value": strPtrOrNil(c.Value), "count": c.Count})
		}
		out["rows"], out["columns"], out["cells"] = rows, cols, cells
	}
	if truncated {
		out["truncated"] = true
	}
	return out
}

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/services/vertical_pack"
	"nexus/platform/internal/workflow"
	"nexus/platform/internal/workqueue"
)

// MetadataReader is the tenant-scoped, READ-ONLY view of the metadata registry the AI tools need.
// It is declared consumer-side (satisfied by *metadata.Repository in production) so the ai package
// never imports a concrete repo type and unit tests can inject a two-tenant fake. Every method
// takes tenantID as its FIRST scoping argument — the tools always pass the tenant from the request
// context, NEVER a value the model produced (spec §7 tenant isolation).
type MetadataReader interface {
	ListEntities(tenantID string) ([]metadata.EntityDefinition, error)
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
	// ListAllRelationships returns every ACTIVE relationship definition (any type) for the
	// tenant (M5 grounding for list_relationships / propose_relationship). The method name and
	// signature deliberately match *metadata.Repository.ListAllRelationships exactly so the
	// production repo satisfies this interface with no adapter.
	ListAllRelationships(ctx context.Context, tenantID string) ([]metadata.RelationshipDefinition, error)
	// ListRoleNames returns the tenant's role names. propose_workqueue and propose_approval both
	// take a role NAME that is resolved to an id at Apply, and the model had no way to see which
	// names exist — so it invented plausible ones ("sales_rep") and Apply hard-failed on a role
	// that was never there. A read tool turns that guess into a choice.
	ListRoleNames(ctx context.Context, tenantID string) ([]string, error)
	// ListRollupsAggregating backs the #234/#284 formula guards in propose_edit_field. It is part
	// of the interface (not an optional assertion) so the wrapper readers cannot hide it.
	ListRollupsAggregating(ctx context.Context, tenantID, childEntity, aggField string) ([]string, error)
}

// DraftItem is one proposed metadata change accumulated during a Chat turn. Exactly one of the
// payload pointers is set, per Kind. NOTHING here is applied — the draft is returned to the FE,
// and a human applies it through the existing gated /admin/entities + /admin/fields +
// /admin/relationships + /admin/layouts + /admin/workflows + /admin/reports + /admin/apps APIs,
// plus PUT/DELETE /admin/fields/:id, DELETE /admin/entities/:name and POST /:entity/bulk for the
// EASE Nước 7 edit/delete/records kinds (HITL, spec §7).
type DraftItem struct {
	Kind         string             `json:"kind"`                   // "entity" | "field" | "relationship" | "layout" | "workflow" | "report" | "app" | "edit_field" | "delete_field" | "delete_entity" | "records" | "approval" | "dashboard" | "workqueue" | "record_create" | "record_update"
	Entity       *DraftEntity       `json:"entity,omitempty"`       // set when Kind=="entity"
	Field        *DraftField        `json:"field,omitempty"`        // set when Kind=="field"
	Relationship *DraftRelationship `json:"relationship,omitempty"` // set when Kind=="relationship"
	Layout       *DraftLayout       `json:"layout,omitempty"`       // set when Kind=="layout"
	Workflow     *DraftWorkflow     `json:"workflow,omitempty"`     // set when Kind=="workflow"
	Report       *DraftReport       `json:"report,omitempty"`       // set when Kind=="report"
	App          *DraftApp          `json:"app,omitempty"`          // set when Kind=="app"
	EditField    *DraftEditField    `json:"editField,omitempty"`    // set when Kind=="edit_field"
	DeleteField  *DraftDeleteField  `json:"deleteField,omitempty"`  // set when Kind=="delete_field"
	DeleteEntity *DraftDeleteEntity `json:"deleteEntity,omitempty"` // set when Kind=="delete_entity"
	Records      *DraftRecords      `json:"records,omitempty"`      // set when Kind=="records"
	// AI-MODULES (Nước 7 POWER): the three "modules pointing at the rest of the product" —
	// approval process, dashboard, work queue. Each maps 1:1 onto its gated admin POST body
	// (see the Draft* type docs for the exact FE apply contract).
	Approval  *DraftApproval  `json:"approval,omitempty"`  // set when Kind=="approval"
	Dashboard *DraftDashboard `json:"dashboard,omitempty"` // set when Kind=="dashboard"
	WorkQueue *DraftWorkQueue `json:"workqueue,omitempty"` // set when Kind=="workqueue" (one-word key, matching the kind token — the FE's primary lookup)
	// RecordCreate/RecordUpdate are the ⌘K assistant's HITL single-record drafts (assistant_tools.go
	// — the only two draft kinds a NON-admin surface can emit). Applied by the FE via the normal
	// POST /:entity / PUT /:entity/:id under the CALLER's own token — never an admin route.
	RecordCreate *DraftRecordCreate `json:"recordCreate,omitempty"` // set when Kind=="record_create"
	RecordUpdate *DraftRecordUpdate `json:"recordUpdate,omitempty"` // set when Kind=="record_update"
	// Change is the Create-or-Update preview (Before/After + changed places) of every builder kind
	// except "app" (specs/067 L10-4a). Absent on sessions saved before it existed.
	Change *DraftChange `json:"change,omitempty"`
}

// DraftEntity is a proposed new entity plus its initial fields (maps to POST /admin/entities).
type DraftEntity struct {
	Name   string       `json:"name"`
	Label  string       `json:"label"`
	Fields []DraftField `json:"fields"`
	// AppName is stamped by finalizeDraft (never by the model) when the turn ran inside an app:
	// the FE then creates the table with that app's id so the default grants are seeded.
	AppName string `json:"appName,omitempty"`
}

// DraftField is a proposed field. Entity is set only for a standalone field proposal (maps to
// POST /admin/fields); it is empty for a field nested inside a DraftEntity.
type DraftField struct {
	Entity    string         `json:"entity,omitempty"`
	Name      string         `json:"name"`
	Label     string         `json:"label"`
	FieldType string         `json:"fieldType"`
	Options   map[string]any `json:"options,omitempty"`
	Required  bool           `json:"required"`
}

// ToolDefinitions returns the 21 function tools advertised to the model on every completion (spec
// §4; M5 adds list_relationships + propose_relationship, M6 adds propose_layout (+ get_layout/list_workflows/list_reports/get_app, specs/067),
// M7 adds propose_workflow, M8 adds propose_report, M10 adds snapshot_model, Apps Phase 3 adds
// propose_app, EASE Nước 7 adds propose_edit_field + propose_delete_field + propose_delete_entity
// + propose_records, AI-MODULES adds propose_approval + propose_dashboard + propose_workqueue +
// explain_access + list_packs).
// CRITICAL invariant: NO schema declares a tenant parameter. The tenant is injected by the
// executor from the request's verified JWT, so it is structurally impossible for the model to
// address another tenant's data — isolation by construction, not a runtime check.
func ToolDefinitions() []Tool {
	return append(toolDefinitionsBase(), docToolDefinitions()...)
}

func toolDefinitionsBase() []Tool {
	return []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "list_entities",
				Description: "Liệt kê các entity (name + label) đang có của tenant hiện tại. Dùng để nắm hiện trạng trước khi đề xuất, tránh trùng.",
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
				Description: "Liệt kê field của một entity đã có (name, label, fieldType, required; field relation kèm options relatedEntity/cardinality/inverseField, field polymorphic_relation kèm options.targetEntities). Dùng để tránh trùng field và gợi ý bổ sung.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity": map[string]any{"type": "string", "description": "Tên entity cần xem field (vd 'patients')."},
					},
					"required": []string{"entity"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "list_relationships",
				Description: "Liệt kê các quan hệ (relationship) đang có của tenant hiện tại: name, parentEntityName, childEntityName, relationshipType (1-N/1-1/N-N), inverseField. Dùng để nắm liên kết sẵn có TRƯỚC khi đề xuất quan hệ mới, tránh trùng.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity": map[string]any{"type": "string", "description": "Tùy chọn: chỉ trả các quan hệ dính tới entity này (ở phía parent hoặc child)."},
					},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_entity",
				Description: "Đề xuất một entity MỚI kèm các field khởi tạo. Chỉ đưa vào draft — KHÔNG tạo. Tên phải khớp ^[a-z][a-z0-9_]{0,25}$.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":   map[string]any{"type": "string", "description": "Tên entity, snake_case số nhiều (vd 'patients')."},
						"label":  map[string]any{"type": "string", "description": "Nhãn hiển thị tiếng Việt (vd 'Bệnh nhân')."},
						"fields": fieldArraySchema(),
					},
					"required": []string{"name", "label", "fields"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_field",
				Description: "Đề xuất THÊM một field vào entity đã có. Chỉ đưa vào draft — KHÔNG tạo.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity":    map[string]any{"type": "string", "description": "Tên entity đích (đã tồn tại)."},
						"name":      map[string]any{"type": "string", "description": "Tên field, snake_case."},
						"label":     map[string]any{"type": "string", "description": "Nhãn hiển thị tiếng Việt."},
						"fieldType": fieldTypeSchema(),
						"options":   fieldOptionsSchema(),
						"required":  map[string]any{"type": "boolean", "description": "Field bắt buộc?"},
					},
					"required": []string{"entity", "name", "label", "fieldType"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_relationship",
				Description: "Đề xuất một quan hệ N-N MỚI giữa hai entity — mỗi phía là entity ĐÃ TỒN TẠI hoặc entity vừa propose_entity TRONG CÙNG lượt này. Chỉ đưa vào draft — KHÔNG tạo (người duyệt Apply entity trước, quan hệ sau). CHỈ dành cho N-N; quan hệ 1-N/1-1 dùng propose_field (fieldType 'relation' trên entity con).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":    map[string]any{"type": "string", "description": "Tên quan hệ, quy ước '<a>_<b>' (vd 'students_courses')."},
						"entityA": map[string]any{"type": "string", "description": "Entity một phía (đã tồn tại)."},
						"entityB": map[string]any{"type": "string", "description": "Entity phía kia (đã tồn tại, KHÁC entityA)."},
					},
					"required": []string{"name", "entityA", "entityB"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_layout",
				Description: "Đề xuất BỐ CỤC (layout) cho MỘT view của entity ĐÃ TỒN TẠI. Chỉ đưa vào draft — KHÔNG lưu. Shape edit/detail: {version:1, sections:[{title?, columns:1-4, collapsed?, fields:[cell]}]} với cell = \"tên_field\" | {field, labelOverride?, required?, readonly?, fullWidth?, colspan?, helpText?, visibleWhen?} | {spacer:true}. visibleWhen = {field, op, value?}: CHỈ hiện ô khi trường điều khiển `field` (một ô KHÁC trong cùng bố cục, kiểu picklist/boolean/text/email/number/rating) thoả op ∈ eq|neq (value = một giá trị đúng kiểu, picklist phải thuộc danh sách) | in|notIn (value = mảng giá trị) | empty|notEmpty (không value) — vd {field:\"customer_type\", op:\"eq\", value:\"business\"}; KHÔNG đặt lên field bắt buộc, không vòng. Shape list: {version:1, columns:[{field, labelOverride?, width?}], defaultSort:{field: created_at|updated_at|id HOẶC field có index (unique/relation/user/indexed), order: asc|desc} (relation/user sắp theo mã nội bộ, KHÔNG theo nhãn hiển thị — ưu tiên field vô hướng có index), kanbanField?, calendarField?}. kanbanField = tên MỘT field kiểu picklist mà bảng Kanban chia cột theo; calendarField = tên MỘT field kiểu date/datetime mà màn Lịch trải theo. Bỏ trống cả hai nếu không chắc — app sẽ tự đoán; khai sai tên hoặc sai kiểu bị từ chối. Edit view PHẢI chứa ĐỦ mọi field bắt buộc; mỗi field xuất hiện tối đa một lần.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity":     map[string]any{"type": "string", "description": "Tên entity đích (đã tồn tại)."},
						"viewType":   map[string]any{"type": "string", "enum": []string{"edit", "detail", "list"}, "description": "View cần bố cục: edit (form nhập), detail (trang xem chi tiết), list (bảng danh sách)."},
						"layoutJson": map[string]any{"type": "object", "description": "Object bố cục theo đúng shape mô tả ở description của tool (edit/detail: sections; list: columns + defaultSort)."},
					},
					"required": []string{"entity", "viewType", "layoutJson"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_workflow",
				Description: "Đề xuất một RULE TỰ-ĐỘNG-HOÁ (workflow) cho entity ĐÃ TỒN TẠI: khi <trigger> NẾU <condition> THÌ chạy <actions>. Chỉ đưa vào draft — KHÔNG tạo. Action cho phép: field_update {field, value} (đổi giá trị field trên chính record — KHÔNG nhắm field formula/lookup/rollup/auto_number), in_app_notification {userId, title?, message} (userId nhận sentinel \"$owner\" = người phụ trách record), email_alert {to, subject, body} (cần SMTP server-side — nói rõ với người duyệt). TUYỆT ĐỐI KHÔNG đề xuất outbound_webhook hoặc create_record (tạo bản ghi mới — phạm vi ảnh hưởng lớn, chỉ cấu hình thủ công).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":      map[string]any{"type": "string", "description": "Tên rule dễ hiểu, tiếng Việt được (vd 'Đóng ticket quá hạn')."},
						"entity":    map[string]any{"type": "string", "description": "Tên entity đích (đã tồn tại)."},
						"trigger":   map[string]any{"type": "string", "enum": []string{"after_create", "after_update", "after_delete"}, "description": "Sự kiện kích hoạt: sau khi tạo / sửa / xoá record."},
						"condition": workflowConditionSchema("Điều kiện lọc (tuỳ chọn — bỏ trống = áp cho mọi record). Các rule nối theo match (and/or). Muốn rule chạy MỘT lần khi field chuyển sang giá trị (vd 'khi đơn vừa chuyển sang Đã duyệt') thì dùng trigger after_update + changedTo, không dùng eq. changed/changedTo CHỈ dùng khi trigger = after_update (lúc tạo hay xoá chưa có giá trị cũ để so — bị từ chối WORKFLOW_CONDITION_NEVER_TRUE)."),
						"actions":   workflowActionArraySchema(),
					},
					"required": []string{"name", "entity", "trigger", "actions"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_report",
				Description: "Đề xuất một BÁO CÁO đã lưu (saved report) trên entity ĐÃ TỒN TẠI. Chỉ đưa vào draft — KHÔNG tạo. reportType 'summary'/'matrix' = báo cáo tổng hợp: BẮT BUỘC groupBy (field có index: unique/relation/indexed, hoặc cột hệ thống created_at/updated_at/id) + metric; metric sum/avg/min/max cần thêm field SỐ (number/currency/rating, hoặc formula có resultType number/currency — KHÔNG formula text, rollup, lookup), count thì không. bucket (gom theo thời gian) CHỈ với summary/matrix VÀ khi groupBy là field date/datetime hoặc created_at/updated_at. reportType 'tabular' = danh sách phẳng: BẮT BUỘC columns, KHÔNG groupBy/metric/bucket. Nhiều trục: matrix + columnGroup = bảng chéo (groupBy = trục hàng, columnGroup = trục cột); summary + subGroups = nhóm nhiều cấp có tổng phụ (tối đa 2 cấp phụ). Mọi trục phải có index như groupBy.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":       map[string]any{"type": "string", "description": "Tên báo cáo dễ hiểu, tiếng Việt được (vd 'Doanh thu theo tháng')."},
						"entity":     map[string]any{"type": "string", "description": "Tên entity nguồn (đã tồn tại)."},
						"reportType": map[string]any{"type": "string", "enum": []string{"summary", "matrix", "tabular"}, "description": "summary: gom nhóm + một phép tính (mặc định nên dùng); matrix: bảng chéo hai trục khi có columnGroup (không có columnGroup thì chạy như summary một trục); tabular: danh sách phẳng có lọc."},
						"columns":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "tabular: danh sách tên field làm cột (ít nhất 1)."},
						"groupBy":    map[string]any{"type": "string", "description": "summary/matrix: field gom nhóm — phải có index (unique/relation/indexed) hoặc là cột hệ thống created_at/updated_at/id."},
						"metric":     map[string]any{"type": "string", "enum": []string{"count", "sum", "avg", "min", "max"}, "description": "summary/matrix: phép tính trên mỗi nhóm."},
						"field":      map[string]any{"type": "string", "description": "summary/matrix: field số để tính khi metric là sum/avg/min/max (bỏ trống với count)."},
						"bucket":     map[string]any{"type": "string", "enum": []string{"day", "week", "month", "quarter", "year"}, "description": "summary/matrix + groupBy là field ngày: gom nhóm theo khoảng thời gian."},
						"subGroups": map[string]any{"type": "array", "maxItems": 2, "description": "CHỈ summary: các cấp nhóm phụ sau groupBy (tối đa 2), mỗi phần tử {field, bucket?}; ra cây có tổng phụ. Trục không được trùng groupBy. Có subGroups + having thì having lọc nhóm cấp lá, không lọc tổng cấp cha.",
							"items": reportGroupAxisSchema()},
						"columnGroup": map[string]any{"type": "object", "description": "CHỈ matrix: trục cột {field, bucket?} — groupBy là trục hàng. Không có columnGroup thì matrix chỉ một trục.",
							"properties": reportGroupAxisSchema()["properties"], "required": []string{"field"}},
						"having": map[string]any{"type": "object", "description": "Ngưỡng lọc kết quả summary/matrix {on, op, values}: on = 'value' (số đo) | 'count' (số bản ghi); op = gt|gte|lt|lte|eq|neq|between; values = 1 số (between: 2 số). Ngưỡng lọc từng ô / nhóm cấp sâu nhất, không lọc tổng hàng hay cấp cha — muốn 'chỉ giữ giai đoạn có tổng > X' thì dùng summary MỘT cấp groupBy giai đoạn, KHÔNG dùng matrix + having và KHÔNG dùng summary có subGroups + having (cũng lọc ở cấp lá). Nếu yêu cầu không làm được, nói rõ giới hạn thay vì đề xuất sai.",
							"properties": map[string]any{
								"on":     map[string]any{"type": "string", "enum": []string{"value", "count"}},
								"op":     map[string]any{"type": "string", "enum": []string{"gt", "gte", "lt", "lte", "eq", "neq", "between"}},
								"values": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Số thập phân dạng chuỗi, vd \"10000\"."},
							},
							"required": []string{"on", "op", "values"}},
						"filters": reportFilterArraySchema(),
					},
					"required": []string{"name", "entity", "reportType"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_app",
				Description: "Đề xuất một APP — container điều hướng trên sidebar trái, gói các entity thành ứng dụng có tên (vd 'Quản lý Spa'). App là 'lens' tham chiếu entity theo thứ tự, KHÔNG sở hữu dữ liệu. Chỉ đưa vào draft — KHÔNG tạo. navEntities chỉ nhận tên entity ĐÃ TỒN TẠI hoặc entity đã propose_entity TRONG CÙNG lượt này. Nói rõ với người duyệt: Apply entity TRƯỚC, app SAU.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]any{"type": "string", "description": "Slug app [a-z][a-z0-9_]* tối đa 60 ký tự (vd 'spa_management')."},
						"label":       map[string]any{"type": "string", "description": "Tên hiển thị tiếng Việt có dấu (vd 'Quản lý Spa')."},
						"icon":        map[string]any{"type": "string", "description": "Tuỳ chọn: tên lucide icon (vd 'sparkles')."},
						"color":       map[string]any{"type": "string", "description": "Tuỳ chọn: brand tone (token màu, ≤20 ký tự)."},
						"description": map[string]any{"type": "string", "description": "Tuỳ chọn: mô tả ngắn app làm gì."},
						"navEntities": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
							"description": "Tên các entity hiện trong nav của app, theo thứ tự hiển thị (ít nhất 1, tối đa 40; entity đã có hoặc vừa đề xuất trong CÙNG lượt)."},
					},
					"required": []string{"name", "label", "navEntities"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_edit_field",
				Description: "Đề xuất SỬA một field ĐÃ TỒN TẠI (đổi label, mô tả, bắt buộc, giá trị mặc định, options/luật validation). Chỉ đưa vào draft — KHÔNG sửa; người duyệt Apply qua PUT /admin/fields. KHÔNG đổi được: fieldType, tên field (cần kiểu/tên khác → tạo field mới + đề xuất xoá field cũ); field relation không đổi được relatedEntity/cardinality/inverseField. Soi list_fields trước để biết giá trị hiện tại.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity": map[string]any{"type": "string", "description": "Tên entity chứa field (đã tồn tại)."},
						"field":  map[string]any{"type": "string", "description": "Tên field cần sửa (đã tồn tại trên entity)."},
						"changes": map[string]any{
							"type":        "object",
							"description": "CHỈ các thuộc tính muốn đổi (bỏ key = giữ nguyên): label, description, isRequired, defaultValue, options. options gửi TRỌN BỘ object mới theo shape của fieldType (như propose_field) — với picklist/multi_select là {values:[...]} đầy đủ, không phải phần thêm. QUAN TRỌNG: nếu field đang có valueLabels (nhãn hiển thị tiếng Việt theo mã) thì PHẢI chép nguyên valueLabels vào options mới — bỏ nó là xoá sạch nhãn của khách.",
							"properties": map[string]any{
								"label":        map[string]any{"type": "string", "description": "Nhãn hiển thị mới (tiếng Việt có dấu)."},
								"description":  map[string]any{"type": "string", "description": "Mô tả field mới (≤500 ký tự)."},
								"isRequired":   map[string]any{"type": "boolean", "description": "Field bắt buộc?"},
								"defaultValue": map[string]any{"type": "string", "description": "Giá trị mặc định mới (chuỗi; \"\" để xoá default). Phải hợp kiểu field."},
								"options":      fieldOptionsSchema(),
							},
						},
					},
					"required": []string{"entity", "field", "changes"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_delete_field",
				Description: "Đề xuất XOÁ một field ĐÃ TỒN TẠI khỏi entity. Chỉ đưa vào draft — KHÔNG xoá; người duyệt Apply qua DELETE /admin/fields (xoá MỀM: định nghĩa field bị gỡ, dữ liệu cũ trong cột data được giữ). Luôn nói rõ hệ quả trong tóm tắt: form/layout/workflow/report đang dùng field này sẽ mất field đó.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity": map[string]any{"type": "string", "description": "Tên entity chứa field (đã tồn tại)."},
						"field":  map[string]any{"type": "string", "description": "Tên field cần xoá (đã tồn tại trên entity)."},
					},
					"required": []string{"entity", "field"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_delete_entity",
				Description: "Đề xuất XOÁ một entity ĐÃ TỒN TẠI. Chỉ đưa vào draft — KHÔNG xoá; người duyệt Apply qua DELETE /admin/entities (xoá MỀM: entity biến mất khỏi hệ thống VÀ TOÀN BỘ record của tenant trong entity đó bị ẩn đi). Đây là thao tác nặng — chỉ đề xuất khi người dùng nói rõ muốn bỏ entity; luôn cảnh báo hệ quả (record bị ẩn, app/relationship liên quan mất tham chiếu) trong tóm tắt.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity": map[string]any{"type": "string", "description": "Tên entity cần xoá (đã tồn tại)."},
					},
					"required": []string{"entity"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_records",
				Description: "Đề xuất DỮ LIỆU MẪU: tối đa 20 record cho MỘT entity (đã tồn tại, hoặc vừa propose_entity TRONG CÙNG lượt). Chỉ đưa vào draft — KHÔNG ghi; người duyệt Apply qua POST /:entity/bulk (Apply entity trước nếu entity còn là draft). Mỗi row là object {field: giá trị} đúng field của entity: picklist đúng values, số trong min/max, ngày YYYY-MM-DD, datetime RFC3339, currency {amount, currencyCode}. KHÔNG set field computed/server-assigned (formula/rollup/lookup/auto_number — engine tự tính). Field `user` (người phụ trách): ĐỂ TRỐNG — không có id người dùng thật để trỏ, bịa id là cả lô bị 422 lúc Apply (field đó đặt required thì bỏ qua dữ liệu mẫu cho entity đó). Field relation và `polymorphic_relation` (liên quan tới) cần id record THẬT — CHƯA có id thật (record được trỏ tới cũng đang được đề xuất trong lượt này) THÌ DÙNG \"@ref\": row cha thêm khoá \"$ref\":\"@tên_ngắn\" (vd \"@du_an_1\"), row con đặt field relation = ĐÚNG chuỗi đó (vd \"du_an_id\":\"@du_an_1\"); với polymorphic_relation đặt id = chuỗi đó và type = ĐÚNG entity của row đã khai \"$ref\" (vd \"lien_quan\":{\"type\":\"crm_accounts\",\"id\":\"@kh_1\"}) — hệ thống tự đổi ra id thật lúc Apply. BẮT BUỘC gọi propose_records cho entity CHA (khai \"$ref\") TRƯỚC, entity CON (dùng \"@ref\" đó) SAU, trong CÙNG lượt chat — thứ tự Apply đi theo đúng thứ tự đề xuất. KHÔNG tự bịa id giả; KHÔNG tham chiếu \"$ref\" của một row KHÁC trong CÙNG một lần gọi propose_records này (chỉ tham chiếu ref đã khai ở một propose_records TRƯỚC) — sai thứ tự hoặc vòng lặp phụ thuộc sẽ bị từ chối rõ ràng kèm mã lỗi.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"entity": map[string]any{"type": "string", "description": "Tên entity nhận dữ liệu mẫu."},
						"rows": map[string]any{
							"type":        "array",
							"description": "Danh sách record mẫu (1..20), mỗi phần tử là object {tên_field: giá trị}. Dữ liệu tiếng Việt thực tế, đa dạng; KHÔNG đặt id hay cột hệ thống (engine tự sinh). Row muốn được row khác (entity khác, propose_records SAU) tham chiếu thì thêm khoá \"$ref\":\"@tên_ngắn\".",
							"items":       map[string]any{"type": "object"},
						},
					},
					"required": []string{"entity", "rows"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "snapshot_model",
				Description: "Trả về TOÀN BỘ model hiện tại của tenant trong MỘT lần gọi: mọi entity kèm tóm tắt field (name, label, fieldType). Dùng ở chế độ Discovery / đánh giá độ phủ (coverage) — thay cho việc gọi list_fields từng entity. Chỉ đọc, không ghi. Kết quả có trần kích thước: tenant quá lớn sẽ bị cắt (entitiesTruncated/fieldsTruncated kèm số tổng) — khi đó soi tiếp phần thiếu bằng list_fields.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_approval",
				Description: "Đề xuất một QUY TRÌNH PHÊ DUYỆT nhiều bước cho entity ĐÃ TỒN TẠI hoặc vừa propose_entity TRONG CÙNG lượt. Chỉ đưa vào draft — KHÔNG tạo; người duyệt Apply qua POST /admin/approval-processes. Khi <trigger> NẾU <criteria> THÌ record chờ duyệt qua từng bước theo thứ tự. Mỗi bước: approverType 'role' (KHUYÊN DÙNG — approverRef là TÊN role, vd 'manager', hệ thống đổi ra id lúc Apply) hoặc 'user' (approverRef là email người duyệt); slaSeconds là hạn xử lý (giây, tuỳ chọn) — mặc định tính theo GIỜ LÀM VIỆC trên lịch làm việc của tổ chức; slaClock 'wall' nếu hạn phải tính theo giờ đồng hồ (tuỳ chọn); id là mã bước cố định (tuỳ chọn). DUYỆT SONG SONG: thay approverType/approverRef bằng approvers (>= 2 người, mỗi người {type, ref}) + completion ('all' = tất cả phải duyệt, một người từ chối là từ chối; 'any' = người đầu tiên quyết) — KHÔNG đưa cả hai kiểu trên cùng một bước. Bước CHỈ nhận đúng các khoá liệt kê — rẽ nhánh, giao nhóm, quorum %… chưa hỗ trợ: gửi khoá khác sẽ bị từ chối (APPROVAL_NODE_KIND_UNSUPPORTED).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":     map[string]any{"type": "string", "description": "Tên quy trình dễ hiểu, tiếng Việt được (vd 'Duyệt giảm giá trên 20%')."},
						"entity":   map[string]any{"type": "string", "description": "Tên entity đích (đã tồn tại hoặc vừa đề xuất trong cùng lượt)."},
						"trigger":  map[string]any{"type": "string", "enum": []string{"after_create", "after_update"}, "description": "Sự kiện kích hoạt: sau khi tạo / sửa record."},
						"criteria": workflowConditionSchema("Điều kiện để record cần phê duyệt (tuỳ chọn — bỏ trống = MỌI record đều cần duyệt). Các rule nối theo match (and/or). changed/changedTo CHỈ dùng khi trigger = after_update (lúc tạo chưa có giá trị cũ để so — bị từ chối APPROVAL_CRITERIA_EDGE_OP_UNSUPPORTED)."),
						"steps": map[string]any{
							"type":        "array",
							"description": "Các bước phê duyệt theo thứ tự (ít nhất 1).",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"id":           map[string]any{"type": "string", "description": "Mã bước cố định (tuỳ chọn): chữ thường không dấu, số, '_' hoặc '-', tối đa 60 ký tự, không trùng giữa các bước (vd 'truong-phong'). Bỏ trống thì hệ thống tự gán s1, s2… theo vị trí."},
									"name":         map[string]any{"type": "string", "description": "Tên bước (tuỳ chọn, vd 'Trưởng phòng duyệt')."},
									"approverType": map[string]any{"type": "string", "enum": []string{"user", "role"}, "description": "Loại người duyệt — ƯU TIÊN 'role' (bền khi đổi nhân sự)."},
									"approverRef":  map[string]any{"type": "string", "description": "role: TÊN role đang có trong hệ thống (vd 'manager'); user: email người duyệt. Hệ thống đổi ra id lúc Apply."},
									"slaSeconds":   map[string]any{"type": "integer", "description": "Hạn xử lý bước này tính bằng giây (tuỳ chọn, >= 0; vd 14400 = 4 giờ làm việc)."},
									"slaClock":     map[string]any{"type": "string", "enum": []string{"business", "wall"}, "description": "slaSeconds đếm theo gì (tuỳ chọn): 'business' = giờ làm việc theo lịch của tổ chức (mặc định khi bỏ trống), 'wall' = giờ đồng hồ liên tục kể cả đêm/cuối tuần."},
									"approvers": map[string]any{
										"type":        "array",
										"description": "Duyệt SONG SONG: danh sách >= 2 người cùng giữ bước (thay cho approverType/approverRef). Mỗi người: type 'role' (ref = TÊN role) hoặc 'user' (ref = email).",
										"items": map[string]any{
											"type": "object",
											"properties": map[string]any{
												"type": map[string]any{"type": "string", "enum": []string{"user", "role"}},
												"ref":  map[string]any{"type": "string", "description": "role: TÊN role; user: email. Hệ thống đổi ra id lúc Apply."},
											},
											"required": []string{"type", "ref"},
										},
									},
									"completionPercent": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "CHỈ cùng completion 'quorum': phần trăm số người duyệt cần có để bước xong (vd 60 trên 3 người = cần 2). Từ chối sớm khi không còn đạt được."},
									"completion":        map[string]any{"type": "string", "enum": []string{"all", "any", "quorum"}, "description": "Chỉ cùng approvers: 'quorum' = đủ completionPercent % số người duyệt (bắt buộc kèm completionPercent), 'all' = tất cả phải duyệt (mặc định), 'any' = người đầu tiên quyết."},
									"onReject": map[string]any{
										"type":        "object",
										"description": "Khi bước này BỊ TỪ CHỐI (tuỳ chọn; bỏ trống = từ chối là kết thúc). to: '@end' (kết thúc), '@submitter' (trả về người gửi sửa lại rồi Gửi lại) hoặc mã (id) của MỘT BƯỚC ĐỨNG TRƯỚC (quay về bước đó, hạn mới; bước không có id thì mã theo vị trí 's1','s2'…). resumeAt chỉ cùng '@submitter': 'rejector' (mặc định — Gửi lại thì quay về đúng người từ chối) hoặc 'start' (làm lại từ bước đầu).",
										"properties": map[string]any{
											"to":       map[string]any{"type": "string"},
											"resumeAt": map[string]any{"type": "string", "enum": []string{"rejector", "start"}},
										},
										"required": []string{"to"},
									},
								},
							},
						},
					},
					"required": []string{"name", "entity", "trigger", "steps"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_dashboard",
				Description: "Đề xuất một DASHBOARD (bảng điều khiển) gồm các widget số liệu. Chỉ đưa vào draft — KHÔNG tạo; người duyệt Apply qua POST /admin/dashboards. Mỗi widget: type kpi (một con số) / bar / line / table / funnel; metric count/sum/avg/min/max (sum/avg/min/max cần field SỐ); groupBy để chia nhóm (bar/line/table nên có); bucket (day/week/month/quarter/year) CHỈ khi groupBy là field ngày. Entity của widget phải ĐÃ TỒN TẠI hoặc vừa propose_entity trong CÙNG lượt. Vị trí/kích thước widget hệ thống tự xếp.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]any{"type": "string", "description": "Tên dashboard, tiếng Việt được (vd 'Tổng quan bán hàng')."},
						"description": map[string]any{"type": "string", "description": "Mô tả ngắn (tuỳ chọn)."},
						"widgets": map[string]any{
							"type":        "array",
							"description": "Danh sách widget (1..12).",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"title":   map[string]any{"type": "string", "description": "Tiêu đề widget (vd 'Doanh thu theo tháng')."},
									"type":    map[string]any{"type": "string", "enum": []string{"kpi", "bar", "line", "table", "funnel"}, "description": "Kiểu hiển thị."},
									"entity":  map[string]any{"type": "string", "description": "Entity nguồn số liệu."},
									"metric":  map[string]any{"type": "string", "enum": []string{"count", "sum", "avg", "min", "max"}, "description": "Phép tính."},
									"field":   map[string]any{"type": "string", "description": "Field SỐ để tính khi metric là sum/avg/min/max (bỏ trống với count)."},
									"groupBy": map[string]any{"type": "string", "description": "Field chia nhóm (tuỳ chọn; kpi thường KHÔNG groupBy). Nên là field có index hoặc cột hệ thống created_at/updated_at."},
									"bucket":  map[string]any{"type": "string", "enum": []string{"day", "week", "month", "quarter", "year"}, "description": "Gom theo thời gian — CHỈ khi groupBy là field date/datetime hoặc created_at/updated_at."},
									"filters": reportFilterArraySchema(),
								},
								"required": []string{"title", "type", "entity", "metric"},
							},
						},
					},
					"required": []string{"name", "widgets"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "propose_workqueue",
				Description: "Đề xuất một HÀNG ĐỢI CÔNG VIỆC (work queue) — thẻ 'việc hôm nay' trên màn hình chính: đếm record khớp bộ lọc và deep-link vào danh sách. Chỉ đưa vào draft — KHÔNG tạo; người duyệt Apply qua POST /admin/work-queues. Entity phải ĐÃ TỒN TẠI hoặc vừa propose_entity trong CÙNG lượt. roleName (tuỳ chọn) là TÊN role được thấy queue (bỏ trống = mọi người; hệ thống đổi ra id lúc Apply). Filter chỉ nhận FIELD CỦA ENTITY (không phải cột hệ thống); field ngày nhận token động today/tomorrow/this_week/this_month/next_7_days… — 'quá hạn' = op lt + value today.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":       map[string]any{"type": "string", "description": "Tên máy của queue, snake_case không dấu, tối đa 64 ký tự (vd 'lead_moi_hom_nay')."},
						"label":      map[string]any{"type": "string", "description": "Nhãn hiển thị tiếng Việt có dấu (vd 'Lead mới hôm nay')."},
						"entity":     map[string]any{"type": "string", "description": "Entity nguồn của queue."},
						"roleName":   map[string]any{"type": "string", "description": "Tuỳ chọn: TÊN role được thấy queue này (vd 'sales_rep'); bỏ trống = mọi người."},
						"filters":    workQueueFilterArraySchema(),
						"sortField":  map[string]any{"type": "string", "description": "Tuỳ chọn: field sắp xếp danh sách khi mở queue — phải có index (unique/relation/indexed) hoặc là cột hệ thống created_at/updated_at/id."},
						"sortOrder":  map[string]any{"type": "string", "enum": []string{"asc", "desc"}, "description": "Hướng sắp xếp (mặc định asc)."},
						"actionVerb": map[string]any{"type": "string", "enum": []string{"open", "approve", "claim"}, "description": "Hành động của thẻ: open = mở danh sách entity đã lọc (mặc định); approve = mở hộp phê duyệt; claim = mở hàng chờ nhận việc (thẻ đếm việc chưa ai nhận)."},
					},
					"required": []string{"name", "label", "entity"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "explain_access",
				Description: "GIẢI THÍCH QUYỀN truy cập của MỘT user trong tenant hiện tại lên MỘT entity (và một record cụ thể nếu truyền recordId): user là ai (role, admin?), tầng module-ACL cho view/edit/delete, tầng row-level (record có hiện ra với user không, vì sao), tầng field-ACL (field bị ẩn / chỉ-đọc). Chỉ đọc, không ghi, không vào draft. Dùng khi được hỏi 'vì sao user X không thấy/không sửa được Y'. Truyền userEmail HOẶC userId (một trong hai).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"userEmail": map[string]any{"type": "string", "description": "Email của user cần soi quyền (user cùng hệ thống với bạn)."},
						"userId":    map[string]any{"type": "string", "description": "Hoặc: id của user cần soi quyền."},
						"entity":    map[string]any{"type": "string", "description": "Entity cần kiểm tra quyền (phải đang tồn tại)."},
						"recordId":  map[string]any{"type": "string", "description": "Tuỳ chọn: id record cụ thể để soi thêm tầng row-level."},
					},
					"required": []string{"entity"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "list_roles",
				Description: "Liệt kê TÊN các vai (role) đang có của tenant. BẮT BUỘC gọi trước khi dùng roleName trong propose_workqueue hoặc người duyệt (approverType='role') trong propose_approval — tên vai không có thật sẽ làm bước Apply thất bại. Chưa có vai phù hợp thì bỏ trống roleName (queue hiện với mọi người) hoặc dùng approverType='user'.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "list_packs",
				Description: "Liệt kê CATALOG các vertical pack dựng sẵn của engine (crm, clinic, ticketing, lms, pm…): slug, tên, version, mô tả và danh sách entity mỗi pack sẽ tạo khi cài. Chỉ đọc. Dùng TRƯỚC khi đề xuất cả bộ entity ở chế độ Discovery: nhu cầu trùng pack sẵn có → khuyên 'cài pack X' (màn Cài mẫu & Bàn giao) rồi CHỈ propose phần delta, thay vì đề xuất lại từ đầu.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
	}
}

// workflowConditionSchema is the shared JSON-schema fragment for a workflow-style condition block
// ({match:"and"|"or", rules:[{field,op,value?}]}) — the SAME filter vocabulary propose_workflow's
// condition and propose_approval's criteria accept (the approval service reuses the workflow
// condition vocabulary — validation-parity B6, one filter dialect). The op enum equals
// workflow.Operators() and the match enum equals what both validators accept — pinned by
// TestConditionSchema_ParityWithValidator; the op enum stays hand-written on purpose so a new
// engine operator fails that test until someone teaches its value rule here.
func workflowConditionSchema(description string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": description,
		"properties": map[string]any{
			"match": map[string]any{"type": "string", "enum": []string{"and", "or"}, "description": "Cách nối các rule: 'and' (mặc định) = thoả TẤT CẢ rule; 'or' = thoả ÍT NHẤT MỘT rule."},
			"rules": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"field": map[string]any{"type": "string", "description": "Tên field của entity."},
						"op": map[string]any{
							"type":        "string",
							"enum":        []string{"eq", "neq", "gt", "gte", "lt", "lte", "contains", "isnull", "notnull", "changed", "changedTo"},
							"description": "Toán tử. eq/neq/gt/gte/lt/lte/contains so trạng thái hiện tại với value; isnull/notnull = rỗng/không rỗng. changed = «vừa đổi» (lần lưu này đã sửa field), changedTo = «vừa đổi thành» value — chỉ đúng MỘT lần lúc field chuyển sang giá trị đó, không lặp lại ở các lần sửa sau (khác eq). changed/changedTo so với giá trị TRƯỚC khi sửa nên chỉ có nghĩa với trigger after_update.",
						},
						"value": map[string]any{"description": "Toán hạng so sánh. BẮT BUỘC với eq/neq/gt/gte/lt/lte/contains/changedTo; KHÔNG được kèm với isnull/notnull/changed."},
					},
					"required": []string{"field", "op"},
				},
			},
		},
	}
}

// workQueueFilterArraySchema is the JSON-schema fragment for propose_workqueue's filters[] — the
// stored {field, op, value} wire shape of sys_work_queues.filters (workqueue.QueueFilter), where
// between/in pack their operands comma-joined into the single value string (mirroring the FE
// FilterBuilder). Date fields additionally accept the runtime's dynamic tokens.
func workQueueFilterArraySchema() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "Tuỳ chọn: bộ lọc record của queue (nối AND).",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"field": map[string]any{"type": "string", "description": "Field để lọc (phải có trong entity)."},
				"op":    map[string]any{"type": "string", "enum": []string{"eq", "neq", "gt", "gte", "lt", "lte", "contains", "in", "between", "isnull", "notnull"}, "description": "Toán tử; bỏ trống = eq."},
				"value": map[string]any{"type": "string", "description": "Giá trị thô dạng chuỗi; in/between nối nhiều giá trị bằng dấu phẩy; isnull/notnull để trống. Field ngày nhận token động: today, yesterday, tomorrow, this_week, last_week, next_week, this_month, last_month, this_quarter, this_year, last_7_days, last_30_days, next_7_days, next_30_days ('quá hạn' = op lt + value today)."},
			},
			"required": []string{"field"},
		},
	}
}

// reportGroupAxisSchema is the JSON-schema fragment of one grouping axis {field, bucket?} —
// shared by propose_report's subGroups[] items and columnGroup.
func reportGroupAxisSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"field":  map[string]any{"type": "string", "description": "Field làm trục (phải có index như groupBy)."},
			"bucket": map[string]any{"type": "string", "enum": []string{"day", "week", "month", "quarter", "year"}, "description": "Trục là field ngày: gom theo khoảng thời gian."},
		},
		"required": []string{"field"},
	}
}

// reportFilterArraySchema is the JSON-schema fragment for propose_report's filters[] — the SAME
// ?field[op]=value vocabulary a live list/aggregate query accepts (one filter dialect, see
// services.ReportFilterInput).
func reportFilterArraySchema() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "Tuỳ chọn: điều kiện lọc dữ liệu trước khi tính (nối AND).",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"field": map[string]any{"type": "string", "description": "Field để lọc (phải tồn tại trong entity)."},
				"op":    map[string]any{"type": "string", "enum": []string{"eq", "neq", "gt", "gte", "lt", "lte", "contains", "in", "between", "isnull", "notnull"}, "description": "Toán tử so sánh; bỏ trống = eq."},
				"values": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Toán hạng (chuỗi thô): 1 phần tử cho eq/neq/gt/…, nhiều cho in, đúng 2 cho between, rỗng cho isnull/notnull."},
			},
			"required": []string{"field"},
		},
	}
}

// workflowActionArraySchema is the JSON-schema fragment for propose_workflow's actions[]. The
// type enum deliberately EXCLUDES outbound_webhook (safe-action policy, spec §14): an outbound
// URL must come from a human through the manual builder, never be invented by the model. The
// exclusion is enforced twice — here structurally, and by a hard reject in proposeWorkflow
// (models do not always honor enums).
func workflowActionArraySchema() map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"type":    map[string]any{"type": "string", "enum": []string{"field_update", "in_app_notification", "email_alert"}},
				"field":   map[string]any{"type": "string", "description": "field_update: field đích (phải tồn tại, không phải computed/auto_number)."},
				"value":   map[string]any{"description": "field_update: giá trị gán (scalar JSON; null để xoá)."},
				"userId":  map[string]any{"type": "string", "description": "in_app_notification: user nhận — dùng \"$owner\" cho người phụ trách record."},
				"title":   map[string]any{"type": "string", "description": "in_app_notification: tiêu đề (tuỳ chọn)."},
				"message": map[string]any{"type": "string", "description": "in_app_notification: nội dung thông báo."},
				"to":      map[string]any{"type": "string", "description": "email_alert: địa chỉ nhận (hỗ trợ {{field}})."},
				"subject": map[string]any{"type": "string", "description": "email_alert: tiêu đề ({{field}} được render lúc chạy)."},
				"body":    map[string]any{"type": "string", "description": "email_alert: nội dung ({{field}} được render lúc chạy)."},
			},
			"required": []string{"type"},
		},
	}
}

// fieldArraySchema is the JSON-schema fragment for the fields[] array inside propose_entity.
func fieldArraySchema() map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":      map[string]any{"type": "string"},
				"label":     map[string]any{"type": "string"},
				"fieldType": fieldTypeSchema(),
				"options":   fieldOptionsSchema(),
				"required":  map[string]any{"type": "boolean"},
			},
			"required": []string{"name", "label", "fieldType"},
		},
	}
}

// fieldOptionsSchema is the shared JSON-schema fragment for a proposed field's options object
// (propose_field top-level and propose_entity's nested fields[]). M9: the description spells out
// the per-fieldType validation keys the config-time validator (services.ValidateAddField →
// validateFieldConfig) actually checks, so the model has the shape in the TOOL CONTRACT, not just
// the persona. Description-only — no new required params, no schema-shape change (the 10-tool
// invariant scan stands).
func fieldOptionsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"description": "Cấu hình + luật validation theo fieldType. " +
			"picklist/multi_select BẮT BUỘC {values:[chuỗi không rỗng,...]} — MÃ lưu trữ ổn định (tiếng Anh không dấu OK); kèm TUỲ CHỌN valueLabels {mã: nhãn tiếng Việt} cho chữ hiện ra — người dùng đọc nhãn, hệ thống lưu mã. " +
			"picklist ĐƠN có thể PHỤ THUỘC một picklist đơn khác trên CÙNG entity (vd Quận phụ thuộc Tỉnh): thêm controllingField:'<tên field cha>' và dependentValues:{'<mã giá trị của cha>':['<mã con được phép>',...]} — mỗi khoá là một mã trong values của field CHA, mỗi mã con phải nằm trong values của chính field này, ít nhất một cha có con; field cha phải có sẵn (hoặc propose trước). Người dùng chỉ chọn được con thuộc cha đã chọn. " +
			"number: min/max (số, min<=max), step (>0). " +
			"text/rich_text: maxLength (số nguyên dương), pattern (regex RE2, so full-match). " +
			"currency: currencyCode BẮT BUỘC (3 chữ in hoa, vd 'VND'), precision (số nguyên 0..8), min/max. " +
			"rating: min (>=1), max (2..10). " +
			"relation BẮT BUỘC {relatedEntity:'<entity cha>'}; thêm được cardinality: '1-N' (mặc định) hoặc '1-1', và inverseField: tên field ngược trên entity cha (mặc định = tên entity con). " +
			"user: KHÔNG cần options — trỏ tới một NHÂN SỰ (tài khoản đăng nhập — người phụ trách/giao cho), KHÔNG phải relation tới entity nào; KHÔNG đặt isUnique, KHÔNG giá trị mặc định. " +
			"polymorphic_relation BẮT BUỘC {targetEntities:['<entity>','<entity>',...]} — mảng tên entity ĐÃ TỒN TẠI mà một record được gắn vào (vd 'liên quan tới' account HOẶC contact); tên field ≤21 ký tự; KHÔNG isUnique, KHÔNG indexed. " +
			"formula BẮT BUỘC {formula:'<biểu thức trên field number/currency của CHÍNH entity: + - * / ( ), so sánh > >= < <= = != CHỈ làm điều kiện, nối điều kiện bằng AND / OR, IF(điều kiện, giá trị nếu đúng, giá trị nếu sai) — vd amount*quantity, hoa hồng bậc thang IF(amount >= 100000000, amount*0.05, amount*0.03), chiết khấu IF(amount > 50000000 AND quantity > 10, amount*0.1, 0); chữ trong ngoặc kép CHỈ làm giá trị của IF, vd xếp loại IF(score >= 8, \"Giỏi\", IF(score >= 6.5, \"Khá\", \"TB\")); không so sánh/nối chữ, không tham chiếu field chữ, không hàm nào khác>'}, resultType 'number'|'currency'|'text' — 'text' khi công thức ra chữ (xếp loại), còn lại ra số. " +
			"rollup BẮT BUỘC {childEntity:'<entity con trỏ về entity này>', aggregateFunction:'SUM'|'COUNT'|'AVG'|'MIN'|'MAX', field:'<field số trên entity con, bỏ khi COUNT>'}. " +
			"lookup BẮT BUỘC {via:'<tên field relation trên CHÍNH entity>', sourceField:'<field lấy từ entity cha qua relation đó>'}. " +
			"auto_number: prefix (chuỗi ≤20 ký tự, vd 'HĐ-'), padding (0..12, số chữ số 0 đệm) — dùng cho SỐ CHỨNG TỪ (mã hoá đơn, mã đơn). KHÔNG đặt isUnique. " +
			"file/image: không cần options (lưu trong cột data, không index/unique được).",
	}
}

// fieldTypeSchema constrains fieldType to the Nexus field types the AI Builder may propose.
// AI-TOOL-COVERAGE (Wave 4.6): the read-time/advanced types (formula, rollup, lookup,
// auto_number, file, image) are NOW on the AI surface — an AI-drafted invoice app was missing
// its document number (auto_number) and computed totals (formula/rollup) BY CONSTRUCTION. Each
// is config-time structurally validated by services.ValidateAddField → validateFieldConfig
// (parity), and the cross-field DEEP check (rollup childEntity exists, lookup via-relation
// resolves) happens at Apply exactly as it already does for `relation` — a 422 the model reads
// and self-corrects, never a silent bad draft.
// The enum equals the engine allowlist (services.AllowedFieldTypes) — pinned by
// TestFieldTypeSchema_MatchesEngineAllowlist. It stays hand-written on purpose: a new engine type
// fails that test until someone also teaches its options shape in fieldOptionsSchema.
func fieldTypeSchema() map[string]any {
	return map[string]any{
		"type": "string",
		"enum": []string{
			"text", "rich_text", "number", "boolean", "date", "datetime",
			"email", "currency", "rating", "picklist", "multi_select", "relation", "composite",
			"formula", "rollup", "lookup", "auto_number", "file", "image",
			"user", "polymorphic_relation",
		},
	}
}

// toolExecutor runs a single tool call against the tenant-scoped metadata reader and the draft
// buffer. tenantID is set ONCE by the agent from the request's verified JWT and is the ONLY tenant
// any tool ever uses; the arguments the model sends can never carry a tenant (no schema field for
// it). draft accumulates the VALID proposals of the turn.
type toolExecutor struct {
	reader   MetadataReader
	tenantID string
	draft    []DraftItem
	// turnTag (ENG-SAMPLE-REF) namespaces every "@ref" token propose_records mints THIS turn, so
	// two unrelated conversations can never collide on a human-chosen name like "@du_an_1" in the
	// server-side cross-request ref registry (services.RecordService — record_ref_registry.go).
	// Set once per agent run from the SAME run id already minted for the usage ledger (agent.go) —
	// stable for the whole turn, empty in unit tests that construct toolExecutor directly (harmless:
	// an empty tag mangles to the raw token, which unit tests can predict without knowing a tag).
	turnTag string
	// docs backs get_layout / list_workflows / list_reports / get_app (specs/067 L10-3); nil => DOC_READ_UNAVAILABLE.
	docs *DocReaders
}

// execute runs the named tool with the model-supplied JSON arguments and returns the tool-result
// content (a JSON string fed back to the model as a role:"tool" message). It NEVER returns a Go
// error: a bad tool call (unknown name, malformed args, off-spec proposal, missing entity) is
// surfaced INSIDE the content as {"ok":false,"error":"<CODE>: ..."} so the model can read the code
// and self-correct on the next step (spec §4). Only VALID proposals append to e.draft.
func (e *toolExecutor) execute(ctx context.Context, name, argsJSON string) string {
	return e.executeWithChange(ctx, name, argsJSON)
}

// dispatch routes one tool call to its implementation (execute adds the change envelope on top).
func (e *toolExecutor) dispatch(ctx context.Context, name, argsJSON string) string {
	switch name {
	case "list_entities":
		return e.listEntities()
	case "list_fields":
		return e.listFields(ctx, argsJSON)
	case "list_relationships":
		return e.listRelationships(ctx, argsJSON)
	case "get_layout":
		return e.getLayout(ctx, argsJSON)
	case "list_workflows":
		return e.listWorkflows(ctx, argsJSON)
	case "list_reports":
		return e.listReports(ctx, argsJSON)
	case "get_app":
		return e.getApp(ctx, argsJSON)
	case "list_roles":
		return e.listRoles(ctx)
	case "snapshot_model":
		return e.snapshotModel(ctx)
	case "propose_entity":
		return e.proposeEntity(argsJSON)
	case "propose_field":
		return e.proposeField(argsJSON)
	case "propose_relationship":
		return e.proposeRelationship(ctx, argsJSON)
	case "propose_layout":
		return e.proposeLayout(ctx, argsJSON)
	case "propose_workflow":
		return e.proposeWorkflow(ctx, argsJSON)
	case "propose_report":
		return e.proposeReport(ctx, argsJSON)
	case "propose_app":
		return e.proposeApp(ctx, argsJSON)
	case "propose_edit_field":
		return e.proposeEditField(ctx, argsJSON)
	case "propose_delete_field":
		return e.proposeDeleteField(ctx, argsJSON)
	case "propose_delete_entity":
		return e.proposeDeleteEntity(ctx, argsJSON)
	case "propose_records":
		return e.proposeRecords(ctx, argsJSON)
	case "propose_approval":
		return e.proposeApproval(ctx, argsJSON)
	case "propose_dashboard":
		return e.proposeDashboard(ctx, argsJSON)
	case "propose_workqueue":
		return e.proposeWorkQueue(ctx, argsJSON)
	case "explain_access":
		return e.explainAccess(ctx, argsJSON)
	case "list_packs":
		return e.listPacks()
	default:
		return toolErr("UNKNOWN_TOOL: tool %q không tồn tại", name)
	}
}

func (e *toolExecutor) listEntities() string {
	ents, err := e.reader.ListEntities(e.tenantID)
	if err != nil {
		return toolErr("LIST_ENTITIES_FAILED: %v", err)
	}
	out := make([]map[string]any, 0, len(ents))
	for _, en := range ents {
		out = append(out, map[string]any{"name": en.Name, "label": en.Label})
	}
	return toolOK(map[string]any{"entities": out})
}

func (e *toolExecutor) listFields(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity string `json:"entity"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	ent, err := e.reader.GetEntityByName(ctx, e.tenantID, a.Entity)
	if errors.Is(err, metadata.ErrEntityNotFound) {
		return toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại trong tenant hiện tại", a.Entity)
	}
	if err != nil {
		return toolErr("LIST_FIELDS_FAILED: %v", err)
	}
	fields, err := e.reader.ListFields(ctx, e.tenantID, ent.ID)
	if err != nil {
		return toolErr("LIST_FIELDS_FAILED: %v", err)
	}
	out := make([]map[string]any, 0, len(fields))
	for _, f := range fields {
		row := map[string]any{
			"name": f.Name, "label": f.Label, "fieldType": f.FieldType, "required": f.IsRequired,
		}
		// M5 grounding upgrade: surface the relation-relevant option keys (and ONLY those —
		// the full options map would bloat every turn's token spend) so the model can see
		// existing links before proposing new ones.
		if opts := relationGroundingOptions(f.Options); opts != nil {
			row["options"] = opts
		}
		out = append(out, row)
	}
	return toolOK(map[string]any{"entity": ent.Name, "fields": out})
}

// relationGroundingOptions extracts the relation-relevant keys (relatedEntity, cardinality,
// inverseField — strings; targetEntities — the string array of a polymorphic_relation, queue #139)
// from a field's stored options JSON. Returns nil when the field has no options, none of those
// keys, or unparseable options — a broken options blob on ONE field must degrade that field's
// grounding, never fail the whole list_fields call.
func relationGroundingOptions(raw *string) map[string]any {
	if raw == nil || *raw == "" {
		return nil
	}
	var opts map[string]any
	if err := json.Unmarshal([]byte(*raw), &opts); err != nil {
		return nil
	}
	out := map[string]any{}
	for _, k := range []string{"relatedEntity", "cardinality", "inverseField"} {
		if v, ok := opts[k].(string); ok && strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	if list, ok := opts["targetEntities"].([]any); ok {
		targets := make([]string, 0, len(list))
		for _, t := range list {
			if s, ok := t.(string); ok && strings.TrimSpace(s) != "" {
				targets = append(targets, s)
			}
		}
		if len(targets) > 0 {
			out["targetEntities"] = targets
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Snapshot output caps (M10, spec §17). snapshot_model exists to make coverage assessment CHEAP —
// one call instead of N list_fields round-trips — so its output must stay a bounded prompt cost:
// a pathological tenant (thousands of entities/fields) must not blow the model's context or the
// turn's token budget. A Discovery-phase tenant is small (tens of entities, tens of fields each),
// so these caps are a safety ceiling, not a working limit. Worst case ≈ 50×40 compact field rows
// (~140 KB JSON). When a cap trims the output, the result carries entitiesTruncated/fieldsTruncated
// plus the true totals, and the tool description tells the model to fall back to list_fields for
// anything trimmed — the snapshot degrades honestly instead of silently lying about coverage.
const (
	maxSnapshotEntities        = 50
	maxSnapshotFieldsPerEntity = 40
)

// snapshotModel returns the tenant's ENTIRE current model in one tool result (M10 grounding for
// Discovery coverage): every entity with a compact per-field summary (name, label, fieldType —
// deliberately nothing else; options/required would multiply the token cost and list_fields
// already serves the detailed view). It composes the SAME tenant-scoped reader calls the existing
// tools use (ListEntities + ListFields per entity) — read-only, no side effect, no draft mutation,
// and no tenant parameter anywhere (tenantID comes from the executor, set from the verified JWT).
// An empty tenant yields {"entities":[]} — a valid empty snapshot, never an error. Any reader
// failure fails the whole call (SNAPSHOT_FAILED): a partial snapshot that silently omitted
// entities would corrupt the very coverage assessment this tool grounds.
func (e *toolExecutor) snapshotModel(ctx context.Context) string {
	ents, err := e.reader.ListEntities(e.tenantID)
	if err != nil {
		return toolErr("SNAPSHOT_FAILED: %v", err)
	}
	entitiesTotal := len(ents)
	entitiesTruncated := false
	if len(ents) > maxSnapshotEntities {
		ents = ents[:maxSnapshotEntities]
		entitiesTruncated = true
	}

	out := make([]map[string]any, 0, len(ents))
	for _, en := range ents {
		fields, err := e.reader.ListFields(ctx, e.tenantID, en.ID)
		if err != nil {
			return toolErr("SNAPSHOT_FAILED: %v", err)
		}
		fieldsTotal := len(fields)
		fieldsTruncated := false
		if len(fields) > maxSnapshotFieldsPerEntity {
			fields = fields[:maxSnapshotFieldsPerEntity]
			fieldsTruncated = true
		}
		fs := make([]map[string]any, 0, len(fields))
		for _, f := range fields {
			fs = append(fs, map[string]any{"name": f.Name, "label": f.Label, "fieldType": f.FieldType})
		}
		row := map[string]any{"name": en.Name, "label": en.Label, "fields": fs}
		if fieldsTruncated {
			row["fieldsTruncated"] = true
			row["fieldsTotal"] = fieldsTotal
		}
		out = append(out, row)
	}

	res := map[string]any{"entities": out}
	if entitiesTruncated {
		res["entitiesTruncated"] = true
		res["entitiesTotal"] = entitiesTotal
	}
	return toolOK(res)
}

// listRelationships returns the tenant's ACTIVE relationships (minimal projection: name, the two
// entity sides, type, inverseField) so the model grounds on existing links before proposing. The
// optional entity arg filters to relationships touching that entity (either side); it is resolved
// tenant-scoped FIRST so a typo'd name surfaces as ENTITY_NOT_FOUND (model self-correction)
// instead of a silently-empty list the model could misread as "no relationships yet".
func (e *toolExecutor) listRelationships(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity string `json:"entity"`
	}
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return toolErr("INVALID_ARGS: %v", err)
		}
	}
	// filterEntity is the CANONICAL name to filter by — resolved from the reader, not the raw
	// model-supplied string. A store whose entity lookup is case/whitespace-tolerant (e.g. a
	// MySQL ci collation on sys_entities.name) can resolve "Treatments"/"treatments " to the
	// stored "treatments" row while still reporting success; filtering by the RAW input string
	// against RelationshipDefinition rows (which always hold the canonical stored name) would
	// then match nothing — an empty list the model reads as "no relationships yet" and proceeds
	// to propose a duplicate. Filtering by the resolved ent.Name closes that gap (F2).
	filterEntity := ""
	if a.Entity != "" {
		ent, err := e.reader.GetEntityByName(ctx, e.tenantID, a.Entity)
		if err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				return toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại trong tenant hiện tại", a.Entity)
			}
			return toolErr("LIST_RELATIONSHIPS_FAILED: %v", err)
		}
		filterEntity = ent.Name
	}

	rels, err := e.reader.ListAllRelationships(ctx, e.tenantID)
	if err != nil {
		return toolErr("LIST_RELATIONSHIPS_FAILED: %v", err)
	}
	out := make([]map[string]any, 0, len(rels))
	for _, r := range rels {
		if filterEntity != "" && r.ParentEntityName != filterEntity && r.ChildEntityName != filterEntity {
			continue
		}
		row := map[string]any{
			"name":             r.Name,
			"parentEntityName": r.ParentEntityName,
			"childEntityName":  r.ChildEntityName,
			"relationshipType": r.RelationshipType,
		}
		if r.InverseField != "" {
			row["inverseField"] = r.InverseField
		}
		out = append(out, row)
	}
	return toolOK(map[string]any{"relationships": out})
}

// proposeEntity validates a proposed entity through the SAME config-time STRUCTURAL validator the
// manual builder uses (services.ValidateCreateEntity) — name/label/fieldType/options shape. This is
// config-time parity ONLY: the DB-dependent rejects the real apply-path adds (duplicate entity name
// → 409 ENTITY_ALREADY_EXISTS, relation-target existence, cross-tenant shared-unique) still surface
// at APPLY, not here — so a buffered draft is structurally sound but NOT apply-guaranteed (HITL +
// the gated apply-path are the backstop). On any ValidationError it returns the code to the model;
// only a structurally valid entity is buffered.
func (e *toolExecutor) proposeEntity(argsJSON string) string {
	var a struct {
		Name   string          `json:"name"`
		Label  string          `json:"label"`
		Fields []proposedField `json:"fields"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	in := services.CreateEntityInput{
		Name:   a.Name,
		Label:  a.Label,
		Fields: make([]services.FieldInput, 0, len(a.Fields)),
	}
	for _, f := range a.Fields {
		in.Fields = append(in.Fields, f.toFieldInput())
	}
	if code, msg, ok := validationOf(services.ValidateCreateEntity(in)); !ok {
		return toolErr("%s: %s", code, msg)
	}

	df := make([]DraftField, 0, len(a.Fields))
	for _, f := range a.Fields {
		df = append(df, f.toDraftField(""))
	}
	e.draft = append(e.draft, DraftItem{
		Kind:   "entity",
		Entity: &DraftEntity{Name: a.Name, Label: a.Label, Fields: df},
	})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("entity %q (%d field)", a.Name, len(df))})
}

// proposeField validates a proposed field through services.ValidateAddField (the add-field
// config-time validator) before buffering — same parity guarantee as proposeEntity.
func (e *toolExecutor) proposeField(argsJSON string) string {
	var a proposedField
	// proposedField only carries the field attributes; entity is a sibling key on the same object.
	var withEntity struct {
		Entity string `json:"entity"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	if err := json.Unmarshal([]byte(argsJSON), &withEntity); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	in := services.AddFieldInput{EntityName: withEntity.Entity, Field: a.toFieldInput()}
	if code, msg, ok := validationOf(services.ValidateAddField(in)); !ok {
		return toolErr("%s: %s", code, msg)
	}

	// specs/052 (queue #230): the cross-field half of AddField - controllingField must be a sibling
	// picklist (live, or drafted this turn), keys among its values, no cycle - judged now, not at Apply.
	if code, msg, ok := validationOf(e.checkAddFieldRefs(context.Background(), withEntity.Entity, a.toFieldInput())); !ok {
		return toolErr("%s: %s", code, msg)
	}

	e.draft = append(e.draft, DraftItem{Kind: "field", Field: ptrDraftField(a.toDraftField(withEntity.Entity))})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("field %q trên %q", a.Name, withEntity.Entity)})
}

// checkAddFieldRefs runs services.ValidateAddFieldRefs against the entity's siblings: its live
// fields (tenant-scoped reader) plus the fields buffered as drafts THIS turn (entity draft or
// earlier propose_field). Only when the options declare controllingField, so a plain field costs
// nothing. An entity that is neither live nor drafted has no siblings - the apply path reports
// ENTITY_NOT_FOUND for it.
func (e *toolExecutor) checkAddFieldRefs(ctx context.Context, entity string, f services.FieldInput) error {
	if f.Options["controllingField"] == nil {
		return nil
	}
	var siblings []services.FieldInput
	if ent, err := e.reader.GetEntityByName(ctx, e.tenantID, entity); err == nil {
		fields, lerr := e.reader.ListFields(ctx, e.tenantID, ent.ID)
		if lerr != nil {
			return fmt.Errorf("ai.checkAddFieldRefs: %w", lerr)
		}
		if siblings, err = services.LiveFieldInputs(fields); err != nil {
			return fmt.Errorf("ai.checkAddFieldRefs: %w", err)
		}
	}
	canonical := strings.ToLower(strings.TrimSpace(entity))
	for _, it := range e.draft {
		if it.Kind == "entity" && it.Entity != nil && it.Entity.Name == canonical {
			for _, df := range it.Entity.Fields {
				siblings = append(siblings, services.FieldInput{Name: df.Name, FieldType: df.FieldType, Options: df.Options})
			}
		}
		if it.Kind == "field" && it.Field != nil && strings.ToLower(it.Field.Entity) == canonical {
			siblings = append(siblings, services.FieldInput{Name: it.Field.Name, FieldType: it.Field.FieldType, Options: it.Field.Options})
		}
	}
	return services.ValidateAddFieldRefs(f, siblings)
}

// maxRelationshipNameBytes mirrors the sys_relationships.name column, VARCHAR(150) — a BYTE
// budget (see metadata.tombstoneName's doc on why these lengths are byte budgets). A longer name
// would pass the apply-path handler's non-empty check and then die as a MySQL 1406 → 500 at
// apply; rejecting it here keeps every buffered draft applyable on the name axis.
const maxRelationshipNameBytes = 150

// maxWorkflowNameBytes mirrors the sys_workflows.name column, VARCHAR(150) — same BYTE-budget
// reasoning as maxRelationshipNameBytes (migrations/000016_create_sys_workflows.up.sql). Without
// this guard a long name passes proposeWorkflow's non-empty check, buffers, and dies as a MySQL
// 1406 → 500 at Apply — breaking the "no DB-dependent reject left for Apply" contract §14 relies on.
const maxWorkflowNameBytes = 150

// maxReportNameBytes mirrors the sys_reports.name column, VARCHAR(150) — same BYTE-budget
// reasoning as maxRelationshipNameBytes (migrations/000020_create_sys_reports.up.sql). Without
// this guard a long name passes proposeReport's non-empty check, buffers, and dies as a MySQL
// 1406 → 500 at Apply — breaking the "no DB-dependent reject left for Apply" contract §15 relies on.
const maxReportNameBytes = 150

// proposeRelationship validates a proposed N-N relationship with config-time PARITY to the apply
// path (POST /admin/relationships → services.RegisterRelationship) WITHOUT touching its DB-write
// path, then buffers it as a DraftItem{Kind:"relationship"}. NOTHING is created — the human
// applies via the gated admin API (HITL).
//
// Parity map (source → mirrored check, same order as the apply path):
//  1. name non-empty — handler-level check (admin_relation_handler.go Create → 400 INVALID_BODY).
//     RegisterRelationship itself enforces NO name pattern; we additionally cap at the column's
//     150-byte budget (see maxRelationshipNameBytes) so the draft can't 500 at apply.
//  2. both entities exist in THIS tenant — RegisterRelationship's first checks
//     (GetEntityByName → 422 ENTITY_NOT_FOUND). reader is tenant-scoped: the model can never
//     probe or target another tenant's entities.
//  3. entity-name sanity + self-referential reject — the EXACT helper the apply path calls
//     (ddl.JoinParts → 422 RELATION_INVALID_ENTITIES).
//
// NOT mirrored (DB-dependent, surfaces at APPLY exactly like propose_entity's duplicate-name):
// 409 RELATIONSHIP_ALREADY_EXISTS — the UNIQUE(tenant_id, name) / (tenant_id, join_table_name)
// indexes. Grounding via list_relationships is the model's tool to avoid proposing a duplicate.
func (e *toolExecutor) proposeRelationship(ctx context.Context, argsJSON string) string {
	var a struct {
		Name    string `json:"name"`
		EntityA string `json:"entityA"`
		EntityB string `json:"entityB"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	if strings.TrimSpace(a.Name) == "" {
		return toolErr("RELATION_NAME_REQUIRED: name là bắt buộc — quy ước '<a>_<b>' (vd 'students_courses')")
	}
	if len(a.Name) > maxRelationshipNameBytes {
		return toolErr("RELATION_NAME_TOO_LONG: name vượt %d byte (cột sys_relationships.name)", maxRelationshipNameBytes)
	}

	// Draft-aware (EASE Nước 7 enabler): accept an endpoint that is EITHER a live entity OR one
	// buffered as a draft THIS turn — the shared draftedEntities scan. This deletes the
	// forced second full-priced turn ("Apply entities first, then re-ask for the N-N") for the
	// whole-app path; the deep existence check still runs at Apply (entities are applied first —
	// FE applies in dependency order), so HITL safety is unchanged.
	drafted := e.draftedEntities()
	for _, ent := range []string{a.EntityA, a.EntityB} {
		if _, ok := drafted[strings.ToLower(strings.TrimSpace(ent))]; ok {
			continue // proposed this turn — structurally sound to reference (apply order: entity first)
		}
		if _, err := e.reader.GetEntityByName(ctx, e.tenantID, ent); err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				return toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại và cũng chưa được propose_entity trong lượt này — propose_entity trước rồi propose_relationship trong CÙNG lượt", ent)
			}
			return toolErr("PROPOSE_RELATIONSHIP_FAILED: %v", err)
		}
	}

	if _, _, _, err := ddl.JoinParts(a.EntityA, a.EntityB); err != nil {
		return toolErr("RELATION_INVALID_ENTITIES: %v", err)
	}

	e.draft = append(e.draft, DraftItem{
		Kind:         "relationship",
		Relationship: &DraftRelationship{Name: a.Name, EntityA: a.EntityA, EntityB: a.EntityB},
	})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("quan hệ N-N %q (%s ↔ %s)", a.Name, a.EntityA, a.EntityB)})
}

// proposedField is the model's per-field argument shape shared by propose_entity's fields[] and
// propose_field's top-level object.
type proposedField struct {
	Name      string         `json:"name"`
	Label     string         `json:"label"`
	FieldType string         `json:"fieldType"`
	Options   map[string]any `json:"options"`
	Required  bool           `json:"required"`
}

func (p proposedField) toFieldInput() services.FieldInput {
	return services.FieldInput{
		Name:       p.Name,
		Label:      p.Label,
		FieldType:  p.FieldType,
		IsRequired: p.Required,
		Options:    p.Options,
	}
}

func (p proposedField) toDraftField(entity string) DraftField {
	return DraftField{
		Entity:    entity,
		Name:      p.Name,
		Label:     p.Label,
		FieldType: p.FieldType,
		Options:   p.Options,
		Required:  p.Required,
	}
}

func ptrDraftField(f DraftField) *DraftField { return &f }

// listRoles returns the tenant's role names so the model can only NAME a role that exists.
//
// propose_workqueue's roleName and propose_approval's role approver are resolved to ids at Apply,
// and until now the model had no way to see the list — so it produced a reasonable-sounding name
// and the Apply failed on a role nobody had created. Failing at Apply is the worst place to learn
// it: the proposal already looked right and the person already pressed the button.
func (e *toolExecutor) listRoles(ctx context.Context) string {
	names, err := e.reader.ListRoleNames(ctx, e.tenantID)
	if err != nil {
		return toolErr("ROLES_READ_FAILED: không đọc được danh sách vai: %v", err)
	}
	if names == nil {
		names = []string{}
	}
	return toolOK(map[string]any{"roles": names})
}

// proposeLayout validates a proposed layout with config-time PARITY to the apply path
// (POST /admin/layouts → LayoutService.SaveLayout) and buffers it as DraftItem{Kind:"layout"}.
// NOTHING is written — the human applies via the gated admin API (HITL).
//
// Parity here is STRUCTURAL, not mirrored: services.ValidateLayoutConfig is the exported wrapper
// around the SAME validateLayout function body SaveLayout/UpdateLayout call — so a buffered draft
// is exactly what the apply path will accept, and an invalid layout comes back to the model as
// the REAL ValidationError code (LAYOUT_INVALID, or REQUIRED_FIELD_OMITTED when an edit view
// omits a required field) for self-correction in-chat instead of failing at Apply. The parity
// map, same order as SaveLayout:
//  1. entity exists in THIS tenant — GetEntityByName (→ ENTITY_NOT_FOUND). reader is
//     tenant-scoped: the model can never probe or target another tenant's entities.
//  2. live fields — the same ListFields read SaveLayout performs before validating.
//  3. ValidateLayoutConfig(viewType, layoutJson, liveFields) — every ../specs/041-layout-engine/spec.md §4.4 rule (strict viewType
//     enum, version, shape per view, field allowlist, duplicates, floor rule, required-omitted,
//     bounds, unknown-key strict-reject).
//
// Unlike propose_entity/propose_relationship there is NO DB-dependent reject left for Apply:
// SaveLayout is an upsert on (tenant, entity, view) — no duplicate conflict exists. The only
// apply-time divergence is a concurrent metadata change between propose and Apply (e.g. a field
// deleted in the meantime), which the apply path re-validates against.
//
// The draft stores the CANONICAL resolved entity name (ent.Name, not the raw model input) so the
// Apply POST body carries the exact stored name.
func (e *toolExecutor) proposeLayout(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity     string         `json:"entity"`
		ViewType   string         `json:"viewType"`
		LayoutJSON map[string]any `json:"layoutJson"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	ent, err := e.reader.GetEntityByName(ctx, e.tenantID, a.Entity)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại trong tenant hiện tại — nếu nó mới nằm trong draft, người duyệt phải Apply entity đó trước", a.Entity)
		}
		return toolErr("PROPOSE_LAYOUT_FAILED: %v", err)
	}
	liveFields, err := e.reader.ListFields(ctx, e.tenantID, ent.ID)
	if err != nil {
		return toolErr("PROPOSE_LAYOUT_FAILED: %v", err)
	}

	if code, msg, ok := validationOf(services.ValidateLayoutConfig(a.ViewType, a.LayoutJSON, liveFields)); !ok {
		return toolErr("%s: %s", code, msg)
	}

	dl := &DraftLayout{Entity: ent.Name, ViewType: a.ViewType, LayoutJSON: a.LayoutJSON}
	change, errStr := e.layoutChange(ctx, dl)
	if errStr != "" {
		return errStr
	}
	e.draft = append(e.draft, DraftItem{Kind: "layout", Layout: dl, Change: change})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("layout %s cho %q", a.ViewType, ent.Name)})
}

// DraftRelationship is a proposed N-N relationship between two EXISTING entities (M5). It maps
// 1:1 onto the body of POST /admin/relationships {name, entityA, entityB} — the human clicks
// Apply and the FE calls that gated API; the agent never writes DDL or sys_relationships itself.
// 1-N/1-1 relations are NOT represented here: they are relation FIELDS on the child entity and
// flow through DraftField (propose_field) like any other field.
type DraftRelationship struct {
	Name    string `json:"name"`
	EntityA string `json:"entityA"`
	EntityB string `json:"entityB"`
}

// DraftLayout is a proposed layout for one (entity, viewType) — M6. It maps 1:1 onto the body of
// POST /admin/layouts {entityName: Entity, viewType: ViewType, layoutJson: LayoutJSON} (upsert on
// the (tenant, entity, view) unique key — the human clicks Apply and the FE calls that gated API;
// the agent never writes sys_layouts itself). Entity carries the canonical stored entity name.
type DraftLayout struct {
	Entity     string         `json:"entity"`
	ViewType   string         `json:"viewType"`
	LayoutJSON map[string]any `json:"layoutJson"`
}

// DraftWorkflow is a proposed automation rule — M7. It maps 1:1 onto the body of
// POST /admin/workflows {name, entityName, trigger, condition, actions, isActive} (the human
// clicks Apply and the FE calls that gated API; the agent never writes sys_workflows itself).
// EntityName carries the canonical stored entity name. IsActive is always true — the tool takes
// no isActive/intervalSeconds params (an inactive or scheduled rule is a human decision in the
// manual builder). Trigger is one of the three after-commit events; the scheduled trigger is
// deliberately NOT proposable (spec §14).
type DraftWorkflow struct {
	Name       string                  `json:"name"`
	EntityName string                  `json:"entityName"`
	Trigger    string                  `json:"trigger"`
	Condition  *DraftWorkflowCondition `json:"condition,omitempty"`
	Actions    []DraftWorkflowAction   `json:"actions"`
	IsActive   bool                    `json:"isActive"`
}

// DraftWorkflowCondition mirrors the condition block of the POST /admin/workflows body. Absent
// (nil) → match-all.
type DraftWorkflowCondition struct {
	Match string              `json:"match,omitempty"`
	Rules []DraftWorkflowRule `json:"rules,omitempty"`
}

// canonicalizeMatch trims + lowercases match in place: both validators read it case-insensitively
// and the apply path persists the lowercase token, but the draft card compares it strictly
// (match === "or") — a buffered "OR" read "và" on the card while Apply created an OR rule. Called
// before validation so the buffered draft and the validator see the same canonical token.
func (c *DraftWorkflowCondition) canonicalizeMatch() {
	if c != nil {
		c.Match = strings.ToLower(strings.TrimSpace(c.Match))
	}
}

// DraftWorkflowRule is one field-op-value predicate of a proposed condition.
type DraftWorkflowRule struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}

// DraftWorkflowAction is one proposed automation step. It deliberately has NO url/secret/headers
// fields: outbound_webhook is hard-rejected by proposeWorkflow (safe-action policy, spec §14), so
// a buffered draft is STRUCTURALLY incapable of carrying a webhook target the model invented.
type DraftWorkflowAction struct {
	Type string `json:"type"`
	// field_update
	Field string `json:"field,omitempty"`
	// Value is `omitempty`: a field_update proposing an explicit JSON null (clear the field) and
	// a field_update that never set Value both serialize to Value simply being ABSENT from the
	// wire JSON — the FE cannot distinguish "clear to null" from "unset" on this shape (NIT-1,
	// cosmetic — FE-M7's concern: the draft-card/Apply mapper needs its own signal, e.g. a
	// `HasValue bool` or dropping `omitempty` here, if "explicit null" must render/apply
	// differently from "no value proposed").
	Value any `json:"value,omitempty"`
	// in_app_notification
	UserID  string `json:"userId,omitempty"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
	// email_alert
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
}

// proposeWorkflow validates a proposed automation rule with config-time PARITY to the apply path
// (POST /admin/workflows → WorkflowService.CreateWorkflow) and buffers it as
// DraftItem{Kind:"workflow"}. NOTHING is written — the human applies via the gated admin API
// (HITL).
//
// Parity here is STRUCTURAL, not mirrored (the M6 ValidateLayoutConfig pattern):
// services.ValidateWorkflowConfig wraps buildWorkflowRule — the SAME single body CreateWorkflow
// runs for trigger/condition/actions (incl. the WF-01 field_update target guard) — so a buffered
// draft is exactly what the apply path will accept, and an invalid rule comes back to the model
// as the REAL ValidationError code (WORKFLOW_TRIGGER_INVALID / WORKFLOW_CONDITION_INVALID /
// WORKFLOW_ACTIONS_REQUIRED / WORKFLOW_ACTION_INVALID / RECORD_UNKNOWN_FIELD /
// RECORD_*_READONLY) for self-correction in-chat. The split, same as M6's liveFields:
//  1. name non-empty (WORKFLOW_NAME_REQUIRED) + ≤150 byte (WORKFLOW_NAME_TOO_LONG, matching the
//     sys_workflows.name column — the M5 maxRelationshipNameBytes lesson: without this a long
//     name buffers clean and dies as a MySQL 1406 → 500 at Apply) — the ONLY hand-mirrored checks
//     (no rule logic to drift).
//  2. entity exists in THIS tenant — GetEntityByName (→ ENTITY_NOT_FOUND; the apply path's
//     equivalent is WORKFLOW_ENTITY_NOT_FOUND). reader is tenant-scoped: the model can never
//     probe or target another tenant's entities.
//  3. fieldTypes — the same field name → fieldType map CreateWorkflow resolves (fieldTypesFor);
//     built non-nil even for a fieldless entity so the WF-01 guard is ALWAYS armed here.
//  4. ValidateWorkflowConfig(trigger, condition, actions, fieldTypes) — one body with apply.
//
// SAFE-ACTION POLICY (hard rules on top of parity — the tool is deliberately STRICTER than the
// apply path, spec §14):
//   - outbound_webhook is REJECTED outright: an outbound URL must come from a human through the
//     manual builder, never be invented by the model (no SSRF-guard exists in buildActionsJSON).
//     Also excluded from the schema enum AND from DraftWorkflowAction's fields.
//   - the scheduled trigger is REJECTED: intervalSeconds cadence is a human decision; the tool
//     only proposes the three after-commit events (schema enum + this guard).
//   - the date_offset trigger (TIMER-1) is likewise REJECTED for v1: its timer config (which
//     date field anchors the deadline, offset size, business-hours calendar) is a human
//     scheduling decision exactly like intervalSeconds — same policy, same guard. The manual
//     builder carries it (condition.timer — services.buildTimerConfig).
//
// No DB-dependent reject is left for Apply (no unique name constraint on sys_workflows) — the
// only apply-time divergence is a concurrent metadata change between propose and Apply, which
// the apply path re-validates against.
func (e *toolExecutor) proposeWorkflow(ctx context.Context, argsJSON string) string {
	var a struct {
		Name      string                  `json:"name"`
		Entity    string                  `json:"entity"`
		Trigger   string                  `json:"trigger"`
		Condition *DraftWorkflowCondition `json:"condition"`
		Actions   []DraftWorkflowAction   `json:"actions"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	if strings.TrimSpace(a.Name) == "" {
		return toolErr("WORKFLOW_NAME_REQUIRED: name workflow là bắt buộc")
	}
	if len(a.Name) > maxWorkflowNameBytes {
		return toolErr("WORKFLOW_NAME_TOO_LONG: name vượt %d byte (cột sys_workflows.name)", maxWorkflowNameBytes)
	}

	// Policy guard 1 — proposable triggers only (STRICTER than ValidTrigger, which also accepts
	// "scheduled" and "date_offset" for the manual builder). trigger is normalized once so the
	// buffered draft and the validation see the same canonical token.
	trigger := strings.TrimSpace(a.Trigger)
	switch workflow.Trigger(trigger) {
	case workflow.TriggerAfterCreate, workflow.TriggerAfterUpdate, workflow.TriggerAfterDelete:
	default:
		// before_save/before_delete (specs/048, FR-011) land here too: a rule that can block every save
		// of an entity is a human decision — named explicitly so the model stops retrying.
		return toolErr("WORKFLOW_TRIGGER_INVALID: trigger %q không đề xuất được (chỉ after_create/after_update/after_delete — rule scheduled/date_offset cần người cấu hình cadence/timer, rule before_save/before_delete chặn được mọi lần lưu nên cần người cấu hình, tất cả trong builder thủ công)", a.Trigger)
	}

	// Policy guard 2 — outbound_webhook and assign must NEVER enter a draft, even if the model
	// ignores the schema enum. Checked BEFORE validation so the reject reason is the policy, not a
	// missing url/team. assign's team is a concrete id the model cannot know (ASSIGN-1, spec §14) —
	// like a webhook URL, it must come from a human in the manual builder.
	for _, act := range a.Actions {
		if strings.TrimSpace(act.Type) == workflow.ActionWebhook {
			return toolErr("WORKFLOW_ACTION_INVALID: outbound_webhook không tự đề xuất — cần URL do người dùng cung cấp (cấu hình trong builder thủ công)")
		}
		if strings.TrimSpace(act.Type) == workflow.ActionAssign {
			return toolErr("WORKFLOW_ACTION_INVALID: assign không tự đề xuất — team là id cụ thể do người dùng chọn (cấu hình trong builder thủ công)")
		}
		// create_record CAN create data (bigger blast radius than a field edit / notification), so it
		// is honest-disabled from AI proposals for v1 — like outbound_webhook/assign. buildActionsJSON
		// would otherwise ACCEPT a create_record the model emits off-schema, so reject it structurally
		// here (before validation), not just by the enum omission. A human wires create_record in the
		// manual builder, where its config-time target/field guards apply.
		if strings.TrimSpace(act.Type) == workflow.ActionCreateRecord {
			return toolErr("WORKFLOW_ACTION_INVALID: create_record không tự đề xuất — tạo bản ghi mới có phạm vi ảnh hưởng lớn, cần người cấu hình trong builder thủ công")
		}
	}

	ent, err := e.reader.GetEntityByName(ctx, e.tenantID, a.Entity)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại trong tenant hiện tại — nếu nó mới nằm trong draft, người duyệt phải Apply entity đó trước", a.Entity)
		}
		return toolErr("PROPOSE_WORKFLOW_FAILED: %v", err)
	}
	fields, err := e.reader.ListFields(ctx, e.tenantID, ent.ID)
	if err != nil {
		return toolErr("PROPOSE_WORKFLOW_FAILED: %v", err)
	}
	// Non-nil even when the entity has no fields — the WF-01 field_update target guard inside
	// buildActionsJSON is armed if and only if fieldTypes != nil.
	fieldTypes := make(map[string]string, len(fields))
	for _, f := range fields {
		fieldTypes[f.Name] = f.FieldType
	}

	a.Condition.canonicalizeMatch()
	var cond *services.WorkflowConditionInput
	if a.Condition != nil {
		rules := make([]services.WorkflowRuleInput, 0, len(a.Condition.Rules))
		for _, r := range a.Condition.Rules {
			rules = append(rules, services.WorkflowRuleInput{Field: r.Field, Op: r.Op, Value: r.Value})
		}
		cond = &services.WorkflowConditionInput{Match: a.Condition.Match, Rules: rules}
	}
	acts := make([]services.WorkflowActionInput, 0, len(a.Actions))
	for _, act := range a.Actions {
		acts = append(acts, services.WorkflowActionInput{
			Type: act.Type, Field: act.Field, Value: act.Value,
			UserID: act.UserID, Title: act.Title, Message: act.Message,
			To: act.To, Subject: act.Subject, Body: act.Body,
		})
	}
	if code, msg, ok := validationOf(services.ValidateWorkflowConfig(ent.Name, trigger, cond, acts, fieldTypes)); !ok {
		return toolErr("%s: %s", code, msg)
	}

	dw := &DraftWorkflow{
		Name:       a.Name,
		EntityName: ent.Name, // canonical resolved name, same as DraftLayout
		Trigger:    trigger,
		Condition:  a.Condition,
		Actions:    a.Actions,
		IsActive:   true,
	}
	change, errStr := e.workflowChange(ctx, dw)
	if errStr != "" {
		return errStr
	}
	e.draft = append(e.draft, DraftItem{Kind: "workflow", Workflow: dw, Change: change})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("workflow %q trên %q (%s, %d action)", a.Name, ent.Name, trigger, len(a.Actions))})
}

// DraftReport is a proposed saved report — M8. It maps 1:1 onto the body of POST /admin/reports
// {name, entityName, type, config, isActive} (the human clicks Apply and the FE calls that gated
// API; the agent never writes sys_reports itself). EntityName carries the canonical stored entity
// name; Type/Metric/Bucket are stored normalized (lowercase-trimmed) exactly as the apply path
// would persist them. IsActive is always true — an inactive report is a human decision in the
// manual builder. There is deliberately NO sort key: a tabular report's default ordering is
// cosmetic polish a human sets in the builder, not something the model should invent.
type DraftReport struct {
	Name       string            `json:"name"`
	EntityName string            `json:"entityName"`
	Type       string            `json:"type"` // "summary" | "matrix" | "tabular"
	Config     DraftReportConfig `json:"config"`
	IsActive   bool              `json:"isActive"`
}

// DraftReportConfig mirrors the config block of the POST /admin/reports body
// (services.ReportConfig): columns for tabular, groupBy/metric/field/bucket for summary/matrix,
// filters for both.
type DraftReportConfig struct {
	Columns []string            `json:"columns,omitempty"`
	Filters []DraftReportFilter `json:"filters,omitempty"`
	GroupBy string              `json:"groupBy,omitempty"`
	Metric  string              `json:"metric,omitempty"`
	Field   string              `json:"field,omitempty"`
	Bucket  string              `json:"bucket,omitempty"`
	// Grid keys (specs/049 S7a) — the SAME shapes as services.ReportConfig, stored normalized.
	SubGroups   []services.ReportGroupInput `json:"subGroups,omitempty"`
	ColumnGroup *services.ReportGroupInput  `json:"columnGroup,omitempty"`
	Having      *services.ReportHavingInput `json:"having,omitempty"`
}

// DraftReportFilter is one filter predicate of a proposed report config — the same
// field/op/values vocabulary as a live list/aggregate query (services.ReportFilterInput).
type DraftReportFilter struct {
	Field  string   `json:"field"`
	Op     string   `json:"op,omitempty"`
	Values []string `json:"values,omitempty"`
}

// proposeReport validates a proposed saved report with config-time PARITY to the apply path
// (POST /admin/reports → ReportService.CreateReport) and buffers it as DraftItem{Kind:"report"}.
// NOTHING is written — the human applies via the gated admin API (HITL).
//
// Parity here is STRUCTURAL, not mirrored (the M6/M7 wrapper pattern):
// services.ValidateReportConfig is the SAME single body CreateReport/UpdateReport run for
// type + config-shape validation (REPORT_TYPE_INVALID / REPORT_CONFIG_INVALID). The split, same
// as M6's liveFields:
//  1. name non-empty (REPORT_NAME_REQUIRED) + ≤150 byte (REPORT_NAME_TOO_LONG, matching the
//     sys_reports.name column — the M5 maxRelationshipNameBytes lesson: without this a long
//     name buffers clean and dies as a MySQL 1406 → 500 at Apply) — the ONLY hand-mirrored
//     checks (no rule logic to drift).
//  2. entity exists in THIS tenant — GetEntityByName (→ ENTITY_NOT_FOUND; the apply path's
//     equivalent is REPORT_ENTITY_NOT_FOUND). reader is tenant-scoped: the model can never
//     probe or target another tenant's entities.
//  3. liveFields — the tool passes reader.ListFields (non-nil even for a fieldless entity), which
//     arms ValidateReportConfig's DEEP mode: the summary/matrix aggregate surface and the filters
//     are resolved through resolveAggregatePlan/resolveFiltersForAggregate — the SAME bodies the
//     RUN engine executes — so the run-time codes (AGGREGATE_FIELD_UNKNOWN /
//     AGGREGATE_FIELD_NOT_NUMERIC / AGGREGATE_REQUIRES_INDEXED / AGGREGATE_BUCKET_* /
//     LIST_UNKNOWN_FILTER / FILTER_*) surface HERE for model self-correction. The tool is thus
//     deliberately STRICTER than the apply path (which defers deep checks to run time so a
//     metadata change after save is caught honestly): a draft that buffers both applies cleanly
//     AND runs cleanly against current metadata.
//
// No DB-dependent reject is left for Apply (no unique name constraint on sys_reports) — the only
// apply-time divergence is a concurrent metadata change between propose and Apply, which the run
// path re-validates against.
func (e *toolExecutor) proposeReport(ctx context.Context, argsJSON string) string {
	var a struct {
		Name        string                      `json:"name"`
		Entity      string                      `json:"entity"`
		ReportType  string                      `json:"reportType"`
		Columns     []string                    `json:"columns"`
		GroupBy     string                      `json:"groupBy"`
		Metric      string                      `json:"metric"`
		Field       string                      `json:"field"`
		Bucket      string                      `json:"bucket"`
		Filters     []DraftReportFilter         `json:"filters"`
		SubGroups   []services.ReportGroupInput `json:"subGroups"`
		ColumnGroup *services.ReportGroupInput  `json:"columnGroup"`
		Having      *services.ReportHavingInput `json:"having"`
		// Joins is decoded ONLY to detect + strip it (REPORT-JOIN v1 policy, spec
		// ../specs/025-report-join/spec.md §5): a cross-object join is a modeling decision for the human
		// builder. It is NOT in the tool schema and NOT in DraftReportConfig, so it can never
		// reach the draft — this field only lets the tool tell the model to stop emitting it
		// (models do not always honor schemas; the M7 outbound_webhook double-guard precedent).
		Joins []json.RawMessage `json:"joins"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	if strings.TrimSpace(a.Name) == "" {
		return toolErr("REPORT_NAME_REQUIRED: name báo cáo là bắt buộc")
	}
	if len(a.Name) > maxReportNameBytes {
		return toolErr("REPORT_NAME_TOO_LONG: name vượt %d byte (cột sys_reports.name)", maxReportNameBytes)
	}

	ent, err := e.reader.GetEntityByName(ctx, e.tenantID, a.Entity)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại trong tenant hiện tại — nếu nó mới nằm trong draft, người duyệt phải Apply entity đó trước", a.Entity)
		}
		return toolErr("PROPOSE_REPORT_FAILED: %v", err)
	}
	liveFields, err := e.reader.ListFields(ctx, e.tenantID, ent.ID)
	if err != nil {
		return toolErr("PROPOSE_REPORT_FAILED: %v", err)
	}
	// Non-nil even when the entity has no fields — ValidateReportConfig's deep mode is armed if
	// and only if liveFields != nil (a nil slice would silently downgrade to shape-only).
	if liveFields == nil {
		liveFields = []metadata.FieldDefinition{}
	}

	cfg := services.ReportConfig{
		Columns:     a.Columns,
		GroupBy:     a.GroupBy,
		Metric:      a.Metric,
		Field:       a.Field,
		Bucket:      a.Bucket,
		SubGroups:   a.SubGroups,
		ColumnGroup: a.ColumnGroup,
		Having:      a.Having,
	}
	for _, f := range a.Filters {
		cfg.Filters = append(cfg.Filters, services.ReportFilterInput{Field: f.Field, Op: f.Op, Values: f.Values})
	}
	typ, configJSON, verr := services.ValidateReportConfig(a.ReportType, cfg, liveFields)
	if code, msg, ok := validationOf(verr); !ok {
		// A grid-axis error names its axis (details.axis) — the tool error is a flat string, so
		// carry it in the text for the model's self-correction.
		var ve *services.ValidationError
		if errors.As(verr, &ve) {
			if axis, has := ve.Details["axis"].(string); has {
				msg += " (trục: " + axis + ")"
			}
		}
		return toolErr("%s: %s", code, msg)
	}
	// The grid keys ride the validator's NORMALIZED config (bucket/op/on lowercased, field trimmed)
	// so the draft persists exactly what Apply would store.
	var norm services.ReportConfig
	if err := json.Unmarshal(configJSON, &norm); err != nil {
		return toolErr("PROPOSE_REPORT_FAILED: %v", err)
	}

	dr := &DraftReport{
		Name:       a.Name,
		EntityName: ent.Name,    // canonical resolved name, same as DraftLayout/DraftWorkflow
		Type:       string(typ), // normalized (lowercase-trimmed) by the validator
		Config: DraftReportConfig{
			Columns: a.Columns,
			Filters: a.Filters,
			GroupBy: a.GroupBy,
			Metric:  strings.ToLower(strings.TrimSpace(a.Metric)),
			Field:   a.Field,
			Bucket:  strings.ToLower(strings.TrimSpace(a.Bucket)),

			SubGroups:   norm.SubGroups,
			ColumnGroup: norm.ColumnGroup,
			Having:      norm.Having,
		},
		IsActive: true,
	}
	change, errStr := e.reportChange(ctx, dr)
	if errStr != "" {
		return errStr
	}
	e.draft = append(e.draft, DraftItem{Kind: "report", Report: dr, Change: change})
	out := map[string]any{"buffered": fmt.Sprintf("báo cáo %s %q trên %q", typ, a.Name, ent.Name)}
	if len(a.Joins) > 0 {
		// Stripped, not rejected: the rest of the draft is valid and buffers; the note steers
		// the model (and the reviewing human) instead of failing the whole proposal.
		out["note"] = "joins đã bị bỏ qua: join propose chưa hỗ trợ; dựng thủ công trong Report Builder"
	}
	return toolOK(out)
}

// maxAppNavEntities caps a proposed app's nav list. 40 mirrors snapshot_model's per-entity field
// cap (maxSnapshotFieldsPerEntity) — the same "bounded tool surface" precedent: an app is a
// curated sidebar lens, and each extra name costs one tenant-scoped registry lookup here, so an
// unbounded list would be both bad UX to apply and an amplification lever on the reader.
const maxAppNavEntities = 40

// DraftApp is a proposed App container — Apps Phase 3 (../specs/010-app-container/spec.md §7). It maps
// onto POST /admin/apps {name, label, icon?, color?, description?} + PUT /admin/apps/:id/nav
// (names resolved to ids at apply): NavEntities carries entity NAMES in display order, and the
// FE resolves each name to its sys_entities id via /metadata/entities when the human clicks
// Apply — the agent never writes sys_apps/sys_app_nav_items itself. Apply order is entities
// FIRST, then the app (a nav name that is still only a draft has no id to resolve; a name
// missing at apply surfaces as APP_NAV_REF_INVALID per-name for the human to fix — HITL).
type DraftApp struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Icon        string   `json:"icon,omitempty"`
	Color       string   `json:"color,omitempty"`
	Description string   `json:"description,omitempty"`
	NavEntities []string `json:"navEntities"`
}

// proposeApp validates a proposed App with config-time PARITY to the apply path
// (POST /admin/apps → AppService.CreateApp) and buffers it as DraftItem{Kind:"app"}. NOTHING is
// written — the human applies via the gated admin API (HITL).
//
// Parity here is STRUCTURAL, not mirrored (the M6/M7/M8 wrapper pattern):
// services.ValidateAppConfig is the SAME single body CreateApp runs for the app's identity
// fields — slug shape (APP_NAME_INVALID), label required (APP_LABEL_REQUIRED), column-width
// caps (APP_FIELD_TOO_LONG) — so an invalid config comes back to the model as the REAL
// ValidationError code for self-correction in-chat. The nav checks on top are TOOL-SIDE rules
// (the apply path validates nav by entity ID at PUT /admin/apps/:id/nav, not by name):
//  1. navEntities non-empty (APP_NAV_ENTITIES_REQUIRED — an app with an empty sidebar is never
//     what the user asked for) and ≤ maxAppNavEntities raw names (APP_NAV_TOO_MANY, checked
//     BEFORE resolution so an oversized list never buys N reader lookups).
//  2. every name resolves in (tenant registry) ∪ (entities ALREADY DRAFTED this turn — a scan of
//     e.draft for Kind=="entity", which is what lets ONE turn propose entities AND the app that
//     wraps them). Registry hits store the CANONICAL resolved name (the F2 ci-collation lesson);
//     draft hits are exact (ValidateCreateEntity already pinned their shape). Unknown →
//     ENTITY_NOT_FOUND. reader is tenant-scoped: the model can never reference another tenant's
//     entities.
//  3. duplicates are DEDUPED silently (first occurrence wins, order preserved) rather than
//     rejected: the apply path's ReplaceNav rejects duplicate (type, ref) pairs, so a deduped
//     draft is exactly what keeps Apply clean — bouncing the whole call over a harmless repeat
//     would just burn a self-correction step.
//
// DB-dependent rejects left for Apply (same stance as propose_entity's duplicate name):
// 409 APP_ALREADY_EXISTS — the UNIQUE(tenant_id, name) slug index.
func (e *toolExecutor) proposeApp(ctx context.Context, argsJSON string) string {
	var a struct {
		Name        string   `json:"name"`
		Label       string   `json:"label"`
		Icon        string   `json:"icon"`
		Color       string   `json:"color"`
		Description string   `json:"description"`
		NavEntities []string `json:"navEntities"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	if code, msg, ok := validationOf(services.ValidateAppConfig(a.Name, a.Label, a.Icon, a.Color)); !ok {
		return toolErr("%s: %s", code, msg)
	}

	if len(a.NavEntities) == 0 {
		return toolErr("APP_NAV_ENTITIES_REQUIRED: navEntities cần ít nhất 1 tên entity — app không có nav thì sidebar trống")
	}
	if len(a.NavEntities) > maxAppNavEntities {
		return toolErr("APP_NAV_TOO_MANY: navEntities có %d tên, vượt trần %d — app là lens điều hướng, chọn các entity chính", len(a.NavEntities), maxAppNavEntities)
	}

	// Entity names already buffered as drafts THIS turn — the "propose entities then the app that
	// wraps them in ONE turn" path (shared draftedEntities scan).
	drafted := e.draftedEntities()

	seen := make(map[string]struct{}, len(a.NavEntities))
	nav := make([]string, 0, len(a.NavEntities))
	for _, raw := range a.NavEntities {
		name := strings.TrimSpace(raw)
		// Draft names are always lowercase (ddl.SanitizeIdentifier), so probe case-insensitively —
		// the registry path below is ci-collation anyway; exact-case here would cost the model a
		// needless self-correction round for "Appointments" vs a same-turn "appointments" draft.
		canonical := strings.ToLower(name)
		if _, ok := drafted[canonical]; !ok {
			ent, err := e.reader.GetEntityByName(ctx, e.tenantID, name)
			if err != nil {
				if errors.Is(err, metadata.ErrEntityNotFound) {
					return toolErr("ENTITY_NOT_FOUND: %q — propose_entity trước hoặc dùng entity có sẵn", name)
				}
				return toolErr("PROPOSE_APP_FAILED: %v", err)
			}
			canonical = ent.Name
		}
		if _, dup := seen[canonical]; dup {
			continue // dedupe, first occurrence wins — see the doc comment
		}
		seen[canonical] = struct{}{}
		nav = append(nav, canonical)
	}

	slug := strings.ToLower(strings.TrimSpace(a.Name)) // normalized exactly as CreateApp persists
	change, errStr := e.appChange(ctx, slug, nav)
	if errStr != "" {
		return errStr
	}
	e.draft = append(e.draft, DraftItem{
		Kind: "app",
		App: &DraftApp{
			Name:        slug,
			Label:       strings.TrimSpace(a.Label),
			Icon:        strings.TrimSpace(a.Icon),
			Color:       strings.TrimSpace(a.Color),
			Description: strings.TrimSpace(a.Description),
			NavEntities: nav,
		},
		Change: change,
	})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("app %q (%d entity trong nav)", slug, len(nav))})
}

// DraftEditField is a proposed edit of ONE existing field — EASE Nước 7 AI-EDIT. It maps onto
// PUT /admin/fields/:id (the human clicks Apply and the FE PUTs the After attributes to FieldID;
// the agent never writes sys_fields itself). Before/After carry the stored vs post-edit values of
// the MUTABLE attributes so the FE renders an old→new diff card; FieldType is echoed once at the
// top level (immutable — display only). Entity/Field carry canonical stored names.
type DraftEditField struct {
	Entity    string          `json:"entity"`
	Field     string          `json:"field"`
	FieldID   string          `json:"fieldId"`
	FieldType string          `json:"fieldType"`
	Before    DraftFieldState `json:"before"`
	After     DraftFieldState `json:"after"`
}

// DraftFieldState is one side (before or after) of an edit-field diff: the mutable attributes of
// a field as PUT /admin/fields/:id understands them. DefaultValue deliberately has NO omitempty —
// "" (clear the default) must stay distinguishable from "absent" on the wire for the FE's Apply
// body.
type DraftFieldState struct {
	Label        string         `json:"label"`
	IsRequired   bool           `json:"isRequired"`
	DefaultValue string         `json:"defaultValue"`
	Description  string         `json:"description,omitempty"`
	Options      map[string]any `json:"options,omitempty"`
}

// DraftDeleteField is a proposed field deletion — maps onto DELETE /admin/fields/:id (soft-delete;
// data JSON preserved per ../specs/040-dynamic-entity-engine/spec.md §4.4). Name/Label/FieldType feed the FE's impact-honest confirm card.
type DraftDeleteField struct {
	Entity    string `json:"entity"`
	Field     string `json:"field"`
	FieldID   string `json:"fieldId"`
	Label     string `json:"label"`
	FieldType string `json:"fieldType"`
}

// DraftDeleteEntity is a proposed entity deletion — maps onto DELETE /admin/entities/:name
// (soft-delete: the entity disappears AND the tenant's records in it are hidden). Label +
// FieldCount feed the FE's impact-honest confirm card. NO cascade analysis beyond what the live
// registry returns — the gated apply path is the enforcement point (HITL).
type DraftDeleteEntity struct {
	Entity     string `json:"entity"`
	Label      string `json:"label"`
	FieldCount int    `json:"fieldCount"`
}

// DraftRecords is a proposed batch of sample records for ONE entity — EASE Nước 7
// FINISH-THE-BUILD. Rows are the RAW proposed payloads (what the model suggested, not the
// normalized/defaulted store shape): the FE renders them as a table preview and applies via
// POST /:entity/bulk {"records": rows}, whose engine re-runs the SAME ValidateRecord per row
// plus the DB-dependent checks (relation existence, unique) that propose cannot see.
type DraftRecords struct {
	Entity string           `json:"entity"`
	Rows   []map[string]any `json:"rows"`
}

// maxProposeRecordRows caps one propose_records call. 20 keeps a sample-data draft a bounded
// prompt/FE cost (the bulk apply path itself accepts more; sample data needs no more than this —
// the tool is for "first screen isn't empty", not migration; use Import for real datasets).
const maxProposeRecordRows = 20

// editFieldChanges is the model's `changes` argument shape for propose_edit_field. Pointers
// distinguish "omitted, keep stored" from an explicit set (the PUT body's apply-when-non-nil
// semantics). Name and FieldType are decoded ONLY to reject immutable-axis edits honestly —
// they are not in the tool schema, but models do not always honor schemas (the M7
// outbound_webhook double-guard precedent).
type editFieldChanges struct {
	Label        *string        `json:"label"`
	Description  *string        `json:"description"`
	IsRequired   *bool          `json:"isRequired"`
	DefaultValue *string        `json:"defaultValue"`
	Options      map[string]any `json:"options"`
	Name         *string        `json:"name"`
	FieldType    string         `json:"fieldType"`
}

// proposeEditField validates a proposed field edit with config-time PARITY to the apply path
// (PUT /admin/fields/:id → FieldService.EditField) and buffers it as DraftItem{Kind:"edit_field"}.
// NOTHING is written — the human applies via the gated admin API (HITL).
//
// Parity here is STRUCTURAL, not mirrored (the M6/M7 wrapper pattern):
// services.ValidateEditFieldChanges is the SAME single body EditField runs pre-DDL — fieldType
// immutability (FIELD_TYPE_IMMUTABLE), ValidateEditField, per-type options config + flag guards,
// formula sibling refs (FIELD_FORMULA_UNKNOWN_REF & co — queue #107, against the field list
// resolveEntityField already read), relation-options immutability
// (FIELD_RELATION_OPTIONS_IMMUTABLE), apply-onto-copy + effective
// default re-check — so an invalid edit comes back to the model as the REAL ValidationError code
// for self-correction in-chat. Tool-side rules on top (no apply-path logic to drift):
//  1. entity + field resolve against the LIVE tenant registry (list_fields path) —
//     ENTITY_NOT_FOUND / FIELD_NOT_FOUND. reader is tenant-scoped: the model can never probe or
//     edit another tenant's fields.
//  2. changes.name is rejected (FIELD_NAME_IMMUTABLE): the apply path has no rename — a name key
//     would be silently dropped by the PUT handler, and a draft that silently drops intent lies.
//  3. an empty changes object is rejected (EDIT_CHANGES_EMPTY) — a no-op edit card is noise.
//
// The buffered draft echoes CURRENT (Before) alongside proposed (After) values — the effective
// copy the validator returns — so the FE renders an old→new diff and the Apply body is exactly
// After. FIELD_REQUIRED_WHILE_LOCKED (queue #142) is judged here too when production wires
// AccessProbes.FieldLocks (queue #319 — services.CheckRequiredWhileLocked, the very body EditField
// runs, so the card names the stranded roles before Apply). One DB-dependent reject is left for
// Apply: FIELD_CURRENCY_CODE_IN_USE (queue #157 — a currency field's configured code changed while
// records still store the old one; EditField counts obj_* rows, which this reader cannot see),
// whose message names the record count.
// Otherwise the only apply-time divergence is a
// concurrent metadata change between propose and Apply, which EditField re-validates against.
func (e *toolExecutor) proposeEditField(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity  string           `json:"entity"`
		Field   string           `json:"field"`
		Changes editFieldChanges `json:"changes"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	if a.Changes.Name != nil {
		return toolErr("FIELD_NAME_IMMUTABLE: không đổi được tên field — tạo field mới (propose_field) rồi đề xuất xoá field cũ (propose_delete_field) nếu cần")
	}
	if a.Changes.Label == nil && a.Changes.Description == nil && a.Changes.IsRequired == nil &&
		a.Changes.DefaultValue == nil && a.Changes.Options == nil && a.Changes.FieldType == "" {
		return toolErr("EDIT_CHANGES_EMPTY: changes không có thuộc tính nào để sửa — truyền ít nhất một trong label/description/isRequired/defaultValue/options")
	}

	ent, stored, siblings, res := e.resolveEntityField(ctx, a.Entity, a.Field)
	if res != "" {
		return res
	}

	in := services.EditFieldInput{
		FieldID:     stored.ID,
		Label:       a.Changes.Label,
		IsRequired:  a.Changes.IsRequired,
		DefaultVal:  a.Changes.DefaultValue,
		Options:     a.Changes.Options,
		FieldType:   a.Changes.FieldType, // non-empty + different from stored → FIELD_TYPE_IMMUTABLE (same body as apply)
		Description: a.Changes.Description,
	}
	var lookupTypes map[string]string
	if stored.FieldType == "formula" {
		var lerr error
		if lookupTypes, lerr = services.ResolveLookupTypes(ctx, e.reader, e.tenantID, ent.Name, siblings); lerr != nil {
			return toolErr("RESOLVE_FIELD_FAILED: %v", lerr)
		}
	}
	effective, storedOpts, verr := services.ValidateEditFieldChanges(*stored, in, siblings, lookupTypes)
	if code, msg, ok := validationOf(verr); !ok {
		return toolErr("%s: %s", code, msg)
	}
	// queue #319: same required-while-locked guard EditField runs (shared predicate), when the
	// reader carries the optional sys_field_acl seam. Tenant comes from the executor (JWT), the
	// entity from the registry-resolved row — never from the model's args.
	if c, ok := e.reader.(AccessProbeCarrier); ok {
		if p := c.AccessProbes(); p != nil && p.FieldLocks != nil {
			lerr := services.CheckRequiredWhileLocked(ctx, p.FieldLocks, e.tenantID,
				func() (string, error) { return ent.Name, nil }, *stored, *effective)
			if code, msg, ok := validationOf(lerr); !ok {
				return toolErr("%s: %s", code, msg)
			}
		}
	}
	// queue #234: same rollup-in-use guard EditField runs.
	if finder := services.RollupAggFinder(e.reader); finder != nil {
		gerr := services.GuardFormulaResultTypeInUse(ctx, finder, e.tenantID, ent.Name, *stored, storedOpts, a.Changes.Options)
		if gerr == nil {
			gerr = services.GuardFormulaBecomesDerived(ctx, finder, e.tenantID, ent.Name, *stored, storedOpts, a.Changes.Options, siblings)
		}
		if code, msg, ok := validationOf(gerr); !ok {
			return toolErr("%s: %s", code, msg)
		}
	}

	// After.Options: the model's full replacement object when supplied, else the stored map —
	// exactly the effective options EditField would persist.
	afterOpts := storedOpts
	if a.Changes.Options != nil {
		afterOpts = a.Changes.Options
	}
	e.draft = append(e.draft, DraftItem{
		Kind: "edit_field",
		EditField: &DraftEditField{
			Entity:    ent.Name,
			Field:     stored.Name,
			FieldID:   stored.ID,
			FieldType: stored.FieldType,
			Before: DraftFieldState{
				Label:        stored.Label,
				IsRequired:   stored.IsRequired,
				DefaultValue: stored.DefaultVal,
				Description:  stored.Description,
				Options:      storedOpts,
			},
			After: DraftFieldState{
				Label:        effective.Label,
				IsRequired:   effective.IsRequired,
				DefaultValue: effective.DefaultVal,
				Description:  effective.Description,
				Options:      afterOpts,
			},
		},
	})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("sửa field %q trên %q", stored.Name, ent.Name)})
}

// proposeDeleteField resolves an existing field against the live tenant registry and buffers a
// deletion proposal as DraftItem{Kind:"delete_field"}. NOTHING is deleted — the human applies via
// DELETE /admin/fields/:id (soft-delete, data preserved per ../specs/040-dynamic-entity-engine/spec.md §4.4). Propose is add-only honest: no
// cascade analysis beyond the registry lookup; the gated apply path enforces the rest (HITL).
func (e *toolExecutor) proposeDeleteField(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity string `json:"entity"`
		Field  string `json:"field"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	ent, stored, _, res := e.resolveEntityField(ctx, a.Entity, a.Field)
	if res != "" {
		return res
	}
	e.draft = append(e.draft, DraftItem{
		Kind: "delete_field",
		DeleteField: &DraftDeleteField{
			Entity:    ent.Name,
			Field:     stored.Name,
			FieldID:   stored.ID,
			Label:     stored.Label,
			FieldType: stored.FieldType,
		},
	})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("xoá field %q trên %q (xoá mềm — dữ liệu cũ được giữ)", stored.Name, ent.Name)})
}

// proposeDeleteEntity resolves an existing entity against the live tenant registry and buffers a
// deletion proposal as DraftItem{Kind:"delete_entity"} carrying label + fieldCount for the FE's
// impact-honest confirm card. NOTHING is deleted — the human applies via DELETE
// /admin/entities/:name (soft-delete; the tenant's records in the entity are hidden).
func (e *toolExecutor) proposeDeleteEntity(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity string `json:"entity"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	ent, err := e.reader.GetEntityByName(ctx, e.tenantID, a.Entity)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại trong tenant hiện tại", a.Entity)
		}
		return toolErr("PROPOSE_DELETE_ENTITY_FAILED: %v", err)
	}
	fields, err := e.reader.ListFields(ctx, e.tenantID, ent.ID)
	if err != nil {
		return toolErr("PROPOSE_DELETE_ENTITY_FAILED: %v", err)
	}
	e.draft = append(e.draft, DraftItem{
		Kind: "delete_entity",
		DeleteEntity: &DraftDeleteEntity{
			Entity:     ent.Name,
			Label:      ent.Label,
			FieldCount: len(fields),
		},
	})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("xoá entity %q (%s, %d field) — xoá mềm, record của tenant sẽ bị ẩn", ent.Name, ent.Label, len(fields))})
}

// proposeRecords validates a batch of sample-record payloads with config-time PARITY to the apply
// path (POST /:entity/bulk → RecordService.CreateBulk, which runs services.ValidateRecord per row)
// and buffers them as DraftItem{Kind:"records"}. NOTHING is written — the human applies via the
// gated bulk API (HITL).
//
// Parity here is STRUCTURAL, not mirrored: services.ValidateRecord is the SAME single body the
// record engine runs per row — strict unknown-field reject (RECORD_UNKNOWN_FIELD),
// computed/server-assigned reject (RECORD_FORMULA_READONLY / RECORD_LOOKUP_READONLY /
// RECORD_ROLLUP_READONLY / RECORD_AUTONUMBER_READONLY — computedReadonlyCode parity), required /
// type / picklist / range / pattern checks — over specs built by services.FieldSpecsOf, the SAME
// conversion the engine's resolve() runs. A bad row surfaces to the model with its 1-based row
// number for self-correction. Tool-side rules on top:
//  1. rows non-empty (RECORDS_ROWS_REQUIRED) and ≤ maxProposeRecordRows (RECORDS_TOO_MANY),
//     checked BEFORE any registry read.
//  2. the entity resolves in (live tenant registry) ∪ (entities drafted THIS turn — the same
//     e.draft scan propose_relationship/propose_app run). A drafted entity gets SHAPE-ONLY specs
//     from its DraftField definitions (name/type/required/options — validated by
//     ValidateCreateEntity when buffered); live entities use the full stored definitions.
//
// ENG-SAMPLE-REF: a `relation` field's value — or the `id` inside a `polymorphic_relation`
// {type, id} value (queue #140) — may be a symbolic "@name": a reference to another
// row's "$ref" declaration — so a REQUIRED relation no longer forces this tool to skip an entity
// (the description used to say "leave it blank"). For a polymorphic id the declaring row must
// also be a record of the value's {type} (checked here against the earlier draft's entity, and
// again at apply). "$ref" is a RESERVED row key (never a real
// field — SanitizeIdentifier never allows "$"); it is stripped from the copy handed to
// ValidateRecord (below) so it is never RECORD_UNKNOWN_FIELD, but kept (mangled) in the BUFFERED
// row, because the apply-time engine (services.RecordService.CreateBulk /
// record_ref_registry.go) needs it to register the real id once the row is actually persisted.
// Every "@name" token is mangled with this turn's tag (turnTag) before buffering, so it only ever
// matches a "$ref" from an EARLIER propose_records call THIS SAME turn — see
// persona_nexus_builder.md for the ordering rule this implies (propose the parent's records
// BEFORE the child's). Two things are rejected HERE, before any draft is even created, because
// apply time structurally cannot tell them apart from an ordinary typo:
//   - RECORD_REF_CYCLE: a same-call "$ref" dependency cycle (row A's ref used by row B AND row
//     B's ref used by row A, directly or transitively) — unsatisfiable in ANY apply order, so it
//     is caught here where the WHOLE graph is visible in one process, never at apply (KHÔNG treo,
//     KHÔNG apply nửa chừng).
//   - a same-call, NON-cyclic reference to a sibling row's "$ref" — also always rejected (with
//     RECORD_REF_UNRESOLVED, the SAME code apply time itself uses for a genuinely-dangling token):
//     the registry only registers a ref AFTER its row is persisted, and every row in THIS call is
//     still unpersisted when apply resolves refs, so a same-call sibling reference could never
//     resolve either way — letting it through here would buffer a draft guaranteed to fail later.
//
// NOT mirrored (DB-dependent, surfaces at APPLY exactly like propose_entity's duplicate-name):
// relation/polymorphic existence probes, unique collisions, field-ACL, and whether a
// cross-turn/cross-call "@ref" ACTUALLY resolves (that is the registry's job, authoritatively, at
// apply time). Rows are buffered RAW (refs mangled in place) — the apply path recomputes
// clean/normalized payloads itself.
func (e *toolExecutor) proposeRecords(ctx context.Context, argsJSON string) string {
	var a struct {
		Entity string           `json:"entity"`
		Rows   []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	if len(a.Rows) == 0 {
		return toolErr("RECORDS_ROWS_REQUIRED: rows rỗng — cần ít nhất 1 record mẫu")
	}
	if len(a.Rows) > maxProposeRecordRows {
		return toolErr("RECORDS_TOO_MANY: %d dòng, vượt trần %d — dữ liệu mẫu chỉ cần vài dòng; dataset thật dùng Import", len(a.Rows), maxProposeRecordRows)
	}

	entityName, specs, res := e.recordSpecsFor(ctx, a.Entity)
	if res != "" {
		return res
	}

	// declaredThisCall: raw (un-mangled, "@"-stripped) ref name -> row index, for every row that
	// declares itself via "$ref" in THIS call. Used ONLY for same-call cycle/sibling detection
	// below — never to "resolve" a reference (see doc comment: a same-call sibling can never
	// resolve at apply time either).
	declaredThisCall := map[string]int{}
	for i, row := range a.Rows {
		if raw, ok := row["$ref"].(string); ok {
			if name := strings.TrimSpace(strings.TrimPrefix(raw, "@")); name != "" {
				declaredThisCall[name] = i
			}
		}
	}
	if cyc := sameCallRefCycle(a.Rows, specs, declaredThisCall); len(cyc) > 0 {
		return toolErr("RECORD_REF_CYCLE: vòng lặp phụ thuộc @ref giữa các dòng %s trong CÙNG một lần gọi propose_records — mỗi dòng chỉ tham chiếu được ref đã \"$ref\" ở một propose_records TRƯỚC, không tham chiếu vòng trong cùng lần gọi này", rowNumbersCSV(cyc))
	}

	priorRefs := e.priorRecordRefs() // mangled tokens declared by EARLIER propose_records calls this turn

	for i, row := range a.Rows {
		// Strip "$ref" from the copy ValidateRecord sees — it is not a real field.
		checkRow := row
		if _, hasRef := row["$ref"]; hasRef {
			checkRow = make(map[string]any, len(row))
			for k, v := range row {
				if k != "$ref" {
					checkRow[k] = v
				}
			}
		}
		if _, err := services.ValidateRecord(specs, checkRow); err != nil {
			code, msg, _ := validationOf(err)
			return toolErr("%s: dòng %d: %s", code, i+1, msg)
		}

		// Resolve/mangle every relation / polymorphic "@name" reference.
		for _, ref := range recordRefsOf(row, specs) {
			name := strings.TrimPrefix(ref.raw, "@")
			mangled := mangleRef(e.turnTag, name)
			if entity, ok := priorRefs[mangled]; ok {
				if ref.typ != "" && ref.typ != entity {
					return toolErr("RECORD_REF_UNRESOLVED: dòng %d: field %q tham chiếu %q — ref này là record của entity %q, không phải %q; đặt type đúng entity của record đó", i+1, ref.field, ref.raw, entity, ref.typ)
				}
				ref.set(mangled)
				continue
			}
			if _, sameCall := declaredThisCall[name]; sameCall {
				return toolErr("RECORD_REF_UNRESOLVED: dòng %d: field %q tham chiếu %q — ref này \"$ref\" TRONG CÙNG lần gọi propose_records này, không dùng được; gọi propose_records riêng cho dữ liệu CHA trước, rồi propose_records cho dữ liệu CON tham chiếu ref đó", i+1, ref.field, ref.raw)
			}
			return toolErr("RECORD_REF_UNRESOLVED: dòng %d: field %q tham chiếu %q — chưa có propose_records nào ở lượt chat này khai \"$ref\" đó; propose_records cho entity cha TRƯỚC (kèm \"$ref\"), rồi mới propose_records cho entity con tham chiếu nó", i+1, ref.field, ref.raw)
		}
		// Mangle this row's OWN "$ref" declaration last (after every check above ran on the raw form).
		if raw, hasRef := row["$ref"].(string); hasRef && strings.TrimSpace(raw) != "" {
			row["$ref"] = mangleRef(e.turnTag, strings.TrimPrefix(raw, "@"))
		}
	}

	e.draft = append(e.draft, DraftItem{
		Kind:    "records",
		Records: &DraftRecords{Entity: entityName, Rows: a.Rows},
	})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("%d record mẫu cho %q", len(a.Rows), entityName)})
}

// mangleRef namespaces a raw ref name (no leading "@") with tag so it can only ever match a
// "$ref"/"@ref" minted in the SAME turn (empty tag — unit tests that build toolExecutor directly
// — degrades to the raw name, unchanged). Deterministic: the same (tag, name) always mangles to
// the same token, so a declaring row and a referencing row in different propose_records calls of
// the same turn agree WITHOUT any shared lookup table.
func mangleRef(tag, name string) string {
	if tag == "" {
		return "@" + name
	}
	return "@" + tag + ":" + name
}

// priorRecordRefs returns every (already-mangled) "$ref" token declared by a "records" DraftItem
// buffered EARLIER this turn (mirrors draftedEntities' e.draft scan), mapped to the entity of the
// declaring row. This is the set a NEW propose_records call's "@name" references may resolve
// against; the entity lets a polymorphic {type, id:"@name"} be checked against its type.
func (e *toolExecutor) priorRecordRefs() map[string]string {
	out := map[string]string{}
	for i := range e.draft {
		if e.draft[i].Kind != "records" || e.draft[i].Records == nil {
			continue
		}
		for _, row := range e.draft[i].Records.Rows {
			if raw, ok := row["$ref"].(string); ok && strings.TrimSpace(raw) != "" {
				out[raw] = e.draft[i].Records.Entity
			}
		}
	}
	return out
}

// recordRef is one "@name" reference carried by a propose_records row: a `relation` value, or
// the id inside a `polymorphic_relation` {type, id} value (typ set — the ref must be a record of
// that entity). set rewrites the token in place (the mangled form) in the row itself.
type recordRef struct {
	field, raw, typ string
	set             func(string)
}

// recordRefsOf lists every "@"-prefixed reference in row, in spec order. The ONE place that
// knows which values can hold a ref — proposeRecords (resolve/mangle) and sameCallRefCycle (the
// dependency graph) both read it, so the two can never disagree about what counts as a ref.
func recordRefsOf(row map[string]any, specs []services.FieldSpec) []recordRef {
	var out []recordRef
	for _, f := range specs {
		switch f.FieldType {
		case "relation":
			if raw, ok := row[f.Name].(string); ok && strings.HasPrefix(raw, "@") {
				name := f.Name
				out = append(out, recordRef{field: name, raw: raw, set: func(v string) { row[name] = v }})
			}
		case "polymorphic_relation":
			m, ok := row[f.Name].(map[string]any)
			if !ok {
				continue
			}
			if raw, ok := m["id"].(string); ok && strings.HasPrefix(raw, "@") {
				typ, _ := m["type"].(string)
				out = append(out, recordRef{field: f.Name, raw: raw, typ: typ, set: func(v string) { m["id"] = v }})
			}
		}
	}
	return out
}

// sameCallRefCycle detects a "$ref" dependency cycle AMONG the rows of a single propose_records
// call: row i depends on row j when one of i's relation / polymorphic values references j's "$ref". Only
// same-call edges are built (declaredThisCall is scoped to THIS call) — a cross-call reference can
// never be part of a cycle by construction (propose_records calls happen strictly in sequence, so
// an EARLIER call's rows can never depend on a LATER call's). Returns the 0-based row indices
// forming a cycle (nil when acyclic). Standard white/gray/black DFS — rows are capped at
// maxProposeRecordRows (20), so this is always O(1)-ish.
func sameCallRefCycle(rows []map[string]any, specs []services.FieldSpec, declaredThisCall map[string]int) []int {
	edges := make([][]int, len(rows))
	for i, row := range rows {
		for _, ref := range recordRefsOf(row, specs) {
			if j, ok := declaredThisCall[strings.TrimPrefix(ref.raw, "@")]; ok && j != i {
				edges[i] = append(edges[i], j)
			}
		}
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make([]int, len(rows))
	var path, found []int
	var dfs func(u int) bool
	dfs = func(u int) bool {
		color[u] = gray
		path = append(path, u)
		for _, v := range edges[u] {
			if color[v] == gray {
				for i, p := range path {
					if p == v {
						found = append([]int{}, path[i:]...)
						break
					}
				}
				return true
			}
			if color[v] == white && dfs(v) {
				return true
			}
		}
		path = path[:len(path)-1]
		color[u] = black
		return false
	}
	for i := range rows {
		if color[i] == white && dfs(i) {
			return found
		}
	}
	return nil
}

// rowNumbersCSV renders 0-based row indices as a 1-based, comma-joined list for a model-facing
// error message (e.g. [0,2] -> "1, 3").
func rowNumbersCSV(rows []int) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = strconv.Itoa(r + 1)
	}
	return strings.Join(parts, ", ")
}

// recordSpecsFor resolves the validation specs for propose_records: the LIVE registry first
// (canonical name + full stored field definitions via services.FieldSpecsOf), falling back to an
// entity drafted THIS turn (shape-only specs from its DraftField list — draft-buffer awareness,
// same scan as propose_relationship/propose_app). Returns ("", nil, toolErr-json) on failure.
func (e *toolExecutor) recordSpecsFor(ctx context.Context, entity string) (string, []services.FieldSpec, string) {
	ent, err := e.reader.GetEntityByName(ctx, e.tenantID, entity)
	if err == nil {
		fields, ferr := e.reader.ListFields(ctx, e.tenantID, ent.ID)
		if ferr != nil {
			return "", nil, toolErr("PROPOSE_RECORDS_FAILED: %v", ferr)
		}
		specs, serr := services.FieldSpecsOf(fields)
		if serr != nil {
			return "", nil, toolErr("PROPOSE_RECORDS_FAILED: %v", serr)
		}
		return ent.Name, specs, ""
	}
	if !errors.Is(err, metadata.ErrEntityNotFound) {
		return "", nil, toolErr("PROPOSE_RECORDS_FAILED: %v", err)
	}

	// Not live — accept an entity buffered as a draft THIS turn (apply order: entity first, then
	// the dependent draft; the FE applies in dependency order). Draft names are canonical
	// lowercase already (ValidateCreateEntity → SanitizeIdentifier), so probe case-insensitively
	// like proposeApp (shared draftedEntities scan).
	canonical := strings.ToLower(strings.TrimSpace(entity))
	if d, ok := e.draftedEntities()[canonical]; ok {
		specs := make([]services.FieldSpec, 0, len(d.Fields))
		for _, f := range d.Fields {
			// IsIndexed mirrors what FieldSpecsOf derives from a LIVE definition's options, so a
			// drafted indexed field validates (e.g. as a workqueue sort target) exactly as it
			// will once applied. IsUnique has no draft-side source — propose_entity fields carry
			// no unique flag — so it honestly stays false (shape-only specs).
			indexed, _ := f.Options["indexed"].(bool)
			specs = append(specs, services.FieldSpec{
				Name:       f.Name,
				FieldType:  f.FieldType,
				IsRequired: f.Required,
				IsIndexed:  indexed,
				Options:    f.Options,
			})
		}
		return d.Name, specs, ""
	}
	return "", nil, toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại và cũng chưa được propose_entity trong lượt này — propose_entity trước rồi propose_records trong CÙNG lượt", entity)
}

// resolveEntityField resolves (entity, field) by NAME against the live tenant registry — the
// shared front door of propose_edit_field / propose_delete_field. Field match is
// case-insensitive on the trimmed input (stored names are canonical lowercase; exact-case would
// cost the model a needless self-correction round). It also returns the entity's field list —
// the siblings a formula edit's refs are checked against (queue #107). Returns
// (nil, nil, nil, toolErr-json) on failure.
func (e *toolExecutor) resolveEntityField(ctx context.Context, entity, field string) (*metadata.EntityDefinition, *metadata.FieldDefinition, []metadata.FieldDefinition, string) {
	ent, err := e.reader.GetEntityByName(ctx, e.tenantID, entity)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, nil, nil, toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại trong tenant hiện tại", entity)
		}
		return nil, nil, nil, toolErr("RESOLVE_FIELD_FAILED: %v", err)
	}
	fields, err := e.reader.ListFields(ctx, e.tenantID, ent.ID)
	if err != nil {
		return nil, nil, nil, toolErr("RESOLVE_FIELD_FAILED: %v", err)
	}
	want := strings.ToLower(strings.TrimSpace(field))
	for i := range fields {
		if fields[i].Name == want {
			return ent, &fields[i], fields, ""
		}
	}
	return nil, nil, nil, toolErr("FIELD_NOT_FOUND: field %q không có trên entity %q — soi list_fields để thấy field đang tồn tại", field, ent.Name)
}

// ---------------------------------------------------------------------------------------------
// AI-MODULES (Nước 7 POWER): propose_approval / propose_dashboard / propose_workqueue drafts +
// the explain_access / list_packs read tools. Every propose below is an ORCHESTRATION of an
// already-gated admin API (HITL — the model buffers, a human applies) with validator-parity:
// the tool runs the SAME exported function body the apply path runs.
// ---------------------------------------------------------------------------------------------

// draftedEntities returns the entities already buffered as drafts THIS turn, keyed by their
// canonical (lowercase — ValidateCreateEntity → SanitizeIdentifier) name. It is the ONE shared
// scan behind every "entity exists OR was drafted this turn" tool rule
// (propose_relationship/propose_app/propose_records/propose_approval/propose_dashboard/
// propose_workqueue).
func (e *toolExecutor) draftedEntities() map[string]*DraftEntity {
	out := make(map[string]*DraftEntity)
	for i := range e.draft {
		if e.draft[i].Kind == "entity" && e.draft[i].Entity != nil {
			out[e.draft[i].Entity.Name] = e.draft[i].Entity
		}
	}
	return out
}

// resolveEntityLiveOrDrafted resolves an entity reference for the draft-aware tools: the LIVE
// tenant registry first (returning the CANONICAL stored name — the F2 ci-collation lesson), then
// the same-turn draft buffer (shape-only; apply order is entity first, so referencing it is
// structurally sound). Returns ("", errJSON) when neither resolves.
func (e *toolExecutor) resolveEntityLiveOrDrafted(ctx context.Context, raw string) (string, string) {
	ent, err := e.reader.GetEntityByName(ctx, e.tenantID, raw)
	if err == nil {
		return ent.Name, ""
	}
	if !errors.Is(err, metadata.ErrEntityNotFound) {
		return "", toolErr("RESOLVE_ENTITY_FAILED: %v", err)
	}
	canonical := strings.ToLower(strings.TrimSpace(raw))
	if _, ok := e.draftedEntities()[canonical]; ok {
		return canonical, ""
	}
	return "", toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại và cũng chưa được propose_entity trong lượt này — propose_entity trước trong CÙNG lượt", raw)
}

// DraftApproval is a proposed multi-step approval process — AI-MODULES. FE APPLY CONTRACT: it
// maps onto the body of POST /admin/approval-processes
//
//	{name, entityName, trigger, criteria?: {match, rules:[{field, op, value?}]},
//	 steps: [{id?, name?, approverType, approverId, slaSeconds?, slaClock?}], isActive}
//
// with ONE resolution step: each step here carries approverRef — a role NAME (approverType
// "role") or a user EMAIL (approverType "user"), because the model can know names/emails but
// never tenant UUIDs — and the FE resolves it to the concrete id (GET /admin/roles /
// GET /admin/users) and sends it as approverId when the human clicks Apply. An unresolvable ref
// surfaces at Apply for the human to fix (HITL), exactly like DraftApp's nav names. Criteria
// reuses the workflow condition shape — the approval service validates it with the SAME
// workflow-condition vocabulary (validation-parity B6). EntityName carries the canonical stored
// entity name (or the canonical draft name when proposed this turn — apply order: entity first).
// IsActive is always true; Trigger is normalized (after_create/after_update).
type DraftApproval struct {
	Name       string                  `json:"name"`
	EntityName string                  `json:"entityName"`
	Trigger    string                  `json:"trigger"`
	Criteria   *DraftWorkflowCondition `json:"criteria,omitempty"` // nil = match-all
	Steps      []DraftApprovalStep     `json:"steps"`
	IsActive   bool                    `json:"isActive"`
}

// DraftApprovalStep is one proposed approval step. ApproverRef is the human-readable reference
// the FE resolves to approverId at Apply (see DraftApproval).
type DraftApprovalStep struct {
	ID           string `json:"id,omitempty"` // stable step id (specs/043 S0) — model-supplied or filled like the apply path
	Name         string `json:"name,omitempty"`
	ApproverType string `json:"approverType"` // "user" | "role"
	ApproverRef  string `json:"approverRef"`  // role NAME or user EMAIL — resolved to approverId at Apply
	SLASeconds   int    `json:"slaSeconds,omitempty"`
	SLAClock     string `json:"slaClock,omitempty"` // "business" | "wall" (specs/043 S1); "" = business
	// Approvers + Completion: a step held by several people (specs/043 S4) INSTEAD of
	// approverType/approverRef. The FE resolves each Ref to an id at Apply, like ApproverRef.
	Approvers  []DraftApprover `json:"approvers,omitempty"`
	Completion string          `json:"completion,omitempty"` // "all" | "any" | "quorum"
	// CompletionPercent (1..100) goes with completion "quorum" (specs/043 S6b).
	CompletionPercent int `json:"completionPercent,omitempty"`
	// OnReject: where a rejection of this step goes (specs/043 S8); nil = the rejection is final.
	OnReject *DraftOnReject `json:"onReject,omitempty"`
}

// DraftOnReject is a draft step's answer to a rejection: To "@end" | "@submitter" | the id of an
// EARLIER step; ResumeAt "rejector" | "start" (only with "@submitter").
type DraftOnReject struct {
	To       string `json:"to"`
	ResumeAt string `json:"resumeAt,omitempty"`
}

// DraftApprover is one approver of a parallel draft step: Type "user" | "role", Ref a role NAME or
// user EMAIL.
type DraftApprover struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

// aiApprovalKeys / aiApprovalStepKeys are the closed propose_approval argument shape (the tool
// schema above): anything else is refused with APPROVAL_NODE_KIND_UNSUPPORTED (D-7). The model
// names an approver by approverRef, never by approverId.
var (
	aiApprovalKeys     = []string{"name", "entity", "trigger", "criteria", "steps"}
	aiApprovalStepKeys = []string{"id", "name", "approverType", "approverRef", "slaSeconds", "slaClock", "approvers", "completion", "completionPercent", "onReject"}
)

// proposeApproval validates a proposed approval process with config-time PARITY to the apply
// path (POST /admin/approval-processes → ApprovalService.CreateProcess) and buffers it as
// DraftItem{Kind:"approval"}. NOTHING is written — the human applies via the gated admin API.
//
// Parity is STRUCTURAL, not mirrored: services.ValidateApprovalProcess is the SAME single body
// CreateProcess/UpdateProcess run for name/trigger/criteria/steps (APPROVAL_PROCESS_NAME_REQUIRED
// / APPROVAL_PROCESS_NAME_TOO_LONG / APPROVAL_TRIGGER_INVALID / APPROVAL_CRITERIA_INVALID /
// APPROVAL_STEPS_REQUIRED / APPROVAL_STEP_INVALID), with the step's approverRef standing in for
// approverId (same non-empty rule; the id itself is minted by the FE's Apply-time resolution).
// The entity resolves live-or-drafted-this-turn (the propose_relationship scan) — the apply
// path's own entity check re-runs at Apply. NOT mirrored (Apply/HITL-side): approverRef → id
// resolution (role/user existence), which needs the tenant's role/user tables the model must
// never browse through a propose tool.
func (e *toolExecutor) proposeApproval(ctx context.Context, argsJSON string) string {
	var a struct {
		Name     string                  `json:"name"`
		Entity   string                  `json:"entity"`
		Trigger  string                  `json:"trigger"`
		Criteria *DraftWorkflowCondition `json:"criteria"`
		Steps    []DraftApprovalStep     `json:"steps"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	// D-7 (specs/043): the typed decode above drops any key it does not know. A step or process
	// key the engine does not run (parallel approvers, a graph…) must be REFUSED, or the model
	// tells the user it configured something the draft silently lost. Same code, same service
	// body as POST /admin/approval-processes.
	var raw struct {
		Steps []json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	if code, msg, ok := validationOf(services.UnsupportedApprovalProcessKeys(
		services.UnsupportedApprovalKeys([]byte(argsJSON), aiApprovalKeys...))); !ok {
		return toolErr("%s: %s", code, msg)
	}

	entityName, res := e.resolveEntityLiveOrDrafted(ctx, a.Entity)
	if res != "" {
		return res
	}

	a.Criteria.canonicalizeMatch()
	match := ""
	var rules []services.ApprovalCriteriaRuleInput
	if a.Criteria != nil {
		match = a.Criteria.Match
		for _, r := range a.Criteria.Rules {
			rules = append(rules, services.ApprovalCriteriaRuleInput{Field: r.Field, Op: r.Op, Value: r.Value})
		}
	}
	steps := make([]services.ApprovalStepInput, 0, len(a.Steps))
	for i, st := range a.Steps {
		in := services.ApprovalStepInput{
			ID:           st.ID,
			Name:         st.Name,
			ApproverType: st.ApproverType,
			ApproverID:   st.ApproverRef, // ref stands in for the id — same non-empty/type rules
			SLASeconds:   st.SLASeconds,
			SLAClock:     st.SLAClock,
		}
		for _, ap := range st.Approvers { // refs stand in for ids here too
			in.Approvers = append(in.Approvers, services.ApprovalApproverInput{Type: ap.Type, ID: ap.Ref})
		}
		if c := strings.TrimSpace(st.Completion); c != "" || st.CompletionPercent != 0 { // a lone percent is a mistake, not ignored (B6)
			in.Completion = &services.ApprovalCompletionInput{Mode: c, Percent: st.CompletionPercent}
		}
		if st.OnReject != nil {
			in.OnReject = &services.ApprovalOnRejectInput{To: st.OnReject.To, ResumeAt: st.OnReject.ResumeAt}
		}
		if i < len(raw.Steps) {
			in.Unsupported = services.UnsupportedApprovalKeys(raw.Steps[i], aiApprovalStepKeys...)
		}
		steps = append(steps, in)
	}
	trigger, _, stepsJSON, graphJSON, verr := services.ValidateApprovalChain(a.Name, a.Trigger, match, rules, steps)
	if code, msg, ok := validationOf(verr); !ok {
		return toolErr("%s: %s", code, msg)
	}
	// The step ids exactly as the apply path persists them (explicit kept, missing filled) —
	// read back from the validator's own output rather than re-derived here. A chain with a
	// parallel step is stored as a graph (steps '[]'), its nodes in the same order.
	var persisted []struct {
		ID string `json:"id"`
	}
	if len(graphJSON) > 0 {
		var g struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(graphJSON, &g); err != nil {
			return toolErr("INTERNAL: không đọc lại được mã bước đã chuẩn hoá (%v)", err)
		}
		persisted = g.Nodes
	} else if err := json.Unmarshal(stepsJSON, &persisted); err != nil {
		return toolErr("INTERNAL: không đọc lại được mã bước đã chuẩn hoá (%v)", err)
	}
	if len(persisted) != len(a.Steps) {
		return toolErr("INTERNAL: không đọc lại được mã bước đã chuẩn hoá (%d/%d bước)", len(persisted), len(a.Steps))
	}

	// Buffer normalized step values (trimmed, like the apply path persists them).
	drafted := make([]DraftApprovalStep, 0, len(a.Steps))
	for i, st := range a.Steps {
		ds := DraftApprovalStep{
			ID:           persisted[i].ID,
			Name:         strings.TrimSpace(st.Name),
			ApproverType: strings.TrimSpace(st.ApproverType),
			ApproverRef:  strings.TrimSpace(st.ApproverRef),
			SLASeconds:   st.SLASeconds,
			SLAClock:     strings.TrimSpace(st.SLAClock),
		}
		if len(st.Approvers) > 0 {
			for _, ap := range st.Approvers {
				ds.Approvers = append(ds.Approvers, DraftApprover{Type: strings.TrimSpace(ap.Type), Ref: strings.TrimSpace(ap.Ref)})
			}
			ds.Completion = strings.TrimSpace(st.Completion)
			if ds.Completion == "" {
				ds.Completion = "all" // the default, written down like the apply path does
			}
			ds.CompletionPercent = st.CompletionPercent
		}
		if st.OnReject != nil {
			ds.OnReject = &DraftOnReject{To: strings.TrimSpace(st.OnReject.To), ResumeAt: strings.TrimSpace(st.OnReject.ResumeAt)}
			if ds.OnReject.To == "@submitter" && ds.OnReject.ResumeAt == "" {
				ds.OnReject.ResumeAt = "rejector" // the default, written down like the apply path does
			}
		}
		drafted = append(drafted, ds)
	}
	e.draft = append(e.draft, DraftItem{
		Kind: "approval",
		Approval: &DraftApproval{
			Name:       strings.TrimSpace(a.Name),
			EntityName: entityName,
			Trigger:    string(trigger),
			Criteria:   a.Criteria,
			Steps:      drafted,
			IsActive:   true,
		},
	})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("quy trình phê duyệt %q trên %q (%d bước)", strings.TrimSpace(a.Name), entityName, len(drafted))})
}

// maxDashboardNameRunes mirrors sys_dashboards.name VARCHAR(150) (migration 000024) — a CHARACTER
// budget. CreateDashboard has no length check of its own, so without this guard a long name
// buffers clean and dies as a MySQL 1406 → 500 at Apply (the M5 maxRelationshipNameBytes lesson).
const maxDashboardNameRunes = 150

// maxDashboardWidgets caps one proposed dashboard. A dashboard is a curated board, not a data
// dump — and each widget is one aggregate query per render, so an unbounded list would be both
// bad UX to apply and a load lever (the maxAppNavEntities "bounded tool surface" precedent).
const maxDashboardWidgets = 12

// DraftDashboard is a proposed dashboard — AI-MODULES. FE APPLY CONTRACT: it maps 1:1 onto the
// body of POST /admin/dashboards
//
//	{name, description?, widgets: [{id, title, type, entity, metric, field?, groupBy?, bucket?,
//	 x, y, w, h, filters?: [{field, op?, values?}]}]}
//
// (services.WidgetConfig IS the wire shape — no re-mapping). Widget ids are pre-assigned
// ("w1".."wN"); x/y/w/h are all 0 = the "layout not yet set" sentinel the FE auto-places
// (DASH-WIDGET-SCHEMA) — the model never invents grid coordinates. Widget entities carry
// canonical names (live or drafted this turn — apply order: entities first, dashboard after).
type DraftDashboard struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Widgets     []services.WidgetConfig `json:"widgets"`
}

// proposeDashboard validates a proposed dashboard with config-time PARITY to the apply path
// (POST /admin/dashboards → DashboardService.CreateDashboard) and buffers it as
// DraftItem{Kind:"dashboard"}. NOTHING is written — the human applies via the gated admin API.
//
// Parity is STRUCTURAL, not mirrored: services.ValidateDashboardWidgets is the SAME single body
// CreateDashboard/UpdateDashboard run per widget (WIDGET_TYPE_INVALID / WIDGET_METRIC_INVALID /
// WIDGET_ENTITY_REQUIRED / WIDGET_BUCKET_* / WIDGET_LAYOUT_INVALID), including its in-place
// bucket normalization. Tool-side rules on top (no apply-path logic to drift): name required +
// ≤150 chars (hand-mirrored — CreateDashboard's own check + the column budget), widgets 1..12,
// and every widget entity resolves live-or-drafted-this-turn. Deep semantic checks (field
// numeric? groupBy indexed?) stay at RUN time via the aggregate engine — exactly the apply
// path's honest stance (a per-widget error renders in its tile, it never kills the board).
func (e *toolExecutor) proposeDashboard(ctx context.Context, argsJSON string) string {
	var a struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Widgets     []struct {
			Title   string              `json:"title"`
			Type    string              `json:"type"`
			Entity  string              `json:"entity"`
			Metric  string              `json:"metric"`
			Field   string              `json:"field"`
			GroupBy string              `json:"groupBy"`
			Bucket  string              `json:"bucket"`
			Filters []DraftReportFilter `json:"filters"`
			// Decoded ONLY to detect + strip (same policy as propose_report): joins are a
			// modelling decision for the builder, not something the model proposes.
			Joins []json.RawMessage `json:"joins"`
		} `json:"widgets"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	if strings.TrimSpace(a.Name) == "" {
		return toolErr("DASHBOARD_NAME_REQUIRED: name dashboard là bắt buộc")
	}
	if len([]rune(a.Name)) > maxDashboardNameRunes {
		return toolErr("DASHBOARD_NAME_TOO_LONG: name vượt %d ký tự (cột sys_dashboards.name)", maxDashboardNameRunes)
	}
	if len(a.Widgets) == 0 {
		return toolErr("DASHBOARD_WIDGETS_REQUIRED: widgets cần ít nhất 1 — dashboard trống không có gì để xem")
	}
	if len(a.Widgets) > maxDashboardWidgets {
		return toolErr("DASHBOARD_TOO_MANY_WIDGETS: %d widget, vượt trần %d — chọn các số liệu chính", len(a.Widgets), maxDashboardWidgets)
	}

	wcs := make([]services.WidgetConfig, 0, len(a.Widgets))
	for i, w := range a.Widgets {
		entityName, res := e.resolveEntityLiveOrDrafted(ctx, w.Entity)
		if res != "" {
			return res
		}
		wc := services.WidgetConfig{
			ID:     fmt.Sprintf("w%d", i+1),
			Title:  strings.TrimSpace(w.Title),
			Type:   strings.TrimSpace(w.Type),
			Entity: entityName,
			Metric: strings.TrimSpace(w.Metric),
			Field:  strings.TrimSpace(w.Field),
			// X/Y/W/H stay 0 — the DASH-WIDGET-SCHEMA "layout not yet set" sentinel; the FE
			// auto-places the tile. Grid geometry is a human/FE concern, never model-invented.
			GroupBy: strings.TrimSpace(w.GroupBy),
			Bucket:  w.Bucket,
		}
		for _, f := range w.Filters {
			wc.Filters = append(wc.Filters, services.ReportFilterInput{Field: f.Field, Op: f.Op, Values: f.Values})
		}
		wcs = append(wcs, wc)
	}
	// Same body as Apply; normalizes wcs[i].Bucket in place so the draft carries what persists.
	if _, err := services.ValidateDashboardWidgets(wcs); err != nil {
		code, msg, _ := validationOf(err)
		return toolErr("%s: %s", code, msg)
	}

	e.draft = append(e.draft, DraftItem{
		Kind: "dashboard",
		Dashboard: &DraftDashboard{
			Name:        strings.TrimSpace(a.Name),
			Description: strings.TrimSpace(a.Description),
			Widgets:     wcs,
		},
	})
	out := map[string]any{"buffered": fmt.Sprintf("dashboard %q (%d widget)", strings.TrimSpace(a.Name), len(wcs))}
	for _, w := range a.Widgets {
		if len(w.Joins) > 0 {
			out["note"] = "joins đã bị bỏ qua: join propose chưa hỗ trợ; ghim thẻ ghép từ Report Builder"
			break
		}
	}
	return toolOK(out)
}

// DraftWorkQueue is a proposed work queue — AI-MODULES. FE APPLY CONTRACT: it maps onto the body
// of POST /admin/work-queues
//
//	{name, label, entityName, roleId?, filters?: [{field, op, value}], sortField?, sortOrder,
//	 actionVerb, position?, isActive}
//
// with ONE resolution step: RoleName here is a role NAME ("" = visible to everyone) the FE
// resolves to roleId (GET /admin/roles) when the human clicks Apply — same reason as
// DraftApproval's approverRef (the model can never know tenant UUIDs). Filters carry the exact
// stored wire shape (workqueue.QueueFilter: field/op/value, between/in comma-joined). Position
// is deliberately absent (cosmetic ordering — Apply defaults it). Values are pre-normalized by
// the SAME validator the apply path runs. EntityName is canonical (live or drafted this turn).
// IsActive is always true.
type DraftWorkQueue struct {
	Name       string                  `json:"name"`
	Label      string                  `json:"label"`
	EntityName string                  `json:"entityName"`
	RoleName   string                  `json:"roleName,omitempty"` // resolved to roleId at Apply; "" = everyone
	Filters    []workqueue.QueueFilter `json:"filters,omitempty"`
	SortField  string                  `json:"sortField,omitempty"`
	SortOrder  string                  `json:"sortOrder"`
	ActionVerb string                  `json:"actionVerb"`
	IsActive   bool                    `json:"isActive"`
}

// proposeWorkQueue validates a proposed work queue with config-time PARITY to the apply path
// (POST /admin/work-queues → WorkQueueService.Create) and buffers it as
// DraftItem{Kind:"workqueue"}. NOTHING is written — the human applies via the gated admin API.
//
// Parity is STRUCTURAL, not mirrored: services.ValidateWorkQueueConfig is the SAME single body
// WorkQueueService.validate runs (WORKQUEUE_INVALID / WORKQUEUE_ACTION_INVALID /
// WORKQUEUE_FILTER_INVALID / WORKQUEUE_SORT_NOT_SORTABLE) — normalization, per-filter operator +
// date-token resolution and the RM-09 sort allowlist all run through the runtime's own
// functions. Specs come from recordSpecsFor: the live registry (full stored definitions) or an
// entity drafted THIS turn (shape-only specs — a drafted field is sortable only if its options
// declare indexed; unique flags don't exist on drafts). NOT mirrored (Apply-side): roleName → id
// resolution + the WORKQUEUE_ALREADY_EXISTS unique-name conflict (DB-dependent, same stance as
// propose_entity's duplicate name).
func (e *toolExecutor) proposeWorkQueue(ctx context.Context, argsJSON string) string {
	var a struct {
		Name       string `json:"name"`
		Label      string `json:"label"`
		Entity     string `json:"entity"`
		RoleName   string `json:"roleName"`
		SortField  string `json:"sortField"`
		SortOrder  string `json:"sortOrder"`
		ActionVerb string `json:"actionVerb"`
		Filters    []struct {
			Field string `json:"field"`
			Op    string `json:"op"`
			Value string `json:"value"`
		} `json:"filters"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	entityName, specs, res := e.recordSpecsFor(ctx, a.Entity)
	if res != "" {
		return res
	}

	in := services.WorkQueueInput{
		Name:       a.Name,
		Label:      a.Label,
		EntityName: entityName,
		SortField:  strings.TrimSpace(a.SortField),
		SortOrder:  a.SortOrder,
		ActionVerb: a.ActionVerb,
	}
	for _, f := range a.Filters {
		in.Filters = append(in.Filters, workqueue.QueueFilter{
			Field:    strings.TrimSpace(f.Field),
			Operator: strings.TrimSpace(f.Op),
			Value:    strings.TrimSpace(f.Value),
		})
	}
	// Same body as Apply; specs are already resolved (live or drafted), so the resolver just
	// hands them over — entity existence was settled by recordSpecsFor above.
	verr := services.ValidateWorkQueueConfig(&in, func(string) ([]services.FieldSpec, error) { return specs, nil })
	if code, msg, ok := validationOf(verr); !ok {
		return toolErr("%s: %s", code, msg)
	}

	e.draft = append(e.draft, DraftItem{
		Kind: "workqueue",
		WorkQueue: &DraftWorkQueue{
			Name:       in.Name, // normalized in place by the shared validator
			Label:      in.Label,
			EntityName: in.EntityName,
			RoleName:   strings.TrimSpace(a.RoleName),
			Filters:    in.Filters,
			SortField:  in.SortField,
			SortOrder:  in.SortOrder,
			ActionVerb: in.ActionVerb,
			IsActive:   true,
		},
	})
	return toolOK(map[string]any{"buffered": fmt.Sprintf("hàng đợi %q trên %q (%d filter)", in.Name, in.EntityName, len(in.Filters))})
}

// ---------------------------------------------------------------------------------------------
// explain_access — the read-only ACL decision-chain probe.
// ---------------------------------------------------------------------------------------------

// accessUserLookup / accessRoleLookup / accessAuthorizer / accessFieldACLLoader /
// accessRecordGetter are consumer-side slices of EXISTING services — signatures match
// *auth.UserRepository, *auth.RoleRepository, *services.ACLService, *auth.FieldACLRepository and
// *services.RecordService exactly, so production wiring is pass-the-service, no adapter code.
type accessUserLookup interface {
	GetUserByEmail(ctx context.Context, tenantID, email string) (*auth.User, error)
	GetUserByID(ctx context.Context, tenantID, id string) (*auth.User, error)
}

type accessRoleLookup interface {
	GetRoleByID(ctx context.Context, tenantID, id string) (*auth.Role, error)
}

type accessAuthorizer interface {
	Authorize(ctx context.Context, tenantID, roleID, entity, action string) (bool, string, error)
}

type accessFieldACLLoader interface {
	LoadFieldACLForEntity(ctx context.Context, tenantID, roleID, entity string) ([]auth.FieldACL, error)
}

type accessRecordGetter interface {
	Get(ctx context.Context, scope services.RecordScope, tenantID, entityName, id string, expand []string, rollup []string) (*dynrepo.Record, error)
}

// AccessProbes bundles the existing tenant-scoped services explain_access composes. Every probe
// call below pins tenantID from the executor (the verified JWT) — the model can never name a
// tenant, and the row-level probe enters through RecordService.Get (NEVER dynrepo directly), so
// the A5 row-scope engine itself answers "would this user see this record", instead of the tool
// re-implementing (and drifting from) the scoping rules.
type AccessProbes struct {
	Users    accessUserLookup     // *auth.UserRepository
	Roles    accessRoleLookup     // *auth.RoleRepository
	ACL      accessAuthorizer     // *services.ACLService
	FieldACL accessFieldACLLoader // *auth.FieldACLRepository
	Records  accessRecordGetter   // *services.RecordService
	// FieldLocks is OPTIONAL and not part of complete(): it arms propose_edit_field's lock parity
	// (queue #319) and is nil-safe — unwired, the proposal skips the check and Apply still enforces.
	FieldLocks services.FieldRestrictionLister // *auth.FieldACLRepository
}

// complete reports whether every probe is wired — explain_access degrades honestly when not.
func (p *AccessProbes) complete() bool {
	return p != nil && p.Users != nil && p.Roles != nil && p.ACL != nil && p.FieldACL != nil && p.Records != nil
}

// AccessProbeCarrier is the OPTIONAL upgrade interface of MetadataReader that arms
// explain_access. The builder agent constructs its tool executor from the reader alone, so the
// probes ride ON the reader (the http.Flusher upgrade pattern): production wires
// ReaderWithAccessProbes(metaRepo, probes) into NewAIBuilderAgent — zero agent-loop changes —
// and a plain reader simply leaves the tool answering "unavailable" (honest-disabled).
type AccessProbeCarrier interface {
	AccessProbes() *AccessProbes
}

// ReaderWithAccessProbes bundles a MetadataReader with the explain_access probes. The returned
// reader forwards every metadata call unchanged and additionally satisfies AccessProbeCarrier.
func ReaderWithAccessProbes(r MetadataReader, p *AccessProbes) MetadataReader {
	return &readerWithAccessProbes{MetadataReader: r, probes: p}
}

type readerWithAccessProbes struct {
	MetadataReader
	probes *AccessProbes
}

func (r *readerWithAccessProbes) AccessProbes() *AccessProbes { return r.probes }

// explainAccess answers "vì sao user X (không) thấy/sửa được Y" with a compact decision-chain
// JSON the model narrates — READ-ONLY, no draft, builder-registry-only (this whole registry sits
// behind the admin gate, so an admin is asking about their own tenant's users). It COMPOSES
// existing engines instead of re-implementing rules:
//
//	tầng user      — auth.UserRepository (tenant-scoped lookup by email or id) + role row;
//	tầng module    — services.ACLService.Authorize per action, the SAME decider RequireACL runs;
//	tầng row-level — RecordService.Get probed UNDER THE TARGET USER'S scope (the engine itself
//	                 decides visibility), disambiguated by an admin-scope existence probe that
//	                 also yields the record's owner for the "vì sao" narration;
//	tầng field     — the raw sys_field_acl rows for (role, entity), default-allow semantics.
//
// The row probe reads the record's owner through the existing service seam only (dynrepo is
// never touched); team_set membership detail is NOT enumerated — the verdict names the blocking
// LEVEL (e.g. access level "team" + not visible ⇒ user outside the record's team_set), which is
// the engine's own answer. Unwired probes → ACCESS_EXPLAIN_UNAVAILABLE (honest-disabled).
func (e *toolExecutor) explainAccess(ctx context.Context, argsJSON string) string {
	var a struct {
		UserEmail string `json:"userEmail"`
		UserID    string `json:"userId"`
		Entity    string `json:"entity"`
		RecordID  string `json:"recordId"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}

	var probes *AccessProbes
	if c, ok := e.reader.(AccessProbeCarrier); ok {
		probes = c.AccessProbes()
	}
	if !probes.complete() {
		return toolErr("ACCESS_EXPLAIN_UNAVAILABLE: server chưa nối bộ probe phân quyền cho AI Builder (ReaderWithAccessProbes) — soi quyền thủ công ở màn Quản trị người dùng")
	}

	a.UserEmail = strings.TrimSpace(a.UserEmail)
	a.UserID = strings.TrimSpace(a.UserID)
	a.RecordID = strings.TrimSpace(a.RecordID)
	if a.UserEmail == "" && a.UserID == "" {
		return toolErr("ACCESS_USER_REQUIRED: cần userEmail hoặc userId để biết soi quyền của ai")
	}

	// Entity must be LIVE — a drafted entity has no ACL rows yet, so there is nothing to explain.
	ent, err := e.reader.GetEntityByName(ctx, e.tenantID, a.Entity)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return toolErr("ENTITY_NOT_FOUND: entity %q không tồn tại trong tenant hiện tại", a.Entity)
		}
		return toolErr("ACCESS_EXPLAIN_FAILED: %v", err)
	}

	// --- tầng user: tenant-scoped resolution (id wins when both are supplied). ---------------
	var user *auth.User
	if a.UserID != "" {
		user, err = probes.Users.GetUserByID(ctx, e.tenantID, a.UserID)
	} else {
		user, err = probes.Users.GetUserByEmail(ctx, e.tenantID, a.UserEmail)
	}
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return toolErr("ACCESS_USER_NOT_FOUND: không tìm thấy user trong tenant hiện tại (soi lại email/id)")
		}
		return toolErr("ACCESS_EXPLAIN_FAILED: %v", err)
	}

	userOut := map[string]any{
		"id":      user.ID,
		"email":   user.Email,
		"status":  user.Status,
		"isAdmin": user.IsAdmin,
	}
	out := map[string]any{"user": userOut, "entity": ent.Name}
	notes := []string{}
	if user.Status != "active" {
		notes = append(notes, fmt.Sprintf("user đang ở trạng thái %q — không đăng nhập được, mọi quyền chỉ là lý thuyết", user.Status))
	}

	// --- admin short-circuit: bypasses module ACL, row scope AND field ACL by construction. ---
	if user.IsAdmin {
		out["actions"] = map[string]any{
			"view":   map[string]any{"allowed": true, "accessLevel": string(dynrepo.AccessAll)},
			"edit":   map[string]any{"allowed": true, "accessLevel": string(dynrepo.AccessAll)},
			"delete": map[string]any{"allowed": true, "accessLevel": string(dynrepo.AccessAll)},
		}
		out["fieldAcl"] = map[string]any{"bypassed": true}
		if a.RecordID != "" {
			rec := e.probeRecordAsAdmin(ctx, probes, ent.Name, a.RecordID, user.ID)
			out["record"] = rec
		}
		out["verdict"] = "user là ADMIN — bypass toàn bộ module-ACL, row-level và field-ACL trong tenant"
		out["notes"] = notes
		return toolOK(out)
	}

	// --- no-role trap: a user without a role is deny-all on every dynamic entity. -------------
	roleID := ""
	if user.RoleID != nil {
		roleID = strings.TrimSpace(*user.RoleID)
	}
	if roleID == "" {
		out["role"] = nil
		out["actions"] = map[string]any{
			"view":   map[string]any{"allowed": false, "reason": "user chưa được gán role"},
			"edit":   map[string]any{"allowed": false, "reason": "user chưa được gán role"},
			"delete": map[string]any{"allowed": false, "reason": "user chưa được gán role"},
		}
		out["verdict"] = "bị chặn ở TẦNG MODULE-ACL: user chưa có role → deny-all (gán role trong Quản trị người dùng)"
		out["notes"] = notes
		return toolOK(out)
	}
	roleOut := map[string]any{"id": roleID}
	if role, rerr := probes.Roles.GetRoleByID(ctx, e.tenantID, roleID); rerr == nil {
		roleOut["name"] = role.Name
		roleOut["label"] = role.Label
	} else if errors.Is(rerr, auth.ErrRoleNotFound) {
		notes = append(notes, "role id gán cho user không còn tồn tại — mọi grant tra theo role sẽ rỗng (deny)")
	} else {
		return toolErr("ACCESS_EXPLAIN_FAILED: %v", rerr)
	}
	out["role"] = roleOut

	// --- tầng module-ACL: the SAME Authorize decision middleware.RequireACL makes. -------------
	actions := map[string]any{}
	viewAllowed, viewLevel := false, ""
	for _, action := range []string{"view", "edit", "delete"} {
		allowed, level, aerr := probes.ACL.Authorize(ctx, e.tenantID, roleID, ent.Name, action)
		if aerr != nil {
			return toolErr("ACCESS_EXPLAIN_FAILED: %v", aerr)
		}
		row := map[string]any{"allowed": allowed}
		if allowed {
			row["accessLevel"] = level // A5 row scope: all | team | owner | none
		}
		actions[action] = row
		if action == "view" {
			viewAllowed, viewLevel = allowed, level
		}
	}
	out["actions"] = actions

	// --- tầng field-ACL: raw sys_field_acl rows, default-allow overlay. -----------------------
	rows, ferr := probes.FieldACL.LoadFieldACLForEntity(ctx, e.tenantID, roleID, ent.Name)
	if ferr != nil {
		return toolErr("ACCESS_EXPLAIN_FAILED: %v", ferr)
	}
	hidden, readOnly := []string{}, []string{}
	for _, r := range rows {
		switch {
		case !r.CanView:
			hidden = append(hidden, r.FieldName) // can_view=0 implies locked too
		case !r.CanEdit:
			readOnly = append(readOnly, r.FieldName)
		}
	}
	sort.Strings(hidden)
	sort.Strings(readOnly)
	out["fieldAcl"] = map[string]any{"hiddenFields": hidden, "readOnlyFields": readOnly}

	// --- tầng row-level: probe the REAL engine under the target user's scope. -----------------
	verdict := fmt.Sprintf("tầng module-ACL: view=%v, edit=%v, delete=%v", viewAllowed, actions["edit"].(map[string]any)["allowed"], actions["delete"].(map[string]any)["allowed"])
	if a.RecordID != "" {
		recOut := map[string]any{"id": a.RecordID}
		if !viewAllowed {
			recOut["visible"] = false
			recOut["reason"] = "chưa tới được tầng row-level — module-ACL đã chặn view trên entity"
			verdict = "bị chặn ở TẦNG MODULE-ACL: role không có quyền view entity này"
		} else {
			// IncludeShares: this probe answers "does the user SEE it when they open it", and a
			// record manually shared with them is visible on that path (specs/051).
			targetScope := services.RecordScope{UserID: user.ID, RoleID: roleID, IsAdmin: false, AccessLevel: viewLevel, IncludeShares: true}
			_, gerr := probes.Records.Get(ctx, targetScope, e.tenantID, ent.Name, a.RecordID, nil, nil)
			switch {
			case gerr == nil:
				recOut["visible"] = true
				verdict = "user THẤY được record này (module-ACL pass + row-level pass); quyền sửa/xoá theo bảng actions, field bị ẩn/khoá theo fieldAcl"
			case errors.Is(gerr, services.ErrRecordNotFound):
				// Invisible to the user — disambiguate "blocked" vs "does not exist" as admin.
				adminRec := e.probeRecordAsAdmin(ctx, probes, ent.Name, a.RecordID, user.ID)
				recOut["visible"] = false
				if exists, _ := adminRec["exists"].(bool); exists {
					recOut["ownerId"] = adminRec["ownerId"]
					recOut["ownedByUser"] = adminRec["ownedByUser"]
					recOut["reason"] = fmt.Sprintf("bị chặn ở tầng row-level: access level %q không phủ record này", viewLevel)
					switch viewLevel {
					case string(dynrepo.AccessOwner):
						recOut["reason"] = "bị chặn ở tầng row-level: access level \"owner\" mà record thuộc người khác (xem ownerId)"
					case string(dynrepo.AccessTeam):
						recOut["reason"] = "bị chặn ở tầng row-level: access level \"team\" mà user không nằm trong team_set của record"
					case string(dynrepo.AccessNone):
						recOut["reason"] = "bị chặn ở tầng row-level: access level \"none\" — grant view không mở row nào"
					}
					verdict = "bị chặn ở TẦNG ROW-LEVEL (A5): " + recOut["reason"].(string)
				} else {
					recOut["exists"] = false
					recOut["reason"] = "record không tồn tại (hoặc đã bị xoá) — không phải vấn đề phân quyền"
					verdict = "record không tồn tại — không phải vấn đề phân quyền"
				}
			default:
				return toolErr("ACCESS_EXPLAIN_FAILED: %v", gerr)
			}
		}
		out["record"] = recOut
	}
	out["verdict"] = verdict
	out["notes"] = notes
	return toolOK(out)
}

// probeRecordAsAdmin reads a record under an admin-scope probe purely to answer existence +
// ownership for the explain chain (through the record SERVICE — tenant pinned, never dynrepo).
// Returns {"exists": false} on a clean miss; on any other error it degrades to an honest
// "unknown" instead of failing the whole explanation (the user-scope probe already answered the
// main question).
func (e *toolExecutor) probeRecordAsAdmin(ctx context.Context, probes *AccessProbes, entityName, recordID, subjectUserID string) map[string]any {
	adminScope := services.RecordScope{UserID: subjectUserID, IsAdmin: true, AccessLevel: string(dynrepo.AccessAll)}
	rec, err := probes.Records.Get(ctx, adminScope, e.tenantID, entityName, recordID, nil, nil)
	switch {
	case err == nil:
		return map[string]any{
			"exists":      true,
			"visible":     true,
			"ownerId":     rec.CreatedBy,
			"ownedByUser": rec.CreatedBy == subjectUserID,
		}
	case errors.Is(err, services.ErrRecordNotFound):
		return map[string]any{"exists": false, "visible": false}
	default:
		return map[string]any{"exists": "unknown", "error": "đọc record thất bại — thử lại"}
	}
}

// listPacks returns the shipped built-in vertical-pack catalog — the EXACT same in-code registry
// GET /admin/verticals/builtin serves (vertical_pack.BuiltinInfos + BuiltinPack), extended with
// each pack's entity names so Discovery can match a blueprint against what a pack already ships
// (PACK-FIRST: "cài pack X + propose phần delta" beats re-proposing 17 raw entities). Read-only,
// static, tenant-free by nature: the catalog is compiled into the binary, no tenant state is
// read or touched, and installing remains a human action on the Cài mẫu & Bàn giao screen.
func (e *toolExecutor) listPacks() string {
	infos := vertical_pack.BuiltinInfos()
	out := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		row := map[string]any{"slug": info.Slug, "entityCount": info.EntityCount}
		if info.Manifest != nil {
			row["name"] = info.Manifest.Name
			row["version"] = info.Manifest.Version
			row["description"] = info.Manifest.Description
		}
		if pack, ok := vertical_pack.BuiltinPack(info.Slug); ok && pack != nil {
			ents := make([]map[string]any, 0, len(pack.Entities))
			for _, en := range pack.Entities {
				ents = append(ents, map[string]any{"name": en.Name, "label": en.Label})
			}
			row["entities"] = ents
		}
		out = append(out, row)
	}
	return toolOK(map[string]any{"packs": out})
}

// validationOf inspects an error from a config-time validator. It returns (code, msg, ok=false)
// for a *services.ValidationError, (generic, msg, false) for any other non-nil error, and
// ("", "", true) when err is nil.
func validationOf(err error) (code, msg string, ok bool) {
	if err == nil {
		return "", "", true
	}
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		return ve.Code, ve.Msg, false
	}
	return "VALIDATION_FAILED", err.Error(), false
}

func toolOK(result any) string {
	b, err := json.Marshal(map[string]any{"ok": true, "result": result})
	if err != nil {
		return `{"ok":false,"error":"MARSHAL_FAILED"}`
	}
	return string(b)
}

func toolErr(format string, args ...any) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": fmt.Sprintf(format, args...)})
	return string(b)
}

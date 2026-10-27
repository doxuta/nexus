package ai

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestTools_ProposeField_OptionsValidationParity is the M9 parity lock: the model's picklist
// values and validation rules travel through propose_field's EXISTING options param and are
// validated by the SAME config-time validator the manual builder uses (services.ValidateAddField
// → validateFieldConfig — the POST /admin/fields path). Accept cases must buffer a draft with the
// options PRESERVED verbatim (the FE Apply mapper passes options through); reject cases must
// surface the REAL config-time code to the model (self-correction in-chat) and buffer NOTHING.
//
// Notably: an empty/blank-element picklist values list is rejected AT CONFIG TIME
// (FIELD_PICKLIST_NO_OPTIONS via hasPicklistValues) — it never falls through to a confusing
// record-time RECORD_INVALID_PICKLIST. No validation gap found for the M9 surface.
func TestTools_ProposeField_OptionsValidationParity(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantCode string         // "" = expect buffered
		wantOpts map[string]any // spot-checks on the buffered draft's Options (accept cases)
	}{
		// ---- accept + buffer: options preserved on the draft ----
		{
			name: "picklist với values tiếng Việt → buffered, values giữ nguyên",
			args: `{"entity":"patients","name":"status","label":"Trạng thái","fieldType":"picklist",
				"options":{"values":["Mới","Đang xử lý","Hoàn tất","Huỷ"]}}`,
			wantOpts: map[string]any{"values": []any{"Mới", "Đang xử lý", "Hoàn tất", "Huỷ"}},
		},
		{
			name: "multi_select với values → buffered",
			args: `{"entity":"patients","name":"symptoms","label":"Triệu chứng","fieldType":"multi_select",
				"options":{"values":["Sốt","Ho","Đau đầu"]}}`,
			wantOpts: map[string]any{"values": []any{"Sốt", "Ho", "Đau đầu"}},
		},
		{
			name: "number với min/max/step → buffered, luật giữ nguyên",
			args: `{"entity":"patients","name":"age","label":"Tuổi","fieldType":"number",
				"options":{"min":0,"max":150,"step":1}}`,
			wantOpts: map[string]any{"min": float64(0), "max": float64(150), "step": float64(1)},
		},
		{
			name: "text với maxLength + pattern → buffered",
			args: `{"entity":"patients","name":"tax_code","label":"Mã số thuế","fieldType":"text",
				"options":{"maxLength":14,"pattern":"[0-9]{10}(-[0-9]{3})?"}}`,
			wantOpts: map[string]any{"maxLength": float64(14), "pattern": "[0-9]{10}(-[0-9]{3})?"},
		},
		{
			name: "currency với currencyCode + precision + min → buffered",
			args: `{"entity":"patients","name":"fee","label":"Phí khám","fieldType":"currency",
				"options":{"currencyCode":"VND","precision":0,"min":0}}`,
			wantOpts: map[string]any{"currencyCode": "VND", "precision": float64(0), "min": float64(0)},
		},
		{
			name: "rating với min/max → buffered",
			args: `{"entity":"patients","name":"satisfaction","label":"Hài lòng","fieldType":"rating",
				"options":{"min":1,"max":5}}`,
			wantOpts: map[string]any{"min": float64(1), "max": float64(5)},
		},

		// ---- reject with the REAL config-time code, nothing buffered ----
		{
			name: "picklist values RỖNG → FIELD_PICKLIST_NO_OPTIONS (config-time, không rơi xuống record-time)",
			args: `{"entity":"patients","name":"status","label":"Trạng thái","fieldType":"picklist",
				"options":{"values":[]}}`,
			wantCode: "FIELD_PICKLIST_NO_OPTIONS",
		},
		{
			name: "picklist values chứa phần tử trắng → FIELD_PICKLIST_NO_OPTIONS",
			args: `{"entity":"patients","name":"status","label":"Trạng thái","fieldType":"picklist",
				"options":{"values":["Mới","  "]}}`,
			wantCode: "FIELD_PICKLIST_NO_OPTIONS",
		},
		{
			name: "multi_select thiếu values → FIELD_MULTI_SELECT_NO_OPTIONS",
			args: `{"entity":"patients","name":"tags","label":"Nhãn","fieldType":"multi_select",
				"options":{}}`,
			wantCode: "FIELD_MULTI_SELECT_NO_OPTIONS",
		},
		{
			name: "number min > max → FIELD_NUMBER_RANGE_INVALID",
			args: `{"entity":"patients","name":"age","label":"Tuổi","fieldType":"number",
				"options":{"min":150,"max":0}}`,
			wantCode: "FIELD_NUMBER_RANGE_INVALID",
		},
		{
			name: "number min sai kiểu (string) → FIELD_NUMBER_RANGE_INVALID (B6 — mistyped không rơi thầm)",
			args: `{"entity":"patients","name":"age","label":"Tuổi","fieldType":"number",
				"options":{"min":"0","max":150}}`,
			wantCode: "FIELD_NUMBER_RANGE_INVALID",
		},
		{
			name: "number step = 0 → FIELD_NUMBER_STEP_INVALID",
			args: `{"entity":"patients","name":"qty","label":"Số lượng","fieldType":"number",
				"options":{"step":0}}`,
			wantCode: "FIELD_NUMBER_STEP_INVALID",
		},
		{
			name: "text maxLength = 0 → FIELD_TEXT_MAXLENGTH_INVALID",
			args: `{"entity":"patients","name":"code","label":"Mã","fieldType":"text",
				"options":{"maxLength":0}}`,
			wantCode: "FIELD_TEXT_MAXLENGTH_INVALID",
		},
		{
			name: "text pattern regex hỏng → FIELD_TEXT_PATTERN_INVALID",
			args: `{"entity":"patients","name":"code","label":"Mã","fieldType":"text",
				"options":{"pattern":"(["}}`,
			wantCode: "FIELD_TEXT_PATTERN_INVALID",
		},
		{
			name: "currency thiếu currencyCode → FIELD_CURRENCY_CODE_REQUIRED",
			args: `{"entity":"patients","name":"fee","label":"Phí","fieldType":"currency",
				"options":{"min":0}}`,
			wantCode: "FIELD_CURRENCY_CODE_REQUIRED",
		},
		{
			name: "currency min > max → FIELD_CURRENCY_RANGE_INVALID",
			args: `{"entity":"patients","name":"fee","label":"Phí","fieldType":"currency",
				"options":{"currencyCode":"VND","min":100,"max":1}}`,
			wantCode: "FIELD_CURRENCY_RANGE_INVALID",
		},
		{
			name: "rating max ngoài 2..10 → FIELD_RATING_MAX_INVALID",
			args: `{"entity":"patients","name":"stars","label":"Sao","fieldType":"rating",
				"options":{"max":20}}`,
			wantCode: "FIELD_RATING_MAX_INVALID",
		},
		// ---- AI-TOOL-COVERAGE (Wave 4.6): the advanced types now on the AI surface ----
		{
			name: "auto_number (số chứng từ) → buffered, prefix/padding giữ nguyên",
			args: `{"entity":"patients","name":"invoice_no","label":"Số hoá đơn","fieldType":"auto_number",
				"options":{"prefix":"HĐ-","padding":5}}`,
			wantOpts: map[string]any{"prefix": "HĐ-", "padding": float64(5)},
		},
		{
			name: "formula (tổng tính toán) → buffered",
			args: `{"entity":"patients","name":"line_total","label":"Thành tiền","fieldType":"formula",
				"options":{"formula":"quantity * unit_price","resultType":"currency"}}`,
			wantOpts: map[string]any{"formula": "quantity * unit_price", "resultType": "currency"},
		},
		{
			name:     "formula thiếu biểu thức → REJECT config-time",
			args:     `{"entity":"patients","name":"bad_f","label":"Bad","fieldType":"formula","options":{}}`,
			wantCode: "FIELD_FORMULA_INVALID",
		},
		// ---- ai-builder-field-type-parity: user + polymorphic_relation on the AI surface ----
		{
			name: "user (người phụ trách) không options → buffered",
			args: `{"entity":"patients","name":"assignee","label":"Người phụ trách","fieldType":"user"}`,
		},
		{
			name: "polymorphic_relation với targetEntities → buffered, đích giữ nguyên",
			args: `{"entity":"patients","name":"related_to","label":"Liên quan tới","fieldType":"polymorphic_relation",
				"options":{"targetEntities":["crm_accounts","crm_contacts"]}}`,
			wantOpts: map[string]any{"targetEntities": []any{"crm_accounts", "crm_contacts"}},
		},
		{
			name:     "polymorphic_relation thiếu targetEntities → FIELD_POLYMORPHIC_NO_TARGETS (không để tới Apply mới 422)",
			args:     `{"entity":"patients","name":"related_to","label":"Liên quan tới","fieldType":"polymorphic_relation"}`,
			wantCode: "FIELD_POLYMORPHIC_NO_TARGETS",
		},
		{
			name: "polymorphic_relation tên > 21 ký tự → FIELD_POLYMORPHIC_NAME_TOO_LONG",
			args: `{"entity":"patients","name":"related_record_targets","label":"Liên quan","fieldType":"polymorphic_relation",
				"options":{"targetEntities":["crm_accounts"]}}`,
			wantCode: "FIELD_POLYMORPHIC_NAME_TOO_LONG",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_field", tt.args))

			if tt.wantCode != "" {
				if ok {
					t.Fatalf("expected reject with %s, but tool succeeded; draft=%+v", tt.wantCode, e.draft)
				}
				if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
					t.Fatalf("expected code %s, got %q", tt.wantCode, errStr)
				}
				if len(e.draft) != 0 {
					t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
				}
				return
			}

			if !ok {
				t.Fatalf("valid propose_field should succeed, got %+v", m)
			}
			if len(e.draft) != 1 || e.draft[0].Kind != "field" || e.draft[0].Field == nil {
				t.Fatalf("expected 1 buffered field draft, got %+v", e.draft)
			}
			got := e.draft[0].Field.Options
			for k, want := range tt.wantOpts {
				if !reflect.DeepEqual(got[k], want) {
					t.Errorf("draft options[%q] = %#v, want %#v", k, got[k], want)
				}
			}
		})
	}
}

// TestTools_ProposeEntity_NestedFieldOptionsParity proves the SAME options validation guards the
// nested fields[] of propose_entity (shared validateFieldConfig via ValidateCreateEntity) — a
// picklist inside a new entity cannot slip through with empty values either.
func TestTools_ProposeEntity_NestedFieldOptionsParity(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	args := `{"name":"orders","label":"Đơn hàng","fields":[
		{"name":"status","label":"Trạng thái","fieldType":"picklist","options":{"values":[]}}
	]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_entity", args))
	if ok {
		t.Fatal("propose_entity with an empty-values picklist field must be rejected")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "FIELD_PICKLIST_NO_OPTIONS") {
		t.Fatalf("expected FIELD_PICKLIST_NO_OPTIONS, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
	}
}

package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// specs/058: field_update with Via writes the RELATED record the triggering record's relation field
// points at — "hoá đơn Đã thanh toán ⇒ lịch hẹn đổi trạng thái", "giao dịch Đặt cọc ⇒ sản phẩm Đã
// giữ chỗ". The target entity comes from metadata (RecordMutator.RelatedEntity), the target id from
// the triggering record's data, the tenant from the event — never from anywhere else.

func relatedMutator() *fakeMutator {
	return &fakeMutator{related: map[string]string{"invoices.appointment": "appointments"}}
}

func TestRunActions_FieldUpdateVia_WritesTheRelatedRecord(t *testing.T) {
	mut := relatedMutator()
	wf := Workflow{ID: "wf1", Actions: []Action{{Type: ActionFieldUpdate, Via: "appointment", Field: "status", Value: "paid"}}}
	evt := Event{TenantID: "t1", EntityName: "invoices", RecordID: "inv-1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{"status": "Đã thanh toán", "appointment": "ap-7"}}

	results, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut})
	if err != nil {
		t.Fatalf("runActions = %v", err)
	}
	if len(mut.calls) != 1 {
		t.Fatalf("UpdateField calls = %d, want 1", len(mut.calls))
	}
	c := mut.calls[0]
	if c.entity != "appointments" || c.tenant != "t1" || c.record != "ap-7" || c.field != "status" || c.value != "paid" {
		t.Errorf("UpdateField call = %+v; want appointments/t1/ap-7/status/paid (the RELATED record, not inv-1)", c)
	}
	if len(results) != 1 || !results[0].OK || results[0].Via != "appointment" || results[0].Field != "status" {
		t.Errorf("results = %+v; want one OK result naming via=appointment field=status", results)
	}
}

func TestRunActions_FieldUpdateVia_UnlinkedIsANoOpWithAWarning(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"relation absent": {"status": "paid"},
		"relation null":   {"appointment": nil},
		"relation blank":  {"appointment": "  "},
	} {
		t.Run(name, func(t *testing.T) {
			mut := relatedMutator()
			wf := Workflow{ID: "wf1", Actions: []Action{{Type: ActionFieldUpdate, Via: "appointment", Field: "status", Value: "paid"}}}
			evt := Event{TenantID: "t1", EntityName: "invoices", RecordID: "inv-1", Trigger: TriggerAfterUpdate, Data: data}

			results, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut})
			if err != nil {
				t.Fatalf("runActions = %v; an unlinked record is nothing to do, not a failure", err)
			}
			if len(mut.calls) != 0 {
				t.Fatalf("UpdateField calls = %d, want 0 — there is no related record", len(mut.calls))
			}
			if len(results) != 1 || !results[0].OK || !strings.Contains(results[0].Warning, "appointment") {
				t.Errorf("results = %+v; want OK with a warning naming the empty relation", results)
			}
		})
	}
}

// A delete event carries the before-image; the related record is alive, so "xoá giao dịch ⇒ sản
// phẩm về Còn trống" is expressible — unlike a same-record field_update, which has nothing to patch.
func TestRunActions_FieldUpdateVia_RunsOnDelete(t *testing.T) {
	mut := relatedMutator()
	wf := Workflow{ID: "wf1", Actions: []Action{{Type: ActionFieldUpdate, Via: "appointment", Field: "status", Value: "open"}}}
	evt := Event{TenantID: "t1", EntityName: "invoices", RecordID: "inv-1", Trigger: TriggerAfterDelete,
		Data: map[string]any{"appointment": "ap-7"}}

	if _, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut}); err != nil {
		t.Fatalf("runActions = %v", err)
	}
	if len(mut.calls) != 1 || mut.calls[0].record != "ap-7" || mut.calls[0].entity != "appointments" {
		t.Fatalf("calls = %+v; want one write on appointments/ap-7", mut.calls)
	}
}

func TestRunActions_FieldUpdateVia_Failures(t *testing.T) {
	tests := []struct {
		name string
		mut  *fakeMutator
		data map[string]any
		want string
	}{
		{name: "via no longer resolves (field deleted / retyped)", mut: &fakeMutator{relatedErr: errors.New("WORKFLOW_RELATED_VIA_INVALID")},
			data: map[string]any{"appointment": "ap-7"}, want: "WORKFLOW_RELATED_VIA_INVALID"},
		// A deleted via is ABSENT from every record created after the delete. The broken rule must
		// still fail, not pass as "bản ghi chưa liên kết" — the dry-run already says error here.
		{name: "via no longer resolves and the record lacks it", mut: &fakeMutator{relatedErr: errors.New("WORKFLOW_RELATED_VIA_INVALID")},
			data: map[string]any{"status": "paid"}, want: "WORKFLOW_RELATED_VIA_INVALID"},
		{name: "via no longer resolves and the record holds null", mut: &fakeMutator{relatedErr: errors.New("WORKFLOW_RELATED_VIA_INVALID")},
			data: map[string]any{"appointment": nil}, want: "WORKFLOW_RELATED_VIA_INVALID"},
		{name: "relation value is not an id string", mut: relatedMutator(),
			data: map[string]any{"appointment": map[string]any{"id": "ap-7"}}, want: "appointment"},
		{name: "store refuses the write (not found / other tenant)", mut: &fakeMutator{related: map[string]string{"invoices.appointment": "appointments"}, err: errors.New("record not found")},
			data: map[string]any{"appointment": "ap-other-tenant"}, want: "record not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := Workflow{ID: "wf1", Actions: []Action{{Type: ActionFieldUpdate, Via: "appointment", Field: "status", Value: "paid"}}}
			evt := Event{TenantID: "t1", EntityName: "invoices", RecordID: "inv-1", Trigger: TriggerAfterUpdate, Data: tt.data}
			results, err := runActions(context.Background(), wf, evt, actionDeps{mut: tt.mut})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("runActions err = %v; want an error mentioning %q", err, tt.want)
			}
			if len(results) != 1 || results[0].OK {
				t.Errorf("results = %+v; want one FAILED result", results)
			}
			if tt.mut.err == nil && len(tt.mut.calls) != 0 {
				t.Errorf("a refused via must not write anything, got %+v", tt.mut.calls)
			}
		})
	}
}

// The same-record path is untouched: no Via ⇒ the triggering record, and after_delete is a no-op.
func TestRunActions_FieldUpdateWithoutVia_Unchanged(t *testing.T) {
	mut := relatedMutator()
	wf := Workflow{ID: "wf1", Actions: []Action{{Type: ActionFieldUpdate, Field: "status", Value: "paid"}}}
	evt := Event{TenantID: "t1", EntityName: "invoices", RecordID: "inv-1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{"appointment": "ap-7"}}
	if _, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut}); err != nil {
		t.Fatalf("runActions = %v", err)
	}
	if len(mut.calls) != 1 || mut.calls[0].entity != "invoices" || mut.calls[0].record != "inv-1" {
		t.Fatalf("calls = %+v; want the triggering record invoices/inv-1", mut.calls)
	}
}

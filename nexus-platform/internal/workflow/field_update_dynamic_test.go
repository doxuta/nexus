package workflow

import (
	"context"
	"reflect"
	"testing"
)

// queue #333: field_update values may be resolved at fire time exactly like create_record's
// ($record.<field> typed copy, {{field}} template) — "copy the appointment date from the invoice".
func TestRunActions_FieldUpdateDynamicValue(t *testing.T) {
	data := map[string]any{"due_date": "2026-10-20", "name": "Nha khoa A", "blank": "", "appointment": "ap-7", "total": map[string]any{"amount": 5, "currencyCode": "VND"}}
	cases := []struct {
		name    string
		a       Action
		want    any
		skipped bool
		target  string
		record  string
	}{
		{"constant untouched", Action{Type: ActionFieldUpdate, Field: "f", Value: "paid"}, "paid", false, "invoices", "inv-1"},
		{"record field typed copy", Action{Type: ActionFieldUpdate, Field: "f", Value: "$record.total"}, map[string]any{"amount": 5, "currencyCode": "VND"}, false, "invoices", "inv-1"},
		{"record id", Action{Type: ActionFieldUpdate, Field: "f", Value: "$record"}, "inv-1", false, "invoices", "inv-1"},
		{"template", Action{Type: ActionFieldUpdate, Field: "f", Value: "KH {{name}}"}, "KH Nha khoa A", false, "invoices", "inv-1"},
		{"template all-empty source skips", Action{Type: ActionFieldUpdate, Field: "f", Value: "{{blank}}"}, nil, true, "", ""},
		{"template missing source skips", Action{Type: ActionFieldUpdate, Field: "f", Value: "co-hoi-{{missing}}"}, nil, true, "", ""},
		{"template partial still writes", Action{Type: ActionFieldUpdate, Field: "f", Value: "{{blank}}{{name}}"}, "Nha khoa A", false, "invoices", "inv-1"},
		{"padded record copy resolves", Action{Type: ActionFieldUpdate, Field: "f", Value: " $record.name "}, "Nha khoa A", false, "invoices", "inv-1"},
		{"via + copy", Action{Type: ActionFieldUpdate, Via: "appointment", Field: "f", Value: "$record.due_date"}, "2026-10-20", false, "appointments", "ap-7"},
		{"empty source skips, never clears", Action{Type: ActionFieldUpdate, Field: "f", Value: "$record.missing"}, nil, true, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mut := relatedMutator()
			wf := Workflow{ID: "wf1", Actions: []Action{tc.a}}
			evt := Event{TenantID: "t1", EntityName: "invoices", RecordID: "inv-1", Trigger: TriggerAfterUpdate, Data: data}
			results, err := runActions(context.Background(), wf, evt, actionDeps{mut: mut})
			if err != nil {
				t.Fatalf("runActions = %v", err)
			}
			if tc.skipped {
				if len(mut.calls) != 0 || len(results) != 1 || results[0].Warning == "" {
					t.Fatalf("calls=%d results=%+v; want no write and a warning naming the empty source", len(mut.calls), results)
				}
				return
			}
			if len(mut.calls) != 1 {
				t.Fatalf("calls = %d", len(mut.calls))
			}
			c := mut.calls[0]
			if c.entity != tc.target || c.record != tc.record || !reflect.DeepEqual(c.value, tc.want) {
				t.Errorf("call = %+v; want %s/%s value %v", c, tc.target, tc.record, tc.want)
			}
		})
	}
}

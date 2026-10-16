package services

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// specs/059 (DAILY-QUEUE #271): the production answer to workflow.TemplateResolver — the patient's
// email for "Gửi tới {{benh_nhan.email}}" on an appointment, and the value a lookup field shows.

// tplCatalog serves fixed entities/fields and records which tenant it was asked for.
type tplCatalog struct {
	fields  map[string][]metadata.FieldDefinition // entity name → fields
	err     error
	tenants []string
}

func (c *tplCatalog) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	c.tenants = append(c.tenants, tenantID)
	if c.err != nil {
		return nil, c.err
	}
	if _, ok := c.fields[name]; !ok {
		return nil, metadata.ErrEntityNotFound
	}
	return &metadata.EntityDefinition{ID: "id-" + name, Name: name}, nil
}

func (c *tplCatalog) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	return c.fields[entityID[len("id-"):]], nil
}

// tplRecords serves records by entity+id and records every read.
type tplRecords struct {
	mu    sync.Mutex
	rows  map[string]map[string]any // "entity/id" → data
	err   error
	reads []string // "tenant|entity/id|admin"
}

func (r *tplRecords) GetRecord(_ context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	admin := "scoped"
	if scope.IsAdmin {
		admin = "admin"
	}
	r.reads = append(r.reads, tenantID+"|"+entity+"/"+id+"|"+admin)
	if r.err != nil {
		return nil, r.err
	}
	data, ok := r.rows[entity+"/"+id]
	if !ok {
		return nil, dynrepo.ErrRecordNotFound
	}
	return &dynrepo.Record{ID: id, Data: data}, nil
}

func tplField(name, typ, options string) metadata.FieldDefinition {
	f := metadata.FieldDefinition{Name: name, FieldType: typ}
	if options != "" {
		f.Options = &options
	}
	return f
}

func tplFixture() *tplCatalog {
	return &tplCatalog{fields: map[string][]metadata.FieldDefinition{
		"lich_hen": {
			tplField("gio_hen", "datetime", ""),
			tplField("ghi_chu", "text", ""),
			tplField("benh_nhan", "relation", `{"relatedEntity":"benh_nhan"}`),
			tplField("bac_si", "relation", `{"relatedEntity":"bac_si"}`),
			tplField("email_bn", "lookup", `{"via":"benh_nhan","sourceField":"email"}`),
		},
		"benh_nhan": {
			tplField("ho_ten", "text", ""), tplField("email", "email", ""),
			// Values that never render as text in a mail: read-time computed (lookup, rollup) or a
			// stored object/array that workflow.toString turns into "".
			tplField("bs_ten", "lookup", `{"via":"bac_si","sourceField":"ten"}`),
			tplField("tong_hen", "rollup", `{"relationship":"lich_hen_benh_nhan","function":"count"}`),
			tplField("cong_no", "currency", ""),
			tplField("tong_phi", "formula", `{"expression":"cong_no * 2","resultType":"currency"}`),
			tplField("diem", "formula", `{"expression":"1 + 1"}`),
			tplField("the", "multi_select", ""),
		},
		"bac_si": {tplField("ten", "text", "")},
	}}
}

func TestWorkflowTemplateResolver(t *testing.T) {
	patient := map[string]any{"ho_ten": "Nguyễn Thị Lan", "email": "lan@nhakhoa.vn"}
	cases := []struct {
		name      string
		data      map[string]any
		names     []string
		want      map[string]any
		wantReads []string
	}{
		{
			name:  "fields of the linked record: ONE read per relation, however many fields",
			data:  map[string]any{"benh_nhan": "p-1"},
			names: []string{"benh_nhan.email", "benh_nhan.ho_ten", "email_bn"},
			want:  map[string]any{"benh_nhan.email": "lan@nhakhoa.vn", "benh_nhan.ho_ten": "Nguyễn Thị Lan", "email_bn": "lan@nhakhoa.vn"},
			// Tenant-pinned, system (admin) scope — the engine is a system principal, and the
			// builder's rule must not depend on whose edit fired it.
			wantReads: []string{"t1|benh_nhan/p-1|admin"},
		},
		{
			name:      "lookup field alone resolves through its via/sourceField",
			data:      map[string]any{"benh_nhan": "p-1"},
			names:     []string{"email_bn"},
			want:      map[string]any{"email_bn": "lan@nhakhoa.vn"},
			wantReads: []string{"t1|benh_nhan/p-1|admin"},
		},
		{
			name:  "unlinked appointment: nothing to read, nothing resolved",
			data:  map[string]any{"benh_nhan": ""},
			names: []string{"benh_nhan.email", "email_bn"},
			want:  map[string]any{},
		},
		{
			name:      "linked record gone (soft-deleted / other tenant): unresolved, not an error",
			data:      map[string]any{"bac_si": "d-gone"},
			names:     []string{"bac_si.ten"},
			want:      map[string]any{},
			wantReads: []string{"t1|bac_si/d-gone|admin"},
		},
		{
			name:  "not a relation, unknown field, two hops deep: never read",
			data:  map[string]any{"ghi_chu": "p-1", "benh_nhan": "p-1"},
			names: []string{"ghi_chu.email", "khong_co.email", "benh_nhan.bac_si.ten", "khong_co"},
			want:  map[string]any{},
		},
		{
			name:      "field missing on the linked record renders empty",
			data:      map[string]any{"benh_nhan": "p-1"},
			names:     []string{"benh_nhan.so_dien_thoai"},
			want:      map[string]any{},
			wantReads: []string{"t1|benh_nhan/p-1|admin"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cat := tplFixture()
			recs := &tplRecords{rows: map[string]map[string]any{"benh_nhan/p-1": patient}}
			got, err := NewWorkflowTemplateResolver(cat, recs).ResolveTemplateValues(context.Background(), "t1", "lich_hen", tc.data, tc.names)
			if err != nil {
				t.Fatalf("ResolveTemplateValues: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v; want %v", got, tc.want)
			}
			if !reflect.DeepEqual(recs.reads, tc.wantReads) && (len(recs.reads) != 0 || len(tc.wantReads) != 0) {
				t.Errorf("reads %v; want %v", recs.reads, tc.wantReads)
			}
			for _, tn := range cat.tenants {
				if tn != "t1" {
					t.Errorf("metadata read for tenant %q; want only t1", tn)
				}
			}
		})
	}
}

// A failed READ is not "no value": the action must fail (and the run say so) rather than send a
// reminder with the patient's name or address silently missing.
func TestWorkflowTemplateResolver_ReadFailuresSurface(t *testing.T) {
	cause := errors.New("db down")
	recs := &tplRecords{err: cause}
	_, err := NewWorkflowTemplateResolver(tplFixture(), recs).
		ResolveTemplateValues(context.Background(), "t1", "lich_hen", map[string]any{"benh_nhan": "p-1"}, []string{"benh_nhan.email"})
	if !errors.Is(err, cause) {
		t.Errorf("record read failure: err = %v; want it wrapped", err)
	}

	cat := tplFixture()
	cat.err = cause
	_, err = NewWorkflowTemplateResolver(cat, &tplRecords{}).
		ResolveTemplateValues(context.Background(), "t1", "lich_hen", map[string]any{"benh_nhan": "p-1"}, []string{"benh_nhan.email"})
	if !errors.Is(err, cause) {
		t.Errorf("metadata read failure: err = %v; want it wrapped", err)
	}
}

// Config-time parity (B6): a dotted placeholder that can never resolve — not a relation, a field the
// linked entity does not have, two hops — is refused when the rule is SAVED, not discovered as an
// empty reminder weeks later. Plain {{field}} placeholders keep their old, unchecked behaviour.
func TestEmailTemplateRefs_ConfigTimeValidation(t *testing.T) {
	tests := []struct {
		name, to, subject, body string
		wantField               string // "" = accepted
	}{
		{name: "linked patient email", to: "{{benh_nhan.email}}", subject: "Nhắc {{benh_nhan.ho_ten}}", body: "{{ghi_chu}}"},
		{name: "lookup field and literal address", to: "{{email_bn}}, le.tan@nhakhoa.vn", subject: "s", body: "b"},
		{name: "not a relation", to: "{{ghi_chu.email}}", subject: "s", body: "b", wantField: "ghi_chu.email"},
		{name: "unknown relation", to: "{{khong_co.email}}", subject: "s", body: "b", wantField: "khong_co.email"},
		{name: "field the linked entity lacks", to: "{{benh_nhan.so_dt}}", subject: "s", body: "b", wantField: "benh_nhan.so_dt"},
		{name: "two hops", to: "a@b.vn", subject: "{{ benh_nhan.bac_si.ten }}", body: "b", wantField: "benh_nhan.bac_si.ten"},
		{name: "checked in the body too", to: "a@b.vn", subject: "s", body: "Bác sĩ {{bac_si.ho_ten}}", wantField: "bac_si.ho_ten"},
		// The field exists but its value never reaches the mail: refused, like FIELD_FORMULA_NESTED
		// refuses a formula over a read-time field (lookup_validate.go).
		{name: "lookup on the linked record", to: "{{benh_nhan.bs_ten}}", subject: "s", body: "b", wantField: "benh_nhan.bs_ten"},
		{name: "rollup on the linked record", to: "a@b.vn", subject: "Tổng {{benh_nhan.tong_hen}}", body: "b", wantField: "benh_nhan.tong_hen"},
		{name: "currency object", to: "a@b.vn", subject: "s", body: "Nợ {{benh_nhan.cong_no}}", wantField: "benh_nhan.cong_no"},
		{name: "currency formula", to: "a@b.vn", subject: "s", body: "{{benh_nhan.tong_phi}}", wantField: "benh_nhan.tong_phi"},
		{name: "multi-select array", to: "a@b.vn", subject: "s", body: "{{benh_nhan.the}}", wantField: "benh_nhan.the"},
		{name: "number formula is stored and renders", to: "a@b.vn", subject: "s", body: "Điểm {{benh_nhan.diem}}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(tplFixture())
			_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
				Name: "nhac-hen", EntityName: "lich_hen", Trigger: "after_create", ActorID: "u1",
				Actions: []WorkflowActionInput{{Type: "email_alert", To: tt.to, Subject: tt.subject, Body: tt.body}},
			})
			if tt.wantField == "" {
				if err != nil || len(repo.created) != 1 {
					t.Fatalf("CreateWorkflow = %v (created %d); want the rule saved", err, len(repo.created))
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "WORKFLOW_TEMPLATE_REF_INVALID" {
				t.Fatalf("err = %v; want WORKFLOW_TEMPLATE_REF_INVALID", err)
			}
			if got, _ := ve.Details["field"].(string); got != tt.wantField {
				t.Errorf("details.field = %q; want %q", got, tt.wantField)
			}
			if len(repo.created) != 0 {
				t.Errorf("a rejected rule was persisted")
			}
		})
	}
}

// "Chạy thử" renders the email through the SAME path as the live send (workflow.RenderEmail + the
// same resolver), so the preview shows the patient's address — and says so when there is none.
func TestPreviewEmailAlert_UsesTheLinkedRecordLikeTheLiveSend(t *testing.T) {
	recs := &tplRecords{rows: map[string]map[string]any{
		"benh_nhan/p-1": {"ho_ten": "Lan", "email": "lan@nhakhoa.vn"},
		"benh_nhan/p-2": {"ho_ten": "Hùng"},
	}}
	res := NewWorkflowTemplateResolver(tplFixture(), recs)
	act := workflow.Action{Type: workflow.ActionEmailAlert, To: "{{benh_nhan.email}}", Subject: "Nhắc hẹn {{benh_nhan.ho_ten}}", Body: "b"}
	evtFor := func(patient string) workflow.Event {
		return workflow.Event{TenantID: "t1", EntityName: "lich_hen", Data: map[string]any{"benh_nhan": patient}}
	}

	detail, would := previewAction(context.Background(), "wf-1", act, evtFor("p-1"), true, nil, nil, res)
	if !would || detail["to"] != "lan@nhakhoa.vn" || detail["subject"] != "Nhắc hẹn Lan" || detail["wouldSend"] != true {
		t.Errorf("linked patient with email: detail=%v would=%v", detail, would)
	}

	detail, _ = previewAction(context.Background(), "wf-1", act, evtFor("p-2"), true, nil, nil, res)
	if detail["wouldSend"] != false || detail["error"] == nil {
		t.Errorf("patient without email must preview as not sendable with a reason, got %v", detail)
	}

	recs.err = errors.New("db down")
	detail, _ = previewAction(context.Background(), "wf-1", act, evtFor("p-1"), true, nil, nil, res)
	if detail["wouldSend"] != false || detail["error"] == nil {
		t.Errorf("read failure must preview as not sendable with a reason, got %v", detail)
	}
}

package services

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

// specs/059 (DAILY-QUEUE #271) — the fields of the LINKED record in an email_alert template.
//
// An appointment's reminder goes to the patient; a ticket's notice to the customer; an enrolment's
// to the student. The address always lives on the parent record the rule's record points at, and
// the engine used to render from the triggering row's own JSON only — so "{{benh_nhan.email}}"
// rendered "" and a lookup field ("{{email_bn}}", computed at READ time, never stored) did too.

// workflowTemplateRecords is the read slice the resolver needs: ONE record of an entity. Satisfied
// by *dynrepo.Repository (tenant_id pinned, soft-deleted rows excluded).
type workflowTemplateRecords interface {
	GetRecord(ctx context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error)
}

// workflowTemplateUsers is the optional second read: the `users` row a `user` field points at
// (#336). Satisfied by *dynrepo.Repository; a records store without it simply resolves no user
// placeholder. Tenant-pinned in the query (users is a system table, not an obj_* record).
type workflowTemplateUsers interface {
	BatchFetchUserLabels(ctx context.Context, tenantID string, ids []string) ([]dynrepo.UserLabelResult, error)
}

// userTemplateFields are the columns of a user a template may print — the only two the
// BatchFetchUserLabels projection returns, so nothing else of `users` (hash, role…) is reachable.
var userTemplateFields = []string{"email", "name"}

// WorkflowTemplateResolver answers workflow.TemplateResolver from metadata + one read per linked
// record:
//
//	{{<relation>.<field>}}  the field of the record the to-one relation points at
//	{{<user field>.email}}  the e-mail / name of the user a `user` field points at (#336)
//	{{<lookup field>}}      the value the record page shows for it (options.via + sourceField)
//
// Depth is ONE, like ?expand and lookup: "a.b.c" is never followed. The read runs under the admin
// row scope — the engine is a system principal, so whose edit fired the rule must not change who
// the reminder reaches — and in the event's tenant only. Field-level ACL does not apply for the
// same reason (a rule is the builder's configuration, not a user's read). Anything that does not
// resolve (empty FK, a linked record deleted or in another tenant, a field that is not there) is
// left out and renders "" exactly like an unknown field; only a failed READ is an error.
type WorkflowTemplateResolver struct {
	catalog EntityCatalog
	records workflowTemplateRecords
}

// NewWorkflowTemplateResolver wires the resolver. Production passes the metadata cache (no MySQL
// round-trip per mail for the field specs) and the dynamic record repository.
func NewWorkflowTemplateResolver(catalog EntityCatalog, records workflowTemplateRecords) *WorkflowTemplateResolver {
	return &WorkflowTemplateResolver{catalog: catalog, records: records}
}

var _ workflow.TemplateResolver = (*WorkflowTemplateResolver)(nil)

// ResolveTemplateValues implements workflow.TemplateResolver. One record read per relation used,
// however many of its fields the template names.
//
// ponytail: a scheduled rule renders once per matching record, so it costs one read per record per
// relation; batch by relation (BatchFetchField) if a scheduled mail-merge over thousands of rows
// ever shows up in the latency.
func (r *WorkflowTemplateResolver) ResolveTemplateValues(ctx context.Context, tenantID, entity string, data map[string]any, names []string) (map[string]any, error) {
	specs, err := r.specsOf(ctx, tenantID, entity)
	if err != nil {
		return nil, fmt.Errorf("services.WorkflowTemplateResolver.ResolveTemplateValues: %w", err)
	}
	byName := make(map[string]FieldSpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}
	out := make(map[string]any, len(names))
	linked := make(map[string]map[string]any) // relation field → linked record data (nil = none)
	for _, name := range names {
		via, field, ok := templateRefTarget(byName, name)
		if !ok {
			continue
		}
		rec, read := linked[via]
		if !read {
			if rec, err = r.linkedRecord(ctx, tenantID, byName[via], data[via]); err != nil {
				return nil, fmt.Errorf("services.WorkflowTemplateResolver.ResolveTemplateValues: %q: %w", name, err)
			}
			linked[via] = rec
		}
		if v, ok := rec[field]; ok {
			out[name] = v
		}
	}
	return out, nil
}

// templateRefTarget maps a placeholder to (relation field, field of the linked record): "a.b" names
// them directly, a lookup field names them through its options. ok=false for anything else.
func templateRefTarget(byName map[string]FieldSpec, name string) (via, field string, ok bool) {
	if v, f, dotted := strings.Cut(name, "."); dotted {
		via, field = v, f
	} else if s, found := byName[name]; found && s.FieldType == "lookup" {
		via, _ = s.Options["via"].(string)
		field, _ = s.Options["sourceField"].(string)
	}
	via, field = strings.TrimSpace(via), strings.TrimSpace(field)
	if via == "" || field == "" || strings.Contains(field, ".") {
		return "", "", false
	}
	rel, found := byName[via]
	if !found {
		return "", "", false
	}
	if rel.FieldType == "user" {
		return via, field, slices.Contains(userTemplateFields, field)
	}
	if rel.FieldType != "relation" || relatedEntityOf(rel) == "" {
		return "", "", false
	}
	return via, field, true
}

// relatedEntityOf is a relation field's target entity ("" for a polymorphic relation, which has
// targetEntities instead and no single place to read from).
func relatedEntityOf(rel FieldSpec) string {
	target, _ := rel.Options["relatedEntity"].(string)
	return strings.TrimSpace(target)
}

// templateRendersEmpty reports a field whose value a mail template can never print: lookup/rollup
// are computed at READ time and never sit in the stored data the resolver reads, and currency,
// multi_select, composite, polymorphic_relation, file/image and a currency formula are stored as
// objects/arrays that workflow.toString renders as "". checkTemplateRefs refuses them at save time
// (parity with the builder's picker, nexus-web/lib/workflowTemplateVars.ts NOT_RENDERABLE).
func templateRendersEmpty(sp FieldSpec) bool {
	switch sp.FieldType {
	case "lookup", "rollup", "currency", "multi_select", "composite", "polymorphic_relation", "file", "image":
		return true
	case "formula":
		rt, _ := sp.Options["resultType"].(string)
		return rt == "currency"
	}
	return false
}

// linkedRecord reads the record a relation value points at. An empty FK or a record that no longer
// resolves in this tenant is nil, not an error.
func (r *WorkflowTemplateResolver) linkedRecord(ctx context.Context, tenantID string, rel FieldSpec, fk any) (map[string]any, error) {
	id, _ := fk.(string)
	if id = strings.TrimSpace(id); id == "" {
		return nil, nil
	}
	if rel.FieldType == "user" {
		return r.linkedUser(ctx, tenantID, id)
	}
	rec, err := r.records.GetRecord(ctx, relatedEntityOf(rel), tenantID, id, dynrepo.AdminScope())
	if errors.Is(err, dynrepo.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec.Data, nil
}

// linkedUser reads the user a `user` field points at. Another tenant's, a deleted or an unknown id
// resolves to nil (BatchFetchUserLabels filters tenant_id and deleted_at); a disabled user still
// resolves — history keeps showing who it was.
func (r *WorkflowTemplateResolver) linkedUser(ctx context.Context, tenantID, id string) (map[string]any, error) {
	users, ok := r.records.(workflowTemplateUsers)
	if !ok {
		return nil, nil
	}
	rows, err := users.BatchFetchUserLabels(ctx, tenantID, []string{id})
	if err != nil {
		return nil, err
	}
	for _, u := range rows {
		if u.ID == id {
			return map[string]any{"email": u.Email, "name": u.Name}, nil
		}
	}
	return nil, nil
}

func (r *WorkflowTemplateResolver) specsOf(ctx context.Context, tenantID, entity string) ([]FieldSpec, error) {
	def, err := r.catalog.GetEntityByName(ctx, tenantID, entity)
	if err != nil {
		return nil, err
	}
	fields, err := r.catalog.ListFields(ctx, tenantID, def.ID)
	if err != nil {
		return nil, err
	}
	return toFieldSpecs(fields)
}

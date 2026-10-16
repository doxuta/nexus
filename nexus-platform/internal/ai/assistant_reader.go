// assistant_reader.go — RecordReadAdapter, the PRODUCTION RecordReader behind the ⌘K assistant
// tools. It composes the exact engines the normal browse endpoints use:
//
//   - services.RecordService (List/Aggregate) — tenant pinning, filter/sort validation, A5
//     row-level scope, field-level ACL strip/guard (trap #4: tools NEVER touch dynrepo directly);
//   - services.ACLService — the per-entity "view" decision + its row-level access_level, i.e. the
//     SAME decider middleware.RequireACL runs on a normal GET /:entity request (trap #1);
//   - the tenant-scoped MetadataReader — entity/field definitions, then ACL-FILTERED before they
//     reach the model (trap #2: the builder's admin-wide listing must not leak entity names an
//     end-user may not view).
//
// Scope contract (trap #1, the leak-class trap): the caller's services.RecordScope arrives from
// the handler (scopeOf) with the VERIFIED UserID/RoleID/IsAdmin. Because /assistant/ask is not an
// /:entity route, no RequireACL stage could resolve a per-entity access_level into it — scopeOf
// fails closed to "none". This adapter runs that missing ACL stage PER TOOL CALL, per entity,
// through ACLService.Authorize (the identical decision the middleware makes), and sets ONLY the
// AccessLevel of the scope it forwards. The identity fields are never substituted or rebuilt from
// ai.Scope — so the assistant's visibility is the caller's normal browsing visibility, by
// construction: ACL check first, then row-scoped read, exactly the documented
// Auth → Tenant → ACL → Business Logic order.
package ai

import (
	"context"
	"errors"
	"fmt"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

// recordQuerier is the slice of services.RecordService the adapter consumes (consumer-side so
// unit tests inject a fake that records the scope it receives). Get backs the
// propose_record_update existence probe — the SAME engine a normal GET /:entity/:id runs.
// HiddenFields is the field-ACL hidden set those reads strip, so Fields() describes exactly the
// fields a read would serve the caller.
type recordQuerier interface {
	List(ctx context.Context, scope services.RecordScope, tenantID, entityName string, q services.ListQuery) (services.ListResult, error)
	Aggregate(ctx context.Context, scope services.RecordScope, tenantID, entityName string, q services.AggregateQuery) (services.AggregateResult, error)
	Get(ctx context.Context, scope services.RecordScope, tenantID, entityName, id string, expand []string, rollup []string) (*dynrepo.Record, error)
	HiddenFields(ctx context.Context, scope services.RecordScope, tenantID, entity string) (map[string]struct{}, error)
}

// reportRunner is the slice of services.ReportService the adapter consumes (A3 semantic-layer
// tools): saved reports are the tenant's REVIEWED metric definitions, so ⌘K answering through
// them yields the same numbers the Reports screen shows — instead of the model re-deriving
// grain/filters ad hoc. RunReport resolves the caller's per-entity "view" level ITSELF
// (records.AuthorizeView inside the service — fail-closed), so the adapter passes the caller's
// scope VERBATIM, exactly like the /reports/:id/run route does.
type reportRunner interface {
	ListReports(ctx context.Context, tenantID string) ([]report.Record, error)
	RunReport(ctx context.Context, scope services.RecordScope, tenantID, id string, overlay *services.RunOverlay) (services.ReportRunResult, error)
}

// entityACL is the slice of services.ACLService the adapter consumes: Authorize yields the
// (allowed, access_level) pair the row scope needs; CanViewEntity is its boolean form used to
// filter metadata listings.
type entityACL interface {
	Authorize(ctx context.Context, tenantID, roleID, entity, action string) (bool, string, error)
	CanViewEntity(ctx context.Context, tenantID, roleID, entityName string) (bool, error)
}

// RecordReadAdapter implements RecordReader over the real record/ACL/metadata services.
type RecordReadAdapter struct {
	records recordQuerier
	acl     entityACL
	meta    MetadataReader
	reports reportRunner
}

// NewRecordReadAdapter wires the production RecordReader. records is *services.RecordService,
// acl is *services.ACLService, meta is the same tenant-scoped metadata repository the builder
// agent reads, reports is *services.ReportService (A3 saved-report tools). All must be non-nil
// in production.
func NewRecordReadAdapter(records recordQuerier, acl entityACL, meta MetadataReader, reports reportRunner) *RecordReadAdapter {
	return &RecordReadAdapter{records: records, acl: acl, meta: meta, reports: reports}
}

// scopeFor runs the per-entity ACL stage for a non-admin caller and returns the scope to hand to
// RecordService: the caller's scope VERBATIM with AccessLevel set to the access_level of the
// matching "view" grant — precisely what middleware.RequireACL + scopeOf produce on a normal
// GET /:entity request. Deny (no grant) and error both fail closed: deny surfaces as
// services.ErrEntityNotFound so a forbidden entity is indistinguishable from a nonexistent one
// (no existence leak — the same reason list_entities is ACL-filtered).
//
// Admins bypass with their scope untouched: dynrepo's effectiveLevel short-circuits IsAdmin to
// AccessAll and every downstream ACL/field-ACL check honors the verified admin flag, mirroring
// the RequireACL middleware's D3 admin bypass.
func (a *RecordReadAdapter) scopeFor(ctx context.Context, scope services.RecordScope, tenantID, entity string) (services.RecordScope, error) {
	if scope.IsAdmin {
		return scope, nil
	}
	allowed, level, err := a.acl.Authorize(ctx, tenantID, scope.RoleID, entity, "view")
	if err != nil {
		// Fail closed: an ACL infrastructure fault must never widen visibility.
		return services.RecordScope{}, fmt.Errorf("ai.RecordReadAdapter.scopeFor: %w", err)
	}
	if !allowed {
		return services.RecordScope{}, services.ErrEntityNotFound
	}
	scoped := scope // identity fields (UserID/RoleID/IsAdmin) stay VERBATIM — only the level is resolved
	scoped.AccessLevel = level
	// ⌘K answers "what do I see when I open it": that includes records manually shared with the
	// caller (specs/051, DAILY-QUEUE #225). Reads only — the assistant never writes through this scope.
	scoped.IncludeShares = true
	return scoped, nil
}

// QueryRecords implements RecordReader: per-entity ACL stage, then RecordService.List (which
// enforces tenant, filter/sort legality, A5 row scope from the resolved level, and field-ACL).
func (a *RecordReadAdapter) QueryRecords(ctx context.Context, scope services.RecordScope, tenantID, entity string, q services.ListQuery) (services.ListResult, error) {
	scoped, err := a.scopeFor(ctx, scope, tenantID, entity)
	if err != nil {
		return services.ListResult{}, err
	}
	res, err := a.records.List(ctx, scoped, tenantID, entity, q)
	if err != nil {
		return services.ListResult{}, fmt.Errorf("ai.RecordReadAdapter.QueryRecords: %w", err)
	}
	return res, nil
}

// GetRecord implements RecordReader: per-entity ACL stage, then RecordService.Get — the SAME
// engine a normal GET /:entity/:id request runs (tenant pin, A5 row scope from the resolved
// level, field-ACL strip). The two not-found sentinels pass through UNWRAPPED so the tool layer
// can tell "entity forbidden/nonexistent" (ErrEntityNotFound) from "record absent/out-of-scope"
// (ErrRecordNotFound) — both no-leak by construction. No expand/rollup: the probe needs only the
// record's own data for the Before diff.
func (a *RecordReadAdapter) GetRecord(ctx context.Context, scope services.RecordScope, tenantID, entity, id string) (*dynrepo.Record, error) {
	scoped, err := a.scopeFor(ctx, scope, tenantID, entity)
	if err != nil {
		return nil, err
	}
	rec, err := a.records.Get(ctx, scoped, tenantID, entity, id, nil, nil)
	if err != nil {
		if errors.Is(err, services.ErrEntityNotFound) || errors.Is(err, services.ErrRecordNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("ai.RecordReadAdapter.GetRecord: %w", err)
	}
	return rec, nil
}

// AggregateRecords implements RecordReader: same ACL stage, then RecordService.Aggregate.
func (a *RecordReadAdapter) AggregateRecords(ctx context.Context, scope services.RecordScope, tenantID, entity string, q services.AggregateQuery) (services.AggregateResult, error) {
	scoped, err := a.scopeFor(ctx, scope, tenantID, entity)
	if err != nil {
		return services.AggregateResult{}, err
	}
	res, err := a.records.Aggregate(ctx, scoped, tenantID, entity, q)
	if err != nil {
		return services.AggregateResult{}, fmt.Errorf("ai.RecordReadAdapter.AggregateRecords: %w", err)
	}
	return res, nil
}

// ViewableEntities implements RecordReader: the tenant's entities FILTERED to those the caller's
// role may "view" (trap #2). Admins see all. A CanViewEntity error fails the whole call closed —
// a partial list that silently skipped an entity on an ACL fault would misground the model.
func (a *RecordReadAdapter) ViewableEntities(ctx context.Context, scope services.RecordScope, tenantID string) ([]metadata.EntityDefinition, error) {
	ents, err := a.meta.ListEntities(tenantID)
	if err != nil {
		return nil, fmt.Errorf("ai.RecordReadAdapter.ViewableEntities: %w", err)
	}
	if scope.IsAdmin {
		return ents, nil
	}
	out := make([]metadata.EntityDefinition, 0, len(ents))
	for _, en := range ents {
		ok, verr := a.acl.CanViewEntity(ctx, tenantID, scope.RoleID, en.Name)
		if verr != nil {
			return nil, fmt.Errorf("ai.RecordReadAdapter.ViewableEntities: %w", verr)
		}
		if ok {
			out = append(out, en)
		}
	}
	return out, nil
}

// ListReports implements RecordReader: the tenant's saved reports — SAME surface the non-admin
// GET /reports catalog already exposes to every authenticated user (unfiltered names; running a
// report on a forbidden entity is refused by RunReport's own AuthorizeView). Mirroring the
// existing catalog keeps ⌘K and the Reports screen showing the same list — no new leak, no
// artificial divergence.
func (a *RecordReadAdapter) ListReports(ctx context.Context, _ services.RecordScope, tenantID string) ([]report.Record, error) {
	rows, err := a.reports.ListReports(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("ai.RecordReadAdapter.ListReports: %w", err)
	}
	return rows, nil
}

// RunReport implements RecordReader: executes a SAVED report definition under the caller's
// scope. ReportService.RunReport performs the per-entity AuthorizeView stage itself (fail-closed
// — same decision as browsing), so the scope is forwarded VERBATIM; overlay stays nil — the tool
// runs the definition exactly as reviewed (date narrowing belongs to ad-hoc aggregate questions).
func (a *RecordReadAdapter) RunReport(ctx context.Context, scope services.RecordScope, tenantID, id string) (services.ReportRunResult, error) {
	res, err := a.reports.RunReport(ctx, scope, tenantID, id, nil)
	if err != nil {
		// Keep ValidationError codes (REPORT_NOT_FOUND / REPORT_INACTIVE / FORBIDDEN …) unwrapped
		// for the tool-error envelope; wrap only infra faults.
		var ve *services.ValidationError
		if errors.As(err, &ve) || errors.Is(err, services.ErrEntityNotFound) {
			return services.ReportRunResult{}, err
		}
		return services.ReportRunResult{}, fmt.Errorf("ai.RecordReadAdapter.RunReport: %w", err)
	}
	return res, nil
}

// Fields implements RecordReader: the entity's field definitions, gated by the caller's "view"
// grant on that entity (ACL first, then metadata — a forbidden entity resolves to
// services.ErrEntityNotFound exactly like QueryRecords, no existence leak), MINUS the fields the
// caller's role may not view (sys_field_acl can_view=0 — the set RecordService's reads strip).
// Both consumers read from here: list_fields (a hidden field is not described to the model) and
// the propose_record_* validators (a hidden required field is not demanded by name, and a hidden
// name in a draft reads RECORD_UNKNOWN_FIELD, the answer a nonexistent field gets). The create is
// still refused at apply — POST /:entity says RECORD_REQUIRED_LOCKED without the name (queue
// #143). A grant-load failure refuses the listing (fail closed, never the unfiltered list).
func (a *RecordReadAdapter) Fields(ctx context.Context, scope services.RecordScope, tenantID, entity string) ([]metadata.FieldDefinition, error) {
	if !scope.IsAdmin {
		ok, err := a.acl.CanViewEntity(ctx, tenantID, scope.RoleID, entity)
		if err != nil {
			return nil, fmt.Errorf("ai.RecordReadAdapter.Fields: %w", err)
		}
		if !ok {
			return nil, services.ErrEntityNotFound
		}
	}
	ent, err := a.meta.GetEntityByName(ctx, tenantID, entity)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			// Normalize to the ONE not-found sentinel that crosses the RecordReader boundary.
			return nil, services.ErrEntityNotFound
		}
		return nil, fmt.Errorf("ai.RecordReadAdapter.Fields: %w", err)
	}
	fields, err := a.meta.ListFields(ctx, tenantID, ent.ID)
	if err != nil {
		return nil, fmt.Errorf("ai.RecordReadAdapter.Fields: %w", err)
	}
	hidden, err := a.records.HiddenFields(ctx, scope, tenantID, ent.Name)
	if err != nil {
		return nil, fmt.Errorf("ai.RecordReadAdapter.Fields: %w", err)
	}
	if len(hidden) == 0 {
		return fields, nil
	}
	visible := make([]metadata.FieldDefinition, 0, len(fields))
	for _, f := range fields {
		if _, isHidden := hidden[f.Name]; !isHidden {
			visible = append(visible, f)
		}
	}
	return visible, nil
}

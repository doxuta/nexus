// Package audit is the general mutation audit-trail adapter (../specs/029-audit-log/spec.md). It
// lives below the service layer: services depend only on the consumer-side Auditor
// interface and call Record, which returns immediately. The concrete AsyncDispatcher
// drains a buffered channel from a pool of worker goroutines and writes one append-only
// row to audit_log per committed mutation.
//
// Invariants (rules + spec §3/§5):
//   - Fail-open: a full buffer or a DB write error NEVER blocks, slows, or breaks the
//     originating business flow (record CRUD, admin entity/field/role changes). The
//     mutation has already committed; the audit row is best-effort. Worst case is a
//     dropped/failed row with a zap warning.
//   - Append-only: the writer only ever INSERTs; rows are never updated or deleted.
//   - Multi-tenant: every Event carries TenantID and the repository filters tenant_id on
//     every read/write — an audit row can never be read or written across tenants.
//   - No accidental secret leakage beyond the record data itself: diff carries only the
//     fields that changed. A field the admin flagged `options.sensitive = true` has its VALUE
//     replaced by "[redacted]" on both sides of the diff BEFORE the event reaches this package
//     (services.redactSensitiveDiff, at the RecordService mutation seams) — the key survives, so
//     the trail still records that the field changed, by whom and when. Reads stay gated behind
//     admin + read-side field-ACL on top of that.
package audit

import (
	"context"
	"encoding/json"
	"reflect"
	"time"
)

// Canonical mutation actions written to audit_log.action (spec §2).
const (
	ActionCreate  = "create"
	ActionUpdate  = "update"
	ActionDelete  = "delete"
	ActionRestore = "restore"
	// ActionShare / ActionUnshare: a record was manually shared with / unshared from one user
	// (specs/051-record-manual-share). Diff carries {"sharedWith": <userId>} (a person) or
	// {"sharedWithTeam": <teamId>} (a team, #224) on the after / before side, so the record's History
	// tab shows who opened it to whom.
	ActionShare   = "share"
	ActionUnshare = "unshare"
	// ActionHardDelete: an operator removed a record from the recycle bin for good
	// (../specs/056-record-hard-delete). Same shape as ActionDelete (before-only diff, sensitive
	// fields masked at the service seam). APPENDED next to the record's earlier rows, which are
	// never rewritten — the trail outlives the obj_* row (record_id carries no FK).
	ActionHardDelete = "hard_delete"
	// ActionDownload: an operator downloaded a per-customer backup file (specs/068-tenant-backup-
	// restore, TenantBackupMeta rows). The file carries the customer's real data, so who took it and
	// when must be answerable. After-side diff {backupId, kind, by}.
	ActionDownload = "download"
	// ActionRotate: a named credential's secret / extra headers / host were replaced
	// (../specs/050-named-credentials S4). The diff is built from the secret-free CredentialView.
	ActionRotate = "rotate"
)

// Reserved entity_name values for admin-meta mutations that have no obj_* record (spec §2).
// Record mutations use the real dynamic entity name ('accounts', ...) instead.
const (
	EntityMeta = "__entity__"
	FieldMeta  = "__field__"
	RoleMeta   = "__role__"
	TeamMeta   = "__team__"
	// FieldACLMeta marks W7 field-level-security config changes (sys_field_acl bulk save /
	// clear) — permission changes are governance events and belong in the same trail.
	FieldACLMeta = "__field_acl__"
	// UserMeta marks admin actions ON a user account that have no obj_* record — the W7
	// reset-link mint (a credential-granting act MUST be attributable; security W-1) and the
	// admin user create/update/delete (RM-19). Unlike the other meta keys, every __user__ row
	// carries RecordID = the TARGET user's id, so an isAdmin/roleId/status/password flip is
	// attributable to the account it happened to, not only to the actor.
	UserMeta = "__user__"
	// SSOProviderMeta marks admin mutations of a tenant's OIDC provider registration
	// (SSO-ADMIN-AUDIT) — create / policy-flip (enabled·trustIdpMfa) / delete. These are
	// governance-critical (trustIdpMfa=true makes SSO logins skip local MFA) and previously
	// left no trail. The provider's client secret NEVER appears in the diff: the emitted
	// snapshot is auth.SSOProvider, whose ClientSecretEnc field carries json:"-", so
	// StructToMap (json.Marshal) drops it before the diff is built.
	SSOProviderMeta = "__sso_provider__"
	// NamedCredentialMeta marks admin mutations of the named-credential vault
	// (../specs/050-named-credentials FR-012) — create / update / delete. RecordID = the key's id.
	// The diff is built from services.CredentialView, an explicit allowlist with no secret,
	// envelope, master-key id or extra-header VALUE (only the names).
	NamedCredentialMeta = "__named_credential__"
	// DoctorMeta marks Nexus Doctor repairs (DOCTOR-CORE, ../specs/008-nexus-doctor/spec.md §2.1:
	// "mỗi fix ghi audit trail"). One row per fixed finding; the after-side diff carries
	// {checkId, key, detail} — what was repaired, attributable to the admin who ran the fix.
	DoctorMeta = "__doctor__"
	// AppACLMeta marks a bulk ACL-backfill run over an app's nav (TRỤ 2 mục 4,
	// POST /admin/apps/:id/acl-backfill, services.AppService.ACLBackfill) — a deliberate,
	// hard-to-reverse grant-expansion action, so it needs a trail like every other admin-meta
	// mutation here. One row per backfill call (not per entity); the after-side diff carries
	// {appId, appName, seeded, skipped, failed}.
	AppACLMeta = "__app_acl__"
	// TenantMeta marks operator actions on a whole customer workspace — today the PURGE
	// (POST /admin/tenants/:id/purge), which permanently removes every row belonging to a
	// retired tenant. It is the single most irreversible action the engine offers, and it is
	// the one someone asks about months later ("where did that customer's data go"), so the
	// trail carries {slug, rows, tables} on the after side. The row lives in the OPERATOR's
	// tenant: the purged tenant's own audit_logs rows are part of what gets deleted.
	TenantMeta = "__tenant__"
	// JobMeta marks a customer admin's "Gửi lại" on a failed background job (DAILY-QUEUE #32,
	// POST /admin/jobs/:id/retry): resending a webhook re-delivers record data to a partner's
	// system, so who pressed it must be answerable. RecordID = the sys_jobs id (= the webhook's
	// X-Nexus-Delivery-Id); the diff carries {status, attempts, lastError} of the failed cycle on
	// the before side and the requeued state on the after side.
	JobMeta = "__job__"
	// EmailMeta marks a customer admin's "Gửi lại" on a failed rule email (specs/066-operate-gaps L2,
	// POST /admin/email-log/:id/resend): a resend mails a real person again, so who pressed it must be
	// answerable. RecordID = the failed sys_email_log row; the diff's after side carries
	// {resendOf, newLogId} — config ids only, never the address or the rendered text.
	EmailMeta = "__email__"
	// PreviewMeta marks a builder opening / ending a "preview as role" session
	// (specs/064-preview-as-role-sample-data S9-1). No new action value: start = ActionCreate
	// (after: {roleId, roleName, expiresAt, appId, draft}), end = ActionDelete (after: {reason,
	// roleId}); the FE picks the verb by (meta key, action). RecordID = the sys_preview_sessions id.
	// ActorUserID is ALWAYS the real builder, never the synthetic preview principal — the
	// customer's trail must name who looked at their app through which role.
	PreviewMeta = "__preview__"
	// WorkspaceSessionMeta marks a builder opening / closing a workspace session inside a customer
	// (specs/062-customer-workspace L1b, D-9: no new action value). open = ActionCreate (after:
	// {expiresAt} on the customer side; {tenantSlug, tenantName, expiresAt} on the builder's home
	// side), close = ActionDelete (before: {reason}). RecordID = the sys_workspace_sessions id.
	// Written TWICE per transition — once under the customer tenant (the customer sees who came in)
	// and once under the builder's home tenant — only by the call that actually changed state
	// (a reused/resumed open and a duplicate close write nothing). ActorUserID is the real builder.
	WorkspaceSessionMeta = "__workspace_session__"
	// LayoutMeta marks a screen-layout write (specs/062-customer-workspace L3: POST/PUT/DELETE
	// /admin/layouts). create = first save of a (entity, view), update = re-save / PUT, delete =
	// revert to the generated default. RecordID = the sys_layouts id. The diff is keyed
	// "<entityName>/<viewType>" so every row names the table and the screen even when only the
	// layout body changed (an update diff carries only the keys that differ).
	LayoutMeta = "__layout__"
	// ProjectMeta marks a builder changing the project registry (specs/062-customer-workspace L5):
	// PATCH /admin/projects/:tenantId (industry, "Chạy thử") and a health-check that rewrote the
	// stored snapshot. No new action value: ActionUpdate (project creation, L6, uses ActionCreate).
	// RecordID = the CUSTOMER's tenant id; the row lives in the builder's HOME tenant (the registry is
	// the workshop's book, not the customer's), so nothing here ever lands in a customer's trail.
	ProjectMeta = "__project__"
	// SampleDataMeta marks a builder seeding sample data (specs/064-preview-as-role-sample-data
	// S9-4; S9-5/S9-7 add the generator and the wipe). RecordID = the batch id (one AI apply). No new
	// action value: seeding = ActionCreate, after: {entity, source, mode, created, failed}. The rows
	// themselves also leave their ordinary per-record create audit.
	SampleDataMeta = "__sample_data__"
	// TenantBackupMeta marks per-customer backup / restore operations (specs/068-tenant-backup-
	// restore): create = ActionCreate, download = ActionDownload (restore, slice D, ActionRestore).
	// RecordID = the sys_tenant_backups id. Written under BOTH the operator's home tenant and the
	// customer's own tenant (the customer's trail must show that someone took / rewound its data);
	// the customer-side diff carries after.by = "Họ tên (Tên xưởng)" — never an operator user id,
	// email or the workshop's tenant id.
	TenantBackupMeta = "__tenant_backup__"
	// WorkflowRuleMeta / ReportMeta / DashboardMeta / BrandingMeta / AppMeta mark a create / update /
	// delete of a configuration document (specs/062-customer-workspace L4, FR-023). RecordID = the
	// document's id (branding: the tenant id — one document per tenant; an app's nav write is an
	// AppMeta update carrying a "nav" key). A workflow action's signing key and header values are
	// replaced by "[redacted]" BEFORE the diff is built. A change that only reached the builder's
	// draft writes nothing (services.emitDocAudit, FR-022).
	WorkflowRuleMeta = "__workflow_rule__"
	ReportMeta       = "__report__"
	DashboardMeta    = "__dashboard__"
	BrandingMeta     = "__branding__"
	AppMeta          = "__app__"
	// InboundHookMeta marks a customer's inbound webhook being created / edited / having its secret
	// rotated / deleted (specs/062 L4, FR-023). RecordID = the hook id. The hook's secret NEVER appears:
	// the snapshot is an allowlist and a rotation records only {secret: "[redacted]"}.
	InboundHookMeta = "__inbound_hook__"
	// PaymentConfigMeta marks a change of the bank account the customer's payment QR pays into
	// (PUT /admin/tenant/payment). Before/after carry the full bankBin / accountNumber / accountName:
	// where the money flows is exactly what someone asks about later. RecordID = the tenant id.
	PaymentConfigMeta = "__payment_config__"
)

// Event is the payload a service emits after a mutation commits. RecordID is empty for
// admin-meta mutations (entity/field) that have no obj_* record; a role's rows carry the role id. Diff is the
// {before,after} of only the fields that changed; create populates only after, delete only
// before. At is the originating event time (the dispatcher stamps the row clock instead, but
// At is carried for context/debug). All maps must be JSON-serializable.
type Event struct {
	TenantID    string    // owning tenant — pins every audit query
	ActorUserID string    // who performed the mutation (verified JWT subject)
	EntityName  string    // dynamic entity name, or one of the reserved meta keys
	RecordID    string    // affected record id; "" → stored as NULL (admin-meta)
	Action      string    // one of the Action* constants
	Diff        *Diff     // changed fields ({before,after}); nil → stored as NULL JSON
	At          time.Time // originating event time (context only)
}

// Diff is the serialized shape stored in audit_log.diff: only the keys that actually
// changed, split into the value before and after the mutation. For a create only After is
// populated; for a delete only Before. omitempty keeps the JSON compact so a one-sided diff
// does not store an empty counterpart object.
type Diff struct {
	Before map[string]any `json:"before,omitempty"`
	After  map[string]any `json:"after,omitempty"`
}

// Auditor is the consumer-side interface the service layer depends on. Record is
// non-blocking by contract: it enqueues the event and returns immediately, so a slow or
// down database can never stall a business flow. Implementations must be safe for
// concurrent use.
type Auditor interface {
	// Record schedules an audit row for asynchronous, append-only persistence and returns
	// at once. It never blocks on I/O and never returns an error: write problems (including
	// a full buffer) are handled out-of-band (logged) by the implementation.
	Record(ctx context.Context, evt Event)
}

// noopAuditor is the default Auditor wired into services so that legacy flows and tests run
// unchanged when auditing is disabled. It discards every event.
type noopAuditor struct{}

// NewNoopAuditor returns an Auditor that drops every event. It is the safe default for
// services (preserving prior behaviour) and for tests that do not exercise auditing.
func NewNoopAuditor() Auditor { return noopAuditor{} }

// Record discards the event. It deliberately does nothing.
func (noopAuditor) Record(context.Context, Event) {}

// StructToMap converts any JSON-serializable struct (or pointer to one) into a map[string]any
// keyed by the struct's json tags, so metadata mutations (entity / field / role definitions)
// can be diffed with ComputeDiff the same way record data maps are. A nil input or a marshal
// failure yields nil — the caller then records a nil/one-sided diff rather than panicking
// (fail-open even on the diff-building path).
func StructToMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// ComputeDiff compares two record/metadata maps and returns a Diff carrying only the keys
// whose value changed, with the before value under Before and the after value under After.
// Semantics (spec §2/§4):
//   - create: pass before=nil → After holds every after key, Before is empty.
//   - delete: pass after=nil  → Before holds every before key, After is empty.
//   - update: only keys whose value differs appear; an unchanged key is omitted from both
//     sides. A key present on one side only is treated as changed (added or removed).
//
// Equality uses reflect.DeepEqual so nested slices/maps (multi_select, currency, composite)
// compare structurally, not by pointer. ComputeDiff returns nil when nothing changed (both
// sides empty / identical) so the caller can store NULL diff rather than an empty object.
func ComputeDiff(before, after map[string]any) *Diff {
	d := &Diff{Before: map[string]any{}, After: map[string]any{}}

	// Every key that exists in either map is a candidate. Walk the union.
	seen := make(map[string]struct{}, len(before)+len(after))
	for k := range before {
		seen[k] = struct{}{}
	}
	for k := range after {
		seen[k] = struct{}{}
	}

	for k := range seen {
		bv, bok := before[k]
		av, aok := after[k]
		switch {
		case bok && aok:
			if !reflect.DeepEqual(bv, av) {
				d.Before[k] = bv
				d.After[k] = av
			}
		case bok && !aok:
			// Key removed (delete, or update that dropped a field).
			d.Before[k] = bv
		case !bok && aok:
			// Key added (create, or update that introduced a field).
			d.After[k] = av
		}
	}

	if len(d.Before) == 0 && len(d.After) == 0 {
		return nil
	}
	// Drop an empty side so a one-sided diff (create/delete) serializes cleanly with omitempty.
	if len(d.Before) == 0 {
		d.Before = nil
	}
	if len(d.After) == 0 {
		d.After = nil
	}
	return d
}

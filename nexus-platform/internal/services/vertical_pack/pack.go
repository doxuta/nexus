// Package vertical_pack implements export/install of a "vertical": a reusable bundle of
// metadata (entities, fields, relationships, layouts, roles) that can be lifted out of one
// tenant and installed into another. A vertical is the unit of vertical-market packaging —
// e.g. a CRM pack or a Clinic pack — so a fresh tenant can be provisioned with a complete,
// opinionated schema in one call instead of hand-building every entity/field/layout/role.
//
// The pack is a pure-data JSON document (no ids, no tenant_id — those are minted per-install)
// so the same pack installs cleanly into any tenant. Install reuses the existing entity /
// field / layout / role services so every install goes through the SAME validation, DDL,
// tenant-isolation and audit path as the hand-built admin endpoints — there is no second,
// weaker write path that could bypass a guard.
package vertical_pack

import (
	"encoding/json"
	"nexus/platform/internal/calendar"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/services"
)

// Pack is a self-contained vertical bundle. It carries NO ids and NO tenant_id: those are
// server-derived at install time so the same pack lands cleanly in any tenant. A pack
// exported from tenant A and installed into tenant B reproduces A's entity/field/relationship/
// layout/role shape under B's own ids and tenant scope.
type Pack struct {
	// Manifest is the self-describing header of the pack: name/slug/semver version/author, so a
	// consumer (Studio library, install-log) can identify WHAT + WHICH VERSION it is holding
	// without inspecting the entity list. It is OPTIONAL on install (install keys off entity
	// names, not the manifest) but, when present, its Version MUST be a valid semver (install
	// rejects a malformed one with VERTICAL_MANIFEST_INVALID_VERSION). Export always synthesises
	// one so an exported pack is self-identifying; every built-in pack ships one.
	Manifest *Manifest `json:"manifest,omitempty"`

	// Name / Description are human-facing pack metadata (e.g. "CRM", "Phòng khám"). They are
	// informational only — install keys off the entity names, not the pack name. Retained for
	// backward compatibility with packs exported before Manifest existed; Manifest.Name/Description
	// supersede them for display when present.
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`

	// Entities are the entity definitions WITH their fields. Relation fields (fieldType
	// "relation") are kept on their owning entity here AND surfaced separately in
	// Relationships for a denormalized, at-a-glance view of the graph; install reads them
	// off the entity's Fields (Relationships is advisory / for export consumers).
	Entities []PackEntity `json:"entities"`

	// Relationships is the denormalized list of every relation field across all entities —
	// a convenience projection so a consumer can see the entity graph without walking each
	// entity's field list. Install does NOT read this slice (it would double-create the
	// relation field); it is populated on export and ignored on install.
	Relationships []PackRelationship `json:"relationships"`

	// Layouts are the per-(entity,view) layout definitions. Each LayoutJSON is the decoded
	// layout config (sections/columns) the layout service validates against the entity's
	// live fields at install time.
	Layouts []PackLayout `json:"layouts"`

	// Roles are the RBAC roles + their grants. Each grant names an entity (or '*') + an
	// action + an access_level, validated by the role service on install.
	Roles []PackRole `json:"roles"`

	// NNRelationships are the explicit N-N join tables to create after all entities exist.
	// Install creates each join table (DDL) and registers its metadata row. Re-installing the
	// same pack is idempotent: RELATIONSHIP_ALREADY_EXISTS is treated as a skip.
	NNRelationships []PackNNRelationship `json:"nnRelationships,omitempty"`

	// Samples are the seed records inserted after all entities and N-N join tables exist.
	// Records are inserted per-entity in listed order. "@ref" values in Data are resolved to
	// real IDs of previously inserted records before the insert. Samples are NOT idempotent:
	// re-installing the same pack inserts duplicates (seeding is a one-time bootstrap step).
	Samples []PackSample `json:"samples,omitempty"`

	// Links are the N-N join-table rows to create after all samples exist. Each link
	// resolves its FromRef and ToRef to real IDs before calling Link. PACK-EXPORT-FULL: Export
	// emits them from the tenant's rel_<a>_<b> tables when ?includeData=true (a link whose
	// endpoint record is not in Samples — soft-deleted or beyond the export cap — is dropped
	// WITH a warning, never silently).
	Links []PackLink `json:"links,omitempty"`

	// Dashboards are the saved dashboard definitions to seed after all entities and sample
	// records exist. Install creates each missing dashboard by name (idempotent: a dashboard
	// with the same name already in the tenant is skipped). PACK-EXPORT-FULL: Export populates
	// this from the tenant's saved dashboards — widgets on entities OUTSIDE the export selection
	// are dropped per-widget (with a warning); a dashboard whose widgets ALL referenced excluded
	// entities is dropped whole (with a warning).
	Dashboards []PackDashboard `json:"dashboards,omitempty"`

	// DeferredFields are fields added AFTER all entities and N-N relationships exist (install
	// Pass 2b) — needed for parent-side rollup fields whose childEntity / relationship cannot
	// exist yet at the parent's CreateEntity time (crm_accounts is created FIRST by dependency
	// order, so an inline rollup over crm_opportunities would fail config-time validation with
	// FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND; an N-N rollup additionally needs its relationship
	// registered in Pass 2). Re-installing is idempotent: FIELD_ALREADY_EXISTS is a skip.
	DeferredFields []PackDeferredField `json:"deferredFields,omitempty"`

	// Workflows are the automation rules (SLA timers, routing/field-update rules) the pack ships.
	// Install creates them LAST — after entities, samples AND dashboards — because seeding records
	// (Pass 3) goes through RecordService.Create, which fires the workflow dispatcher: any workflow
	// created BEFORE the samples would trigger on the seed inserts. Each workflow goes through the
	// SAME full config-time validation (buildWorkflowRule: trigger/timer/condition/action + the
	// WF-01 field-target guard) as POST /admin/workflows, so a pack rule referencing a missing
	// field/team fails install with the real error code. Install is idempotent by NAME: a workflow
	// whose name already exists in the tenant is skipped. PACK-EXPORT-FULL: Export populates this
	// from the tenant's workflow rules on the selected entities — an assign action's team ID is
	// resolved back to its NAME (and the team added to Teams below); a rule that cannot round-trip
	// (business-hours calendar reference, create_record into an excluded entity, unresolvable team)
	// is dropped WITH a warning naming why, never silently.
	//
	// A pack's assign action (round-robin routing) names its team by NAME, not id (a pack cannot
	// know the installed team's id); the teams pass builds a name→id map and the workflows pass
	// resolves it before CreateWorkflow — see Teams below and toWorkflowInput.
	Workflows []PackWorkflow `json:"workflows,omitempty"`

	// Teams are the teams the pack seeds (PACK-TEAMS) so it can ship an `assign` routing workflow:
	// the assign action targets a team by NAME, and a pack cannot know the installed team's id. The
	// teams pass runs AFTER roles and BEFORE workflows so the workflows pass can resolve each assign
	// action's team NAME → the seeded (or pre-existing) team id. Install is idempotent by NAME: a
	// team whose name already exists in the tenant is skipped (its id is still mapped for routing).
	// A newly-created team gets the INSTALL USER added as its first member so round-robin has ≥1
	// active member immediately. PACK-EXPORT-FULL: Export populates this with the teams referenced
	// by the exported workflows' assign actions (by NAME, matching the installer's pass-6b
	// name-resolution) — never the tenant's whole team tree.
	Teams []PackTeam `json:"teams,omitempty"`

	// WorkQueues are the "Việc Hôm Nay" queue definitions the pack ships (PACK-QUEUES,
	// ../specs/012-work-queues/spec.md §5) so a freshly-installed vertical answers "hôm nay tôi phải làm gì?"
	// from day 1. Each queue goes through the SAME config-time validation as POST /admin/work-queues
	// (services.WorkQueueService.Create: entity/field/operator/date-token/sort/role checks), so a
	// pack queue referencing a missing field fails install with the real error code. A queue may
	// target a pack role by NAME (RoleName) — install resolves it to the created (or pre-existing)
	// role's id; an unresolvable role name FAILS install (like a workflow assign team). Install is
	// idempotent by NAME (sys_work_queues HAS UNIQUE(tenant_id,name), but list-then-skip keeps
	// re-install clean instead of surfacing a 409). PACK-EXPORT-FULL: Export populates this from
	// the tenant's active queues, resolving role_id back to the role NAME; an inactive queue or one
	// whose role no longer exists is dropped WITH a warning, never silently.
	WorkQueues []PackWorkQueue `json:"workQueues,omitempty"`

	// Reports are the saved reports (Salesforce summary/matrix + Metabase saved-question analogues)
	// the pack ships, so a freshly-installed vertical tenant gets working saved reports out of the
	// box — not just entities + dashboards. Install creates them in a pass AFTER samples, dashboards
	// AND workflows (order is immaterial for correctness — a report is a stored DEFINITION that only
	// RUNS on demand and fires no dispatcher — but keeping it LAST-ish mirrors the dashboards/
	// workflows convention). Each report goes through the SAME config-time validation as
	// POST /admin/reports (services.ReportService.CreateReport → ValidateReportConfig), so a pack
	// report with a malformed shape (bad type, summary without groupBy/metric, unknown entity) fails
	// install with the real error code — there is no second, weaker write path. Install is idempotent
	// by NAME: a report whose name already exists in the tenant is skipped (sys_reports has no
	// UNIQUE(tenant_id,name), so — like dashboards/workflows — the skip is list-then-skip-by-name +
	// intra-pack dedup). PACK-EXPORT-FULL: Export populates this from the tenant's saved reports
	// on the selected entities; joins/sort travel too (queue #720).
	Reports []PackReport `json:"reports,omitempty"`

	// Calendars are the working calendars (business hours + holidays) the pack's rules read
	// (queue #720). A calendar travels BY NAME: the rules that pin one carry calendarName, install
	// creates the calendar (skip when the target already has that name) and resolves the name to
	// its own id. The source's DEFAULT calendar rides along when a rule reads the default; it
	// becomes the target's default only when the target has none.
	Calendars []PackCalendar `json:"calendars,omitempty"`

	// Views are the SHARED saved list views (tenant-wide presets: filters, sort, columns) of the
	// selected entities (queue #720). Personal views never travel — they belong to a person of the
	// source tenant. The config is the opaque document the list screen stores; it names fields, not
	// ids, so it re-applies in the target tenant. Install creates them through ViewService (shared =
	// admin-only, same validation as POST /views), skipping a (entity, name) the tenant already has.
	Views []PackView `json:"views,omitempty"`

	// Approvals are the approval processes the pack ships (PACK-APPROVALS, ../specs/027-handover-wizard/
	// spec.md §3.2 — closes the F6b silent-loss: an app's approval flow used to vanish on handover).
	// Install creates them in a pass AFTER workflows through the SAME validated path as
	// POST /admin/approval-processes (ApprovalService.CreateProcess), so a malformed pack process
	// fails install with the real error code. Idempotent by (entityName, name): a process already
	// present in the tenant under that pair is skipped. Portability: a step's approver travels as a
	// role NAME (roles are already in the pack) — install resolves name → the TARGET tenant's role
	// id; a name that resolves nowhere is a per-item WARNING + skip (fail-open like appMint, never
	// failing the committed install). approverType "user" is never exported (a user id is not
	// portable across tenants — export warns and drops the whole process); a hand-authored pack
	// carrying one is skipped at install with the same warning.
	Approvals []PackApproval `json:"approvals,omitempty"`

	// InboundHooks are the webhook receivers a pack ships (PACK-HOOKS). They carry NO SECRET —
	// install mints a fresh one per tenant. That is the whole design: a hook's secret is a live
	// signing credential, so exporting it would hand customer B a working key for customer A's
	// endpoint, and it would land at rest a second time in sys_pack_items (provenance stores the
	// canonical JSON of every pack-side struct). Portability here is by REGENERATION, not by
	// reference resolution — stronger than the role-name substitution PackApproval uses, because
	// this is a credential rather than a pointer.
	//
	// Consequence the install surfaces rather than hides: the admin must fetch the NEW secret from
	// Cài đặt before telling the partner to send. Install emits a warning naming each hook.
	InboundHooks []PackInboundHook `json:"inboundHooks,omitempty"`

	// Credentials are the vault key SLOTS the pack's outbound_webhook rules point at (specs/050 S6),
	// by name: label, base URL, auth type and the NAMES of the headers — never a secret, a header
	// value, a hint or the source tenant's key id. Install reuses a same-named key of the target or
	// creates an empty one (needs_secret) and warns the admin to enter the secret.
	Credentials []PackCredential `json:"credentials,omitempty"`

	// FieldACLs are the field-level restrictions a pack carries (PACK-FIELD-ACL). Keyed by ROLE
	// NAME, not role id, for the same reason approvals are: ids do not survive a tenant boundary.
	//
	// Without this section field-level security was SILENTLY DROPPED at handover — a field hidden
	// from a role in the factory tenant became fully visible to the same-named role in the
	// customer's. Nothing failed and nothing warned; the strictest configuration in the app was the
	// one thing the pack did not carry.
	FieldACLs []PackFieldACL `json:"fieldAcls,omitempty"`

	// DemoSamples marks Pass 3's sample rows as SAMPLE data (specs/064 S9-6): source "pack", batch =
	// the install's ledger id — so the builder can count, regenerate and wipe them. It has NO JSON
	// name on purpose: only BuiltinPack (code we ship) turns it on, so a pack a client uploads can
	// never claim the mark or smuggle rows past the sample-data surface.
	DemoSamples bool `json:"-"`
}

// PackFieldACL is one field restriction, addressed by role NAME + entity + field. canView=false
// hides the field from that role; canEdit=false makes it read-only. A field with no row here is
// fully visible and editable (default-allow, same as sys_field_acl).
type PackFieldACL struct {
	RoleName   string `json:"roleName"`
	EntityName string `json:"entityName"`
	FieldName  string `json:"fieldName"`
	CanView    bool   `json:"canView"`
	CanEdit    bool   `json:"canEdit"`
}

// PackInboundHook mirrors the POST /admin/inbound-hooks body MINUS the secret (see Pack.InboundHooks).
// Idempotency key is the hook NAME (hooks are UNIQUE per (tenant, name), so a re-install must skip
// rather than 409). Two entity references need resolving in the target tenant: EntityName (where
// each delivery is written) and, when present, PaymentMatch.TargetEntity (the record whose status
// flips) — an unresolvable one warns and skips, matching the approvals pass, because by the time
// hooks install the entities and samples have already committed and late passes do not roll back.
type PackInboundHook struct {
	Name         string                `json:"name"`
	EntityName   string                `json:"entityName"`
	Mapping      []PackHookMapping     `json:"mapping"`
	MatchField   string                `json:"matchField,omitempty"`
	PaymentMatch *PackHookPaymentMatch `json:"paymentMatch,omitempty"`
	IsActive     *bool                 `json:"isActive,omitempty"`
}

// PackHookMapping is one payload→field extraction rule (dot-path source → target field name).
type PackHookMapping struct {
	Source string `json:"source"`
	Field  string `json:"field"`
}

// PackHookPaymentMatch is the bank-credit matching step. Field-for-field the runtime shape
// (inboundhook.PaymentMatch) — every member is a NAME or a pattern, nothing tenant-scoped, so it
// travels as-is. RefField must be a UNIQUE field of TargetEntity in the target tenant; the service
// validator enforces that at install time and the pass warns+skips rather than failing.
type PackHookPaymentMatch struct {
	TargetEntity string `json:"targetEntity"`
	RefField     string `json:"refField"`
	RefPattern   string `json:"refPattern"`
	MemoSource   string `json:"memoSource"`
	StatusField  string `json:"statusField"`
	StatusValue  string `json:"statusValue"`
	AmountSource string `json:"amountSource,omitempty"`
	AmountField  string `json:"amountField,omitempty"`
}

// PackApproval is one approval process a pack ships. It mirrors the POST /admin/approval-processes
// request body ({name, entityName, trigger, criteria?, steps, isActive}) with ONE portability
// substitution: each step carries ApproverRef — a role NAME — instead of the API's approverId
// (a raw role/user id from the source tenant would be meaningless in the target). Trigger is one
// of after_create/after_update/manual (bpm.ValidTrigger); isActive defaults to true when omitted.
// The (entityName, name) pair is the idempotency key.
//
// Outcome (specs/043 S0) is the terminal write-back — it used to be dropped on export, so a
// process that set the quote to "Chấp nhận" installed as one that writes nothing. Two keys are NOT
// here yet (specs/043 S3a2): `options` — its column exists since 000070 but no option has an engine
// behind it (lane S12b, calendar/pause S13, fallback S6a), and `graph` — the API stores graph-form
// processes since S3a2, but a portable node shape (role NAMES per node) arrives with parallel
// approvers in S4 (T083); export leaves a graph process out by name. A pack that carries either is
// refused before any write, not half-installed (UnmarshalJSON records them, approvalShapeGate
// refuses them).
type PackApproval struct {
	Name       string                `json:"name"`
	EntityName string                `json:"entityName"`
	Trigger    string                `json:"trigger"`
	Criteria   *PackApprovalCriteria `json:"criteria,omitempty"`
	Steps      []PackApprovalStep    `json:"steps"`
	IsActive   *bool                 `json:"isActive,omitempty"`
	Outcome    *PackApprovalOutcome  `json:"outcome,omitempty"`
	// unsupported: keys the pack JSON carried outside this shape (graph, options…). buildPlan's
	// approvalShapeGate refuses them (D-7) before any write, for install and dry-run alike —
	// never persisted, never serialized.
	unsupported []string
}

// packApprovalKeys / packApprovalStepKeys are the closed pack wire shapes of an approval.
var (
	packApprovalKeys     = []string{"name", "entityName", "trigger", "criteria", "steps", "isActive", "outcome"}
	packApprovalStepKeys = []string{"id", "name", "approverType", "approverRef", "slaSeconds", "slaClock", "approvers", "completion", "completionPercent", "assignment", "onReject"}
)

// UnmarshalJSON decodes the pack shape and records any key outside it. The rest of the pack format
// tolerates unknown keys; an approval does not, because a process configured for a capability
// this engine does not run (parallel approvers, a graph) would otherwise install as a plain
// sequential process that approves on the wrong terms.
func (a *PackApproval) UnmarshalJSON(b []byte) error {
	type plain PackApproval
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*a = PackApproval(p)
	a.unsupported = services.UnsupportedApprovalKeys(b, packApprovalKeys...)
	return nil
}

// PackApprovalOutcome is the pack shape of the terminal write-back (bpm.Outcome): the field on the
// approved record and the value written on approve / reject. Field NAMES and option VALUES are
// portable as-is (metadata names, not ids).
type PackApprovalOutcome struct {
	StatusField   string `json:"statusField"`
	ApprovedValue string `json:"approvedValue,omitempty"`
	RejectedValue string `json:"rejectedValue,omitempty"`
	// ReturnedValue: written when a rejection returns the record to its submitter (specs/043 S8).
	ReturnedValue string `json:"returnedValue,omitempty"`
}

// PackApprovalCriteria is the optional entry-criteria block ({match, rules}) of a pack approval.
// Rules reuse the workflow rule shape ({field, op, value}) — approval criteria share the workflow
// condition vocabulary end-to-end (services.buildApprovalCriteriaJSON), so one pack type serves both.
type PackApprovalCriteria struct {
	Match string             `json:"match,omitempty"`
	Rules []PackWorkflowRule `json:"rules,omitempty"`
}

// PackApprovalStep is one sequential approval step. ApproverType is "role" in every exported pack
// (user approvers are not portable — export drops those processes with a warning); ApproverRef is
// the role NAME install resolves to the target tenant's role id. SLASeconds ≥ 0 (0 = no step SLA).
// ID is the step's stable id (specs/043 S0), carried so the installed process keeps the ids its
// own history and later versions refer to; a pack authored without ids gets positional ones.
type PackApprovalStep struct {
	ID           string `json:"id,omitempty"`
	Name         string `json:"name,omitempty"`
	ApproverType string `json:"approverType"`
	ApproverRef  string `json:"approverRef"`
	SLASeconds   int    `json:"slaSeconds,omitempty"`
	// SLAClock is what SLASeconds counts (specs/043 S1): "business" (the TARGET tenant's working
	// calendar) or "wall"; absent = business. Portable as-is — a word, not an id.
	SLAClock string `json:"slaClock,omitempty"`
	// Approvers + Completion (specs/043 S4): a step held by several people INSTEAD of
	// approverType/approverRef. Every approver is a role NAME (a person is not portable) and
	// install resolves each to the target tenant's role id; Completion is "all" | "any".
	Approvers  []PackApprover `json:"approvers,omitempty"`
	Completion string         `json:"completion,omitempty"`
	// CompletionPercent (1..100) goes with completion "quorum" (specs/043 S6b).
	CompletionPercent int `json:"completionPercent,omitempty"`
	// Assignment (specs/043 S6b): the step is handed to a role / team / work queue BY NAME, or by a
	// rule (the person in a record field, the submitter), INSTEAD of approverType/approverRef and
	// approvers. A team may be dealt out by a strategy; an each_member team carries the completion
	// policy above. A named person is not portable (type "user" skips the process, warned).
	Assignment *PackAssignment `json:"assignment,omitempty"`
	// OnReject (specs/043 S8): where a rejection of this step goes. "to" is "@end", "@submitter" or
	// the id of an EARLIER step of the same process; portable as-is (ids are the pack's own).
	OnReject    *PackOnReject `json:"onReject,omitempty"`
	unsupported []string
}

// PackOnReject is a pack step's answer to a rejection (bpm.OnReject).
type PackOnReject struct {
	To       string `json:"to"`
	ResumeAt string `json:"resumeAt,omitempty"`
}

// packOnRejectInput maps a pack step's onReject to the service input (nil stays nil).
func packOnRejectInput(o *PackOnReject) *services.ApprovalOnRejectInput {
	if o == nil {
		return nil
	}
	return &services.ApprovalOnRejectInput{To: o.To, ResumeAt: o.ResumeAt}
}

// packOnRejectOf is the pack form of node i's stored onReject (nil when it has none).
func packOnRejectOf(nodes []*bpm.OnReject, i int) *PackOnReject {
	if i >= len(nodes) || nodes[i] == nil {
		return nil
	}
	return &PackOnReject{To: nodes[i].To, ResumeAt: nodes[i].ResumeAt}
}

// PackAssignment is the pack form of bpm.Assignment: Type is role | team | queue (Ref = the NAME,
// resolved to the target tenant's id on install), field (Field = the record field, or owner_id) or
// submitter. Strategy is only meaningful on a team (pool | each_member | round_robin | load_based).
type PackAssignment struct {
	Type     string `json:"type"`
	Ref      string `json:"ref,omitempty"`
	Field    string `json:"field,omitempty"`
	Strategy string `json:"strategy,omitempty"`
}

// PackApprover is one approver of a parallel pack step: Type is "role", Ref the role NAME.
type PackApprover struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

// UnmarshalJSON records step keys outside the pack step shape — see PackApproval.UnmarshalJSON.
func (s *PackApprovalStep) UnmarshalJSON(b []byte) error {
	type plain PackApprovalStep
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*s = PackApprovalStep(p)
	s.unsupported = services.UnsupportedApprovalKeys(b, packApprovalStepKeys...)
	return nil
}

// PackWorkQueue is one "Việc Hôm Nay" work queue a pack ships (PACK-QUEUES). It mirrors the
// POST /admin/work-queues request body in pack-authored JSON minus RoleID: a pack cannot know the
// installed role's id, so it names the role (RoleName) and install resolves name→id via the roles
// pass. The name is the idempotency key. SortOrder defaults to "asc" and ActionVerb to "open"
// when omitted (the service's own defaults — diff canonicalization fills them the same way).
type PackWorkQueue struct {
	Name       string            `json:"name"`
	Label      string            `json:"label"`
	EntityName string            `json:"entityName"`
	RoleName   string            `json:"roleName,omitempty"` // "" = visible to everyone
	Filters    []PackQueueFilter `json:"filters,omitempty"`
	SortField  string            `json:"sortField,omitempty"`
	SortOrder  string            `json:"sortOrder,omitempty"`  // asc|desc (default asc)
	ActionVerb string            `json:"actionVerb,omitempty"` // open|approve|claim (default open)
	Position   int               `json:"position,omitempty"`
}

// PackQueueFilter is one stored queue predicate {field, op, value}. It mirrors
// workqueue.QueueFilter's wire shape exactly (between/in values comma-joined in Value; relative
// date tokens like "today"/"this_month" ride through verbatim and are validated at install).
type PackQueueFilter struct {
	Field    string `json:"field"`
	Operator string `json:"op,omitempty"` // empty == eq
	Value    string `json:"value,omitempty"`
}

// PackTeam is one team a pack seeds. It carries only a NAME (no id, no tenant — those are minted
// per-install). The name is the idempotency key (list-then-skip-by-name, mirroring dashboards) AND
// the handle an `assign` workflow action references so the workflows pass can resolve it to the
// seeded team's real id.
type PackTeam struct {
	Name string `json:"name"`
}

// Manifest is the pack's self-describing header. Version is a semantic version string
// ("MAJOR.MINOR.PATCH", optionally with a "-prerelease" and/or "+build" suffix); install
// validates its format when a manifest is supplied. Slug is a stable machine key ("crm",
// "clinic") used to record installs and offer built-in packs; MinEngine is an optional lower
// bound on the platform version a pack needs (informational — not yet enforced). All fields
// except Version are advisory.
type Manifest struct {
	Name        string `json:"name"`
	Slug        string `json:"slug,omitempty"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
	MinEngine   string `json:"minEngine,omitempty"`
	// Icon is the lucide icon name for the sidebar app install mints from this pack (PACK-EXPORT-
	// APPSLUG). Advisory: mintApp falls back to a slug-derived icon when empty, so legacy packs are
	// unaffected. Capped at CreateApp's 50-rune icon bound on install.
	Icon string `json:"icon,omitempty"`
}

// PackDeferredField is a field added AFTER all entities and N-N relationships exist
// (Pass 2b). Needed for parent-side rollup fields whose childEntity / relationship
// cannot exist yet at the parent's CreateEntity time.
type PackDeferredField struct {
	EntityName string    `json:"entityName"`
	Field      PackField `json:"field"`
}

// PackEntity is one entity plus all of its fields (ordinary, unique, indexed, and relation).
type PackEntity struct {
	Name        string      `json:"name"`
	Label       string      `json:"label"`
	Description string      `json:"description,omitempty"`
	Fields      []PackField `json:"fields"`
}

// PackField is a field definition stripped of ids/tenant. Options carries the per-type nested
// config (picklist values, relation target, currency code, etc.) exactly as the field service
// expects it.
type PackField struct {
	Name       string         `json:"name"`
	Label      string         `json:"label"`
	FieldType  string         `json:"fieldType"`
	IsRequired bool           `json:"isRequired,omitempty"`
	IsUnique   bool           `json:"isUnique,omitempty"`
	Options    map[string]any `json:"options,omitempty"`
}

// PackRelationship is the denormalized view of a relation field: the owning (child) entity,
// the field that holds the FK, and the target (parent) entity. Type is always "1-N" in the
// current FK-on-child model. This mirrors the engine's relation model (a relation is a field,
// not a sys_relationships row) so export/install stay consistent with how relations actually work.
type PackRelationship struct {
	Name         string `json:"name"`         // the relation field name on the child entity
	ChildEntity  string `json:"childEntity"`  // entity that owns the relation field (holds <field>_id)
	ParentEntity string `json:"parentEntity"` // options.relatedEntity (the FK target)
	Type         string `json:"type"`         // "1-N"
	// ViewConfig is the related-list ("subpanel") config pinned on the registry row (queue #720);
	// install applies it through RelationLifecycleService.SetViewConfig — same validation as the admin PUT.
	ViewConfig json.RawMessage `json:"viewConfig,omitempty"`
}

// PackLayout is one stored layout (entity + view + decoded layout config).
type PackLayout struct {
	EntityName string         `json:"entityName"`
	ViewType   string         `json:"viewType"`
	LayoutJSON map[string]any `json:"layoutJson"`
}

// PackRole is one RBAC role and its grants.
type PackRole struct {
	Name    string           `json:"name"`
	Label   string           `json:"label,omitempty"`
	Actions []PackRoleAction `json:"actions"`
}

// PackRoleAction is one grant: action on entity ('*' allowed) at an access level.
type PackRoleAction struct {
	EntityName  string `json:"entityName"`
	Action      string `json:"action"`
	AccessLevel string `json:"accessLevel,omitempty"`
}

// PackNNRelationship describes an N-N relationship between two entities in the pack.
// Install creates the join table (rel_<a>_<b>, canonical alphabetical order) and registers
// the metadata row. The Name is the stable slug used when calling Link (e.g.
// "crm_contacts_opportunities"). EntityA and EntityB are entity names; order is
// insignificant — JoinParts sorts them alphabetically.
type PackNNRelationship struct {
	Name    string `json:"name"`
	EntityA string `json:"entityA"`
	EntityB string `json:"entityB"`
	// ViewConfig: see PackRelationship.ViewConfig.
	ViewConfig json.RawMessage `json:"viewConfig,omitempty"`
}

// PackSample holds the seed records for one entity. Records are inserted in the order
// listed, after all entities and N-N join tables have been created. Each record may carry
// a Ref (a symbolic "@key" handle) so subsequent records or N-N Links can reference the
// freshly minted UUID without knowing it in advance.
type PackSample struct {
	Entity  string       `json:"entity"`
	Records []PackRecord `json:"records"`
}

// PackRecord is one seed record. Ref is the local symbolic key (e.g. "@acme"); it is NOT
// written to the database — it is only used to resolve cross-references within the pack.
// Data values that start with "@" are resolved to the actual ID of the referenced record
// before the record is written (so a contact can reference an account by "@acme" and get
// the real UUID). Unresolved "@" references abort the install with a clear error.
type PackRecord struct {
	Ref  string         `json:"ref,omitempty"` // "@key" handle, no persisting
	Data map[string]any `json:"data"`
}

// PackLink is one N-N join-table row: it links the record identified by FromRef to the
// record identified by ToRef via the named N-N relationship. Both refs must have been
// defined in a preceding PackSample (or an earlier PackLink for that matter, though
// link-to-link self-bootstrapping is not a common pattern). Relation is the Pack's
// PackNNRelationship.Name (the stable slug), not the physical join-table name.
type PackLink struct {
	Relation string `json:"relation"` // PackNNRelationship.Name
	FromRef  string `json:"fromRef"`  // "@key" of the source record
	ToRef    string `json:"toRef"`    // "@key" of the target record
}

// PackDashboard is one dashboard definition in the pack: a human name and an ordered list of
// widgets. The name is the idempotency key used by install (list-then-skip-by-name, mirroring
// the roles pattern). Widgets carry NO id (install mints them at persist time). Every widget
// must use a metric/type the real DashboardService already validates (count/sum/avg/min/max;
// kpi/bar/line/table) and a GroupBy that resolves to an indexed column at run time.
type PackDashboard struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Widgets     []PackWidget `json:"widgets"`
}

// PackWidget is one widget in a PackDashboard. It mirrors services.WidgetConfig but lives in
// the pack layer (no import of the services package — pack.go has zero dependencies). Install
// maps each PackWidget to a services.WidgetConfig before calling the DashboardWriter.
// PACK-EXPORT-FULL: Bucket, the react-grid-layout coordinates (X/Y/W/H — emitted
// unconditionally like WidgetConfig so an origin widget round-trips its position) and Filters
// are carried too, so an exported dashboard re-installs with its full arrangement, not just its
// metric queries. Built-in packs may omit them (zero coords = the FE's "unset" sentinel).
type PackWidget struct {
	ID      string `json:"id"`                // stable slug used as widget id (e.g. "w1")
	Title   string `json:"title"`             // human label shown on the dashboard card
	Type    string `json:"type"`              // render hint: kpi|bar|line|table|funnel
	Entity  string `json:"entity"`            // dynamic entity name (e.g. "crm_opportunities")
	Metric  string `json:"metric"`            // count|sum|avg|min|max
	Field   string `json:"field,omitempty"`   // required for sum/avg/min/max
	GroupBy string `json:"groupBy,omitempty"` // presence → grouped result; must be indexed
	Bucket  string `json:"bucket,omitempty"`  // W4: day|week|month|quarter|year over a date GroupBy
	// react-grid-layout coordinates (DASH-WIDGET-SCHEMA). No omitempty: x=0/y=0 is a real position.
	X       int                `json:"x"`
	Y       int                `json:"y"`
	W       int                `json:"w"`
	H       int                `json:"h"`
	Filters []PackReportFilter `json:"filters,omitempty"` // same filter dialect as reports
}

// PackReport is one saved report a pack ships. It mirrors the POST /admin/reports request body in
// pack-authored JSON so a pack report is EXACTLY what the admin API accepts; install maps it to
// services.CreateReportInput and runs the SAME config-time validation. The name is the idempotency
// key (list-then-skip-by-name, mirroring dashboards/workflows). Type is one of summary/matrix/
// tabular. Config carries the flat report definition (metric/field/groupBy/columns/filters/bucket).
// isActive defaults to true when omitted. It carries NO id/tenant (minted per-install).
type PackReport struct {
	Name       string           `json:"name"`
	EntityName string           `json:"entityName"`
	Type       string           `json:"type"` // summary|matrix|tabular
	Config     PackReportConfig `json:"config"`
	IsActive   *bool            `json:"isActive,omitempty"`
}

// PackReportConfig is the flat report config a pack ships — the subset of services.ReportConfig a
// pack authors (no id/tenant). It mirrors the report input's shape field-for-field:
//   - summary/matrix: GroupBy + Metric (+ Field for sum/avg/min/max); Bucket optional over a date
//     GroupBy; Filters optional; matrix: ColumnGroup optional (the cross-tab column axis);
//     summary: SubGroups optional (≤ 2 further levels — a tree with sub-totals); both: Having
//     optional (the threshold on the leaf groups).
//   - tabular: Columns (the projection); Filters optional.
//
// Sort and Joins ride along (queue #720): a join names its relationship (a tenant-scoped
// sys_relationships name, which the same pack installs), so it resolves in the target tenant.
// install maps this to services.ReportConfig before calling CreateReport.
type PackReportConfig struct {
	Metric  string             `json:"metric,omitempty"`  // count|sum|avg|min|max (summary/matrix)
	Field   string             `json:"field,omitempty"`   // required for sum/avg/min/max
	GroupBy string             `json:"groupBy,omitempty"` // required for summary/matrix; must be indexed at run
	Columns []string           `json:"columns,omitempty"` // the projection (tabular)
	Filters []PackReportFilter `json:"filters,omitempty"` // optional filter predicates (both types)
	Bucket  string             `json:"bucket,omitempty"`  // day|week|month|quarter|year over a date GroupBy
	// ColumnGroup is the matrix column axis (specs/049 S2b) — same shape as services.ReportGroupInput.
	ColumnGroup *PackReportGroup `json:"columnGroup,omitempty"`
	// SubGroups are a summary's next grouping levels (specs/049 S3), in order.
	SubGroups []PackReportGroup `json:"subGroups,omitempty"`
	// Having is the threshold on the leaf groups (specs/049 S5) — same shape as
	// services.ReportHavingInput; install validates it through the admin path.
	Having *PackReportHaving `json:"having,omitempty"`
	// Sort is a tabular report's ordering; Joins the (at most one) cross-object hop of a summary/matrix.
	Sort  *PackReportSort  `json:"sort,omitempty"`
	Joins []PackReportJoin `json:"joins,omitempty"`
}

// PackReportSort mirrors services.ReportSortInput.
type PackReportSort struct {
	Field string `json:"field"`
	Dir   string `json:"dir,omitempty"`
}

// PackReportJoin mirrors services.ReportJoinInput (relationship by name).
type PackReportJoin struct {
	Relationship string `json:"relationship"`
	Optional     bool   `json:"optional,omitempty"`
}

// PackReportHaving is a pack report's threshold: on value|count, an operator, 1–2 decimal values.
type PackReportHaving struct {
	On     string   `json:"on"`
	Op     string   `json:"op"`
	Values []string `json:"values"`
}

// PackReportGroup is one extra GROUP BY axis of a pack report config (field + optional bucket).
type PackReportGroup struct {
	Field  string `json:"field"`
	Bucket string `json:"bucket,omitempty"`
}

// PackReportFilter is one filter predicate {field, op, values} of a pack report config. It mirrors
// services.ReportFilterInput exactly, so a pack filter reuses the SAME list/aggregate filter engine
// (empty Op == eq). install maps each to services.ReportFilterInput before CreateReport.
type PackReportFilter struct {
	Field  string   `json:"field"`
	Op     string   `json:"op,omitempty"`     // empty == eq
	Values []string `json:"values,omitempty"` // one (eq/neq) or more (in/between)
}

// PackWorkflow is one automation rule shipped by a pack. It mirrors the POST /admin/workflows
// request body (workflowRequest) in pack-authored JSON so a pack rule is EXACTLY what the admin
// API accepts; install maps it to services.CreateWorkflowInput and runs the same validation. The
// name is the idempotency key (list-then-skip-by-name, mirroring dashboards/roles). trigger is one
// of after_create/after_update/after_delete/scheduled/date_offset; condition is optional (nil =
// match-all); at least one action is required. isActive defaults to true when omitted.
type PackWorkflow struct {
	Name       string                 `json:"name"`
	EntityName string                 `json:"entityName"`
	Trigger    string                 `json:"trigger"`
	Condition  *PackWorkflowCondition `json:"condition,omitempty"`
	Actions    []PackWorkflowAction   `json:"actions"`
	IsActive   *bool                  `json:"isActive,omitempty"`
	// IntervalSeconds is a scheduled rule's cadence (fire at most once per interval). 0/absent =
	// every scheduler tick. Meaningful only for trigger "scheduled" (#295).
	IntervalSeconds int `json:"intervalSeconds,omitempty"`
}

// PackWorkflowCondition is the condition block of a pack workflow: optional entry-criteria rules
// (match "and"/"or", default "and"), an optional TIMER-1 date_offset timer config and the specs/048
// saveOn of a before_save rule ("create" | "update"; absent = both). Mirrors
// workflowConditionRequest — a key missing here is dropped in silence on export AND on install.
type PackWorkflowCondition struct {
	Match  string             `json:"match,omitempty"`
	Rules  []PackWorkflowRule `json:"rules,omitempty"`
	Timer  *PackWorkflowTimer `json:"timer,omitempty"`
	SaveOn string             `json:"saveOn,omitempty"`
}

// PackWorkflowRule is one condition predicate {field, op, value?}. Mirrors workflowRuleRequest.
type PackWorkflowRule struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}

// PackWorkflowTimer is the date_offset trigger config (TIMER-1): {dateField, offset:{amount,unit},
// direction, calendarId?}. Mirrors workflowTimerRequest.
type PackWorkflowTimer struct {
	DateField  string                  `json:"dateField"`
	Offset     PackWorkflowTimerOffset `json:"offset"`
	Direction  string                  `json:"direction"`
	CalendarID string                  `json:"calendarId,omitempty"`
	// CalendarName names a pack.Calendars entry (queue #720). Export writes this instead of the
	// source-tenant id; install resolves it to the target tenant's calendar id.
	CalendarName string `json:"calendarName,omitempty"`
}

// PackWorkflowTimerOffset is the {amount, unit} pair of a timer config.
type PackWorkflowTimerOffset struct {
	Amount int    `json:"amount"`
	Unit   string `json:"unit"`
}

// PackWorkflowAction is one action of a pack workflow. It carries the union of every action type's
// params (field_update / in_app_notification / email_alert / outbound_webhook / assign /
// create_record / set_deadline / veto — field + message) exactly as
// workflowActionRequest does; the service validates the shape per Type at install time.
type PackWorkflowAction struct {
	Type    string `json:"type"`
	Field   string `json:"field,omitempty"`
	Value   any    `json:"value,omitempty"`
	UserID  string `json:"userId,omitempty"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
	// email_alert
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
	// outbound_webhook
	URL     string            `json:"url,omitempty"`
	Secret  string            `json:"secret,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// outbound_webhook through the vault (specs/050 S6): the key travels as its NAME (the id is the
	// source tenant's) and must be declared in Pack.Credentials; Path is appended to its base URL.
	CredentialName string `json:"credentialName,omitempty"`
	Path           string `json:"path,omitempty"`
	// assign (ASSIGN-1): strategy ("round_robin") + team id (the target field reuses `field`).
	Strategy string `json:"strategy,omitempty"`
	Team     string `json:"team,omitempty"`
	// create_record (CREATE-RECORD, PACK-EXPORT-FULL): EntityName is the target entity to insert
	// into; Values is the field→value map. Mirrors workflowActionRequest/WorkflowActionInput so a
	// follow-up-task rule round-trips through export→install like every other action type.
	EntityName string         `json:"entityName,omitempty"`
	Values     map[string]any `json:"values,omitempty"`
	// set_deadline (SLA-DEADLINE p1): how far AFTER the fire instant the computed deadline lands.
	// Same reason create_record's pair above exists — an action param that export drops is an
	// install that dies at Pass 8 with no rollback, and the dropping happens silently.
	Offset *PackWorkflowTimerOffset `json:"offset,omitempty"`
	// set_deadline policy (queue #81, specs/043 S14a): same three params workflowActionRequest
	// carries. Dropping Mode on export would turn a tighten_only rule into an OVERWRITE on the
	// destination tenant — one that replaces hand-typed deadlines on every priority change.
	Mode       string `json:"mode,omitempty"`
	StampField string `json:"stampField,omitempty"`
	Anchor     string `json:"anchor,omitempty"`
	// CalendarID pins set_deadline's business_hours calendar (queue #79) — same contract as
	// PackWorkflowTimer.CalendarID: a pack author may name a calendar of the TARGET tenant (the
	// save checks it exists); export emits calendarName instead (#720) — an id is dropped only when it no longer resolves, since a
	// source-tenant calendar id resolves to nothing on the other side.
	CalendarID string `json:"calendarId,omitempty"`
	// CalendarName: see PackWorkflowTimer.CalendarName (queue #720).
	CalendarName string `json:"calendarName,omitempty"`
	// field_update through a relation (specs/058). Dropped on export, the rule would install as a
	// write on the TRIGGERING record's field of the same name — or fail Pass 8 when it has none.
	Via string `json:"via,omitempty"`
	// field_update fanned out to every child record (specs/084). Dropped on export, the rule would
	// install as a write on the TRIGGERING record's field of the same name.
	ChildEntity string `json:"childEntity,omitempty"`
	ChildVia    string `json:"childVia,omitempty"`
	// before_save field_update (specs/048 S4). Dropping OnlyIfEmpty turns "fill when empty" into
	// "always overwrite"; dropping ValueSource writes a {{field}} template literally.
	OnlyIfEmpty bool   `json:"onlyIfEmpty,omitempty"`
	ValueSource string `json:"valueSource,omitempty"`
	// start_approval (specs/043 S9b): the NAME of a manual approval process on the rule's own entity
	// — an id does not survive a tenant boundary. Install creates approvals first, then resolves it.
	ProcessName string `json:"processName,omitempty"`
	// normalize (specs/048 S5): the text rewrites, in order. Dropped, the installed rule is refused
	// (NORMALIZE_OPS_INVALID) — or worse, a pack edited by hand installs a different order.
	Ops []string `json:"ops,omitempty"`
}

// isRelationField reports whether a field is a 1-N relation (materializes a <field>_id FK
// column). Install must defer relation fields until every target entity's table exists.
func isRelationField(f PackField) bool {
	return f.FieldType == "relation"
}

// PackCredential is one vault key slot of a pack (specs/050 S6): everything about how to call the
// partner EXCEPT the secret. ExtraHeaderNames lists header names only; their values are the
// destination admin's to enter.
type PackCredential struct {
	Name             string   `json:"name"`
	Label            string   `json:"label,omitempty"`
	BaseURL          string   `json:"baseUrl"`
	AuthType         string   `json:"authType"`
	HeaderName       string   `json:"headerName,omitempty"`
	HeaderPrefix     string   `json:"headerPrefix,omitempty"`
	Username         string   `json:"username,omitempty"`
	ExtraHeaderNames []string `json:"extraHeaderNames,omitempty"`
}

// PackCalendar is one working calendar a pack ships — the CalendarInput of the admin CRUD, by name.
type PackCalendar struct {
	Name      string                         `json:"name"`
	Timezone  string                         `json:"timezone"`
	Hours     map[string][]calendar.Interval `json:"hours"`
	Holidays  []string                       `json:"holidays,omitempty"`
	IsDefault bool                           `json:"isDefault,omitempty"`
}

// PackView is one shared saved view of an entity.
type PackView struct {
	EntityName string          `json:"entityName"`
	Name       string          `json:"name"`
	Config     json.RawMessage `json:"config"`
}

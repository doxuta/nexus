package vertical_pack

import (
	"context"
	"errors"
	"fmt"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/metaversion"
	"strings"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// InstallPlan is the read-only diff a dry-run produces: exactly what a real install WOULD do,
// computed without writing anything. The deploy wizard renders it (creates = teal, skips = gray,
// conflicts = red) so an admin can review a handover before running it live.
//
// The exported fields are the public contract (JSON, camelCase). The unexported fields are the
// decision sets consumed by execute() so that a REAL install and a dry-run share ONE source of
// truth: buildPlan() decides (read-only), and execute() applies the same decisions — there is no
// second, parallel dry-run copy of the install logic that could rot out of sync.
type InstallPlan struct {
	// EntitiesToCreate / EntitiesToSkip: an entity whose name already exists for the tenant is
	// skipped (idempotency); the rest are created (Pass 1), in dependency order.
	EntitiesToCreate []string `json:"entitiesToCreate"`
	EntitiesToSkip   []string `json:"entitiesToSkip"`

	// FieldsToAdd lists every field install would ADD as a discrete op (mirrors the install
	// result's RelationFieldsAdded + DeferredFieldsAdded): relation fields materialized WITH a
	// newly-created entity (deferred=false) and Pass-2b deferred rollup fields (deferred=true).
	// Ordinary scalar fields are NOT listed — they are created implicitly with their entity.
	FieldsToAdd []PlanField `json:"fieldsToAdd"`

	// NNToRegister / NNToSkip: N-N relationships to register (Pass 2) vs. already-registered.
	NNToRegister []string `json:"nnToRegister"`
	NNToSkip     []string `json:"nnToSkip"`

	// LayoutsToInstall is every "<entity>:<view>" the pack upserts (Pass 5). A layout that would
	// OVERWRITE an existing one is additionally reported in Conflicts as kind "layout_overwrite".
	LayoutsToInstall []string `json:"layoutsToInstall"`

	// HooksToCreate / HooksToSkip: inbound webhook hooks to create (Pass 8e) vs. already present
	// by NAME. Reported so a dry-run never under-states what install will do — an install that
	// silently seeds a receiving endpoint the plan did not mention is a gate that lies.
	HooksToCreate []string `json:"hooksToCreate"`
	HooksToSkip   []string `json:"hooksToSkip"`

	// RolesToCreate / RolesToSkip: roles to create (Pass 6) vs. already present by name.
	RolesToCreate []string `json:"rolesToCreate"`
	RolesToSkip   []string `json:"rolesToSkip"`

	// The late passes (7+) — dashboards, workflows, reports, work queues, approval processes and
	// teams. buildPlan has ALWAYS decided these (the dashSkip/wfSkip/… maps below); they were kept
	// out of the serialized contract, so a dry-run answered "what will installing this do?" while
	// omitting that it stands up rules that fire, queues that route work, and approval gates that
	// block records. An operator approving a plan is giving consent — consent to a list that does
	// not mention half of what happens is not consent. Same reasoning as HooksToCreate above.
	DashboardsToCreate []string `json:"dashboardsToCreate"`
	DashboardsToSkip   []string `json:"dashboardsToSkip"`
	WorkflowsToCreate  []string `json:"workflowsToCreate"`
	WorkflowsToSkip    []string `json:"workflowsToSkip"`
	ReportsToCreate    []string `json:"reportsToCreate"`
	ReportsToSkip      []string `json:"reportsToSkip"`
	WorkQueuesToCreate []string `json:"workQueuesToCreate"`
	WorkQueuesToSkip   []string `json:"workQueuesToSkip"`
	// Approvals read as "<entity>:<name>" (the install result's shape) but dedupe by (entity,name).
	ApprovalsToCreate []string `json:"approvalsToCreate"`
	ApprovalsToSkip   []string `json:"approvalsToSkip"`
	TeamsToCreate     []string `json:"teamsToCreate"`
	TeamsToSkip       []string `json:"teamsToSkip"`

	// SamplesCount is the total number of seed records the pack would insert (Pass 3).
	SamplesCount int `json:"samplesCount"`

	// Conflicts are the pre-flight warnings the read-only probes found (see probeConflicts):
	//   - "unique_slot_held" (HARD): a live row of THIS tenant already holds a unique value a
	//     sample would insert → the install WILL hit 409 RECORD_DUPLICATE (trap B3).
	//   - "layout_overwrite" (SOFT): an existing layout will be silently overwritten.
	Conflicts []PlanConflict `json:"conflicts"`

	// Warnings are the install notes a dry run can already predict, worded exactly as the real
	// install words them in InstallResult.warnings (DAILY-QUEUE #104). Today that is the Pass 8a
	// working-calendar sentence (packCalendarWarning): read here, the installer learns that the
	// SLA rules will run on wall-clock time BEFORE clicking "Cài", not after the pack has landed.
	// Unlike a Conflict, a warning never gates the confirm.
	Warnings []string `json:"warnings"`

	// --- internal decision sets consumed by execute() (never serialized) -----------------------
	ordered []PackEntity // dependency-ordered entities (from orderEntities, computed once)
	// reconcile holds pack fields that are MISSING from an already-existing entity — the upgrade
	// path. Install skips an entity that already exists, which also skipped its NEW inline fields:
	// a tenant on CRM 1.14.0 re-installing 1.15.0 got the new crm_products entity but never the
	// six new fields on crm_quotes/quote_lines/contracts, so the whole convert chain and the
	// payable-quote amount silently never arrived. execute() adds these through the same AddField
	// path, AFTER entities exist so a relation field's target is present.
	reconcile  []reconcileField
	entitySkip map[string]bool // entity name → already exists → skip create
	nnSkip     map[string]bool // N-N name → already registered → skip
	fieldSkip  map[string]bool // "entity.field" → deferred field already present → skip add
	roleSkip   map[string]bool // role name → already exists → skip create
	dashSkip   map[string]bool // dashboard name → already exists → skip create
	wfSkip     map[string]bool // workflow name → already exists → skip create
	reportSkip map[string]bool // report name → already exists → skip create (PACK-REPORTS)
	wqSkip     map[string]bool // work-queue name → already exists → skip create (PACK-QUEUES)
	// wqIDByName maps a PRE-EXISTING work-queue name→id, read when an approval step is handed to a
	// queue by name (specs/043 S6b); a queue this install creates resolves via createdIDs.
	wqIDByName map[string]string
	apSkip     map[string]bool // approvalDupKey(entity,name) → already exists → skip (PACK-APPROVALS)
	hookSkip   map[string]bool // hook NAME → already exists → skip (PACK-HOOKS; hooks are UNIQUE per (tenant,name), so a blind create 409s)
	// roleIDByName maps every PRE-EXISTING role name→id (captured in the Pass 6 decision read) so
	// the work-queues pass can resolve a queue's RoleName without re-reading roles; freshly-created
	// pack roles resolve via createdIDs["role/<name>"] in execute().
	roleIDByName map[string]string
	// teamSkip / teamIDByName back the PACK-TEAMS pass: teamSkip marks a pack team whose name already
	// exists in the tenant (idempotent skip), and teamIDByName maps every PRE-EXISTING team name→id so
	// execute() can populate the name→id routing map for the workflows pass without re-reading teams.
	teamSkip     map[string]bool   // team name → already exists → skip create
	teamIDByName map[string]string // pre-existing team name → id (for the assign name→id map)
}

// PlanField is one field install would add: which entity, which field, and whether it is added in
// the deferred pass (Pass 2b — parent-side rollups) vs. inline with a freshly-created entity.
//
// reconcileField carries a full PackField to ADD to an entity that already exists — the upgrade
// path. Unlike PlanField (a display row), it holds the whole field definition execute() needs.
type PlanField struct {
	Entity   string `json:"entity"`
	Field    string `json:"field"`
	Deferred bool   `json:"deferred"`
}

type reconcileField struct {
	Entity string
	Field  PackField
}

// PlanConflict is one pre-flight warning. Kind classifies severity for the wizard (hard vs soft);
// Detail is a human sentence; Entity/Field pin the resource when applicable.
type PlanConflict struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Entity string `json:"entity,omitempty"`
	Field  string `json:"field,omitempty"`
}

// newEmptyPlan returns a plan with every public slice initialized to a non-nil empty slice, so the
// JSON envelope always serializes `[]` (never `null`) — the same stable-empty contract the rest of
// the API honors (e.g. ListInstalls). The decision maps are ready for buildPlan to populate.
func newEmptyPlan() *InstallPlan {
	return &InstallPlan{
		EntitiesToCreate: []string{},
		HooksToCreate:    []string{},
		HooksToSkip:      []string{},

		DashboardsToCreate: []string{},
		DashboardsToSkip:   []string{},
		WorkflowsToCreate:  []string{},
		WorkflowsToSkip:    []string{},
		ReportsToCreate:    []string{},
		ReportsToSkip:      []string{},
		WorkQueuesToCreate: []string{},
		WorkQueuesToSkip:   []string{},
		ApprovalsToCreate:  []string{},
		ApprovalsToSkip:    []string{},
		TeamsToCreate:      []string{},
		TeamsToSkip:        []string{},
		EntitiesToSkip:     []string{},
		FieldsToAdd:        []PlanField{},
		NNToRegister:       []string{},
		NNToSkip:           []string{},
		LayoutsToInstall:   []string{},
		RolesToCreate:      []string{},
		RolesToSkip:        []string{},
		Conflicts:          []PlanConflict{},
		Warnings:           []string{},
		entitySkip:         map[string]bool{},
		nnSkip:             map[string]bool{},
		fieldSkip:          map[string]bool{},
		roleSkip:           map[string]bool{},
		dashSkip:           map[string]bool{},
		wfSkip:             map[string]bool{},
		reportSkip:         map[string]bool{},
		wqSkip:             map[string]bool{},
		wqIDByName:         map[string]string{},
		apSkip:             map[string]bool{},
		hookSkip:           map[string]bool{},
		roleIDByName:       map[string]string{},
		teamSkip:           map[string]bool{},
		teamIDByName:       map[string]string{},
	}
}

// DryRunInstall computes the install plan for a pack WITHOUT writing anything. It runs the same
// hard-reject gates as a real install (empty pack → ErrPackEmpty; malformed manifest version →
// VERTICAL_MANIFEST_INVALID_VERSION; a relation cycle → VERTICAL_PACK_CYCLE) so the two paths
// agree, then the read-only diff (buildPlan) and the read-only conflict probes (probeConflicts).
// Zero writes: it never touches the executor, so tenant metadata, obj_* rows, and the install
// ledger are all left untouched.
func (s *Service) DryRunInstall(ctx context.Context, tenantID string, pack *Pack) (*InstallPlan, error) {
	plan, err := s.buildPlan(ctx, tenantID, pack)
	if err != nil {
		return nil, err
	}
	if err := s.probeConflicts(ctx, tenantID, pack, plan); err != nil {
		return nil, err
	}
	// Same order as the install result: the #171 stale-rule note, then Pass 8a's calendar note.
	if w := s.staleWorkflowsWarning(ctx, tenantID, pack, plan); w != "" {
		plan.Warnings = append(plan.Warnings, w)
	}
	if w := s.packCalendarWarning(ctx, tenantID, pack); w != "" {
		plan.Warnings = append(plan.Warnings, w)
	}
	return plan, nil
}

// buildPlan is the pure DECISION function shared by Install and DryRunInstall: given the current
// tenant state (all reads, no writes) and the pack, it decides — per pass — what would be created
// vs. skipped, and returns the plan. Install then feeds the plan to execute(); DryRunInstall adds
// the conflict probes and returns it. Keeping the decisions here (and only here) is what makes the
// dry-run and the real install ONE source of truth.
//
// The hard-reject gates (empty pack, manifest version, approval shape, relation cycle) live here
// too so both entry points reject the same way BEFORE producing a plan or writing anything.
func (s *Service) buildPlan(ctx context.Context, tenantID string, pack *Pack) (*InstallPlan, error) {
	// The plan is decided against the LIVE configuration (specs/063 FR-044), never the builder's drafts.
	ctx = metaversion.WithLiveRead(ctx)
	if pack == nil || len(pack.Entities) == 0 {
		return nil, ErrPackEmpty
	}
	// A supplied manifest must carry a valid semver (parity with Install — rejected before any
	// write so a mistyped version never half-installs; nil manifest is legacy-allowed).
	if err := validateManifest(pack.Manifest); err != nil {
		return nil, err
	}
	if err := approvalShapeGate(pack.Approvals); err != nil {
		return nil, err
	}
	if err := beforeRuleShapeGate(pack.Workflows); err != nil {
		return nil, err
	}
	if err := s.credentialGate(pack); err != nil {
		return nil, err
	}
	// Dependency order (and cycle detection) — computed ONCE here; execute() reuses plan.ordered.
	ordered, err := orderEntities(pack.Entities)
	if err != nil {
		return nil, err
	}

	plan := newEmptyPlan()
	plan.ordered = ordered

	// The tenant's existing entities are the source of every skip decision. Fetch once: it gives
	// both the name set (create vs skip) AND each entity's id (needed to list fields for the
	// deferred-field present-check below), so no per-entity round-trip is required.
	existing, err := s.entityRd.ListEntities(tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.buildPlan: list entities: %w", err)
	}
	existingIDByName := make(map[string]string, len(existing))
	for _, e := range existing {
		existingIDByName[e.Name] = e.ID
	}

	// Pass 1 decision: create vs skip, in dependency order (mirrors execute()'s loop exactly).
	for _, pe := range ordered {
		name := strings.TrimSpace(pe.Name)
		if _, exists := existingIDByName[name]; exists {
			plan.EntitiesToSkip = append(plan.EntitiesToSkip, name)
			plan.entitySkip[name] = true
			continue
		}
		plan.EntitiesToCreate = append(plan.EntitiesToCreate, name)
		// Relation fields materialize WITH the entity (deferred=false), mirroring the install
		// result's RelationFieldsAdded.
		for _, f := range pe.Fields {
			if isRelationField(f) {
				plan.FieldsToAdd = append(plan.FieldsToAdd, PlanField{Entity: name, Field: f.Name, Deferred: false})
			}
		}
	}

	// Reconcile decision: an EXISTING entity may be missing fields the pack added in a newer
	// version. For each skipped entity, diff the pack's inline fields against the live field set
	// and queue the missing ones. This is what makes a pack UPGRADE (not just a fresh install)
	// actually deliver new fields to tenants provisioned on an older version.
	// Only when a field writer is wired: reconcile ADDS fields, and with no writer there is nothing
	// to add (and nothing to plan). A fresh install needs no reconcile — inline fields materialize
	// with their entity — so this never affects first-time installs.
	for _, pe := range ordered {
		if s.fieldWr == nil {
			break
		}
		name := strings.TrimSpace(pe.Name)
		eid, exists := existingIDByName[name]
		if !exists {
			continue // freshly created — its fields materialized with it in Pass 1
		}
		liveFields, ferr := s.entityRd.ListFields(ctx, tenantID, eid)
		if ferr != nil {
			return nil, fmt.Errorf("vertical_pack.buildPlan: list fields of %q for reconcile: %w", name, ferr)
		}
		live := make(map[string]bool, len(liveFields))
		for _, lf := range liveFields {
			live[lf.Name] = true
		}
		for _, f := range pe.Fields {
			if live[strings.TrimSpace(f.Name)] {
				continue // already there — nothing to add
			}
			// SCALAR fields only. A relation field creates a generated FK column and a registry
			// entry, and adding one to an entity that already has records collides with existing
			// column/inverse state in ways a plain field does not (FIELD_RELATION_COLUMN_CONFLICT /
			// _INVERSE_TAKEN). A `lookup` depends on such a relation. The reconcile pass therefore
			// delivers the value fields a version adds — currency/text/picklist/number/date — which
			// is what the money loop needs (crm_quotes.amount_due). Adding a NEW relation to an old
			// entity is a heavier migration left as its own slice; the dry-run below still lists
			// what was skipped so the gap is visible, not silent.
			if isRelationField(f) || f.FieldType == "lookup" {
				continue
			}
			plan.reconcile = append(plan.reconcile, reconcileField{Entity: name, Field: f})
			plan.FieldsToAdd = append(plan.FieldsToAdd, PlanField{Entity: name, Field: f.Name, Deferred: false})
		}
	}

	// Pass 2b decision: deferred fields — add vs skip (field already present on its entity). On a
	// fresh install the target entity does not exist yet → treated as "to add" (never fatal).
	for _, df := range pack.DeferredFields {
		present, err := s.deferredFieldPresent(ctx, tenantID, existingIDByName, df)
		if err != nil {
			return nil, err
		}
		key := df.EntityName + "." + df.Field.Name
		if present {
			plan.fieldSkip[key] = true
			continue
		}
		plan.FieldsToAdd = append(plan.FieldsToAdd, PlanField{Entity: df.EntityName, Field: df.Field.Name, Deferred: true})
	}

	// Pass 2 decision: N-N relationships — register vs skip (already in the registry).
	nnExisting := map[string]bool{}
	if s.relationRd != nil && len(pack.NNRelationships) > 0 {
		rels, err := s.relationRd.ListAllRelationships(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.buildPlan: list relationships: %w", err)
		}
		for _, r := range rels {
			nnExisting[r.Name] = true
		}
	}
	for _, nn := range pack.NNRelationships {
		if nnExisting[nn.Name] {
			plan.NNToSkip = append(plan.NNToSkip, nn.Name)
			plan.nnSkip[nn.Name] = true
			continue
		}
		plan.NNToRegister = append(plan.NNToRegister, nn.Name)
	}

	// Pass 5 (informational): every layout is upserted (SaveLayout is a safe overwrite). Whether
	// one OVERWRITES an existing layout is surfaced as a soft conflict by probeConflicts.
	for _, l := range pack.Layouts {
		plan.LayoutsToInstall = append(plan.LayoutsToInstall, l.EntityName+":"+l.ViewType)
	}

	// Pass 6 decision: roles — create vs skip by name.
	existingRoles, err := s.roleRd.ListRoles(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.buildPlan: list roles: %w", err)
	}
	roleNames := make(map[string]bool, len(existingRoles))
	for _, r := range existingRoles {
		roleNames[r.Name] = true
		if auth.IsOwnerRoleName(r.Name) {
			continue // the reserved owner role never takes part in a pack (specs/065 D-15): no reference may resolve to it
		}
		plan.roleIDByName[r.Name] = r.ID // pre-existing name→id for the work-queues pass (PACK-QUEUES)
	}
	for _, pr := range pack.Roles {
		name := strings.TrimSpace(pr.Name)
		if auth.IsOwnerRoleName(name) {
			continue // neither created nor "skipped": Install warns and ignores it
		}
		if roleNames[name] {
			plan.RolesToSkip = append(plan.RolesToSkip, name)
			plan.roleSkip[name] = true
			continue
		}
		plan.RolesToCreate = append(plan.RolesToCreate, name)
	}

	// Pass 7 decision: dashboards — create vs skip by name. Internal only (not in the public
	// contract), but execute() needs the skip set to stay idempotent (sys_dashboards has no
	// UNIQUE(tenant_id,name), so a blind re-insert would duplicate the row).
	if s.dashboardWr != nil && len(pack.Dashboards) > 0 {
		existingDash, err := s.dashboardWr.ListDashboards(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.buildPlan: list dashboards: %w", err)
		}
		dashNames := make(map[string]bool, len(existingDash))
		for _, d := range existingDash {
			dashNames[d.Name] = true
		}
		for _, pd := range pack.Dashboards {
			if dashNames[strings.TrimSpace(pd.Name)] {
				plan.dashSkip[strings.TrimSpace(pd.Name)] = true
			}
		}
		plan.DashboardsToCreate, plan.DashboardsToSkip = splitPlanNames(pack.Dashboards, plan.dashSkip,
			func(d PackDashboard) (string, string) { n := strings.TrimSpace(d.Name); return n, n })
	}

	// Workflows decision: create vs skip by name. Internal only (not in the public contract), but
	// execute() needs the skip set to stay idempotent (sys_workflows has no UNIQUE(tenant_id,name),
	// so a blind re-insert would duplicate the rule).
	if s.workflowWr != nil && len(pack.Workflows) > 0 {
		existingWF, err := s.workflowWr.ListWorkflows(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.buildPlan: list workflows: %w", err)
		}
		wfNames := make(map[string]bool, len(existingWF))
		for _, w := range existingWF {
			wfNames[w.Name] = true
		}
		for _, pw := range pack.Workflows {
			if wfNames[strings.TrimSpace(pw.Name)] {
				plan.wfSkip[strings.TrimSpace(pw.Name)] = true
			}
		}
		plan.WorkflowsToCreate, plan.WorkflowsToSkip = splitPlanNames(pack.Workflows, plan.wfSkip,
			func(w PackWorkflow) (string, string) { n := strings.TrimSpace(w.Name); return n, n })
	}

	// Reports decision (PACK-REPORTS): create vs skip by name. Internal only (not in the public
	// contract), but execute() needs the skip set to stay idempotent (sys_reports has no
	// UNIQUE(tenant_id,name), so a blind re-insert would duplicate the report row — same shape as
	// dashboards/workflows).
	if s.reportWr != nil && len(pack.Reports) > 0 {
		existingReports, err := s.reportWr.ListReports(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.buildPlan: list reports: %w", err)
		}
		reportNames := make(map[string]bool, len(existingReports))
		for _, r := range existingReports {
			reportNames[r.Name] = true
		}
		for _, pr := range pack.Reports {
			if reportNames[strings.TrimSpace(pr.Name)] {
				plan.reportSkip[strings.TrimSpace(pr.Name)] = true
			}
		}
		plan.ReportsToCreate, plan.ReportsToSkip = splitPlanNames(pack.Reports, plan.reportSkip,
			func(r PackReport) (string, string) { n := strings.TrimSpace(r.Name); return n, n })
	}

	// Work-queues decision (PACK-QUEUES): create vs skip by name. Internal only. sys_work_queues
	// HAS UNIQUE(tenant_id,name), so the skip is what keeps a re-install idempotent instead of
	// surfacing a 409 WORKQUEUE_ALREADY_EXISTS.
	if s.workQueueWr != nil && len(pack.WorkQueues) > 0 {
		existingQ, err := s.workQueueWr.ListAdmin(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.buildPlan: list work queues: %w", err)
		}
		qNames := make(map[string]bool, len(existingQ))
		for _, q := range existingQ {
			qNames[q.Name] = true
		}
		for _, pq := range pack.WorkQueues {
			if qNames[strings.TrimSpace(pq.Name)] {
				plan.wqSkip[strings.TrimSpace(pq.Name)] = true
			}
		}
		plan.WorkQueuesToCreate, plan.WorkQueuesToSkip = splitPlanNames(pack.WorkQueues, plan.wqSkip,
			func(q PackWorkQueue) (string, string) { n := strings.TrimSpace(q.Name); return n, n })
	}

	// Approvals decision (PACK-APPROVALS): create vs skip by (entity, name). Internal only.
	// sys_approval_processes has no UNIQUE(tenant_id,entity,name), so — like dashboards/workflows —
	// the list-then-skip is what keeps a re-install from silently duplicating the process.
	if s.approvalWr != nil && len(pack.Approvals) > 0 {
		existingAP, err := s.approvalWr.ListProcesses(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.buildPlan: list approval processes: %w", err)
		}
		apKeys := make(map[string]bool, len(existingAP))
		for _, p := range existingAP {
			apKeys[approvalDupKey(p.EntityName, p.Name)] = true
		}
		for _, pa := range pack.Approvals {
			key := approvalDupKey(strings.TrimSpace(pa.EntityName), strings.TrimSpace(pa.Name))
			if apKeys[key] {
				plan.apSkip[key] = true
			}
		}
		// A step handed to a team / queue by NAME resolves against the TARGET tenant (specs/043
		// S6b): read the names it can resolve to now, whatever else the pack carries.
		needTeams, needQueues := false, false
		for _, pa := range pack.Approvals {
			for _, st := range pa.Steps {
				if a := st.Assignment; a != nil {
					needTeams = needTeams || strings.TrimSpace(a.Type) == "team"
					needQueues = needQueues || strings.TrimSpace(a.Type) == "queue"
				}
			}
		}
		if needTeams && s.teamWr != nil {
			teams, err := s.teamWr.ListTeams(ctx, tenantID)
			if err != nil {
				return nil, fmt.Errorf("vertical_pack.buildPlan: list teams for approvals: %w", err)
			}
			for _, t := range teams {
				if _, ok := plan.teamIDByName[t.Name]; !ok {
					plan.teamIDByName[t.Name] = t.ID
				}
			}
		}
		if needQueues && s.workQueueWr != nil {
			queues, err := s.workQueueWr.ListAdmin(ctx, tenantID)
			if err != nil {
				return nil, fmt.Errorf("vertical_pack.buildPlan: list work queues for approvals: %w", err)
			}
			for _, q := range queues {
				plan.wqIDByName[strings.TrimSpace(q.Name)] = q.ID
			}
		}
		plan.ApprovalsToCreate, plan.ApprovalsToSkip = splitPlanNames(pack.Approvals, plan.apSkip,
			func(a PackApproval) (string, string) {
				e, n := strings.TrimSpace(a.EntityName), strings.TrimSpace(a.Name)
				return approvalDupKey(e, n), e + ":" + n
			})
	}

	// Inbound hooks decision (PACK-HOOKS): create vs skip BY NAME. sys_inbound_hooks has a
	// per-tenant UNIQUE(name) so a blind re-create returns HOOK_ALREADY_EXISTS — the skip is what
	// keeps a re-install idempotent, exactly like teams.
	if s.hookWr != nil && len(pack.InboundHooks) > 0 {
		existingH, err := s.hookWr.ListAdmin(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.buildPlan: list inbound hooks: %w", err)
		}
		hookNames := make(map[string]bool, len(existingH))
		for _, h := range existingH {
			hookNames[h.Name] = true
		}
		for _, ph := range pack.InboundHooks {
			name := strings.TrimSpace(ph.Name)
			if hookNames[name] {
				plan.hookSkip[name] = true
				plan.HooksToSkip = append(plan.HooksToSkip, name)
				continue
			}
			plan.HooksToCreate = append(plan.HooksToCreate, name)
		}
	}

	// Teams decision (PACK-TEAMS): create vs skip by name, and capture EVERY pre-existing team's
	// name→id so execute() can build the assign name→id routing map without re-reading. Internal
	// only (not in the public plan contract, like dashboards/workflows), but execute() needs both:
	// teams have a per-tenant UNIQUE(name), so a blind re-create would 409 ErrTeamAlreadyExists.
	if s.teamWr != nil && len(pack.Teams) > 0 {
		existingTeams, err := s.teamWr.ListTeams(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.buildPlan: list teams: %w", err)
		}
		for _, t := range existingTeams {
			plan.teamIDByName[t.Name] = t.ID
		}
		for _, pt := range pack.Teams {
			name := strings.TrimSpace(pt.Name)
			if _, exists := plan.teamIDByName[name]; exists {
				plan.teamSkip[name] = true
			}
		}
		plan.TeamsToCreate, plan.TeamsToSkip = splitPlanNames(pack.Teams, plan.teamSkip,
			func(t PackTeam) (string, string) { n := strings.TrimSpace(t.Name); return n, n })
	}

	// Pass 3 count: total seed records across all entities.
	for i := range pack.Samples {
		plan.SamplesCount += len(pack.Samples[i].Records)
	}

	return plan, nil
}

// deferredFieldPresent reports whether a deferred field already exists on its (existing) entity.
// When the target entity does not exist yet for the tenant (the common FRESH-install case), the
// field cannot exist → returns false ("to add") without error. This is the guard that keeps a
// fresh-tenant dry-run from mistaking an absent entity for a failure.
func (s *Service) deferredFieldPresent(ctx context.Context, tenantID string, existingIDByName map[string]string, df PackDeferredField) (bool, error) {
	entityID, ok := existingIDByName[df.EntityName]
	if !ok {
		return false, nil // entity absent (fresh install) → deferred field is to-add
	}
	fields, err := s.entityRd.ListFields(ctx, tenantID, entityID)
	if err != nil {
		return false, fmt.Errorf("vertical_pack.buildPlan: list fields of %q: %w", df.EntityName, err)
	}
	for _, f := range fields {
		if f.Name == df.Field.Name {
			return true, nil
		}
	}
	return false, nil
}

// approvalDupKey builds the (entity, name) idempotency key of an approval process. NUL-joined so
// no legal entity/process name can collide with another pair's concatenation.
func approvalDupKey(entityName, name string) string { return entityName + "\x00" + name }

// probeConflicts runs the read-only pre-flight probes and appends any findings to plan.Conflicts.
// It is called ONLY on the dry-run path (a real install would just hit the same conditions live).
//
// (1) unique_slot_held (HARD, trap B3): for each pack entity whose obj_* table already exists AND
// that declares a unique scalar field, check whether any sample's value for that field is already
// held by a LIVE row of THIS tenant. If so, the sample insert WILL fail with 409 RECORD_DUPLICATE
// at real-install time — the exact re-install/handover foot-gun the plan exists to expose. The
// probe is tenant-scoped: the unique index is (tenant_id, <col>), so a CO-TENANT's identical value
// never collides and is deliberately NOT reported (reporting it would be a false positive). Only
// same-tenant live rows squat the slot — either leftover records after the entity's METADATA was
// soft-deleted, or the previous install's samples on a re-install.
//
// (2) layout_overwrite (SOFT): a layout the pack installs for an (entity,view) that already has
// one will silently overwrite it. Reported so the wizard can warn, though it is idempotent-safe.
func (s *Service) probeConflicts(ctx context.Context, tenantID string, pack *Pack, plan *InstallPlan) error {
	// (1) unique-slot conflicts.
	if s.objProber != nil {
		for i := range pack.Entities {
			pe := pack.Entities[i]
			// Trim the entity name once (mirror buildPlan): the probe's SanitizeIdentifier rejects
			// surrounding whitespace, so a name that slipped through buildPlan's trim would otherwise
			// 500 here instead of surfacing as a validation error on the real install (security W3).
			entityName := strings.TrimSpace(pe.Name)
			uniqueFields := map[string]bool{}
			for _, f := range pe.Fields {
				// Relation uniqueness (1-1 slots) is out of scope: sample @refs are unresolved at
				// plan time, so there is no concrete value to probe.
				if f.IsUnique && !isRelationField(f) {
					uniqueFields[f.Name] = true
				}
			}
			if len(uniqueFields) == 0 {
				continue
			}
			exists, err := s.objProber.TableExists(ctx, entityName)
			if err != nil {
				return fmt.Errorf("vertical_pack.probeConflicts: table exists %q: %w", entityName, err)
			}
			if !exists {
				continue // fresh physical table → no live row can collide
			}
			// Probe each unique field once (dedupe: a 20-row colliding sample emits ONE conflict).
			// probed tracks (field\x00value) so a MISS is not re-queried either (security W2: a
			// large no-collision sample fires one SELECT per distinct value, not per record).
			reported := map[string]bool{}
			probed := map[string]bool{}
			for si := range pack.Samples {
				if strings.TrimSpace(pack.Samples[si].Entity) != entityName {
					continue
				}
				for ri := range pack.Samples[si].Records {
					for k, v := range pack.Samples[si].Records[ri].Data {
						if !uniqueFields[k] || reported[k] {
							continue
						}
						val, ok := v.(string)
						if !ok || strings.TrimSpace(val) == "" {
							continue
						}
						probeKey := k + "\x00" + val
						if probed[probeKey] {
							continue
						}
						probed[probeKey] = true
						held, err := s.objProber.LiveUniqueValueExists(ctx, tenantID, entityName, k, val)
						if err != nil {
							return fmt.Errorf("vertical_pack.probeConflicts: probe %q.%q: %w", entityName, k, err)
						}
						if held {
							reported[k] = true
							plan.Conflicts = append(plan.Conflicts, PlanConflict{
								Kind:   "unique_slot_held",
								Detail: fmt.Sprintf("obj_%s.%s: unique slot đã bị một record đang tồn tại (live) của tenant giữ — cài lại sẽ dính 409 RECORD_DUPLICATE", entityName, k),
								Entity: entityName,
								Field:  k,
							})
						}
					}
				}
			}
		}
	}

	// (2) layout-overwrite warnings.
	if s.layoutRd != nil && len(pack.Layouts) > 0 {
		viewsByEntity := map[string]map[string]bool{} // entity → existing viewType set (cached)
		for _, l := range pack.Layouts {
			views, ok := viewsByEntity[l.EntityName]
			if !ok {
				existing, err := s.layoutRd.GetByEntity(ctx, tenantID, l.EntityName)
				if err != nil {
					return fmt.Errorf("vertical_pack.probeConflicts: layouts of %q: %w", l.EntityName, err)
				}
				views = map[string]bool{}
				for _, ex := range existing {
					views[ex.ViewType] = true
				}
				viewsByEntity[l.EntityName] = views
			}
			if views[l.ViewType] {
				plan.Conflicts = append(plan.Conflicts, PlanConflict{
					Kind:   "layout_overwrite",
					Detail: fmt.Sprintf("%s:%s: layout hiện có sẽ bị pack ghi đè", l.EntityName, l.ViewType),
					Entity: l.EntityName,
				})
			}
		}
	}

	// (3) upgrade_available (SOFT, specs/069 FR-049): the pack already has a baseline in this tenant,
	// so "Nâng cấp" keeps what the builder customised while a reinstall only skips by name.
	if s.items != nil && pack.Manifest != nil && strings.TrimSpace(pack.Manifest.Slug) != "" {
		slug := strings.TrimSpace(pack.Manifest.Slug)
		rows, err := s.items.ListItems(ctx, tenantID, slug)
		if err != nil {
			return fmt.Errorf("vertical_pack.probeConflicts: baseline of %q: %w", slug, err)
		}
		if len(rows) > 0 {
			plan.Conflicts = append(plan.Conflicts, PlanConflict{
				Kind:   "upgrade_available",
				Detail: fmt.Sprintf("Gói %q đã cài ở workspace này — dùng \"Nâng cấp\" để giữ phần đã tuỳ chỉnh thay vì cài lại", slug),
			})
		}
	}
	return nil
}

// splitPlanNames turns a pack's resources into the public create/skip lists, using the skip set
// buildPlan already computed from the tenant's live rows.
//
// It also reproduces execute()'s INTRA-pack dedupe (first occurrence creates, every later one
// skips). That is not a detail: dashboards, workflows, reports, queues and approvals have no
// UNIQUE(tenant_id,name), so execute() carries its own createdX map to avoid inserting a repeated
// name twice — a plan that listed both copies as "to create" would over-promise by exactly the
// number of duplicates. The plan must predict the install, not paraphrase the pack.
func splitPlanNames[T any](items []T, skip map[string]bool, id func(T) (key, display string)) (toCreate, toSkip []string) {
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		key, display := id(it)
		if skip[key] || seen[key] {
			toSkip = append(toSkip, display)
			continue
		}
		seen[key] = true
		toCreate = append(toCreate, display)
	}
	return toCreate, toSkip
}

// approvalShapeGate refuses a pack approval that carries a process/step key outside the pack's
// closed approval shape (specs/043 D-7: graph, options, parallel approvers, slaClock…) — with the
// shared validator's code, located as approvals[i].<field>. It runs before any write: the keys are
// known once the pack is decoded, and Pass 8d (the only other place that sees them) runs after
// entities, samples, roles and workflows have committed, with no rollback — a refusal there left
// the tenant half-installed and the dry-run listing the process as installable. The same holds for
// an outcome that would restart its own process (specs/043 S2b) — also knowable from the pack
// alone, so it is refused here too, by the check CreateProcess runs.
func approvalShapeGate(approvals []PackApproval) error {
	for i, pa := range approvals {
		err := services.UnsupportedApprovalProcessKeys(pa.unsupported)
		for j := 0; err == nil && j < len(pa.Steps); j++ {
			err = services.UnsupportedApprovalStepKeys(j, pa.Steps[j].unsupported)
			if err == nil {
				err = parallelStepShapeGate(j, pa.Steps[j])
			}
		}
		if err == nil {
			err = onRejectShapeGate(pa.Steps)
		}
		if err == nil {
			err = returnedValueShapeGate(pa)
		}
		if err == nil && pa.Criteria != nil && pa.Outcome != nil {
			rules := make([]services.ApprovalCriteriaRuleInput, 0, len(pa.Criteria.Rules))
			for _, r := range pa.Criteria.Rules {
				rules = append(rules, services.ApprovalCriteriaRuleInput{Field: r.Field, Op: r.Op, Value: r.Value})
			}
			err = services.ApprovalOutcomeSelfTrigger(pa.Trigger, rules, &services.ApprovalOutcomeInput{
				StatusField: pa.Outcome.StatusField, ApprovedValue: pa.Outcome.ApprovedValue, RejectedValue: pa.Outcome.RejectedValue,
				ReturnedValue: pa.Outcome.ReturnedValue})
		}
		if err == nil {
			continue
		}
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			ve.Field = fmt.Sprintf("approvals[%d].%s", i, ve.Field)
			ve.Details["field"] = ve.Field
			ve.Msg = fmt.Sprintf("quy trình duyệt %q: %s", strings.TrimSpace(pa.Name), ve.Msg)
		}
		return err
	}
	return nil
}

// returnedValueShapeGate refuses an outcome.returnedValue no pack step can produce (the check
// CreateProcess runs; here it must run before any write or the install half-commits).
func returnedValueShapeGate(pa PackApproval) error {
	if pa.Outcome == nil {
		return nil
	}
	steps := make([]services.ApprovalStepInput, len(pa.Steps))
	for i, st := range pa.Steps {
		if st.OnReject != nil {
			steps[i].OnReject = &services.ApprovalOnRejectInput{To: st.OnReject.To}
		}
	}
	return services.CheckReturnedValueUsed(&services.ApprovalOutcomeInput{ReturnedValue: pa.Outcome.ReturnedValue}, steps, nil)
}

// onRejectShapeGate runs the engine's own onReject check (bpm.CheckOnReject) on every pack step that
// carries one — an unknown or non-earlier target, a stray resumeAt — BEFORE any write (the same
// reason as parallelStepShapeGate). Step ids are the ones the process will store (explicit kept,
// missing ones positional), so a target names the same step the installer will see.
func onRejectShapeGate(steps []PackApprovalStep) error {
	chain := make([]bpm.Step, len(steps))
	any := false
	for i, st := range steps {
		chain[i] = bpm.Step{ID: strings.TrimSpace(st.ID)}
		any = any || st.OnReject != nil
	}
	if !any {
		return nil
	}
	chain = bpm.NormalizeSteps(chain)
	for i, st := range steps {
		if st.OnReject == nil {
			continue
		}
		o := &bpm.OnReject{To: strings.TrimSpace(st.OnReject.To), ResumeAt: strings.TrimSpace(st.OnReject.ResumeAt)}
		if f, reason := bpm.CheckOnReject(o, bpm.NodeApproval, i, chain); f != "" {
			return services.ApprovalStepShapeError(i, "onReject."+f, reason)
		}
	}
	return nil
}

// parallelStepShapeGate runs the engine's own approver check (bpm.CheckApprovers) on a pack step
// that carries approvers/completion — fewer than two people, a stray approverType beside approvers,
// a duplicate, a bad mode — BEFORE any write. Pass 8d only reaches CreateProcess after entities,
// samples, roles and workflows committed with no rollback, so a malformed council found there
// left the install half done. Role NAMES stand in for ids: the shape rules (count, duplicates,
// one source of approver, mode) do not depend on which id a name resolves to.
func parallelStepShapeGate(i int, st PackApprovalStep) error {
	if len(st.Approvers) == 0 && strings.TrimSpace(st.Completion) == "" && st.CompletionPercent == 0 && st.Assignment == nil {
		return nil
	}
	cand := bpm.Step{ApproverType: bpm.ApproverType(strings.TrimSpace(st.ApproverType)), ApproverID: strings.TrimSpace(st.ApproverRef)}
	for _, ap := range st.Approvers {
		cand.Approvers = append(cand.Approvers, bpm.Approver{Type: bpm.ApproverType(strings.TrimSpace(ap.Type)), ID: strings.TrimSpace(ap.Ref)})
	}
	if a := st.Assignment; a != nil { // S6b: the NAME stands in for the id — the shape rules do not depend on which id it becomes
		cand.Assignment = &bpm.Assignment{
			Type: bpm.AssignmentType(strings.TrimSpace(a.Type)), ID: strings.TrimSpace(a.Ref),
			Field: strings.TrimSpace(a.Field), Strategy: strings.TrimSpace(a.Strategy),
		}
	}
	if c := strings.TrimSpace(st.Completion); c != "" || st.CompletionPercent != 0 {
		cand.Completion = &bpm.Completion{Mode: bpm.CompletionMode(c), Percent: st.CompletionPercent}
	}
	if f, reason := bpm.CheckApprovers(cand); f != "" {
		return services.ApprovalStepShapeError(i, f, reason)
	}
	return nil
}

// beforeRuleShapeGate refuses a pack whose before_save / before_delete rule has a malformed shape (a
// veto mixed with a value-changing action, a saveOn that is neither create nor update, an edge
// operator that can never be true, an outside-effect action...) with the code the admin write path
// would give it, naming the rule. The shape is knowable from the pack alone, but the only other place
// that sees it, Pass 8, runs after entities and sample rows have committed with no rollback; the
// dry-run would also list the rule as installable. It calls the ONE validation body behind
// POST /admin/workflows (services.ValidateWorkflowConfig) with nil field types: the type-dependent
// checks need the entity's fields and stay on the apply path, as they do for the AI Builder.
func beforeRuleShapeGate(workflows []PackWorkflow) error {
	for _, pw := range workflows {
		if !workflow.IsBeforeTrigger(workflow.Trigger(strings.TrimSpace(pw.Trigger))) {
			continue
		}
		// A before rule may not carry an assign action at all; resolve its team to itself so the
		// shape check reports THAT, not a team the pack never needed to seed.
		teams := map[string]string{}
		for _, a := range pw.Actions {
			teams[strings.TrimSpace(a.Team)] = a.Team
		}
		in, err := toWorkflowInput(pw, "", teams, nil, nil)
		if err == nil {
			err = services.ValidateWorkflowConfig(in.EntityName, in.Trigger,
				&services.WorkflowConditionInput{Match: in.Match, Rules: in.Rules, Timer: in.Timer, SaveOn: in.SaveOn}, in.Actions, nil)
		}
		if err == nil {
			continue
		}
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			out := *ve // copied: the validator's error is not ours to rewrite
			out.Msg = fmt.Sprintf("quy tắc %q của gói: %s", strings.TrimSpace(pw.Name), ve.Msg)
			return &out
		}
		return fmt.Errorf("vertical_pack.beforeRuleShapeGate: workflow %q: %w", pw.Name, err)
	}
	return nil
}

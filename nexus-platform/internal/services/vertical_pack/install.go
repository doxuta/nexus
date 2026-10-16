package vertical_pack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
	"nexus/platform/internal/workqueue"
)

// InstallResult is the per-install summary returned to the caller: what was created vs. skipped
// (idempotency), broken out per resource kind so a re-install of the same pack shows everything
// skipped and nothing created.
type InstallResult struct {
	EntitiesCreated     []string `json:"entitiesCreated"`
	EntitiesSkipped     []string `json:"entitiesSkipped"`
	RelationFieldsAdded []string `json:"relationFieldsAdded"` // "<entity>.<field>"
	FieldsSkipped       []string `json:"fieldsSkipped"`       // relation + Pass 2b deferred fields already present
	LayoutsInstalled    []string `json:"layoutsInstalled"`    // "<entity>:<view>"
	RolesCreated        []string `json:"rolesCreated"`
	RolesSkipped        []string `json:"rolesSkipped"`
	// RoleGrantsAdded reports roles that already EXISTED and had grants appended for entities this
	// delivery shipped ("sales (+4)"). Reported separately from RolesCreated because on a
	// re-handover it is the line that tells the operator the new tables are actually visible.
	RoleGrantsAdded []string `json:"roleGrantsAdded,omitempty"`
	// FieldACLsApplied counts field restrictions installed (PACK-FIELD-ACL). Reported because a
	// handover that silently loses field-level security looks identical to one that never had any.
	FieldACLsApplied    int      `json:"fieldAclsApplied,omitempty"`
	NNRelationsCreated  []string `json:"nnRelationsCreated"`         // N-N relationships registered
	NNRelationsSkipped  []string `json:"nnRelationsSkipped"`         // already existed — idempotent skip
	DeferredFieldsAdded []string `json:"deferredFieldsAdded"`        // Pass 2b fields added ("<entity>.<field>")
	SamplesInserted     int      `json:"samplesInserted"`            // total seed records written
	SamplesSkipped      int      `json:"samplesSkipped"`             // seed rows skipped: unique value already present (re-install), or a ref to such a row
	LinksCreated        int      `json:"linksCreated"`               // N-N join-table rows created
	LinksSkipped        int      `json:"linksSkipped"`               // N-N links skipped because an endpoint sample row was skipped
	DashboardsCreated   []string `json:"dashboardsCreated"`          // dashboard names created
	DashboardsSkipped   []string `json:"dashboardsSkipped"`          // already existed by name — idempotent skip
	CalendarsCreated    []string `json:"calendarsCreated,omitempty"` // queue #720: working calendars made by Pass 6c
	ViewsCreated        []string `json:"viewsCreated,omitempty"`     // queue #720: shared saved views made
	WorkflowsCreated    []string `json:"workflowsCreated"`           // workflow rule names created
	WorkflowsSkipped    []string `json:"workflowsSkipped"`           // already existed by name — idempotent skip
	ReportsCreated      []string `json:"reportsCreated"`             // saved report names created (PACK-REPORTS)
	ReportsSkipped      []string `json:"reportsSkipped"`             // already existed by name — idempotent skip
	WorkQueuesCreated   []string `json:"workQueuesCreated"`          // work queue names created (PACK-QUEUES)
	WorkQueuesSkipped   []string `json:"workQueuesSkipped"`          // already existed by name — idempotent skip
	ApprovalsCreated    []string `json:"approvalsCreated"`           // approval processes created, "<entity>:<name>" (PACK-APPROVALS)
	ApprovalsSkipped    []string `json:"approvalsSkipped"`           // already existed by (entity,name), or unresolvable role approver (warned)
	HooksCreated        []string `json:"hooksCreated"`               // inbound webhook hooks created (PACK-HOOKS) — each gets a FRESH secret
	HooksSkipped        []string `json:"hooksSkipped"`               // already existed by name, or an unresolvable entity reference (warned)
	TeamsCreated        []string `json:"teamsCreated"`               // team names created (PACK-TEAMS) — install user added as first member
	TeamsSkipped        []string `json:"teamsSkipped"`               // already existed by name — idempotent skip (id still mapped for routing)

	// App-mint outcome (Phase 4, ../specs/010-app-container/spec.md §0.2): a successful install mints ONE
	// sys_app per pack slug so the pack surfaces in the sidebar App-container IA. Fail-open:
	// AppError records a mint/nav failure WITHOUT failing the (already committed) install.
	// AppSlug is set whenever a mint was attempted; AppMinted=false with AppSlug set and
	// AppError empty means the app already existed (idempotent re-install, left untouched).
	AppMinted bool   `json:"appMinted"`
	AppSlug   string `json:"appSlug,omitempty"`
	AppError  string `json:"appError,omitempty"`

	// Warnings collects non-fatal install notes the caller should surface (KHUON-SONG-P1:
	// provenance skipped for slug-less packs, provenance write failures). Fail-open passes
	// append here instead of failing an already-committed install.
	Warnings []string `json:"warnings,omitempty"`
}

// Install creates every entity/field/relationship/layout/role in the pack into the CURRENT
// tenant. It is:
//
//   - IDEMPOTENT: an entity that already exists in the tenant is skipped (not re-created and not
//     errored); a role whose name already exists is skipped; a layout upsert is a safe overwrite.
//     N-N relationships that already exist are also skipped. Sample records are only PARTIALLY
//     idempotent: a row whose unique value already exists (RECORD_DUPLICATE — typically a
//     re-install) is skipped (SamplesSkipped) together with any row/link that references it by
//     "@ref" (the existing record's id is unknown here); rows WITHOUT unique fields are
//     re-inserted as duplicates (seeding is a one-time bootstrap step). The point of the skip is
//     that a re-install on a populated tenant COMPLETES — reaching layouts/roles/dashboards and
//     the app-mint pass — instead of aborting mid-seed.
//   - ORDERED: entities are created in DEPENDENCY order — a relation field's FK target entity is
//     created BEFORE the entity that references it — so CreateEntity can verify the target exists
//     in the tenant's metadata. Each entity is created WITH all of its fields (including relation
//     fields), so CreateEntity's own shared-obj_* reconcile materializes the FK column (or skips
//     it when a co-tenant already added it to the shared table). A relation graph with cycles
//     (only possible via self-reference, which CreateEntity handles) does not deadlock: a
//     self-referencing entity is created with its own relation field in one call.
//   - BEST-EFFORT REVERSIBLE: obj_* DDL auto-commits (MySQL), so a single DB transaction cannot
//     wrap the whole install. Instead, install tracks the entities IT created (never the ones it
//     skipped) and, on a hard failure, soft-deletes exactly those — leaving a pre-existing
//     entity or a co-tenant's data untouched. Idempotent re-install then completes the rest.
//
// A pack with no entities is rejected (ErrPackEmpty). A per-resource validation failure (bad
// field type, unknown relation target, invalid layout, invalid grant) surfaces as the
// underlying service's ValidationError so the handler maps it to the right status — after the
// created-so-far entities are rolled back.
//
// userID is the authenticated user who triggered the install; it is recorded as created_by on
// every seed record. Pass a well-formed UUID for real users; a stable placeholder for seeds.
func (s *Service) Install(ctx context.Context, tenantID, userID string, pack *Pack) (*InstallResult, error) {
	// A delivered workspace takes changes through drafts only (specs/063 FR-009): refused BEFORE any
	// read or write, for the plain install and for ?targetTenant= alike (both land here).
	if s.delivery != nil {
		delivered, err := s.delivery.Delivered(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.Service.Install: delivered check: %w", err)
		}
		if delivered {
			return nil, &services.ValidationError{Code: "PROJECT_DELIVERED_INSTALL_BLOCKED",
				Msg:     "Dự án đã giao: chưa cài gói vào bản nháp được",
				Details: map[string]any{}}
		}
	}
	// A pack upgrade of the same slug is mid-run (specs/069 FR-050): installing now would race its
	// writes. A run past its lease is not "running" (RunningRun skips it), so a dead process never
	// blocks installs for good.
	if err := s.refuseWhileUpgrading(ctx, tenantID, pack); err != nil {
		return nil, err
	}
	// DECIDE (read-only): compute what would be created vs. skipped, per pass — the SAME source of
	// truth as the dry-run. buildPlan runs the hard-reject gates (empty pack, manifest semver,
	// relation cycle) and every idempotency read, so execute() below only APPLIES the decisions;
	// there is no parallel copy of the install logic that could rot out of sync (dry-run = decide
	// + conflict probes then STOP; real install = decide + execute).
	plan, err := s.buildPlan(ctx, tenantID, pack)
	if err != nil {
		return nil, err
	}
	return s.execute(ctx, tenantID, userID, pack, plan)
}

// execute APPLIES an install plan: it runs every write pass in dependency order, consulting the
// plan's decision sets (which entity/field/role/N-N/dashboard to skip) instead of re-deciding
// inline. It preserves the install's two runtime invariants unchanged: (1) best-effort rollback
// of the entities THIS call created on a hard failure, and (2) for the passes that tolerated it
// BEFORE the refactor — roles, deferred fields and N-N registrations — a concurrent install racing
// us to an ALREADY_EXISTS between decide and write is still an idempotent skip (the entity and
// dashboard passes never tolerated that race and still do not). The behavior — and the resulting
// InstallResult — is byte-for-byte identical to the previous inline body; only the read-only
// decisions moved up-front into buildPlan.
func (s *Service) execute(ctx context.Context, tenantID, userID string, pack *Pack, plan *InstallPlan) (*InstallResult, error) {
	res := &InstallResult{}
	// createdEntities tracks the entity names THIS install created (in order), so compensation
	// can soft-delete exactly those — never a pre-existing entity or a co-tenant's table.
	var createdEntities []string
	// createdIDs maps "<itemType>/<itemKey>" → the live id each create path returned, feeding
	// provenance's live_id anchor (KHUON-SONG-P1 §4.1). Passes without a returned id stay absent.
	createdIDs := map[string]string{}

	// rollback soft-deletes the entities this install created, in reverse order. Best-effort:
	// a cleanup failure is swallowed (the originating error is already being returned and there
	// is no second channel to surface a cleanup failure on without masking the real cause).
	rollback := func() {
		for i := len(createdEntities) - 1; i >= 0; i-- {
			_ = s.entityWr.SoftDeleteEntity(ctx, tenantID, createdEntities[i])
		}
	}

	// --- Pass 1: entities (WITH all fields, including relation), in dependency order. -------
	// plan.ordered was sorted (by buildPlan → orderEntities) so a relation's FK-target entity is
	// created before the entity that references it, letting CreateEntity verify the target exists
	// in this tenant's metadata. CreateEntity builds every field — including the relation FK column
	// — and reconciles a shared obj_* table (a co-tenant may already own the physical table/
	// columns) without double-adding. The create-vs-skip decision comes from plan.entitySkip.
	for _, pe := range plan.ordered {
		name := strings.TrimSpace(pe.Name)
		// Record this entity's relation fields in the result summary regardless of create/skip.
		if plan.entitySkip[name] {
			res.EntitiesSkipped = append(res.EntitiesSkipped, name)
			for _, f := range pe.Fields {
				if isRelationField(f) {
					res.FieldsSkipped = append(res.FieldsSkipped, name+"."+f.Name)
				}
			}
			continue
		}

		in := services.CreateEntityInput{
			Name:        pe.Name,
			Label:       pe.Label,
			Description: pe.Description,
		}
		for _, f := range pe.Fields {
			in.Fields = append(in.Fields, toFieldInput(f))
		}

		ent, _, err := s.entityWr.CreateEntity(ctx, tenantID, in)
		if err != nil {
			rollback()
			// Surface a ValidationError unchanged so the handler maps it to 422/409; wrap others.
			var ve *services.ValidationError
			if errors.As(err, &ve) {
				return nil, err
			}
			return nil, fmt.Errorf("vertical_pack.Install: create entity %q: %w", name, err)
		}
		if ent != nil {
			createdIDs[ItemEntity+"/"+name] = ent.ID
		}
		createdEntities = append(createdEntities, name)
		res.EntitiesCreated = append(res.EntitiesCreated, name)
		for _, f := range pe.Fields {
			if isRelationField(f) {
				res.RelationFieldsAdded = append(res.RelationFieldsAdded, name+"."+f.Name)
			}
		}
	}

	// --- Pass 2: N-N relationships. -------------------------------------------------------
	// Create join tables (DDL) and register metadata rows for every N-N relationship in the
	// pack. Requires both endpoint entities to already exist in this tenant (Pass 1 guarantees
	// that for intra-pack entities). RELATIONSHIP_ALREADY_EXISTS is idempotent-skip.
	// relationWr may be nil when the pack carries no N-N relationships (optional dep).
	if s.relationWr != nil {
		for _, nn := range pack.NNRelationships {
			if plan.nnSkip[nn.Name] {
				res.NNRelationsSkipped = append(res.NNRelationsSkipped, nn.Name)
				continue
			}
			_, err := s.relationWr.RegisterRelationship(ctx, tenantID, services.RegisterRelationshipInput{
				Name:    nn.Name,
				EntityA: nn.EntityA,
				EntityB: nn.EntityB,
			})
			if err != nil {
				if isRelAlreadyExists(err) {
					res.NNRelationsSkipped = append(res.NNRelationsSkipped, nn.Name)
					continue
				}
				rollback()
				var ve *services.ValidationError
				if errors.As(err, &ve) {
					return nil, err
				}
				return nil, fmt.Errorf("vertical_pack.Install: register N-N %q: %w", nn.Name, err)
			}
			res.NNRelationsCreated = append(res.NNRelationsCreated, nn.Name)
		}
	}

	// --- Pass 2b: deferred fields. ----------------------------------------------------------
	// Add the fields that CANNOT be declared inline on their entity in Pass 1 — parent-side
	// rollup fields whose options.childEntity is created LATER in dependency order (the parent
	// of a relation graph is created first, so its rollup's child entity does not exist yet at
	// CreateEntity time → FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND), and N-N rollups whose
	// options.relationship is only registered in Pass 2. Each field goes through the SAME
	// FieldService.AddField path as the admin endpoint (full config-time + cross-entity
	// validation). FIELD_ALREADY_EXISTS is an idempotent skip so re-installing the pack is a
	// clean no-op for deferred fields too. Runs BEFORE Pass 3 so seed records are validated
	// against the complete field set, and BEFORE Pass 5 so layouts may reference these fields.
	if len(pack.DeferredFields) > 0 {
		if s.fieldWr == nil {
			rollback()
			return nil, fmt.Errorf("vertical_pack.Install: pack declares %d deferred fields but no field writer is wired", len(pack.DeferredFields))
		}
		for _, df := range pack.DeferredFields {
			key := df.EntityName + "." + df.Field.Name
			if plan.fieldSkip[key] {
				res.FieldsSkipped = append(res.FieldsSkipped, key)
				continue
			}
			if _, err := s.fieldWr.AddField(ctx, tenantID, services.AddFieldInput{
				EntityName: df.EntityName,
				Field:      toFieldInput(df.Field),
			}); err != nil {
				if isAlreadyExists(err) {
					res.FieldsSkipped = append(res.FieldsSkipped, key)
					continue
				}
				rollback()
				var ve *services.ValidationError
				if errors.As(err, &ve) {
					return nil, err
				}
				return nil, fmt.Errorf("vertical_pack.Install: add deferred field %q: %w", key, err)
			}
			res.DeferredFieldsAdded = append(res.DeferredFieldsAdded, key)
		}
	}

	// --- Reconcile: add fields the pack gained since an EXISTING entity was created. ------
	// The upgrade path. Runs AFTER Pass 1 (so a relation field's target entity exists) and AFTER
	// Pass 2b (deferred fields), BEFORE Pass 3 so seed records validate against the full field set.
	// FIELD_ALREADY_EXISTS stays an idempotent skip. A relation whose target is genuinely absent
	// surfaces the real validation error — the same as any other bad field.
	if len(plan.reconcile) > 0 {
		if s.fieldWr == nil {
			rollback()
			return nil, fmt.Errorf("vertical_pack.Install: pack has %d fields to reconcile but no field writer is wired", len(plan.reconcile))
		}
		for _, rf := range plan.reconcile {
			key := rf.Entity + "." + rf.Field.Name
			if _, err := s.fieldWr.AddField(ctx, tenantID, services.AddFieldInput{
				EntityName: rf.Entity,
				Field:      toFieldInput(rf.Field),
			}); err != nil {
				if isAlreadyExists(err) {
					res.FieldsSkipped = append(res.FieldsSkipped, key)
					continue
				}
				rollback()
				var ve *services.ValidationError
				if errors.As(err, &ve) {
					return nil, err
				}
				return nil, fmt.Errorf("vertical_pack.Install: reconcile field %q: %w", key, err)
			}
			// Reported alongside relation-field adds — it is the same kind of change (a field
			// added to an entity), just on an entity that already existed.
			res.RelationFieldsAdded = append(res.RelationFieldsAdded, key)
		}
	}

	// --- Pass 2c: related-list view config (queue #720). ----------------------------------------
	// Needs the registry rows Pass 1/2 created and the child fields Pass 2b added. A config the
	// target cannot take (unknown column…) is a warning, never a rollback — it is presentation, and
	// the admin PUT's own validation stays the single judge. A relationship the customer already
	// tuned (view_config set) is left alone.
	if err := s.applyRelationViewConfigs(ctx, tenantID, pack, res); err != nil {
		rollback()
		return nil, err
	}

	// --- Pass 3 + 4: seed records, then the N-N links between them. ----------------------------
	// seedSamples (samples.go) holds the walk: entity-dependency order so "@ref" values resolve to
	// real UUIDs, skip-on-duplicate / skip-on-veto with cascade, then Link. A built-in pack
	// (DemoSamples) seeds them as SAMPLE data — source "pack", batch = this install's ledger id — so
	// the builder can count, regenerate and wipe them (specs/064 S9-6). Any other pack seeds
	// ordinary rows, exactly as before.
	installID := newInstallID()
	entityOrder := make([]string, 0, len(pack.Entities))
	for _, pe := range plan.ordered {
		entityOrder = append(entityOrder, pe.Name)
	}
	var mark *dynrepo.SampleMark
	if pack.DemoSamples {
		mark = &dynrepo.SampleMark{BatchID: installID, Source: dynrepo.SampleSourcePack}
	}
	seeded, err := s.seedSamples(ctx, tenantID, userID, pack, entityOrder, seedOpts{mark: mark})
	if err != nil {
		rollback()
		return nil, err
	}
	res.SamplesInserted += seeded.inserted
	res.SamplesSkipped += seeded.skipped
	res.LinksCreated += seeded.linksCreated
	res.LinksSkipped += seeded.linksSkipped
	res.Warnings = append(res.Warnings, seeded.warnings...)

	// --- Pass 5: layouts. -----------------------------------------------------------------
	// SaveLayout is an upsert keyed by (tenant, entity, view), so installing a layout for an
	// entity that already has one overwrites it — re-install stays idempotent (same JSON in →
	// same row). A layout referencing an entity not in this pack (and not in the tenant) is a
	// validation error from the layout service → rollback + surface.
	for _, l := range pack.Layouts {
		in := services.SaveLayoutInput{
			EntityName: l.EntityName,
			ViewType:   l.ViewType,
			LayoutJSON: l.LayoutJSON,
		}
		if _, err := s.layoutWr.SaveLayout(ctx, tenantID, in); err != nil {
			rollback()
			var ve *services.ValidationError
			if errors.As(err, &ve) {
				return nil, err
			}
			return nil, fmt.Errorf("vertical_pack.Install: save layout %q:%q: %w", l.EntityName, l.ViewType, err)
		}
		res.LayoutsInstalled = append(res.LayoutsInstalled, l.EntityName+":"+l.ViewType)
	}

	// --- Pass 6: roles. -------------------------------------------------------------------
	// Skip a role whose name already exists in the tenant (plan.roleSkip, decided from the tenant's
	// current roles); the role service's CreateRole would otherwise 409. Each grant is validated by
	// the role service. A concurrent install racing us to the same role between decide and write is
	// still tolerated (ROLE_ALREADY_EXISTS → idempotent skip).
	for _, pr := range pack.Roles {
		name := strings.TrimSpace(pr.Name)
		if auth.IsOwnerRoleName(name) {
			// The customer-owner role belongs to the workspace, not to an app (specs/065 D-15): a
			// pack must not create it, grant to it or narrow it.
			res.Warnings = append(res.Warnings, fmt.Sprintf("bỏ qua vai hệ thống %q trong gói — vai của riêng không gian, không thuộc app", name))
			continue
		}
		if plan.roleSkip[name] {
			// RE-HANDOVER. Skipping the role whole was right for its NAME and wrong for its GRANTS:
			// on delivery 2+ the role already exists, so every entity shipped in this delivery got
			// no grant on it and the new tables were invisible to every non-admin in the customer
			// tenant — the factory's core promise failing on the second delivery, silently, because
			// the operator is is_admin and sees everything.
			//
			// Reconcile ADDITIVELY. UpdateRole replaces the grant set, so this reads the current
			// grants first and only appends what is missing: a grant the customer widened by hand
			// (owner -> all) is never narrowed back, and one they added is never dropped. A pack
			// must extend a live tenant, never overwrite it.
			added, refusedWildcard, err := s.reconcileRoleGrants(ctx, tenantID, name, pr)
			if err != nil {
				rollback()
				var ve *services.ValidationError
				if errors.As(err, &ve) {
					return nil, err
				}
				return nil, fmt.Errorf("vertical_pack.Install: reconcile role %q: %w", name, err)
			}
			if added > 0 {
				res.RoleGrantsAdded = append(res.RoleGrantsAdded, fmt.Sprintf("%s (+%d)", name, added))
			}
			if refusedWildcard > 0 {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"vai %q: %d quyền wildcard ('*' = MỌI entity) trong pack đã bị TỪ CHỐI, không gắn vào vai sẵn có của tenant — pack chỉ được mở rộng trong phạm vi entity nó mang theo. Nếu thật sự cần, admin tenant tự cấp bằng tay.",
					name, refusedWildcard))
			}
			res.RolesSkipped = append(res.RolesSkipped, name)
			continue
		}
		in := services.CreateRoleInput{Name: pr.Name, Label: pr.Label}
		for _, a := range pr.Actions {
			in.Actions = append(in.Actions, services.RoleActionInput{
				EntityName:  a.EntityName,
				Action:      a.Action,
				AccessLevel: a.AccessLevel,
			})
		}
		rv, err := s.roleWr.CreateRole(ctx, tenantID, in)
		if err != nil {
			// A concurrent install may have created the same role between decide and create;
			// treat ROLE_ALREADY_EXISTS as idempotent-skip rather than a failure.
			if isAlreadyExists(err) {
				res.RolesSkipped = append(res.RolesSkipped, name)
				continue
			}
			rollback()
			var ve *services.ValidationError
			if errors.As(err, &ve) {
				return nil, err
			}
			return nil, fmt.Errorf("vertical_pack.Install: create role %q: %w", name, err)
		}
		if rv != nil {
			createdIDs[ItemRole+"/"+name] = rv.ID
		}
		res.RolesCreated = append(res.RolesCreated, name)
	}

	// --- Pass 6a: field-level ACL (PACK-FIELD-ACL). --------------------------------------------
	// Runs right after roles so a role this install just created resolves; keyed by role NAME
	// because ids do not survive a tenant boundary (same rule as approvals and work queues).
	//
	// Without this the STRICTEST configuration in the app was the one thing handover did not carry:
	// a field hidden from a role in the factory tenant became fully visible to the same-named role
	// in the customer's, silently, with nothing failing and nothing warning.
	if len(pack.FieldACLs) > 0 {
		if s.fieldACLWr == nil {
			// Loud, not silent. Dropping field-level security without saying so is exactly the bug
			// this pass exists to end — so an unwired writer warns instead of quietly continuing.
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"KHÔNG cài được %d quy tắc ẩn/khoá field (chức năng chưa được nối) — kiểm tra lại phân quyền field ở workspace đích trước khi bàn giao.",
				len(pack.FieldACLs)))
		} else {
			grants := make([]services.FieldACLGrantInput, 0, len(pack.FieldACLs))
			for _, fa := range pack.FieldACLs {
				rn := strings.TrimSpace(fa.RoleName)
				if auth.IsOwnerRoleName(rn) {
					continue // the role itself was ignored above; so is every rule that names it
				}
				roleID := createdIDs[ItemRole+"/"+rn]
				if roleID == "" {
					roleID = plan.roleIDByName[rn]
				}
				if roleID == "" {
					// A rule naming a role the target does not have cannot be applied. Warn rather
					// than fail: the rest of the app is installable, and a missing restriction the
					// operator KNOWS about is recoverable, whereas a failed install is not.
					res.Warnings = append(res.Warnings, fmt.Sprintf(
						"bỏ qua quy tắc field %q.%q: không tìm thấy vai %q ở workspace đích",
						fa.EntityName, fa.FieldName, rn))
					continue
				}
				grants = append(grants, services.FieldACLGrantInput{
					RoleID: roleID, EntityName: fa.EntityName, FieldName: fa.FieldName,
					CanView: fa.CanView, CanEdit: fa.CanEdit,
				})
			}
			if len(grants) > 0 {
				if _, err := s.fieldACLWr.PutGrants(ctx, tenantID, grants); err != nil {
					rollback()
					var ve *services.ValidationError
					if errors.As(err, &ve) {
						return nil, err
					}
					return nil, fmt.Errorf("vertical_pack.Install: field ACL: %w", err)
				}
				res.FieldACLsApplied = len(grants)
			}
		}
	}

	// --- Pass 6b: teams (PACK-TEAMS). --------------------------------------------------------
	// Seed the pack's teams AFTER roles and BEFORE dashboards/workflows. This ORDER is load-bearing:
	// a pack ships routing (an `assign` action) by NAMING a team, and a pack cannot know the
	// installed team's id — so this pass must run before the workflows pass (Pass 8) can resolve
	// each assign action's team NAME → id (teamNameToID, built here). Teams don't depend on entities,
	// so running before dashboards/samples is fine.
	//
	// Idempotency: plan.teamSkip (decided from the tenant's current teams) marks the ones already
	// present by name — skipped, but their id is STILL mapped so an assign referencing an existing
	// team resolves. A newly-created team gets the INSTALL USER added as its first active member so
	// round-robin routing has ≥1 member immediately (the Nexus admin adds real agents post-install;
	// the ring rotates over the sole installer until then). Like dashboards/workflows this is a
	// committed-write seed pass, so a failure here does NOT run rollback() — the entity-rollback
	// would orphan the already-committed roles/samples/layouts, and an idempotent re-install
	// completes the rest.
	teamNameToID := map[string]string{}
	if s.teamWr != nil && len(pack.Teams) > 0 {
		for _, pt := range pack.Teams {
			name := strings.TrimSpace(pt.Name)
			// Pre-existing by name (or created earlier in THIS loop for an intra-pack dup): skip the
			// create, but keep the id mapped so an assign action referencing it still resolves.
			if plan.teamSkip[name] {
				res.TeamsSkipped = append(res.TeamsSkipped, name)
				teamNameToID[name] = plan.teamIDByName[name]
				continue
			}
			if _, done := teamNameToID[name]; done {
				res.TeamsSkipped = append(res.TeamsSkipped, name) // intra-pack duplicate team name
				continue
			}
			team := &auth.Team{ID: newInstallID(), TenantID: tenantID, Name: name}
			if err := s.teamWr.CreateTeam(ctx, team); err != nil {
				var ve *services.ValidationError
				if errors.As(err, &ve) {
					return nil, err
				}
				return nil, fmt.Errorf("vertical_pack.Install: create team %q: %w", name, err)
			}
			// Seeding the installer as first member is a CONVENIENCE (so round-robin has ≥1 member
			// immediately), not a correctness requirement — so it must never fail the install.
			//
			// It is also impossible on the path that matters most: a cross-tenant handover
			// (?targetTenant=) runs as the SOURCE tenant's admin, who is not a member of the target
			// tenant, so AddMember returns "member user not found in tenant". Before this fix that
			// hard-failed the whole install with a 500 — AFTER entities, roles, layouts and samples
			// had already committed — meaning every handover of a pack shipping teams (the CRM pack
			// ships "Đội Sales") left the customer a half-built workspace and an opaque error.
			// Measured 2026-08-02 on a fresh tenant.
			//
			// Warn instead: the team exists, it simply has no members yet, and the admin adds real
			// agents post-install exactly as the comment above always said they would.
			if err := s.teamWr.AddMember(ctx, tenantID, team.ID, userID, newInstallID()); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"đội %q đã tạo nhưng chưa có thành viên (%v) — thêm người vào đội trước khi bật định tuyến vòng tròn", name, err))
			}
			res.TeamsCreated = append(res.TeamsCreated, name)
			teamNameToID[name] = team.ID
			createdIDs[ItemTeam+"/"+name] = team.ID
		}
	}

	// --- Pass 5b: shared saved views (queue #720). ----------------------------------------------
	// After entities (a view's target must exist). A committed-write pass like dashboards: a view
	// that cannot be made is a warning, never a rollback — it is a presentation preset.
	if err := s.installViews(ctx, tenantID, userID, pack, res); err != nil {
		return nil, err
	}

	// --- Pass 6c: working calendars (queue #720). ------------------------------------------------
	// Before the workflows pass, which resolves each rule's calendarName to the id made (or found)
	// here. Created through BusinessCalendarService (the admin CRUD's validation); a name the tenant
	// already has is kept as is — never overwritten — and its id still maps.
	calNameToID, err := s.installCalendars(ctx, tenantID, pack, res)
	if err != nil {
		return nil, err
	}

	// --- Pass 7: dashboards. -----------------------------------------------------------------
	// sys_dashboards has no UNIQUE(tenant_id,name) constraint, so a blind insert on re-install
	// would silently duplicate the row. plan.dashSkip (decided from the tenant's existing
	// dashboards) marks the ones already present by name; create only the rest. A non-
	// ValidationError failure surfaces immediately (rollback already complete from prior passes;
	// dashboards are the last pass so rollback is a no-op here — entities/fields/layouts/roles
	// are all committed; a dashboard failure surfaces cleanly without corrupting prior work).
	if s.dashboardWr != nil && len(pack.Dashboards) > 0 {
		// createdNames dedupes intra-pack duplicate dashboard names: sys_dashboards has no
		// UNIQUE(tenant_id,name), so unlike roles/fields/N-N there is no 1062 to catch a second
		// same-name create. plan.dashSkip only reflects PRE-install state, so restore the old
		// inline "create 1st, skip 2nd" behavior here (reviewer: keeps execute byte-identical).
		createdNames := map[string]bool{}
		for _, pd := range pack.Dashboards {
			name := strings.TrimSpace(pd.Name)
			if plan.dashSkip[name] || createdNames[name] {
				res.DashboardsSkipped = append(res.DashboardsSkipped, name)
				continue
			}
			drec, err := s.dashboardWr.CreateDashboard(ctx, tenantID, toDashboardInput(pd, userID))
			if err != nil {
				var ve *services.ValidationError
				if errors.As(err, &ve) {
					return nil, err
				}
				return nil, fmt.Errorf("vertical_pack.Install: create dashboard %q: %w", name, err)
			}
			if drec != nil {
				createdIDs[ItemDashboard+"/"+name] = drec.ID
			}
			res.DashboardsCreated = append(res.DashboardsCreated, name)
			createdNames[name] = true
		}
	}

	// --- Pass 8: workflows. ------------------------------------------------------------------
	// Seed the pack's automation rules LAST — after samples (Pass 3) AND dashboards (Pass 7).
	// THIS ORDER IS CRITICAL: a pack's sample inserts go through s.recordWr.Create =
	// RecordService.Create, which fires the workflow dispatcher; a workflow created BEFORE the
	// samples would TRIGGER on the seed inserts (unwanted side effects during install). Creating
	// them here means the seed data is already in place and untouched. (Running after Pass 1 also
	// means the rule's entity is already in metadata, so WORKFLOW_ENTITY_NOT_FOUND cannot hit.)
	//
	// Each workflow goes through the SAME full config-time validation as POST /admin/workflows
	// (buildWorkflowRule + the WF-01 field-target guard + the DB-dependent timer/team checks) — so
	// a pack rule referencing a missing field/team FAILS install with the real error code (the
	// contract: install reuses the admin write path, never a weaker one). sys_workflows has no
	// UNIQUE(tenant_id,name), so — like dashboards — plan.wfSkip marks the ones already present by
	// name and createdWF dedupes intra-pack same-name rules. Like the dashboard pass, a failure here
	// does NOT run rollback(): the prior data passes (entities/samples/layouts/roles/dashboards) are
	// already committed and auto-committing DDL cannot be undone, so tearing the entities down would
	// leave orphaned committed data — the failure surfaces cleanly and an idempotent re-install
	// completes the rest instead.
	//
	// start_approval rules (specs/043 S9b) name a process of THIS pack, so they wait: the approvals
	// pass (8d) creates the processes, then lateWF installs these through the same body.
	// specs/050 S6: the vault key slots the pack's webhook rules name are made BEFORE the rules, so
	// each rule saves against a live key of THIS tenant (the same checker the admin path runs).
	credIDByName, err := s.ensureCredentialSlots(ctx, tenantID, userID, pack, res)
	if err != nil {
		return nil, err
	}
	createdWF := map[string]bool{}
	var lateWF []PackWorkflow
	installWorkflow := func(pw PackWorkflow, processIDByKey map[string]string) error {
		name := strings.TrimSpace(pw.Name)
		if plan.wfSkip[name] || createdWF[name] {
			res.WorkflowsSkipped = append(res.WorkflowsSkipped, name)
			return nil
		}
		// A pack that still carries outbound_webhook credentials (authored before export
		// started stripping them, or hand-edited) has them removed by toWorkflowInput below.
		// Say so: the rule installs but cannot sign its requests until THIS workspace has its
		// own key — in the vault (specs/050 S3a: a key on the rule itself is refused now), and
		// a silently unsigned webhook is the kind of half-working this codebase keeps paying for.
		if _, stripped := stripWorkflowCredentials(pw); stripped > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"workflow %q: %d action outbound_webhook mang khoá ký/header của tenant nguồn — đã GỠ khi cài (khoá không đi xuyên tenant). Tạo khoá ở Quản trị › Kho khoá kết nối rồi chọn khoá đó cho action trong trình dựng quy trình thì rule mới ký/gọi được endpoint.",
				name, stripped))
		}
		// A start_approval whose process did not make it into this tenant (a non-portable approver,
		// a name nobody seeded) cannot run: skip THAT rule with a warning — Pass 8 is past the
		// point where an install can be rolled back, and the other rules are unaffected.
		for _, a := range pw.Actions {
			if strings.TrimSpace(a.Type) != "start_approval" {
				continue
			}
			if _, ok := processIDByKey[approvalDupKey(strings.TrimSpace(pw.EntityName), strings.TrimSpace(a.ProcessName))]; !ok {
				res.WorkflowsSkipped = append(res.WorkflowsSkipped, name)
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"workflow %q: bỏ qua — action start_approval trỏ quy trình duyệt %q của %q mà tổ chức này không có (quy trình bị bỏ khi cài vì người duyệt không mang được qua gói, hoặc pack không khai báo nó). Tạo quy trình rồi dựng lại luật.",
					name, a.ProcessName, pw.EntityName))
				return nil
			}
		}
		// Resolve any assign action's team NAME → the seeded team id (teamNameToID, built by the
		// teams pass above). A name the pack never seeded is a hard install error (fail fast) —
		// never persist a routing rule that can never route.
		pw, err := resolveCalendarNames(pw, calNameToID)
		if err != nil {
			return err
		}
		in, err := toWorkflowInput(pw, userID, teamNameToID, processIDByKey, credIDByName)
		if err != nil {
			var ve *services.ValidationError
			if errors.As(err, &ve) {
				return err
			}
			return fmt.Errorf("vertical_pack.Install: resolve workflow %q: %w", name, err)
		}
		wrec, err := s.workflowWr.CreateWorkflow(ctx, tenantID, in)
		if err != nil {
			if isAlreadyExists(err) { // race vs plan snapshot (3.1) — idempotent skip
				res.WorkflowsSkipped = append(res.WorkflowsSkipped, name)
				createdWF[name] = true
				return nil
			}
			var ve *services.ValidationError
			if errors.As(err, &ve) {
				// A start_approval naming a process that exists but is not manual/active (a same-named
				// process already in the tenant, or a pack that ships it switched off): by Pass 8d' the
				// install is past rollback, so skip THAT rule with a warning instead of aborting.
				if ve.Code == "WF_START_APPROVAL_INVALID" {
					res.WorkflowsSkipped = append(res.WorkflowsSkipped, name)
					res.Warnings = append(res.Warnings, fmt.Sprintf(
						"workflow %q: bỏ qua — action start_approval trỏ quy trình duyệt không dùng được (cần quy trình 'gửi tay' đang bật, cùng đối tượng): %s",
						name, ve.Msg))
					createdWF[name] = true
					return nil
				}
				return err
			}
			return fmt.Errorf("vertical_pack.Install: create workflow %q: %w", name, err)
		}
		if wrec != nil {
			createdIDs[ItemWorkflow+"/"+name] = wrec.ID
		}
		res.WorkflowsCreated = append(res.WorkflowsCreated, name)
		createdWF[name] = true
		return nil
	}
	if s.workflowWr != nil {
		for _, pw := range pack.Workflows {
			if packWorkflowStartsApproval(pw) {
				lateWF = append(lateWF, pw)
				continue
			}
			if err := installWorkflow(pw, nil); err != nil {
				return nil, err
			}
		}
	}

	// Rules skipped by name that are not what this pack version ships (DAILY-QUEUE #171).
	if w := s.staleWorkflowsWarning(ctx, tenantID, pack, plan); w != "" {
		res.Warnings = append(res.Warnings, w)
	}

	// --- Pass 8a: working-calendar assurance (DAILY-QUEUE #82). -------------------------------
	// A business_hours rule in a tenant with no usable DEFAULT calendar still runs — on the
	// documented wall-clock fallback (specs/023 §1) — so nothing fails, and nothing told the
	// installer either: Ticketing's four SLA tiers quietly promised wall-clock deadlines in a tenant
	// whose calendars were all deleted. The install result is the one screen the installer reads,
	// so the fact goes there. Checked even when the rules were skipped as already present: they
	// exist either way, and the calendar may have been removed since the last install.
	if w := s.packCalendarWarning(ctx, tenantID, pack); w != "" {
		res.Warnings = append(res.Warnings, w)
	}

	// --- Pass 8b: reports (PACK-REPORTS). ----------------------------------------------------
	// Seed the pack's saved reports through the SAME validated path as POST /admin/reports
	// (ReportService.CreateReport → ValidateReportConfig: type/shape + entity existence). A report
	// is a stored DEFINITION that only RUNS on demand and fires no dispatcher, so — unlike workflows
	// — its position relative to the seed inserts is immaterial for side effects; it runs here (after
	// samples/dashboards/workflows) purely to keep the "analytics surfaces last" convention. Pass 1
	// guarantees the report's entity is already in metadata, so REPORT_ENTITY_NOT_FOUND cannot hit a
	// pack entity. sys_reports has no UNIQUE(tenant_id,name), so — like dashboards/workflows —
	// plan.reportSkip marks the ones already present by name and createdReports dedupes intra-pack
	// same-name reports. A malformed pack report FAILS install with the real code (surfaced unchanged
	// so the handler maps it to 422); like the dashboard/workflow passes a failure here does NOT run
	// rollback() (prior committed data can't be undone; an idempotent re-install completes the rest).
	if s.reportWr != nil && len(pack.Reports) > 0 {
		createdReports := map[string]bool{}
		for _, pr := range pack.Reports {
			name := strings.TrimSpace(pr.Name)
			if plan.reportSkip[name] || createdReports[name] {
				res.ReportsSkipped = append(res.ReportsSkipped, name)
				continue
			}
			rrec, err := s.reportWr.CreateReport(ctx, tenantID, toReportInput(pr, userID))
			if err != nil {
				if isAlreadyExists(err) { // race vs plan snapshot (3.1) — idempotent skip
					res.ReportsSkipped = append(res.ReportsSkipped, name)
					createdReports[name] = true
					continue
				}
				var ve *services.ValidationError
				if errors.As(err, &ve) {
					return nil, err
				}
				return nil, fmt.Errorf("vertical_pack.Install: create report %q: %w", name, err)
			}
			if rrec != nil {
				createdIDs[ItemReport+"/"+name] = rrec.ID
			}
			res.ReportsCreated = append(res.ReportsCreated, name)
			createdReports[name] = true
		}
	}

	// --- Pass 8c: work queues (PACK-QUEUES). -------------------------------------------------
	// Seed the pack's "Việc Hôm Nay" queues through the SAME validated path as POST /admin/
	// work-queues (WorkQueueService.Create: entity/field/operator/date-token/sort/role checks).
	// Runs after roles (Pass 6) so a queue's RoleName resolves: a role this install created is in
	// createdIDs["role/<name>"]; a pre-existing one is in plan.roleIDByName. An unresolvable role
	// name FAILS install with a real 422 — a role-scoped queue silently installed as visible-to-
	// everyone would be a security-posture downgrade, the same reason a workflow assign team hard-
	// fails. plan.wqSkip keeps re-install idempotent (list-then-skip; the table's UNIQUE would 409).
	if s.workQueueWr != nil && len(pack.WorkQueues) > 0 {
		createdQ := map[string]bool{}
		for _, pq := range pack.WorkQueues {
			name := strings.TrimSpace(pq.Name)
			if plan.wqSkip[name] || createdQ[name] {
				res.WorkQueuesSkipped = append(res.WorkQueuesSkipped, name)
				continue
			}
			roleID := ""
			if rn := strings.TrimSpace(pq.RoleName); rn != "" {
				roleID = createdIDs[ItemRole+"/"+rn]
				if roleID == "" {
					roleID = plan.roleIDByName[rn]
				}
				if roleID == "" {
					return nil, invalid("WORKQUEUE_ROLE_NOT_FOUND",
						"pack queue %q: role %q không tồn tại trong pack lẫn tenant", name, rn)
				}
			}
			qrec, err := s.workQueueWr.Create(ctx, tenantID, toWorkQueueInput(pq, roleID))
			if err != nil {
				var ve *services.ValidationError
				if errors.As(err, &ve) {
					// Upgrade-path guard (review SHOULD-FIX 1): when the queue's entity PRE-EXISTED
					// this install (Pass 1 skip), its schema is the tenant's historical one — an
					// older pack version may lack the indexed sort column or a filter field the new
					// queue expects. That is not a broken pack: degrade to a warned skip so the
					// re-install COMPLETES instead of wedging on 422 forever (field options are
					// never reconciled on skip-entity). A fresh entity failing validation is a
					// genuinely broken pack → still hard-fails with the real code.
					if plan.entitySkip[strings.TrimSpace(pq.EntityName)] {
						res.Warnings = append(res.Warnings, fmt.Sprintf(
							"work queue %q: bỏ qua — entity %q có sẵn từ bản pack cũ không đáp ứng cấu hình queue (%s)",
							name, strings.TrimSpace(pq.EntityName), ve.Msg))
						res.WorkQueuesSkipped = append(res.WorkQueuesSkipped, name)
						continue
					}
					return nil, err
				}
				return nil, fmt.Errorf("vertical_pack.Install: create work queue %q: %w", name, err)
			}
			if qrec != nil {
				createdIDs[ItemWorkQueue+"/"+name] = qrec.ID
			}
			res.WorkQueuesCreated = append(res.WorkQueuesCreated, name)
			createdQ[name] = true
		}
	}

	// --- Pass 8d: approval processes (PACK-APPROVALS). ---------------------------------------
	// Seed the pack's approval flows AFTER workflows (../specs/027-handover-wizard/spec.md §3.2) — and, like
	// workflows, strictly AFTER the samples: an after_create process created before Pass 3 would
	// arm approval instances on the seed inserts. Each process goes through the SAME config-time
	// validation as POST /admin/approval-processes (ApprovalService.CreateProcess: name/entity/
	// trigger/criteria/steps — validator-parity by construction, no second write path), so a
	// malformed pack process FAILS install with the real error code. Idempotency is by
	// (entity, name): plan.apSkip marks pre-existing pairs, createdAP dedupes intra-pack repeats.
	//
	// Portability (fail-open, mirroring appMint): a step's role NAME is resolved to the TARGET
	// tenant's role id — a role THIS install created (createdIDs) or a pre-existing one
	// (plan.roleIDByName). A name that resolves nowhere — or a non-portable "user" approver in a
	// hand-authored pack — SKIPS that process with a warning instead of failing the committed
	// install (the honest-warnings panel tells the admin to reassign approvers post-install).
	// Like the other late passes, a hard failure here does NOT run rollback().
	if s.approvalWr != nil && len(pack.Approvals) > 0 {
		createdAP := map[string]bool{}
		for _, pa := range pack.Approvals {
			name := strings.TrimSpace(pa.Name)
			entityName := strings.TrimSpace(pa.EntityName)
			key := approvalDupKey(entityName, name)
			display := entityName + ":" + name
			if plan.apSkip[key] || createdAP[key] {
				res.ApprovalsSkipped = append(res.ApprovalsSkipped, display)
				continue
			}

			in, unresolved, unresolvedKind := toApprovalInput(pa, userID, approvalRefs{
				role: func(ref string) string {
					ref = strings.TrimSpace(ref)
					if id := createdIDs[ItemRole+"/"+ref]; id != "" {
						return id
					}
					return plan.roleIDByName[ref]
				},
				team: func(ref string) string {
					if id := teamNameToID[ref]; id != "" {
						return id
					}
					return plan.teamIDByName[ref]
				},
				queue: func(ref string) string {
					if id := createdIDs[ItemWorkQueue+"/"+ref]; id != "" {
						return id
					}
					return plan.wqIDByName[ref]
				},
			})
			if unresolved != "" {
				if unresolved == approvalUserApprover {
					res.Warnings = append(res.Warnings, fmt.Sprintf(
						"quy trình duyệt '%s': người duyệt chỉ định đích danh không đóng gói được — gán lại sau khi cài", name))
				} else {
					res.Warnings = append(res.Warnings, fmt.Sprintf(
						"quy trình duyệt %q: bỏ qua — %s %q không tồn tại trong pack lẫn tenant đích; gán lại người duyệt sau khi cài", name, unresolvedKind, unresolved))
				}
				res.ApprovalsSkipped = append(res.ApprovalsSkipped, display)
				createdAP[key] = true // intra-pack repeat of the same broken pair warns once
				continue
			}

			rec, err := s.approvalWr.CreateProcess(ctx, tenantID, in)
			if err != nil {
				var ve *services.ValidationError
				if errors.As(err, &ve) {
					return nil, err
				}
				return nil, fmt.Errorf("vertical_pack.Install: create approval process %q: %w", name, err)
			}
			if rec != nil {
				createdIDs[ItemApproval+"/"+display] = rec.ID
			}
			res.ApprovalsCreated = append(res.ApprovalsCreated, display)
			createdAP[key] = true
		}
	}

	// --- Pass 8d': start_approval rules (specs/043 S9b). ---------------------------------------------
	// The processes they name now exist (created above, or already in the tenant): resolve NAME →
	// id by (entity, name) and install them through the same body as Pass 8.
	if s.workflowWr != nil && len(lateWF) > 0 {
		processIDByKey := map[string]string{}
		if s.approvalWr != nil {
			procs, err := s.approvalWr.ListProcesses(ctx, tenantID)
			if err != nil {
				return nil, fmt.Errorf("vertical_pack.Install: list approval processes: %w", err)
			}
			for _, p := range procs {
				processIDByKey[approvalDupKey(strings.TrimSpace(p.EntityName), strings.TrimSpace(p.Name))] = p.ID
			}
		}
		for _, pw := range lateWF {
			if err := installWorkflow(pw, processIDByKey); err != nil {
				return nil, err
			}
		}
	}

	// --- Pass 8e: inbound webhook hooks (PACK-HOOKS). ----------------------------------------
	// Seeds the pack's receiving endpoints so a money loop (or any partner feed) arrives WIRED
	// instead of hand-built per customer. Runs last among the config passes: a hook can reference
	// any entity the earlier passes created, and it must never be live before the samples land.
	//
	// The secret is MINTED HERE, never carried: PackInboundHook has no secret field at all, so an
	// exported pack can neither hand another tenant a working key for this one's endpoint nor
	// leave a second copy at rest in sys_pack_items (provenance stores every pack struct's JSON).
	// The cost is that the admin must fetch the new secret before the partner sends — surfaced as
	// a warning per hook rather than left to be discovered by a silent 401.
	//
	// Failure split mirrors the approvals pass: an unresolvable ENTITY reference warns and skips
	// (entities and samples have already committed; late passes do not roll back), while a
	// malformed hook — bad regex, empty mapping, a refField that is not unique in the target —
	// fails loudly, because that means we shipped a broken pack and silence would leave the
	// customer with a dead endpoint they believe is live.
	if s.hookWr != nil && len(pack.InboundHooks) > 0 {
		createdHooks := map[string]bool{}
		for _, ph := range pack.InboundHooks {
			name := strings.TrimSpace(ph.Name)
			if plan.hookSkip[name] || createdHooks[name] {
				res.HooksSkipped = append(res.HooksSkipped, name)
				continue
			}

			in := toInboundHookInput(ph)

			created, err := s.hookWr.Create(ctx, tenantID, userID, in)
			if err != nil {
				// An entity this pack does not carry (or a field the target tenant shaped
				// differently) is a portability problem, not a broken pack: warn + skip.
				if isMissingReference(err) {
					res.HooksSkipped = append(res.HooksSkipped, name)
					res.Warnings = append(res.Warnings, fmt.Sprintf(
						"hook nhận tín hiệu %q: bỏ qua — %v; dựng lại thủ công sau khi cài", name, err))
					continue
				}
				return nil, fmt.Errorf("vertical_pack.Install: create inbound hook %q: %w", name, err)
			}
			res.HooksCreated = append(res.HooksCreated, name)
			createdHooks[name] = true
			if created != nil {
				createdIDs[ItemInboundHook+"/"+name] = created.ID
			}
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"hook nhận tín hiệu %q đã tạo với SECRET MỚI — lấy trong Cài đặt rồi mới báo đối tác gửi", name))
		}
	}

	// --- Pass 9: mint the sidebar App (Phase 4, ../specs/010-app-container/spec.md §0.2). --------------------
	// One sys_app per pack slug, nav = pack entities (pack order) + dashboards. Strictly
	// fail-open — the metadata above is committed and correct, the app is a cosmetic lens —
	// so a mint failure logs + records on the result (AppMinted/AppSlug/AppError) instead of
	// failing the install. No-op when no app service is wired (nil optional dep) or the pack
	// has no manifest slug. Runs BEFORE recordInstall so the ledger row captures the outcome.
	s.mintApp(ctx, tenantID, userID, pack, res)

	// Ledger: record one row per successful install (slug/version from the manifest, empty when
	// the pack has none). Fail-open — a ledger insert failure is logged, never fails the install,
	// which has already committed its schema/data (auto-committing DDL cannot be un-done here).
	s.recordInstall(ctx, installID, tenantID, userID, pack, res)

	// Provenance (KHUON-SONG-P1): persist each pack item's as-shipped content so the future
	// upgrade 3-way merge (and today's diff report) has its ancestor. Fail-open like the ledger,
	// but every skip/failure reason lands in res.Warnings — never silent.
	s.recordProvenance(ctx, tenantID, pack, plan, res, createdIDs)

	return res, nil
}

// recordInstall writes one fail-open row to the install ledger. slug/version come from the pack's
// manifest (empty for legacy manifest-less packs). No-op when no install store is wired. Any error
// is swallowed after a warning — the install itself already succeeded and must not be reported as
// failed because the ledger note could not be written.
func (s *Service) recordInstall(ctx context.Context, installID, tenantID, userID string, pack *Pack, res *InstallResult) {
	if s.installs == nil {
		return
	}
	var slug, version string
	if pack.Manifest != nil {
		slug = pack.Manifest.Slug
		version = pack.Manifest.Version
	}
	result := installResultToMap(res)
	// MS-C actor traceability: sys_pack_installs has no installed_by column (spec §2: no new
	// migration), so the acting user rides result_json — enough for the target-side ledger to say
	// WHO ran the install (a cross-tenant handover records the OPERATOR here, plus the source-
	// tenant __handover__ audit event emitted by the handler).
	if result != nil && strings.TrimSpace(userID) != "" {
		result["installedBy"] = userID
	}
	// A built-in install says so: the sample-data surface re-seeds samples only from packs the ledger
	// names this way, so an upload that merely reuses a built-in slug (a handover export of the CRM app
	// is named "crm") never passes for the pack we ship (specs/064 S9-6).
	if result != nil && pack.DemoSamples {
		result["demoSamples"] = true
	}
	rec := InstallRecord{
		ID:          installID,
		TenantID:    tenantID,
		Slug:        slug,
		Version:     version,
		InstalledAt: nowUTC(),
		Result:      result,
	}
	if err := s.installs.RecordInstall(ctx, rec); err != nil {
		s.logger.Warn("vertical_pack.recordInstall: ledger write failed (install succeeded)",
			zap.String("tenant_id", tenantID),
			zap.String("user_id", userID),
			zap.String("slug", slug),
			zap.String("version", version),
			zap.Error(err))
	}
}

// orderEntities returns the pack's entities sorted so that every entity appears AFTER all of its
// relation targets that are themselves in the pack (parents before children). This lets each
// CreateEntity verify its relation fields' FK targets already exist in the tenant's metadata.
//
// Only intra-pack dependencies constrain the order: a relation pointing at an entity NOT in the
// pack (assumed already present in the tenant) imposes no ordering. A SELF-reference imposes no
// ordering either — CreateEntity materializes a self-referencing relation in the same call.
//
// A genuine cycle between two distinct entities (A→B and B→A) cannot be satisfied by ordering;
// it surfaces as VERTICAL_PACK_CYCLE so the caller sees a clear 422 rather than a confusing
// downstream FIELD_RELATION_TARGET_NOT_FOUND. (The 1-N FK-on-child model does not normally
// produce such cycles.)
func orderEntities(entities []PackEntity) ([]PackEntity, error) {
	// Build the set of in-pack entity names.
	inPack := make(map[string]struct{}, len(entities))
	for _, e := range entities {
		inPack[e.Name] = struct{}{}
	}

	// deps[child] = set of in-pack parent names it must follow.
	deps := make(map[string]map[string]struct{}, len(entities))
	for _, e := range entities {
		set := map[string]struct{}{}
		for _, f := range e.Fields {
			if !isRelationField(f) {
				continue
			}
			target, _ := f.Options["relatedEntity"].(string)
			target = strings.TrimSpace(target)
			if target == "" || target == e.Name {
				continue // external target or self-reference: no ordering constraint
			}
			if _, ok := inPack[target]; ok {
				set[target] = struct{}{}
			}
		}
		deps[e.Name] = set
	}

	// Kahn's algorithm with stable tie-breaking (input order) for deterministic output.
	resolved := make(map[string]struct{}, len(entities))
	var out []PackEntity
	for len(out) < len(entities) {
		progressed := false
		for _, e := range entities {
			if _, done := resolved[e.Name]; done {
				continue
			}
			ready := true
			for parent := range deps[e.Name] {
				if _, ok := resolved[parent]; !ok {
					ready = false
					break
				}
			}
			if ready {
				out = append(out, e)
				resolved[e.Name] = struct{}{}
				progressed = true
			}
		}
		if !progressed {
			// No entity became ready this round → a dependency cycle among the remaining ones.
			var stuck []string
			for _, e := range entities {
				if _, done := resolved[e.Name]; !done {
					stuck = append(stuck, e.Name)
				}
			}
			return nil, invalid("VERTICAL_PACK_CYCLE",
				"vertical pack có quan hệ relation vòng (cycle) giữa các entity: %s", strings.Join(stuck, ", "))
		}
	}
	return out, nil
}
func toFieldInput(f PackField) services.FieldInput {
	return services.FieldInput{
		Name:       f.Name,
		Label:      f.Label,
		FieldType:  f.FieldType,
		IsRequired: f.IsRequired,
		IsUnique:   f.IsUnique,
		Options:    f.Options,
	}
}

// businessHoursRuleNames lists the pack workflows whose time math walks the tenant's DEFAULT
// working calendar: a set_deadline in business_hours that pins no calendar of its own (queue #79),
// a set_elapsed (always — DAILY-QUEUE #41), or a business_hours date_offset timer that pins no
// calendar of its own (DAILY-QUEUE #82).
func businessHoursRuleNames(pack *Pack) []string {
	var names []string
	for _, pw := range pack.Workflows {
		uses := pw.Condition != nil && pw.Condition.Timer != nil &&
			strings.TrimSpace(pw.Condition.Timer.Offset.Unit) == workflow.TimerUnitBusinessHours &&
			strings.TrimSpace(pw.Condition.Timer.CalendarID) == "" && strings.TrimSpace(pw.Condition.Timer.CalendarName) == ""
		for _, a := range pw.Actions {
			typ := strings.TrimSpace(a.Type)
			// A set_deadline pinned to its own calendar (queue #79) never reads the default — same
			// exclusion as the pinned timer above.
			if typ == workflow.ActionSetElapsed || (typ == workflow.ActionSetDeadline && a.Offset != nil &&
				strings.TrimSpace(a.Offset.Unit) == workflow.TimerUnitBusinessHours && strings.TrimSpace(a.CalendarID) == "" && strings.TrimSpace(a.CalendarName) == "") {
				uses = true
			}
		}
		if uses {
			names = append(names, strings.TrimSpace(pw.Name))
		}
	}
	return names
}

// packCalendarWarning is Pass 8a's one decision, shared by Install (InstallResult.warnings) and
// DryRunInstall (InstallPlan.warnings, DAILY-QUEUE #104) so the plan says exactly what the install
// will say. "" when the workflows pass is not wired (no rule would land), no resolver is wired, the
// pack has no rule that reads the default calendar, or the tenant has a usable default one.
func (s *Service) packCalendarWarning(ctx context.Context, tenantID string, pack *Pack) string {
	if s.workflowWr == nil || s.sched == nil {
		return ""
	}
	names := businessHoursRuleNames(pack)
	if len(names) == 0 {
		return ""
	}
	return s.calendarWarning(ctx, tenantID, names)
}

// staleWorkflowsWarning names the pack workflows this install SKIPS because a rule with the same
// name already exists, and whose live config is not what this pack version ships (DAILY-QUEUE
// #171). Re-installing Ticketing 1.8.0 on a 1.7.0 tenant added due_date_auto and the four
// "Tính lại hạn SLA" rules but silently kept the four old "Đặt hạn SLA — phiếu …" rules without
// stampField — so no ticket was ever stamped and the recompute never fired, with nothing on the
// install screen to say so. Shared by Install and DryRunInstall (the plan says it before "Cài").
//
// The comparison is the provenance diff engine (Export → packContentIndex → comparePackItems,
// the one whose fresh-install gate proves a builtin reads 100% unchanged right after install), so
// write-path defaults never read as a difference. A live rule the export cannot round-trip is
// named "không đối chiếu được" rather than guessed either way. Read-only and fail-open: an export
// failure is logged and reported as a FIXED sentence.
// ponytail: warns instead of updating. Updating a same-name rule needs the three-way decision
// (pack changed / builder changed / both) and an apply path with its own history — that is
// specs/069-pack-apply-upgrade L2a (DAILY-QUEUE #430), not an install side effect.
func (s *Service) staleWorkflowsWarning(ctx context.Context, tenantID string, pack *Pack, plan *InstallPlan) string {
	// Export reads entities, layouts and roles unguarded; a service built without that read side
	// (install-only wiring in tests) cannot compare, and the server always wires it.
	if s.workflowWr == nil || s.layoutRd == nil || s.roleRd == nil || len(plan.wfSkip) == 0 {
		return ""
	}
	live, _, err := s.Export(ctx, tenantID, nil, false, nil)
	if err != nil {
		s.logger.Warn("vertical_pack: could not compare the tenant's existing rules with the pack",
			zap.String("tenant_id", tenantID), zap.Error(err))
		return "Không đối chiếu được các luật tự động đã có sẵn với bản gói (lỗi đọc dữ liệu) — cài lại không sửa luật trùng tên, nên có thể có luật đang khác bản gói."
	}
	liveIdx := s.packContentIndex(live)
	seen := map[string]bool{}
	var stale []string
	for _, pw := range pack.Workflows {
		name := strings.TrimSpace(pw.Name)
		if !plan.wfSkip[name] || seen[name] {
			continue
		}
		seen[name] = true
		sw, _ := stripWorkflowCredentials(pw) // the provenance/export shape never carries keys
		packRaw, err := marshalItem(sw)
		liveRaw, inExport := liveIdx[ItemWorkflow+"/"+name]
		var paths []string
		if err == nil && inExport {
			paths, err = comparePackItems(ItemWorkflow, packRaw, liveRaw)
		}
		switch {
		case err != nil || !inExport:
			stale = append(stale, name+" (không đối chiếu được)")
		case len(paths) > 0:
			if len(paths) > 3 {
				paths = append(paths[:3], "…")
			}
			stale = append(stale, fmt.Sprintf("%s (khác ở %s)", name, strings.Join(paths, ", ")))
		}
	}
	if len(stale) == 0 {
		return ""
	}
	version := ""
	if pack.Manifest != nil && strings.TrimSpace(pack.Manifest.Version) != "" {
		version = " " + strings.TrimSpace(pack.Manifest.Version)
	}
	return fmt.Sprintf("%d luật tự động đã có sẵn nên KHÔNG được cập nhật, dù khác bản gói%s: %s. Cài lại chỉ thêm phần còn thiếu, không sửa luật trùng tên — muốn dùng bản của gói, sửa từng luật ở Studio → Tự động hoá theo bản gói, hoặc xoá luật đó rồi cài lại gói.",
		len(stale), version, strings.Join(stale, "; "))
}

// calendarWarning answers the #82 question for one tenant: "" when a default working calendar
// resolves, an install warning naming the affected rules otherwise. A store failure is logged with
// its error and reported to the client as a FIXED sentence — a driver message belongs in the
// server log, not in an API response body. Fail-open either way: the install already committed.
func (s *Service) calendarWarning(ctx context.Context, tenantID string, names []string) string {
	rules := strings.Join(names, ", ")
	sched, err := s.sched.ResolveSchedule(ctx, tenantID, "")
	if err != nil {
		s.logger.Warn("vertical_pack: could not check the tenant's working calendar after install",
			zap.String("tenant_id", tenantID), zap.Strings("rules", names), zap.Error(err))
		return fmt.Sprintf("Không kiểm được lịch làm việc của tổ chức (lỗi đọc dữ liệu) — %d luật tính theo giờ làm việc (%s) sẽ tính theo giờ treo tường nếu tổ chức chưa có lịch mặc định. Kiểm tra Quản trị → Lịch làm việc.",
			len(names), rules)
	}
	if sched != nil {
		return ""
	}
	return fmt.Sprintf("%d luật của gói tính theo giờ làm việc (%s) nhưng tổ chức chưa có lịch làm việc mặc định dùng được — cho tới khi Quản trị → Lịch làm việc có một lịch đặt làm mặc định, các luật này tính theo giờ treo tường (không trừ đêm, cuối tuần, ngày nghỉ).",
		len(names), rules)
}

// toWorkflowInput maps a pack-authored PackWorkflow to services.CreateWorkflowInput — the SAME
// shape the admin handler (admin_workflow_handler.go: toServiceRules + toServiceWorkflowActions)
// produces from a POST /admin/workflows body — so a pack workflow validates and persists through
// the exact admin path. ActorID is the install user (recorded as created_by on the rule).
//
// PACK-TEAMS: an `assign` action carries its team by NAME (a pack cannot know the installed team's
// id). teamNameToID (built by the teams pass, which runs BEFORE this) maps every team the pack
// references (created OR pre-existing-by-name) to its real id. A name found → the id replaces it;
// a name NOT found → an install error (fail fast — never persist a rule that can never route). A
// non-assign action, or a pack with no assign actions, needs no map entry (empty map is fine).
//
// It also strips outbound_webhook credentials (stripWorkflowCredentials) rather than trusting the
// pack to arrive clean. Export removes them now, but a pack authored before that change — or one
// handed over as a JSON file — still carries the source tenant's live HMAC secret and Authorization
// header, and CreateWorkflow stores them in the TARGET tenant's sys_workflows.config in plaintext,
// where any admin of that (different) company can read them back out of GET /admin/workflows.
// Install is the boundary where a foreign pack becomes this tenant's data, so it validates the
// credential contract here instead of assuming it.
func toWorkflowInput(pw PackWorkflow, userID string, teamNameToID, processIDByKey, credIDByName map[string]string) (services.CreateWorkflowInput, error) {
	pw, _ = stripWorkflowCredentials(pw)
	var match, saveOn string
	var rules []services.WorkflowRuleInput
	var timer *services.WorkflowTimerInput
	if pw.Condition != nil {
		match, saveOn = pw.Condition.Match, pw.Condition.SaveOn
		rules = make([]services.WorkflowRuleInput, len(pw.Condition.Rules))
		for i, r := range pw.Condition.Rules {
			rules[i] = services.WorkflowRuleInput{Field: r.Field, Op: r.Op, Value: r.Value}
		}
		if pw.Condition.Timer != nil {
			timer = &services.WorkflowTimerInput{
				DateField:  pw.Condition.Timer.DateField,
				Offset:     services.WorkflowTimerOffsetInput{Amount: pw.Condition.Timer.Offset.Amount, Unit: pw.Condition.Timer.Offset.Unit},
				Direction:  pw.Condition.Timer.Direction,
				CalendarID: pw.Condition.Timer.CalendarID,
			}
		}
	}
	actions := make([]services.WorkflowActionInput, len(pw.Actions))
	for i, a := range pw.Actions {
		team := a.Team
		// "assign" is the workflow action type string (workflow.ActionAssign). Its Team field is a
		// team NAME in a pack — resolve it to the seeded team's id before handing it to the service.
		if strings.TrimSpace(a.Type) == "assign" {
			name := strings.TrimSpace(a.Team)
			id, ok := teamNameToID[name]
			if !ok {
				return services.CreateWorkflowInput{}, invalid("ASSIGN_TEAM_UNRESOLVED",
					"workflow %q: action assign tham chiếu team %q mà pack chưa seed (khai báo nó trong pack.Teams)", pw.Name, name)
			}
			team = id
		}
		actions[i] = services.WorkflowActionInput{
			Type:     a.Type,
			Field:    a.Field,
			Value:    a.Value,
			UserID:   a.UserID,
			Title:    a.Title,
			Message:  a.Message,
			To:       a.To,
			Subject:  a.Subject,
			Body:     a.Body,
			URL:      a.URL,
			Secret:   a.Secret,
			Headers:  a.Headers,
			Strategy: a.Strategy,
			Team:     team,
			// create_record: without these the CreateWorkflow validator sees an empty target and
			// fails the whole install at Pass 8 (WORKFLOW_ACTION_INVALID) with no rollback — the
			// export side emits them (PackWorkflowAction.EntityName/Values), so the consumer must map.
			CreateEntity: a.EntityName,
			Values:       a.Values,
			Mode:         a.Mode,
			StampField:   a.StampField,
			Anchor:       a.Anchor,
			CalendarID:   a.CalendarID, // queue #79 — the target tenant's save checks it exists
			Via:          a.Via,
			ChildEntity:  a.ChildEntity,
			ChildVia:     a.ChildVia,
			OnlyIfEmpty:  a.OnlyIfEmpty,
			ValueSource:  a.ValueSource,
			Ops:          a.Ops,
		}
		// outbound_webhook through the vault: the pack names the key; the target tenant's id was
		// made/found by ensureCredentialSlots. An unresolved name stays "" and the save refuses it.
		if n := strings.TrimSpace(a.CredentialName); n != "" {
			actions[i].CredentialID = credIDByName[n]
			actions[i].Path = a.Path
		}
		// start_approval: the pack names the process; the target tenant's id is looked up here. An
		// unresolved name stays "" and the save refuses it (WF_START_APPROVAL_INVALID).
		if strings.TrimSpace(a.Type) == "start_approval" {
			actions[i].ProcessID = processIDByKey[approvalDupKey(strings.TrimSpace(pw.EntityName), strings.TrimSpace(a.ProcessName))]
		}
		// set_deadline: same contract as the create_record pair above — the offset IS the action,
		// and an unmapped one fails the whole install at Pass 8 with no rollback.
		if a.Offset != nil {
			actions[i].Offset = &services.WorkflowTimerOffsetInput{Amount: a.Offset.Amount, Unit: a.Offset.Unit}
		}
	}
	return services.CreateWorkflowInput{
		Name:       pw.Name,
		EntityName: pw.EntityName,
		Trigger:    pw.Trigger,
		Match:      match,
		Rules:      rules,
		Timer:      timer,
		SaveOn:     saveOn,
		Actions:    actions,
		IsActive:   pw.IsActive,
		ActorID:    userID,

		IntervalSeconds: pw.IntervalSeconds,
	}, nil
}

// toDashboardInput maps a pack-authored PackDashboard to services.CreateDashboardInput — the SAME
// shape Install's dashboard pass always built (extracted so the upgrade adapter shares it). The
// widget filters, time bucket and grid position are carried or the dashboard comes back unfiltered.
func toDashboardInput(pd PackDashboard, userID string) services.CreateDashboardInput {
	widgets := make([]services.WidgetConfig, 0, len(pd.Widgets))
	for _, pw := range pd.Widgets {
		widgets = append(widgets, services.WidgetConfig{
			ID:      pw.ID,
			Title:   pw.Title,
			Type:    pw.Type,
			Entity:  pw.Entity,
			Metric:  pw.Metric,
			Field:   pw.Field,
			GroupBy: pw.GroupBy,
			Bucket:  pw.Bucket,
			X:       pw.X,
			Y:       pw.Y,
			W:       pw.W,
			H:       pw.H,
			Filters: packFiltersToInput(pw.Filters),
		})
	}
	return services.CreateDashboardInput{
		Name:        strings.TrimSpace(pd.Name),
		Description: strings.TrimSpace(pd.Description),
		Widgets:     widgets,
		ActorID:     userID,
	}
}

// toReportInput maps a pack-authored PackReport to services.CreateReportInput — the SAME shape the
// admin handler (admin_report_handler.go) produces from a POST /admin/reports body — so a pack
// report validates and persists through the exact admin path (ValidateReportConfig). ActorID is the
// install user (recorded as created_by on the report). Sort/Joins are carried (queue #720).
func toReportInput(pr PackReport, userID string) services.CreateReportInput {
	cfg := services.ReportConfig{
		Metric:  pr.Config.Metric,
		Field:   pr.Config.Field,
		GroupBy: pr.Config.GroupBy,
		Columns: pr.Config.Columns,
		Bucket:  pr.Config.Bucket,
		Filters: packFiltersToInput(pr.Config.Filters),
	}
	if g := pr.Config.ColumnGroup; g != nil {
		cfg.ColumnGroup = &services.ReportGroupInput{Field: g.Field, Bucket: g.Bucket}
	}
	for _, g := range pr.Config.SubGroups {
		cfg.SubGroups = append(cfg.SubGroups, services.ReportGroupInput{Field: g.Field, Bucket: g.Bucket})
	}
	if h := pr.Config.Having; h != nil {
		cfg.Having = &services.ReportHavingInput{On: h.On, Op: h.Op, Values: h.Values}
	}
	if so := pr.Config.Sort; so != nil {
		cfg.Sort = &services.ReportSortInput{Field: so.Field, Dir: so.Dir}
	}
	for _, j := range pr.Config.Joins {
		cfg.Joins = append(cfg.Joins, services.ReportJoinInput{Relationship: j.Relationship, Optional: j.Optional})
	}
	return services.CreateReportInput{
		Name:       pr.Name,
		EntityName: pr.EntityName,
		Type:       pr.Type,
		Config:     cfg,
		IsActive:   pr.IsActive,
		ActorID:    userID,
	}
}

// toWorkQueueInput maps a pack-authored PackWorkQueue to services.WorkQueueInput — the SAME shape
// the admin handler produces from a POST /admin/work-queues body — so a pack queue validates and
// persists through the exact admin path (PACK-QUEUES). roleID is the pre-resolved role id ("" =
// visible to everyone); the pass resolves RoleName→id before calling. Defaults (sortOrder "asc",
// actionVerb "open", isActive true) are the service's own — WorkQueueService.validate fills them.
func toWorkQueueInput(pq PackWorkQueue, roleID string) services.WorkQueueInput {
	filters := make([]workqueue.QueueFilter, 0, len(pq.Filters))
	for _, f := range pq.Filters {
		filters = append(filters, workqueue.QueueFilter{Field: f.Field, Operator: f.Operator, Value: f.Value})
	}
	return services.WorkQueueInput{
		Name:       pq.Name,
		Label:      pq.Label,
		EntityName: pq.EntityName,
		RoleID:     roleID,
		Filters:    filters,
		SortField:  pq.SortField,
		SortOrder:  pq.SortOrder,
		ActionVerb: pq.ActionVerb,
		Position:   pq.Position,
	}
}

// packFiltersToInput converts pack-authored filters (PackReportFilter) to the service
// ReportFilterInput shape shared by saved reports AND dashboard widgets — both validate the same
// filter dialect, so a single converter keeps the two install passes from drifting. Returns nil
// for an empty input (no filters => unfiltered, same as before this slice).
func packFiltersToInput(fs []PackReportFilter) []services.ReportFilterInput {
	if len(fs) == 0 {
		return nil
	}
	out := make([]services.ReportFilterInput, 0, len(fs))
	for _, f := range fs {
		out = append(out, services.ReportFilterInput{Field: f.Field, Op: f.Op, Values: f.Values})
	}
	return out
}

// isAlreadyExists reports whether err is a ValidationError whose code marks an idempotent
// "already there" outcome (a duplicate entity/field/role). Install treats these as skip, not
// failure, so re-installing a pack is a clean no-op.
func isAlreadyExists(err error) bool {
	var ve *services.ValidationError
	if !errors.As(err, &ve) {
		return false
	}
	switch ve.Code {
	case "ENTITY_ALREADY_EXISTS", "FIELD_ALREADY_EXISTS", "ROLE_ALREADY_EXISTS",
		// AI-APPLY-IDEMPOTENT (3.1): Create now 409s on a same-name workflow/report. The
		// pack passes already list-then-skip via the plan, so this only fires on a race —
		// treat as the idempotent skip it is, never a failed install.
		"WORKFLOW_ALREADY_EXISTS", "REPORT_ALREADY_EXISTS":
		return true
	default:
		return false
	}
}

// isRecordDuplicate reports whether err is the record service's RECORD_DUPLICATE (a unique
// sample value already present — typically from a previous install of the same pack). Install
// treats it as a per-row skip so a re-install into a populated tenant completes; every other
// record error keeps its abort-and-rollback behavior.
func isRecordDuplicate(err error) bool {
	var ve *services.ValidationError
	return errors.As(err, &ve) && ve.Code == "RECORD_DUPLICATE"
}

// isHookVeto reports whether err is a before_save/before_delete refusal (RECORD_HOOK_VETO) of the
// record service. "Rules unreadable" (ErrRecordHooksUnavailable) is NOT one: nobody knows whether the
// row is allowed, so the install keeps failing closed on it.
func isHookVeto(err error) bool {
	var ve *services.ValidationError
	return errors.As(err, &ve) && ve.Code == "RECORD_HOOK_VETO"
}

// refersToSkipped reports whether any "@ref" value in data points at a sample row that was
// skipped as a duplicate (its real id is unknown, so this row cannot be inserted correctly).
// "@@"-escaped literals are not refs (same contract as resolveRefs).
func refersToSkipped(data map[string]any, skipped map[string]struct{}) bool {
	for _, v := range data {
		s, ok := v.(string)
		if !ok || !strings.HasPrefix(s, "@") || strings.HasPrefix(s, "@@") {
			continue
		}
		if _, hit := skipped[s]; hit {
			return true
		}
	}
	return false
}

// isRelAlreadyExists reports whether err signals that an N-N relationship was already
// registered (MySQL 1062 unique-key violation on sys_relationships, surfaced by
// RelationService.RegisterRelationship as RELATIONSHIP_ALREADY_EXISTS). Install skips these
// instead of failing so that re-running the same pack is idempotent for N-N definitions.
func isRelAlreadyExists(err error) bool {
	var ve *services.ValidationError
	if !errors.As(err, &ve) {
		return false
	}
	return ve.Code == "RELATIONSHIP_ALREADY_EXISTS"
}

// resolveRefs returns a shallow copy of data with every string value that starts with "@"
// replaced by the real record ID from refMap. The refMap key includes the "@" prefix (e.g.
// "@acme"). Returns an error if any "@"-prefixed value is not present in refMap (an
// unresolved reference would silently write a literal "@key" string into the record, which
// would fail the relation FK check — better to surface it early with a clear message).
func resolveRefs(data map[string]any, refMap map[string]refEntry) (map[string]any, error) {
	out := make(map[string]any, len(data))
	for k, v := range data {
		s, ok := v.(string)
		switch {
		case !ok:
			out[k] = v
		case strings.HasPrefix(s, "@@"):
			// Escaped literal (export doubled a leading "@" on a non-relation value): drop one.
			out[k] = s[1:]
		case strings.HasPrefix(s, "@"):
			entry, found := refMap[s]
			if !found {
				return nil, fmt.Errorf("unresolved ref %q (no record with that ref was inserted before this one)", s)
			}
			out[k] = entry.id
		default:
			out[k] = s
		}
	}
	return out, nil
}

// refEntry is the install-local map value that links a symbolic "@key" to the entity name
// and real UUID minted when the record was inserted.
type refEntry struct{ entity, id string }

// isMissingReference reports whether a hook-install error is a PORTABILITY problem (a name that
// does not resolve in the target tenant) rather than a malformed pack.
//
// The split matters because the two need opposite handling and by Pass 8e nothing rolls back: a
// dangling reference must warn+skip so the rest of the install survives, while a bad regex or an
// empty mapping must fail loudly — that is a pack we shipped broken, and a silent skip would hand
// the customer an endpoint they believe is receiving.
//
// Matched on the service's own error CODES, not on message text: HOOK_INVALID carries a
// details.field naming which part failed, and only the entity/field-reference ones are portability
// cases. Anything unrecognised is treated as a hard failure — the safe default for a credentialed
// endpoint.
func isMissingReference(err error) bool {
	var ve *services.ValidationError
	if !errors.As(err, &ve) {
		return false
	}
	switch ve.Code {
	case "ENTITY_NOT_FOUND", "HOOK_ENTITY_NOT_FOUND":
		return true
	case "HOOK_INVALID":
		// Reference-shaped fields only. A regex/mapping/shape complaint is OUR bug.
		switch ve.Field {
		case "entityName", "matchField", "paymentMatch.targetEntity", "paymentMatch.refField",
			"paymentMatch.statusField", "paymentMatch.amountField":
			return true
		}
	}
	return false
}

// wildcardEntityName is the grant EntityName that ACLService.Authorize resolves as "every entity"
// (acl_service.go). Named here because install must recognise it to refuse it.
const wildcardEntityName = "*"

// reconcileRoleGrants adds the pack's grants that an EXISTING role is missing. It returns how many
// it added and how many wildcard grants it REFUSED. Additive by construction: existing grants are
// carried over untouched, and a (entity, action) the role already has is left exactly as the tenant
// has it — including a level the customer widened themselves. Returns 0, 0 when the role already
// covers the pack.
func (s *Service) reconcileRoleGrants(ctx context.Context, tenantID, name string, pr PackRole) (int, int, error) {
	roles, err := s.roleWr.ListRoles(ctx, tenantID)
	if err != nil {
		return 0, 0, fmt.Errorf("list roles: %w", err)
	}
	var roleID string
	for _, r := range roles {
		if r.Name == name {
			roleID = r.ID
			break
		}
	}
	if roleID == "" {
		// The name was in plan.roleSkip but is gone now (deleted between plan and install). Nothing
		// to reconcile; the create path already treats this family of races as idempotent.
		return 0, 0, nil
	}
	cur, err := s.roleWr.GetRole(ctx, tenantID, roleID)
	if err != nil || cur == nil {
		return 0, 0, fmt.Errorf("get role: %w", err)
	}

	have := make(map[string]struct{}, len(cur.Actions))
	merged := make([]services.RoleActionInput, 0, len(cur.Actions)+len(pr.Actions))
	for _, a := range cur.Actions {
		have[a.EntityName+"|"+a.Action] = struct{}{}
		merged = append(merged, services.RoleActionInput{
			EntityName: a.EntityName, Action: a.Action, AccessLevel: a.AccessLevel,
		})
	}
	added, refusedWildcard := 0, 0
	for _, a := range pr.Actions {
		// NEVER widen a pre-existing role to every entity. ACLService.Authorize resolves '*' as
		// "any entity" (acl_service.go: wildcardMatched → return true), so appending ("*","view")
		// to a role the customer already had grants every holder read access to EVERY entity in
		// their workspace — including entities from unrelated later deliveries (HR, payroll,
		// finance) this pack never shipped and its author never saw. The merge key is
		// entity|action, so a wildcard looks "missing" from any role with only per-entity grants
		// and would always be appended. A pack may extend a live tenant only within its own
		// footprint; a wildcard has no footprint.
		if strings.TrimSpace(a.EntityName) == wildcardEntityName {
			refusedWildcard++
			continue
		}
		if _, ok := have[a.EntityName+"|"+a.Action]; ok {
			continue // the tenant's own level wins — never narrow what is already granted
		}
		merged = append(merged, services.RoleActionInput{
			EntityName: a.EntityName, Action: a.Action, AccessLevel: a.AccessLevel,
		})
		added++
	}
	if added == 0 {
		return 0, refusedWildcard, nil
	}
	if _, err := s.roleWr.UpdateRole(ctx, tenantID, roleID, services.UpdateRoleInput{Actions: merged}); err != nil {
		return 0, refusedWildcard, err
	}
	return added, refusedWildcard, nil
}

// packWorkflowStartsApproval reports whether a rule carries a start_approval action — those wait for
// the approvals pass.
func packWorkflowStartsApproval(pw PackWorkflow) bool {
	for _, a := range pw.Actions {
		if strings.TrimSpace(a.Type) == "start_approval" {
			return true
		}
	}
	return false
}

// applyRelationViewConfigs is Pass 2c: set each pack relationship's viewConfig on its registry row
// (1-N rows are named <child>_<field>, N-N rows by name). Never overwrites an existing config.
func (s *Service) applyRelationViewConfigs(ctx context.Context, tenantID string, pack *Pack, res *InstallResult) error {
	type item struct {
		name string
		raw  json.RawMessage
	}
	var items []item
	for _, r := range pack.Relationships {
		if len(r.ViewConfig) > 0 {
			items = append(items, item{r.ChildEntity + "_" + r.Name, r.ViewConfig})
		}
	}
	for _, r := range pack.NNRelationships {
		if len(r.ViewConfig) > 0 {
			items = append(items, item{r.Name, r.ViewConfig})
		}
	}
	if len(items) == 0 {
		return nil
	}
	if s.relViewWr == nil || s.relationRd == nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d cấu hình danh sách liên quan không áp dụng được — thiếu dịch vụ quan hệ", len(items)))
		return nil
	}
	rels, err := s.relationRd.ListAllRelationships(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("vertical_pack.Install: list relationships for viewConfig: %w", err)
	}
	byName := make(map[string]metadata.RelationshipDefinition, len(rels))
	for _, r := range rels {
		byName[r.Name] = r
	}
	for _, it := range items {
		rel, ok := byName[it.name]
		if !ok || (rel.ViewConfig != nil && strings.TrimSpace(*rel.ViewConfig) != "") {
			continue
		}
		if _, err := s.relViewWr.SetViewConfig(ctx, tenantID, rel.ID, it.raw); err != nil {
			var ve *services.ValidationError
			if errors.As(err, &ve) {
				res.Warnings = append(res.Warnings, fmt.Sprintf("quan hệ %q: bỏ qua cấu hình danh sách liên quan (%s)", it.name, ve.Error()))
				continue
			}
			return fmt.Errorf("vertical_pack.Install: set viewConfig of %q: %w", it.name, err)
		}
	}
	return nil
}

// installCalendars is Pass 6c: create the pack's calendars (skip by name), return name → id. The
// pack's default becomes the tenant's default ONLY when the tenant has none — a customer's own
// default is never taken away by a delivery; the mismatch is a warning.
func (s *Service) installCalendars(ctx context.Context, tenantID string, pack *Pack, res *InstallResult) (map[string]string, error) {
	out := map[string]string{}
	if len(pack.Calendars) == 0 {
		return out, nil
	}
	if s.calendarWr == nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d lịch làm việc của gói không cài được — thiếu dịch vụ lịch", len(pack.Calendars)))
		return out, nil
	}
	existing, err := s.calendarWr.ListCalendars(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.Install: list calendars: %w", err)
	}
	hasDefault := false
	for _, c := range existing {
		out[c.Name] = c.ID
		hasDefault = hasDefault || c.IsDefault
	}
	for _, pc := range pack.Calendars {
		name := strings.TrimSpace(pc.Name)
		if _, ok := out[name]; ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf("lịch làm việc %q đã có sẵn — giữ nguyên, không ghi đè giờ làm", name))
			continue
		}
		rec, err := s.calendarWr.CreateCalendar(ctx, tenantID, services.CalendarInput{
			Name: name, Timezone: pc.Timezone, Hours: pc.Hours, Holidays: pc.Holidays,
			IsDefault: pc.IsDefault && !hasDefault,
		})
		if err != nil {
			var ve *services.ValidationError
			if errors.As(err, &ve) {
				return nil, err
			}
			return nil, fmt.Errorf("vertical_pack.Install: create calendar %q: %w", name, err)
		}
		out[name] = rec.ID
		if pc.IsDefault && hasDefault {
			res.Warnings = append(res.Warnings, fmt.Sprintf("lịch %q là lịch mặc định ở nơi xuất nhưng tổ chức này đã có lịch mặc định — giữ lịch mặc định cũ", name))
		}
		res.CalendarsCreated = append(res.CalendarsCreated, name)
	}
	return out, nil
}

// resolveCalendarNames returns pw with every calendarName replaced by the target tenant's calendar
// id (calendarId). A name nothing resolves is a hard error — never persist a rule whose pin points
// nowhere (it would silently degrade to wall-clock). pw is not mutated (the slices are copied).
func resolveCalendarNames(pw PackWorkflow, calNameToID map[string]string) (PackWorkflow, error) {
	if pw.Condition != nil && pw.Condition.Timer != nil && strings.TrimSpace(pw.Condition.Timer.CalendarName) != "" {
		id, ok := calNameToID[strings.TrimSpace(pw.Condition.Timer.CalendarName)]
		if !ok {
			return pw, invalid("CALENDAR_UNRESOLVED", "workflow %q: timer ghim lịch %q mà gói chưa khai báo (pack.Calendars)", pw.Name, pw.Condition.Timer.CalendarName)
		}
		c, t := *pw.Condition, *pw.Condition.Timer
		t.CalendarID = id
		c.Timer = &t
		pw.Condition = &c
	}
	acts := make([]PackWorkflowAction, len(pw.Actions))
	copy(acts, pw.Actions)
	for i := range acts {
		if n := strings.TrimSpace(acts[i].CalendarName); n != "" {
			id, ok := calNameToID[n]
			if !ok {
				return pw, invalid("CALENDAR_UNRESOLVED", "workflow %q: action ghim lịch %q mà gói chưa khai báo (pack.Calendars)", pw.Name, n)
			}
			acts[i].CalendarID = id
		}
	}
	pw.Actions = acts
	return pw, nil
}

// installViews is Pass 5b: create the pack's shared views, skipping an (entity, name) the tenant
// already has. Goes through ViewService as an admin, so name/config/entity validation is the one
// POST /views runs.
func (s *Service) installViews(ctx context.Context, tenantID, userID string, pack *Pack, res *InstallResult) error {
	if len(pack.Views) == 0 {
		return nil
	}
	if s.viewWr == nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d view dùng chung của gói không cài được — thiếu dịch vụ view", len(pack.Views)))
		return nil
	}
	have := map[string]map[string]bool{} // entity → shared view names already present
	for _, v := range pack.Views {
		if _, ok := have[v.EntityName]; ok {
			continue
		}
		recs, err := s.viewWr.List(ctx, tenantID, "", v.EntityName)
		if err != nil {
			return fmt.Errorf("vertical_pack.Install: list views of %q: %w", v.EntityName, err)
		}
		have[v.EntityName] = map[string]bool{}
		for _, r := range recs {
			if r.Shared {
				have[v.EntityName][r.Name] = true
			}
		}
	}
	for _, v := range pack.Views {
		name := strings.TrimSpace(v.Name)
		if have[v.EntityName][name] {
			continue
		}
		if _, err := s.viewWr.Create(ctx, tenantID, userID, true, services.CreateViewInput{
			EntityName: v.EntityName, Name: name, Config: v.Config, Shared: true,
		}); err != nil {
			var ve *services.ValidationError
			if errors.As(err, &ve) {
				res.Warnings = append(res.Warnings, fmt.Sprintf("view %q của %q: bỏ qua (%s)", name, v.EntityName, ve.Error()))
				continue
			}
			return fmt.Errorf("vertical_pack.Install: create view %q: %w", name, err)
		}
		have[v.EntityName][name] = true
		res.ViewsCreated = append(res.ViewsCreated, v.EntityName+":"+name)
	}
	return nil
}

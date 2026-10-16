package vertical_pack

import (
	"context"
	"encoding/json"
	"fmt"
	"nexus/platform/internal/metaversion"
	"sort"
	"strings"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// exportRecordPageLimit is the page size used when reading a tenant's records for sample export.
// dynrepo clamps list limit at 100, so this is the max efficient page.
const exportRecordPageLimit = 100

// exportRecordCap bounds how many records per entity are dumped into a pack. Sample export is for
// seed/demo-sized data (a pack is an opinionated bootstrap, not a bulk backup); a very large
// obj_* table is truncated at this cap rather than inlining megabytes of rows into a JSON pack.
const exportRecordCap = 5000

// ExportManifest carries the source app's identity into a pack's Manifest (PACK-EXPORT-APPSLUG) so
// re-install mints a correctly-named sidebar app. All fields optional; a blank Slug leaves the pack
// app-less (install mints nothing, as before). Built by the handler from ?appSlug/appName/appIcon.
type ExportManifest struct {
	Slug        string
	Name        string
	Icon        string
	Description string
}

// computedFieldType reports whether a field's stored/returned value is server-assigned or
// read-time computed — auto_number (assigned inside RecordService.Create), lookup/rollup (resolved
// at read time), formula (computed at read time). Such keys must NEVER appear in a pack's Samples:
// RecordService.Create rejects them as readonly (422) and aborts+rolls back the WHOLE install
// (trap B9 / G1). Sample export strips them from every record's Data.
func computedFieldType(t string) bool {
	switch t {
	case "auto_number", "lookup", "rollup", "formula":
		return true
	}
	return false
}

// Export reads the requested entities of the current tenant and packages their full metadata
// — entities + fields, relationships (relation fields AND the N-N registry), layouts, EVERY
// role, and (PACK-EXPORT-FULL) the workflows / reports / dashboards / teams that hang off the
// selected entities — into a Pack that RE-INSTALLS cleanly into another tenant.
//
// entityNames selects which entities to include; an empty/nil slice exports ALL of the tenant's
// entities. A requested name that does not exist for the tenant is reported as a 404-style
// ENTITY_NOT_FOUND so the caller knows the pack would be incomplete rather than silently dropping
// it. Roles are exported in full (they reference entities by name, including '*').
//
// Re-installability (the EXPORT-DEFERRED fix):
//   - rollup fields are pushed into DeferredFields (install Pass 2b) instead of inline on the
//     entity — an inline rollup would fail config-time validation because its child entity /
//     N-N relationship does not exist yet when the parent entity is created;
//   - N-N relationships are emitted from the tenant's registry (they used to be dropped, so the
//     matching N-N rollups had nothing to bind to on re-install);
//   - the denormalized Relationships projection reads options.cardinality (a 1-1 field now
//     projects as "1-1", not a hardcoded "1-N").
//
// PACK-EXPORT-FULL (the "half-the-app" fix — these sections used to be pack-authored only and
// were silently DROPPED by export):
//   - Workflows: every rule on a selected entity, serialized to the same PackWorkflow shape the
//     built-in packs author. An assign action's team ID is resolved BACK to its NAME (the
//     installer's pass 6b resolves name→id on the other side) and that team joins Pack.Teams.
//   - Reports / Dashboards: saved reports on selected entities; dashboards filtered per-widget
//     to the selection (a dashboard whose every widget referenced an excluded entity is dropped
//     whole). Widget grid coordinates / bucket / filters are carried.
//   - Links: with ?includeData=true the N-N join rows between exported samples are emitted as
//     PackLinks via the same "@ref" handles the samples use.
//
// FIDELITY HONESTY: anything export CANNOT carry is named in the returned warnings slice —
// a workflow on an excluded entity, an assign team that no longer exists, a business-hours
// calendar pin that no longer resolves (calendars travel by name since #720), a webhook
// secret traveling in pack JSON, a truncated sample dump. NEVER silently dropped.
//
// includeData controls the OPTIONAL Samples (+ Links) section. false (default) → a metadata-only
// pack. true → every record is read and inlined with a synthetic "@ref" so relation FKs AND N-N
// links resolve on re-install into a DIFFERENT tenant; computed/server-assigned keys are stripped
// so the target tenant re-derives them fresh.
//
// The pack is pure data: no ids, no tenant_id, no timestamps — only the reusable shape.
//
// manifest (PACK-EXPORT-APPSLUG) optionally identifies the source app so the pack's Manifest
// carries a real slug/name/icon and re-install mints a correctly-named sidebar app; nil keeps the
// generic self-identifying manifest (back-compat, no app minted on install).
func (s *Service) Export(ctx context.Context, tenantID string, entityNames []string, includeData bool, manifest *ExportManifest) (*Pack, []string, error) {
	// A pack is the LIVE configuration, never the builder's drafts (specs/063 FR-044): the services
	// read below overlay drafts for a builder on /admin/*, so force the live read once, here.
	ctx = metaversion.WithLiveRead(ctx)
	warnings := []string{}
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	// #303: items hanging off a table outside the selection belong to OTHER apps of the same
	// workspace — counted into ONE line (flushed after the sections) instead of 40+ lines that
	// bury the warnings about the app being handed over.
	outCounts := map[string]int{}
	outOfScope := func(kind string) { outCounts[kind]++ }

	all, err := s.entityRd.ListEntities(tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("vertical_pack.Export: list entities: %w", err)
	}

	// Index live entities by name for selection + existence checks.
	byName := make(map[string]int, len(all))
	for i := range all {
		byName[all[i].Name] = i
	}

	// Resolve the selection. Empty selection = all entities (stable name order).
	var selected []string
	if len(entityNames) == 0 {
		for _, e := range all {
			selected = append(selected, e.Name)
		}
		sort.Strings(selected)
	} else {
		seen := make(map[string]struct{}, len(entityNames))
		for _, raw := range entityNames {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			if _, ok := byName[name]; !ok {
				return nil, nil, invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại trong tenant hiện tại", name)
			}
			selected = append(selected, name)
		}
		sort.Strings(selected)
	}

	// selectedSet backs every "does X hang off a selected entity?" filter below (N-N endpoints,
	// workflows, reports, dashboard widgets).
	selectedSet := make(map[string]bool, len(selected))
	for _, n := range selected {
		selectedSet[n] = true
	}

	pack := &Pack{
		Entities:        make([]PackEntity, 0, len(selected)),
		Relationships:   []PackRelationship{},
		Layouts:         []PackLayout{},
		Roles:           []PackRole{},
		NNRelationships: []PackNNRelationship{},
		DeferredFields:  []PackDeferredField{},
	}

	// Per-entity field classification captured during the field walk, so sample export can strip
	// computed keys and rewrite relation FKs without re-reading sys_fields.
	computedByEntity := make(map[string]map[string]bool, len(selected))
	relationByEntity := make(map[string]map[string]bool, len(selected))
	// polymorphic_relation fields are deferred (below) so they land in NEITHER computedSet nor
	// relationSet — track them separately so sample export can STRIP their {type,id} value (a raw
	// source-tenant record UUID that would both leak cross-tenant AND 422 on re-install).
	polymorphicByEntity := make(map[string]map[string]bool, len(selected))

	for _, name := range selected {
		ent := all[byName[name]]
		fields, err := s.entityRd.ListFields(ctx, tenantID, ent.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("vertical_pack.Export: list fields of %q: %w", name, err)
		}

		computedSet := map[string]bool{}
		relationSet := map[string]bool{}
		polymorphicSet := map[string]bool{}

		pe := PackEntity{
			Name:        ent.Name,
			Label:       ent.Label,
			Description: ent.Description,
			Fields:      make([]PackField, 0, len(fields)),
		}
		for _, f := range fields {
			opts, err := decodeOptions(f.Options)
			if err != nil {
				return nil, nil, fmt.Errorf("vertical_pack.Export: decode options of %q.%q: %w", name, f.Name, err)
			}
			pf := PackField{
				Name:       f.Name,
				Label:      f.Label,
				FieldType:  f.FieldType,
				IsRequired: f.IsRequired,
				IsUnique:   f.IsUnique,
				Options:    opts,
			}
			if computedFieldType(pf.FieldType) {
				computedSet[pf.Name] = true
			}
			// #296: a field whose target table is not in the pack would break on install.
			for _, ref := range fieldRefs(pf.FieldType, opts) {
				if !selectedSet[ref] {
					warn("trường %q của bảng %q cần bảng %q nhưng bảng đó không nằm trong gói — trường này sẽ gãy khi cài. Chọn thêm bảng %q ở bước chọn bảng.", f.Label, ent.Label, entityLabel(all, byName, ref), entityLabel(all, byName, ref))
				}
			}

			// Rollup fields CANNOT be inline on their entity (the child entity / N-N relationship
			// they aggregate over is created later in the install order) — defer them to Pass 2b.
			if pf.FieldType == "rollup" {
				pack.DeferredFields = append(pack.DeferredFields, PackDeferredField{EntityName: ent.Name, Field: pf})
				continue
			}
			// POLYMORPHIC-1: a polymorphic_relation field references SEVERAL entities
			// (options.targetEntities) that may sort AFTER this one in the alphabetical install
			// order — its config-time existence check would then fail. Defer to Pass 2b (after all
			// entities exist), exactly like rollup. AddField reconciles a shared obj_* table on
			// re-install (both sub-columns already present → register metadata, skip the DDL).
			if pf.FieldType == "polymorphic_relation" {
				polymorphicSet[pf.Name] = true // sample export strips its {type,id} value (leak + re-install 422)
				pack.DeferredFields = append(pack.DeferredFields, PackDeferredField{EntityName: ent.Name, Field: pf})
				continue
			}

			pe.Fields = append(pe.Fields, pf)

			// Surface relation fields in the denormalized Relationships projection, reading the
			// real cardinality (1-1 for an exclusive pointer, else 1-N) instead of hardcoding.
			if isRelationField(pf) {
				relationSet[pf.Name] = true
				parent, _ := opts["relatedEntity"].(string)
				card := "1-N"
				if c, ok := opts["cardinality"].(string); ok && strings.TrimSpace(c) != "" {
					card = strings.TrimSpace(c)
				}
				pack.Relationships = append(pack.Relationships, PackRelationship{
					Name:         f.Name,
					ChildEntity:  ent.Name,
					ParentEntity: strings.TrimSpace(parent),
					Type:         card,
				})
			}
		}
		pack.Entities = append(pack.Entities, pe)
		computedByEntity[ent.Name] = computedSet
		relationByEntity[ent.Name] = relationSet
		polymorphicByEntity[ent.Name] = polymorphicSet

		// Layouts for this entity (all stored views).
		layouts, err := s.layoutRd.GetByEntity(ctx, tenantID, ent.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("vertical_pack.Export: layouts of %q: %w", name, err)
		}
		// Stable order by view type so two exports of the same tenant are byte-identical.
		sort.Slice(layouts, func(i, j int) bool { return layouts[i].ViewType < layouts[j].ViewType })
		for _, l := range layouts {
			var cfg map[string]any
			if strings.TrimSpace(l.LayoutJSON) != "" {
				if err := json.Unmarshal([]byte(l.LayoutJSON), &cfg); err != nil {
					// A malformed stored layout is skipped rather than failing the whole export.
					continue
				}
			}
			pack.Layouts = append(pack.Layouts, PackLayout{
				EntityName: l.EntityName,
				ViewType:   l.ViewType,
				LayoutJSON: cfg,
			})
		}
	}

	// N-N relationships: emit the tenant's registered N-N rows whose BOTH endpoints are in the
	// selection (a dangling endpoint would fail to install). Without this, an N-N rollup exported
	// as a deferred field would have no relationship to bind to on re-install.
	if s.relationRd == nil {
		warn("quan hệ N-N: không xuất được — RelationshipReader chưa được wire (section bị bỏ qua)")
	}
	if s.relationRd != nil {
		rels, err := s.relationRd.ListAllRelationships(ctx, tenantID)
		if err != nil {
			return nil, nil, fmt.Errorf("vertical_pack.Export: list relationships: %w", err)
		}
		// Queue #720: related-list view config rides on the registry row — 1-N rows are named
		// <child>_<field> (newInverseRelationship), N-N rows by their own name.
		viewCfg := map[string]json.RawMessage{}
		for _, rel := range rels {
			if rel.ViewConfig != nil && strings.TrimSpace(*rel.ViewConfig) != "" {
				viewCfg[rel.Name] = json.RawMessage(*rel.ViewConfig)
			}
		}
		for i := range pack.Relationships {
			pack.Relationships[i].ViewConfig = viewCfg[pack.Relationships[i].ChildEntity+"_"+pack.Relationships[i].Name]
		}
		nnSeen := map[string]struct{}{}
		for _, rel := range rels {
			if rel.RelationshipType != "N-N" {
				continue
			}
			if !selectedSet[rel.ParentEntityName] || !selectedSet[rel.ChildEntityName] {
				continue
			}
			if _, dup := nnSeen[rel.Name]; dup {
				continue
			}
			nnSeen[rel.Name] = struct{}{}
			pack.NNRelationships = append(pack.NNRelationships, PackNNRelationship{
				Name:       rel.Name,
				EntityA:    rel.ParentEntityName,
				EntityB:    rel.ChildEntityName,
				ViewConfig: viewCfg[rel.Name],
			})
		}
		sort.Slice(pack.NNRelationships, func(i, j int) bool {
			return pack.NNRelationships[i].Name < pack.NNRelationships[j].Name
		})
	}

	// Roles: SCOPED to the selected entities (MỘT XƯỞNG RỔ-4 P0, 2026-07-29).
	//
	// This used to export EVERY role in the tenant with its FULL grant list, and the old comment
	// said so out loud. That is a cross-customer leak by construction: Nexus is a factory that
	// builds many customers' apps inside ONE tenant, so that tenant accumulates a role per
	// engagement — and every handover faithfully recreated all of them inside the new customer's
	// workspace (install.go recreates what the pack declares). The wizard scopes the export by
	// ENTITY, so the operator reasonably believes the whole bundle is scoped.
	//
	// Now a role is exported only if it grants something on a SELECTED entity, and only those
	// grants travel. A wildcard grant ("*") is kept — it is meaningful for the schema being
	// handed over — but a role whose grants are all for entities outside this app is dropped
	// entirely, with a warning so the drop is never silent.
	roles, err := s.roleRd.ListRoles(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("vertical_pack.Export: list roles: %w", err)
	}
	droppedRoles := 0
	for i := range roles {
		acts, err := s.roleRd.ListActionsForRole(ctx, tenantID, roles[i].ID)
		if err != nil {
			return nil, nil, fmt.Errorf("vertical_pack.Export: actions of role %q: %w", roles[i].Name, err)
		}
		if auth.IsOwnerRoleName(roles[i].Name) {
			// The customer-owner role (and so its field-ACL, keyed off the exported roles below)
			// belongs to the workspace, not to an app (specs/065 D-15).
			warn("Bỏ vai hệ thống %q (của riêng không gian này, không thuộc app).", roles[i].Label)
			continue
		}
		kept, skipped := scopeRoleActions(acts, selectedSet)
		pr := PackRole{Name: roles[i].Name, Label: roles[i].Label, Actions: kept}
		if len(pr.Actions) == 0 {
			droppedRoles++
			continue
		}
		outCounts["quyền của vai"] += skipped
		// A wildcard survives the entity filter by design, but it is the one grant that means
		// "everything in the DESTINATION workspace" — including entities from unrelated later
		// deliveries. It produced no warning at all, so an export carrying it looked clean.
		if wc := countWildcardActions(kept); wc > 0 {
			warn("⚠️ Vai %q mang %d quyền WILDCARD ('*' = MỌI đối tượng, kể cả những thứ giao ở đợt sau). Cân nhắc thay bằng quyền theo từng đối tượng trước khi bàn giao — khi cài, quyền wildcard sẽ KHÔNG được gắn vào vai trùng tên có sẵn của khách.", roles[i].Name, wc)
		}
		pack.Roles = append(pack.Roles, pr)
	}
	outCounts["vai"] += droppedRoles // no grant on a selected table: left out so no other customer's role leaks

	// PACK-FIELD-ACL: carry the field-level restrictions for the EXPORTED roles × SELECTED entities.
	// Field ACL was the strictest configuration in the app and the only one handover dropped: a
	// field hidden from a role here became fully visible to the same-named role at the customer,
	// silently. Scoped exactly like the roles above — a restriction naming an entity outside the
	// selection would leak the existence of that entity into the pack.
	if s.fieldACLRd != nil {
		exported := make(map[string]string, len(pack.Roles)) // role name → role id in THIS tenant
		for i := range roles {
			for _, pr := range pack.Roles {
				if pr.Name == roles[i].Name {
					exported[roles[i].Name] = roles[i].ID
				}
			}
		}
		for roleName, roleID := range exported {
			for entName := range selectedSet {
				grants, gerr := s.fieldACLRd.ListGrants(ctx, tenantID, roleID, entName)
				if gerr != nil {
					// Best-effort per (role, entity): a read failure must not fail the export, but
					// it MUST be said — a missing restriction is a security difference, not a
					// cosmetic one.
					warn("Không đọc được phân quyền field của vai %q trên %q — kiểm tra lại ở workspace đích.", roleName, entName)
					continue
				}
				for _, g := range grants {
					pack.FieldACLs = append(pack.FieldACLs, PackFieldACL{
						RoleName: roleName, EntityName: g.EntityName, FieldName: g.FieldName,
						CanView: g.CanView, CanEdit: g.CanEdit,
					})
				}
			}
		}
		sort.SliceStable(pack.FieldACLs, func(i, j int) bool {
			a, b := pack.FieldACLs[i], pack.FieldACLs[j]
			if a.RoleName != b.RoleName {
				return a.RoleName < b.RoleName
			}
			if a.EntityName != b.EntityName {
				return a.EntityName < b.EntityName
			}
			return a.FieldName < b.FieldName
		})
	}

	// PACK-EXPORT-FULL: workflows (+ the teams their assign actions reference), reports and
	// dashboards on the selected entities. Each helper appends its own fidelity warnings.
	if err := s.exportWorkflows(ctx, tenantID, selectedSet, relationByEntity, pack, warn, outOfScope); err != nil {
		return nil, nil, err
	}
	if err := s.exportReports(ctx, tenantID, selectedSet, pack, warn, outOfScope); err != nil {
		return nil, nil, err
	}
	if err := s.exportViews(ctx, tenantID, selectedSet, pack); err != nil {
		return nil, nil, err
	}
	if err := s.exportDashboards(ctx, tenantID, selectedSet, pack, warn, outOfScope); err != nil {
		return nil, nil, err
	}
	if err := s.exportWorkQueues(ctx, tenantID, selectedSet, pack, warn, outOfScope); err != nil {
		return nil, nil, err
	}
	if err := s.exportHooks(ctx, tenantID, selectedSet, pack, warn); err != nil {
		return nil, nil, err
	}
	if err := s.exportApprovals(ctx, tenantID, selectedSet, pack, warn, outOfScope); err != nil {
		return nil, nil, err
	}
	if line := outOfScopeSummary(outCounts); line != "" {
		warn("%s", line)
	}

	// Samples (opt-in). Read every record of the selected entities, strip computed keys, and
	// rewrite relation FKs to synthetic "@ref"s so the pack re-installs into a fresh tenant.
	// The id→ref map feeds the N-N link export right after (links reference sample refs).
	if includeData && s.recordRd == nil {
		warn("dữ liệu mẫu: không xuất được — RecordLister chưa được wire (samples + liên kết N-N bị bỏ qua dù đã bật includeData)")
	}
	if includeData && s.recordRd != nil {
		samples, idToRef, err := s.exportSamples(ctx, tenantID, selected, computedByEntity, relationByEntity, polymorphicByEntity, warn)
		if err != nil {
			return nil, nil, err
		}
		pack.Samples = samples
		if err := s.exportLinks(ctx, tenantID, pack, idToRef, warn); err != nil {
			return nil, nil, err
		}
	}

	// Self-identifying manifest. PACK-EXPORT-APPSLUG: when the caller names the source app, carry
	// its slug/name/icon/description so re-install mints a correctly-named sidebar app; otherwise a
	// generic self-ID (no slug → install mints no app, as before).
	pack.Manifest = &Manifest{Name: "Exported vertical", Version: "1.0.0"}
	if manifest != nil {
		if v := strings.TrimSpace(manifest.Slug); v != "" {
			// App slugs are snake ([a-z0-9_]); a pack Manifest.Slug is kebab ([a-z0-9-]) and install
			// validates it as such. Normalize _→- so the exported pack installs, then mintApp maps
			// -→_ back to the app's snake slug (exact round-trip; app slugs carry no dashes).
			pack.Manifest.Slug = strings.ReplaceAll(v, "_", "-")
		}
		if v := strings.TrimSpace(manifest.Name); v != "" {
			pack.Manifest.Name = v
		}
		pack.Manifest.Icon = strings.TrimSpace(manifest.Icon)
		if v := strings.TrimSpace(manifest.Description); v != "" {
			pack.Manifest.Description = v
		}
	}

	return pack, warnings, nil
}

// exportWorkflows serializes the tenant's workflow rules on the selected entities into
// pack.Workflows, resolving each assign action's team ID back to its NAME and collecting those
// teams into pack.Teams (the installer's pass 6b resolves name→id on the way back in). A rule
// that cannot round-trip is dropped WITH a warning naming why (fidelity honesty):
//   - entityName outside the selection (partial export);
//   - stored condition/actions JSON that no longer decodes (defensive — config validation guards
//     the shape on write);
//   - a business-hours timer referencing a calendar that no longer exists (calendars travel by name since #720; the target
//     tenant's install would fail checkTimerCalendar), or a set_deadline action pinning one
//     (queue #79 — checkDeadlineCalendars, same reason);
//   - an assign action whose team ID no longer resolves to a live team (the installed rule could
//     never route);
//   - a create_record action targeting an entity outside the selection (the target tenant may
//     not have it — install would fail checkCreateRecordTargets).
//
// Rules that round-trip WITH a caveat are exported and warned about: a scheduled rule's cadence
// (intervalSeconds is not in the pack format — it re-installs as fire-every-tick), an
// outbound_webhook secret traveling in pack JSON, an in_app_notification userId that is a
// source-tenant user (only "$owner" is portable).
func (s *Service) exportWorkflows(ctx context.Context, tenantID string, selectedSet map[string]bool, relationByEntity map[string]map[string]bool, pack *Pack, warn func(string, ...any), outOfScope func(kind string)) error {
	if s.workflowWr == nil {
		warn("workflows: không xuất được — WorkflowWriter chưa được wire (section bị bỏ qua)")
		return nil
	}
	recs, err := s.workflowWr.ListWorkflows(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("vertical_pack.Export: list workflows: %w", err)
	}

	// Lazily-loaded team id→name map (only needed when a rule carries an assign action).
	var teamNameByID map[string]string
	loadTeams := func() error {
		if teamNameByID != nil {
			return nil
		}
		teamNameByID = map[string]string{}
		if s.teamWr == nil {
			return nil // resolution below fails per-rule with a warning
		}
		teams, err := s.teamWr.ListTeams(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("vertical_pack.Export: list teams: %w", err)
		}
		for i := range teams {
			teamNameByID[teams[i].ID] = teams[i].Name
		}
		return nil
	}

	// Lazily-loaded approval process id→name map (only needed when a rule carries start_approval).
	var processNameByID map[string]string
	loadProcesses := func() error {
		if processNameByID != nil {
			return nil
		}
		processNameByID = map[string]string{}
		if s.approvalWr == nil {
			return nil // resolution below fails per-rule with a warning
		}
		procs, err := s.approvalWr.ListProcesses(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("vertical_pack.Export: list approval processes: %w", err)
		}
		for i := range procs {
			processNameByID[procs[i].ID] = procs[i].Name
		}
		return nil
	}

	// Lazily-loaded vault key id→view (only needed when a rule carries a vault webhook). A key
	// travels as a slot (name + definition, no secret); usedKeys collects the slots of the rules that
	// actually ship, so a dropped rule never leaves an orphan slot behind (specs/050 S6).
	var keyByID map[string]services.CredentialView
	loadKeys := func() error {
		if keyByID != nil {
			return nil
		}
		keyByID = map[string]services.CredentialView{}
		if s.vault == nil {
			return nil // resolution below fails per-rule with a warning
		}
		views, err := s.vault.List(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("vertical_pack.Export: list credentials: %w", err)
		}
		for _, v := range views {
			keyByID[v.ID] = v
		}
		return nil
	}
	usedKeys := map[string]PackCredential{}

	teamNames := map[string]struct{}{} // assign-referenced team names → pack.Teams

	// Lazily-loaded calendar id→row map (queue #720); usedCals collects the calendars the shipped
	// rules pin, so a dropped rule never leaves an orphan calendar behind.
	var calByID map[string]calmeta.BusinessCalendar
	var calErr error
	loadCalendars := func() error {
		if calByID != nil {
			return nil
		}
		calByID = map[string]calmeta.BusinessCalendar{}
		if s.calendarWr == nil {
			return nil // a pin then fails to resolve → the rule is dropped with a warning
		}
		cals, err := s.calendarWr.ListCalendars(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("vertical_pack.Export: list calendars: %w", err)
		}
		for _, c := range cals {
			calByID[c.ID] = c
		}
		return nil
	}
	usedCals := map[string]bool{}
	for i := range recs {
		rec := recs[i]
		if !selectedSet[rec.EntityName] {
			outOfScope("luật tự động")
			continue
		}
		wf, err := rec.ToWorkflow()
		if err != nil {
			warn("workflow %q: bỏ qua — config JSON lưu trữ không decode được (%v)", rec.Name, err)
			continue
		}
		// Queue #720: a pinned calendar travels BY NAME (pack.Calendars); a pin that no longer
		// resolves in the source tenant is the only case still dropped.
		calNameOf := func(id string) (string, bool) {
			id = strings.TrimSpace(id)
			if id == "" {
				return "", true
			}
			if err := loadCalendars(); err != nil {
				calErr = err
				return "", false
			}
			c, ok := calByID[id]
			return c.Name, ok
		}
		calOK := true
		if wf.Condition != nil && wf.Condition.Timer != nil {
			if _, ok := calNameOf(wf.Condition.Timer.CalendarID); !ok {
				calOK = false
			}
		}
		for _, a := range wf.Actions {
			if _, ok := calNameOf(a.CalendarID); !ok {
				calOK = false
			}
		}
		if calErr != nil {
			return calErr
		}
		if !calOK {
			warn("workflow %q: bỏ qua — ghim lịch làm việc không còn tồn tại (hoặc gói chưa được nối dịch vụ lịch)", rec.Name)
			continue
		}

		pw := PackWorkflow{
			Name:       rec.Name,
			EntityName: rec.EntityName,
			Trigger:    rec.Trigger,
			IsActive:   boolPtr(rec.IsActive),
		}
		if wf.Condition != nil {
			pc := &PackWorkflowCondition{Match: wf.Condition.Match, SaveOn: wf.Condition.SaveOn}
			for _, r := range wf.Condition.Rules {
				pc.Rules = append(pc.Rules, PackWorkflowRule{Field: r.Field, Op: r.Op, Value: r.Value})
			}
			if t := wf.Condition.Timer; t != nil {
				pc.Timer = &PackWorkflowTimer{
					DateField: t.DateField,
					Offset:    PackWorkflowTimerOffset{Amount: t.Offset.Amount, Unit: t.Offset.Unit},
					Direction: t.Direction,
					// CalendarID deliberately omitted — it travels as CalendarName (queue #720).
				}
				if n, _ := calNameOf(t.CalendarID); n != "" {
					pc.Timer.CalendarName = n
					usedCals[n] = true
				}
			}
			pw.Condition = pc
		}

		dropped := false
		ruleKeys := map[string]PackCredential{}
		for _, a := range wf.Actions {
			pa := PackWorkflowAction{
				Type: a.Type, Field: a.Field, Value: a.Value,
				UserID: a.UserID, Title: a.Title, Message: a.Message,
				To: a.To, Subject: a.Subject, Body: a.Body,
				URL: a.URL, Secret: a.Secret, Headers: a.Headers,
				Strategy:   a.Strategy,
				EntityName: a.CreateEntity, Values: a.Values,
				Mode: a.Mode, StampField: a.StampField, Anchor: a.Anchor,
				// CalendarID deliberately omitted — it travels as CalendarName (queue #720).
				Via:         a.Via,
				ChildEntity: a.ChildEntity,
				ChildVia:    a.ChildVia,
				OnlyIfEmpty: a.OnlyIfEmpty, ValueSource: a.ValueSource,
				Ops: a.Ops,
			}
			if a.Offset != nil {
				pa.Offset = &PackWorkflowTimerOffset{Amount: a.Offset.Amount, Unit: a.Offset.Unit}
			}
			if n, _ := calNameOf(a.CalendarID); n != "" {
				pa.CalendarName = n
				usedCals[n] = true
			}
			switch strings.TrimSpace(a.Type) {
			case "assign":
				if err := loadTeams(); err != nil {
					return err
				}
				name, ok := teamNameByID[strings.TrimSpace(a.Team)]
				if !ok || strings.TrimSpace(name) == "" {
					warn("workflow %q: bỏ qua — action assign trỏ team id %q không còn tồn tại trong tenant", rec.Name, a.Team)
					dropped = true
				} else {
					pa.Team = name // installer pass 6b resolves NAME → the target tenant's id
					teamNames[name] = struct{}{}
				}
			case "start_approval":
				// specs/043 S9b: the process travels as its NAME (the id is this tenant's). The process
				// itself rides pack.Approvals when it is portable; install resolves the name after the
				// approvals pass and skips the rule, said aloud, when it did not make it.
				if err := loadProcesses(); err != nil {
					return err
				}
				name, ok := processNameByID[strings.TrimSpace(a.ProcessID)]
				if !ok || strings.TrimSpace(name) == "" {
					warn("workflow %q: bỏ qua — action start_approval trỏ quy trình duyệt id %q không còn tồn tại trong tenant", rec.Name, a.ProcessID)
					dropped = true
				} else {
					pa.ProcessName = name
				}
			case "field_update":
				// A field_update writing a LITERAL into a relation field carries a source-tenant
				// record UUID that won't resolve after install (RECORD_RELATION_INVALID) — never silent.
				if relationByEntity[rec.EntityName][strings.TrimSpace(a.Field)] {
					if sv, ok := a.Value.(string); ok && strings.TrimSpace(sv) != "" {
						warn("workflow %q: field_update đặt trường quan hệ %q sang giá trị cố định (UUID bản ghi tenant nguồn) — sẽ không nối được ở tenant đích", rec.Name, a.Field)
					}
				}
			case "create_record":
				if !selectedSet[strings.TrimSpace(a.CreateEntity)] {
					warn("workflow %q: bỏ qua — action create_record trỏ entity %q không nằm trong selection export", rec.Name, a.CreateEntity)
					dropped = true
				} else {
					// Same relation-literal leak as field_update, but keyed by the TARGET entity's
					// relation fields (Values keys are the created entity's fields).
					relSet := relationByEntity[strings.TrimSpace(a.CreateEntity)]
					for k, v := range a.Values {
						if relSet[k] {
							if sv, ok := v.(string); ok && strings.TrimSpace(sv) != "" {
								warn("workflow %q: create_record đặt trường quan hệ %q sang giá trị cố định (UUID bản ghi tenant nguồn) — sẽ không nối được ở tenant đích", rec.Name, k)
							}
						}
					}
				}
			case "outbound_webhook":
				// specs/050 S6: a vault action travels as the key's NAME + path; the slot definition
				// rides pack.Credentials. A key that is gone, or a path that carries a query value
				// (could be the credential itself, same mask as the admin API), stays home.
				if cid := strings.TrimSpace(a.CredentialID); cid != "" {
					if err := loadKeys(); err != nil {
						return err
					}
					key, ok := keyByID[cid]
					switch {
					case !ok:
						warn("workflow %q: bỏ qua — outbound_webhook dùng khoá kết nối không còn trong Kho khoá kết nối của tổ chức", rec.Name)
						dropped = true
					case workflow.RedactTarget(a.Path) != a.Path:
						warn("workflow %q: bỏ qua — outbound_webhook có giá trị query trong đường dẫn (có thể là khoá); gói không mang khoá của tổ chức nguồn", rec.Name)
						dropped = true
					default:
						pa.CredentialName, pa.Path = key.Name, a.Path
						ruleKeys[key.Name] = PackCredential{
							Name: key.Name, Label: key.Label, BaseURL: key.BaseURL, AuthType: string(key.AuthType),
							HeaderName: key.HeaderName, HeaderPrefix: key.HeaderPrefix, Username: key.Username,
							ExtraHeaderNames: key.ExtraHeaderNames,
						}
					}
				}
				// specs/050 S3a review: the url survives stripWorkflowCredentials, and a url can be the
				// credential itself (user:pass@, ?token=) — the same mask the admin API applies on read.
				// A pack goes to another customer and install copies it into that tenant's rule and
				// provenance rows, so such a rule stays home. (A token in the PATH of a known host cannot
				// be told from a path; it is listed by Doctor C10; the operator moves it by hand onto a url_token key, S5.)
				if u := strings.TrimSpace(a.URL); u != "" && workflow.RedactTarget(u) != u {
					warn("workflow %q: bỏ qua — outbound_webhook có tài khoản/giá trị query ngay trong URL (có thể là khoá); gói không mang khoá của tổ chức nguồn. Dựng lại rule ở tổ chức đích với khoá trong Kho khoá kết nối", rec.Name)
					dropped = true
				}
				// The credential itself is removed below by stripWorkflowCredentials — this only
				// tells the operator what the destination will have to set up. It used to be the
				// ONLY mitigation ("cân nhắc xoá trước khi chia sẻ pack"), i.e. a warning string
				// standing in for a control. Since specs/050 S3a a key cannot be put back on the rule
				// (WEBHOOK_INLINE_SECRET_REMOVED), so the way forward named is the vault.
				if strings.TrimSpace(a.Secret) != "" {
					warn("workflow %q: outbound_webhook — HMAC secret KHÔNG đi theo pack (đã gỡ). Ở tenant đích: tạo khoá ở Quản trị › Kho khoá kết nối rồi chọn khoá đó cho action trong trình dựng quy trình thì rule mới ký được request.", rec.Name)
				}
				if len(a.Headers) > 0 {
					warn("workflow %q: outbound_webhook — %d header tuỳ chỉnh KHÔNG đi theo pack (đã gỡ; có thể chứa Authorization/API-key của tenant nguồn). Ở tenant đích, nếu endpoint cần: tạo khoá ở Quản trị › Kho khoá kết nối (header xác thực + header bổ sung) rồi chọn khoá đó trong trình dựng quy trình.", rec.Name, len(a.Headers))
				}
			case "in_app_notification":
				// "$owner" and "$field:<name>" travel (the field is part of the pack); a fixed user, a
				// role id or a team id belongs to the source tenant.
				if uid := strings.TrimSpace(a.UserID); uid != "" && uid != "$owner" && !strings.HasPrefix(uid, workflow.RecipientFieldPrefix) {
					warn("workflow %q: in_app_notification người nhận %q là người/vai/nhóm của tenant nguồn — sẽ không tồn tại ở tenant đích (dùng \"$owner\" hoặc một trường người dùng của bản ghi nếu muốn portable)", rec.Name, a.UserID)
				}
			case "set_deadline":
				// The offset is the whole action: without it the install dies at Pass 8. Copying it
				// above is what makes this arm normally silent — warn only if it somehow arrived nil,
				// so a lost param is never the thing nobody said out loud.
				if pa.Offset == nil {
					warn("workflow %q: set_deadline không có offset — cài ở tenant đích sẽ hỏng (WORKFLOW_ACTION_INVALID)", rec.Name)
				}
			case "email_alert":
				// To/Subject/Body are copied above — known, so silent (it used to fall into default
				// and raise a false "exporter chưa biết" warning on every export).
			case "set_elapsed":
				// Its only param is `field`, copied above — known, so silent.
			case "veto":
				// specs/048: field + message, both copied above — known, so silent. (saveOn rides on
				// the condition, copied with it.)
			case "normalize":
				// specs/048 S5: field + ops, both copied above — known, so silent.
			default:
				// An action type this exporter has never heard of is copied field-by-field above,
				// which means any param it carries OUTSIDE that list is dropped right here, in
				// silence. That silence is the bug class create_record and set_deadline each paid
				// for once; say it out loud instead of shipping a pack that dies on install.
				warn("workflow %q: action type %q chưa được exporter biết tới — pack có thể thiếu tham số của nó, kiểm lại trước khi chia sẻ", rec.Name, a.Type)
			}
			if dropped {
				break
			}
			pw.Actions = append(pw.Actions, pa)
		}
		if dropped {
			continue
		}
		if rec.Trigger == "scheduled" {
			pw.IntervalSeconds = rec.IntervalSeconds // #295: cadence travels, else the rule fires every tick
		}
		// A pack is handed to a DIFFERENT customer. No live signing key or Authorization header
		// leaves this tenant inside it.
		pw, _ = stripWorkflowCredentials(pw)
		pack.Workflows = append(pack.Workflows, pw)
		for name, pc := range ruleKeys {
			usedKeys[name] = pc
		}
	}
	for _, pc := range usedKeys {
		pack.Credentials = append(pack.Credentials, pc)
	}
	sort.Slice(pack.Credentials, func(i, j int) bool { return pack.Credentials[i].Name < pack.Credentials[j].Name })

	// Stable order: ListWorkflows returns newest-first (insert-order dependent); sort by name so
	// two exports of the same tenant serialize identically.
	sort.SliceStable(pack.Workflows, func(i, j int) bool { return pack.Workflows[i].Name < pack.Workflows[j].Name })

	// A rule that reads the tenant's DEFAULT calendar (no pin) needs it on the other side too.
	if len(businessHoursRuleNames(pack)) > 0 && s.calendarWr != nil {
		if err := loadCalendars(); err != nil {
			return err
		}
		for _, c := range calByID {
			if c.IsDefault {
				usedCals[c.Name] = true
			}
		}
	}
	for _, c := range calByID {
		if !usedCals[c.Name] {
			continue
		}
		pc := PackCalendar{Name: c.Name, Timezone: c.Timezone, IsDefault: c.IsDefault}
		if err := json.Unmarshal(c.Hours, &pc.Hours); err != nil {
			warn("lịch %q: giờ làm việc lưu trữ không decode được (%v) — lịch bị bỏ qua", c.Name, err)
			continue
		}
		if len(c.Holidays) > 0 && string(c.Holidays) != "null" {
			if err := json.Unmarshal(c.Holidays, &pc.Holidays); err != nil {
				warn("lịch %q: ngày lễ lưu trữ không decode được (%v) — lịch bị bỏ qua", c.Name, err)
				continue
			}
		}
		pack.Calendars = append(pack.Calendars, pc)
	}
	sort.Slice(pack.Calendars, func(i, j int) bool { return pack.Calendars[i].Name < pack.Calendars[j].Name })

	for name := range teamNames {
		pack.Teams = append(pack.Teams, PackTeam{Name: name})
	}
	sort.Slice(pack.Teams, func(i, j int) bool { return pack.Teams[i].Name < pack.Teams[j].Name })
	return nil
}

// exportReports serializes the tenant's saved reports on the selected entities into pack.Reports.
// A report outside the selection, with an undecodable stored config, is dropped WITH a warning.
func (s *Service) exportReports(ctx context.Context, tenantID string, selectedSet map[string]bool, pack *Pack, warn func(string, ...any), outOfScope func(kind string)) error {
	if s.reportWr == nil {
		warn("reports: không xuất được — ReportWriter chưa được wire (section bị bỏ qua)")
		return nil
	}
	recs, err := s.reportWr.ListReports(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("vertical_pack.Export: list reports: %w", err)
	}
	for i := range recs {
		rec := recs[i]
		if !selectedSet[rec.EntityName] {
			outOfScope("báo cáo")
			continue
		}
		var cfg services.ReportConfig
		if len(rec.Config) > 0 && string(rec.Config) != "null" {
			if err := json.Unmarshal(rec.Config, &cfg); err != nil {
				warn("report %q: bỏ qua — config JSON lưu trữ không decode được (%v)", rec.Name, err)
				continue
			}
		}
		pr := PackReport{
			Name:       rec.Name,
			EntityName: rec.EntityName,
			Type:       rec.Type,
			Config: PackReportConfig{
				Metric:  cfg.Metric,
				Field:   cfg.Field,
				GroupBy: cfg.GroupBy,
				Columns: cfg.Columns,
				Bucket:  cfg.Bucket,
			},
			IsActive: boolPtr(rec.IsActive),
		}
		for _, f := range cfg.Filters {
			pr.Config.Filters = append(pr.Config.Filters, PackReportFilter{Field: f.Field, Op: f.Op, Values: f.Values})
		}
		if cfg.ColumnGroup != nil {
			pr.Config.ColumnGroup = &PackReportGroup{Field: cfg.ColumnGroup.Field, Bucket: cfg.ColumnGroup.Bucket}
		}
		for _, g := range cfg.SubGroups {
			pr.Config.SubGroups = append(pr.Config.SubGroups, PackReportGroup{Field: g.Field, Bucket: g.Bucket})
		}
		if h := cfg.Having; h != nil {
			pr.Config.Having = &PackReportHaving{On: h.On, Op: h.Op, Values: h.Values}
		}
		if cfg.Sort != nil {
			pr.Config.Sort = &PackReportSort{Field: cfg.Sort.Field, Dir: cfg.Sort.Dir}
		}
		for _, j := range cfg.Joins {
			pr.Config.Joins = append(pr.Config.Joins, PackReportJoin{Relationship: j.Relationship, Optional: j.Optional})
		}
		pack.Reports = append(pack.Reports, pr)
	}
	sort.SliceStable(pack.Reports, func(i, j int) bool { return pack.Reports[i].Name < pack.Reports[j].Name })
	return nil
}

// exportWorkQueues serializes the tenant's ACTIVE work queues on the selected entities into
// pack.WorkQueues (PACK-QUEUES), resolving each queue's role_id back to the role NAME (the
// installer's Pass 8c resolves name→id on the way back in). Dropped WITH a warning, never
// silently: a queue outside the selection, an inactive queue (PackWorkQueue installs as active —
// exporting a disabled one would flip its semantics), or one whose role no longer resolves.
func (s *Service) exportWorkQueues(ctx context.Context, tenantID string, selectedSet map[string]bool, pack *Pack, warn func(string, ...any), outOfScope func(kind string)) error {
	if s.workQueueWr == nil {
		return nil // work queues not wired — nothing to export, and nothing was installable either
	}
	qs, err := s.workQueueWr.ListAdmin(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("vertical_pack.Export: list work queues: %w", err)
	}
	// Lazily-loaded role id→name map (only needed when a queue is role-scoped).
	var roleNameByID map[string]string
	for i := range qs {
		q := qs[i]
		if !selectedSet[q.EntityName] {
			outOfScope("hàng đợi việc")
			continue
		}
		if !q.IsActive {
			warn("work queue %q: bỏ qua — đang tắt (pack format v1 chỉ mang queue đang bật)", q.Name)
			continue
		}
		roleName := ""
		if q.RoleID != nil && *q.RoleID != "" {
			if roleNameByID == nil {
				roleNameByID = map[string]string{}
				roles, rerr := s.roleRd.ListRoles(ctx, tenantID)
				if rerr != nil {
					return fmt.Errorf("vertical_pack.Export: list roles for work queues: %w", rerr)
				}
				for _, r := range roles {
					roleNameByID[r.ID] = r.Name
				}
			}
			roleName = roleNameByID[*q.RoleID]
			if roleName == "" {
				warn("work queue %q: bỏ qua — role_id không còn trỏ tới role nào (đã bị xoá?)", q.Name)
				continue
			}
			if auth.IsOwnerRoleName(roleName) {
				warn("work queue %q: bỏ qua — chỉ định cho vai hệ thống của không gian, không đi theo gói", q.Name)
				continue
			}
		}
		pq := PackWorkQueue{
			Name:       q.Name,
			Label:      q.Label,
			EntityName: q.EntityName,
			RoleName:   roleName,
			SortOrder:  q.SortOrder,
			ActionVerb: q.ActionVerb,
			Position:   q.Position,
		}
		if q.SortField != nil {
			pq.SortField = *q.SortField
		}
		for _, f := range q.Filters {
			pq.Filters = append(pq.Filters, PackQueueFilter{Field: f.Field, Operator: f.Operator, Value: f.Value})
		}
		pack.WorkQueues = append(pack.WorkQueues, pq)
	}
	sort.SliceStable(pack.WorkQueues, func(i, j int) bool { return pack.WorkQueues[i].Name < pack.WorkQueues[j].Name })
	return nil
}

// exportApprovals serializes the tenant's approval processes on the selected entities into
// pack.Approvals (PACK-APPROVALS, ../specs/027-handover-wizard/spec.md §3.2). Portability rule: a step's approver
// travels as a role NAME (roles ride the pack in full, so the name resolves in the target tenant);
// a process with ANY "user" approver step is NOT exported — a user id from the source tenant is
// meaningless cross-tenant, and half-exporting the process (only its role steps) would silently
// change who approves. Dropped WITH a warning, never silently:
//   - a process on an entity outside the export selection;
//   - a process with a user-approver step (the F6b honest-warning: reassign after install);
//   - a role-approver step whose role id no longer resolves to a live role;
//   - stored steps/criteria JSON that no longer decodes (defensive — CreateProcess guards writes).
func (s *Service) exportApprovals(ctx context.Context, tenantID string, selectedSet map[string]bool, pack *Pack, warn func(string, ...any), outOfScope func(kind string)) error {
	if s.approvalWr == nil {
		warn("quy trình duyệt: không xuất được — ApprovalWriter chưa được wire (section bị bỏ qua)")
		return nil
	}
	recs, err := s.approvalWr.ListProcesses(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("vertical_pack.Export: list approval processes: %w", err)
	}
	if len(recs) == 0 {
		return nil
	}
	// Lazily-loaded role id→name map (only needed when a process has role-approver steps).
	var roleNameByID map[string]string
	loadRoles := func() error {
		if roleNameByID != nil {
			return nil
		}
		roleNameByID = map[string]string{}
		roles, rerr := s.roleRd.ListRoles(ctx, tenantID)
		if rerr != nil {
			return fmt.Errorf("vertical_pack.Export: list roles for approvals: %w", rerr)
		}
		for _, r := range roles {
			roleNameByID[r.ID] = r.Name
		}
		return nil
	}

	// Lazily-loaded team / work-queue id→name maps (only needed when a step is handed to one).
	var teamNameByID, queueNameByID map[string]string
	loadTeams := func() error {
		if teamNameByID != nil {
			return nil
		}
		teamNameByID = map[string]string{}
		if s.teamWr == nil {
			return nil
		}
		teams, terr := s.teamWr.ListTeams(ctx, tenantID)
		if terr != nil {
			return fmt.Errorf("vertical_pack.Export: list teams for approvals: %w", terr)
		}
		for _, t := range teams {
			teamNameByID[t.ID] = t.Name
		}
		return nil
	}
	loadQueues := func() error {
		if queueNameByID != nil {
			return nil
		}
		queueNameByID = map[string]string{}
		if s.workQueueWr == nil {
			return nil
		}
		qs, qerr := s.workQueueWr.ListAdmin(ctx, tenantID)
		if qerr != nil {
			return fmt.Errorf("vertical_pack.Export: list work queues for approvals: %w", qerr)
		}
		for _, q := range qs {
			queueNameByID[q.ID] = q.Name
		}
		return nil
	}

	for i := range recs {
		rec := recs[i]
		if !selectedSet[rec.EntityName] {
			outOfScope("quy trình duyệt")
			continue
		}
		var steps []bpm.Step
		var nodeOnReject []*bpm.OnReject // graph form only: node i's onReject (S8), in chain order
		if len(rec.Graph) > 0 && string(rec.Graph) != "null" {
			// A graph-form process (specs/043 S3a2/S4) travels as its node CHAIN: the pack's
			// `steps` carry parallel steps too, and install lifts them back to a graph. A graph
			// this binary cannot run (a node kind or key of a later slice) has no pack shape yet —
			// left out by name rather than shipped as a shorter chain.
			var g bpm.Graph
			if err := json.Unmarshal(rec.Graph, &g); err != nil {
				warn("quy trình duyệt %q: bỏ qua — graph JSON lưu trữ không decode được (%v)", rec.Name, err)
				continue
			}
			if g.HasRouting() { // specs/043 S7: branches / machine steps have no flat `steps` form
				warn("quy trình duyệt %q: bỏ qua — quy trình lưu ở dạng đồ thị có rẽ nhánh, bước tự động hoặc bước kết thúc mà gói ngành chưa đóng gói được; dựng lại sau khi cài", rec.Name)
				continue
			}
			if steps = (bpm.Process{Graph: &g}).Chain(); steps == nil {
				warn("quy trình duyệt %q: bỏ qua — quy trình lưu ở dạng đồ thị có loại bước hoặc khoá mà gói ngành chưa đóng gói được; dựng lại sau khi cài", rec.Name)
				continue
			}
			for _, n := range g.Nodes {
				nodeOnReject = append(nodeOnReject, n.OnReject)
			}
		} else if len(rec.Steps) > 0 && string(rec.Steps) != "null" {
			if err := json.Unmarshal(rec.Steps, &steps); err != nil {
				warn("quy trình duyệt %q: bỏ qua — steps JSON lưu trữ không decode được (%v)", rec.Name, err)
				continue
			}
		}
		if len(steps) == 0 {
			warn("quy trình duyệt %q: bỏ qua — không có bước phê duyệt nào trong định nghĩa lưu trữ", rec.Name)
			continue
		}
		steps = bpm.NormalizeSteps(steps) // a legacy row exports the positional ids it runs under

		// Routing (specs/043 S6a/S6b): a step handed to a role / team / queue travels BY NAME and a
		// field / submitter rule as it is (see the step loop). The process's fallback assignee has no
		// portable pack form yet — left out by name, never shipped without it (a plain process that
		// would install with the wrong, or no, holder when the rule finds nobody).
		if len(rec.Options) > 0 && string(rec.Options) != "null" {
			warn("quy trình duyệt %q: bỏ qua — có người dự phòng (options) mà gói ngành chưa đóng gói được; dựng lại sau khi cài", rec.Name)
			continue
		}

		pa := PackApproval{
			Name:       rec.Name,
			EntityName: rec.EntityName,
			Trigger:    rec.Trigger,
			IsActive:   boolPtr(rec.IsActive),
			Steps:      make([]PackApprovalStep, 0, len(steps)),
		}
		// The terminal write-back travels with the process: without it the installed process
		// approves quotes that never change status (specs/043 S0 — outcome used to drop here).
		if len(rec.Outcome) > 0 && string(rec.Outcome) != "null" {
			var o bpm.Outcome
			if err := json.Unmarshal(rec.Outcome, &o); err != nil {
				warn("quy trình duyệt %q: bỏ qua — outcome JSON lưu trữ không decode được (%v)", rec.Name, err)
				continue
			}
			pa.Outcome = &PackApprovalOutcome{StatusField: o.StatusField, ApprovedValue: o.ApprovedValue, RejectedValue: o.RejectedValue, ReturnedValue: o.ReturnedValue}
		}
		dropped := false
		for si, st := range steps {
			if a := st.Assignment; a != nil { // S6b: a role / team / queue BY NAME, or a field / submitter rule
				pas := &PackAssignment{Type: string(a.Type), Field: a.Field, Strategy: a.Strategy}
				var name, kind string
				switch a.Type {
				case bpm.AssignUser:
					warn("quy trình duyệt '%s': người duyệt chỉ định đích danh không đóng gói được — gán lại sau khi cài", rec.Name)
					dropped = true
				case bpm.AssignRole:
					if err := loadRoles(); err != nil {
						return err
					}
					name, kind = roleNameByID[a.ID], "role"
				case bpm.AssignTeam:
					if err := loadTeams(); err != nil {
						return err
					}
					name, kind = teamNameByID[a.ID], "nhóm"
				case bpm.AssignQueue:
					if err := loadQueues(); err != nil {
						return err
					}
					name, kind = queueNameByID[a.ID], "hàng đợi"
				}
				if dropped {
					break
				}
				if kind != "" && strings.TrimSpace(name) == "" {
					warn("quy trình duyệt %q: bỏ qua — bước phê duyệt trỏ %s id %q không còn tồn tại trong tenant", rec.Name, kind, a.ID)
					dropped = true
					break
				}
				pas.Ref = name
				ps := PackApprovalStep{ID: st.ID, Name: st.Name, SLASeconds: st.SLASeconds, SLAClock: st.SLAClock, Assignment: pas, OnReject: packOnRejectOf(nodeOnReject, si)}
				if a.EachMember() { // the seats form a council: its policy travels with the step
					ps.Completion, ps.CompletionPercent = string(st.CompletionMode()), st.CompletionPolicy().Percent
				}
				pa.Steps = append(pa.Steps, ps)
				continue
			}
			if st.Parallel() { // S4: every approver must be a role the target can resolve by NAME
				ps := PackApprovalStep{ID: st.ID, Name: st.Name, SLASeconds: st.SLASeconds, SLAClock: st.SLAClock, Completion: string(st.CompletionMode()), CompletionPercent: st.CompletionPolicy().Percent, OnReject: packOnRejectOf(nodeOnReject, si)}
				for _, ap := range st.Approvers {
					if ap.Type == bpm.ApproverUser {
						warn("quy trình duyệt '%s': người duyệt chỉ định đích danh không đóng gói được — gán lại sau khi cài", rec.Name)
						dropped = true
						break
					}
					if err := loadRoles(); err != nil {
						return err
					}
					roleName := roleNameByID[ap.ID]
					if strings.TrimSpace(roleName) == "" {
						warn("quy trình duyệt %q: bỏ qua — bước phê duyệt trỏ role id %q không còn tồn tại trong tenant", rec.Name, ap.ID)
						dropped = true
						break
					}
					ps.Approvers = append(ps.Approvers, PackApprover{Type: string(bpm.ApproverRole), Ref: roleName})
				}
				if dropped {
					break
				}
				pa.Steps = append(pa.Steps, ps)
				continue
			}
			if st.ApproverType == bpm.ApproverUser {
				// Task-mandated F6b honest-warning: a named person cannot be packaged.
				warn("quy trình duyệt '%s': người duyệt chỉ định đích danh không đóng gói được — gán lại sau khi cài", rec.Name)
				dropped = true
				break
			}
			if err := loadRoles(); err != nil {
				return err
			}
			roleName := roleNameByID[st.ApproverID]
			if strings.TrimSpace(roleName) == "" {
				warn("quy trình duyệt %q: bỏ qua — bước phê duyệt trỏ role id %q không còn tồn tại trong tenant", rec.Name, st.ApproverID)
				dropped = true
				break
			}
			pa.Steps = append(pa.Steps, PackApprovalStep{
				ID:           st.ID,
				Name:         st.Name,
				ApproverType: string(bpm.ApproverRole),
				ApproverRef:  roleName, // installer resolves NAME → the target tenant's role id
				SLASeconds:   st.SLASeconds,
				SLAClock:     st.SLAClock,
				OnReject:     packOnRejectOf(nodeOnReject, si),
			})
		}
		if dropped {
			continue
		}

		if len(rec.Criteria) > 0 && string(rec.Criteria) != "null" {
			var cond workflow.Condition
			if err := json.Unmarshal(rec.Criteria, &cond); err != nil {
				warn("quy trình duyệt %q: bỏ qua — criteria JSON lưu trữ không decode được (%v)", rec.Name, err)
				continue
			}
			if len(cond.Rules) > 0 {
				pc := &PackApprovalCriteria{Match: cond.Match}
				for _, r := range cond.Rules {
					pc.Rules = append(pc.Rules, PackWorkflowRule{Field: r.Field, Op: r.Op, Value: r.Value})
				}
				pa.Criteria = pc
			}
		}
		pack.Approvals = append(pack.Approvals, pa)
	}

	// Stable order (entity, name) — the same composite the install skip keys on — so two exports
	// of the same tenant serialize identically.
	sort.SliceStable(pack.Approvals, func(i, j int) bool {
		if pack.Approvals[i].EntityName != pack.Approvals[j].EntityName {
			return pack.Approvals[i].EntityName < pack.Approvals[j].EntityName
		}
		return pack.Approvals[i].Name < pack.Approvals[j].Name
	})
	return nil
}

// exportDashboards serializes the tenant's saved dashboards into pack.Dashboards. Filtering is
// PER-WIDGET: a widget on an entity outside the selection is dropped (warned), the dashboard
// itself survives with its qualifying widgets — CreateDashboard validates widgets independently,
// so a subset installs cleanly. A dashboard whose EVERY widget referenced excluded entities is
// dropped whole (warned): shipping an empty shell for it would misrepresent the source. A
// dashboard that had NO widgets to begin with is kept as-is (a named empty board is valid).
func (s *Service) exportDashboards(ctx context.Context, tenantID string, selectedSet map[string]bool, pack *Pack, warn func(string, ...any), outOfScope func(kind string)) error {
	if s.dashboardWr == nil {
		warn("dashboards: không xuất được — DashboardWriter chưa được wire (section bị bỏ qua)")
		return nil
	}
	recs, err := s.dashboardWr.ListDashboards(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("vertical_pack.Export: list dashboards: %w", err)
	}
	for i := range recs {
		rec := recs[i]
		var widgets []services.WidgetConfig
		if len(rec.Widgets) > 0 && string(rec.Widgets) != "null" {
			if err := json.Unmarshal(rec.Widgets, &widgets); err != nil {
				warn("dashboard %q: bỏ qua — widgets JSON lưu trữ không decode được (%v)", rec.Name, err)
				continue
			}
		}
		pd := PackDashboard{Name: rec.Name, Widgets: make([]PackWidget, 0, len(widgets))}
		if rec.Description != nil {
			pd.Description = *rec.Description
		}
		var widgetWarns []string // only worth saying when the dashboard survives (else it is one "other app" count)
		for _, w := range widgets {
			if !selectedSet[w.Entity] {
				widgetWarns = append(widgetWarns, fmt.Sprintf("dashboard %q: bỏ widget %q — bảng %q không nằm trong gói", rec.Name, w.Title, w.Entity))
				continue
			}
			if len(w.Joins) > 0 {
				// The pack widget format has no joins (#295 carries them) — shipping the card without them
				// would install a "<rel>.<field>" token that dies on every run.
				widgetWarns = append(widgetWarns, fmt.Sprintf("dashboard %q: bỏ widget %q — widget ghép quan hệ chưa nằm trong định dạng gói", rec.Name, w.Title))
				continue
			}
			pw := PackWidget{
				ID: w.ID, Title: w.Title, Type: w.Type, Entity: w.Entity,
				Metric: w.Metric, Field: w.Field, GroupBy: w.GroupBy, Bucket: w.Bucket,
				X: w.X, Y: w.Y, W: w.W, H: w.H,
			}
			for _, f := range w.Filters {
				pw.Filters = append(pw.Filters, PackReportFilter{Field: f.Field, Op: f.Op, Values: f.Values})
			}
			pd.Widgets = append(pd.Widgets, pw)
		}
		// Drop any dashboard that contributes no IN-SCOPE widget, not just one that HAD widgets and
		// lost them all. The old `len(widgets) > 0 &&` conjunct meant a widget-less dashboard failed
		// the first test and was appended with its Name and Description regardless of ?entities= —
		// the only pack section with no scoping predicate at all. The factory tenant accumulates one
		// app per customer engagement, so its dashboard list holds other customers' board names, and
		// install materialized them (and packNavItems could put them in the minted sidebar): the
		// customer opens their dashboards and reads a competitor's name.
		if len(pd.Widgets) == 0 {
			outOfScope("dashboard")
			continue
		}
		for _, w := range widgetWarns {
			warn("%s", w)
		}
		pack.Dashboards = append(pack.Dashboards, pd)
	}
	sort.SliceStable(pack.Dashboards, func(i, j int) bool { return pack.Dashboards[i].Name < pack.Dashboards[j].Name })
	return nil
}

// exportLinks emits the N-N join rows between exported samples as PackLinks — the missing half
// of ?includeData=true (samples used to re-install WITHOUT their links, so N-N rollups read 0 in
// the target tenant). It walks pack.NNRelationships (already filtered to both-endpoints-selected,
// name-sorted) and maps each join row's two record ids through the sample id→ref table. A row
// whose endpoint is not among the samples (soft-deleted record, or beyond the export cap) cannot
// be represented — it is dropped and COUNTED into one warning per relationship. FromRef always
// carries the alphabetically-first entity's side (dynrepo.ListJoinRows' SourceID contract);
// install's Link resolves either direction through the relationship registry.
func (s *Service) exportLinks(ctx context.Context, tenantID string, pack *Pack, idToRef map[string]string, warn func(string, ...any)) error {
	if len(pack.NNRelationships) == 0 {
		return nil
	}
	if s.linkRd == nil {
		warn("links: không xuất được — LinkReader chưa được wire (section bị bỏ qua)")
		return nil
	}
	for _, nn := range pack.NNRelationships {
		rows, err := s.linkRd.ListJoinRows(ctx, nn.EntityA, nn.EntityB, tenantID)
		if err != nil {
			return fmt.Errorf("vertical_pack.Export: list join rows of %q: %w", nn.Name, err)
		}
		skipped := 0
		for _, row := range rows {
			fromRef, okFrom := idToRef[row.SourceID]
			toRef, okTo := idToRef[row.TargetID]
			if !okFrom || !okTo {
				skipped++
				continue
			}
			pack.Links = append(pack.Links, PackLink{Relation: nn.Name, FromRef: fromRef, ToRef: toRef})
		}
		if skipped > 0 {
			warn("links %q: bỏ %d liên kết — bản ghi đầu-cuối không nằm trong samples (đã soft-delete hoặc vượt trần %d bản ghi/entity)", nn.Name, skipped, exportRecordCap)
		}
	}
	return nil
}

// boolPtr returns a pointer to b — PackWorkflow/PackReport carry IsActive as *bool so an
// exported inactive rule round-trips honestly instead of silently re-activating.
func boolPtr(b bool) *bool { return &b }

// exportSamples reads the records of the selected entities and returns them as PackSamples with
// synthetic "@ref" handles. It runs in two passes so a relation pointing at ANY exported record
// (regardless of insert order) resolves: first every record is read and assigned a globally-unique
// ref (id → ref map built across all entities); then each record's Data is rewritten — computed
// keys stripped, relation-field UUIDs replaced by the target record's "@ref" (a relation whose
// target was not exported is dropped rather than left as a dangling UUID that would 422 on install).
// The id→ref map is returned so exportLinks can express N-N join rows over the same refs. An
// entity truncated at exportRecordCap is warned about (fidelity honesty), never silent.
func (s *Service) exportSamples(
	ctx context.Context,
	tenantID string,
	selected []string,
	computedByEntity, relationByEntity, polymorphicByEntity map[string]map[string]bool,
	warn func(string, ...any),
) ([]PackSample, map[string]string, error) {
	type entityRecords struct {
		entity  string
		records []dynrepo.Record
	}

	var grouped []entityRecords
	idToRef := map[string]string{}
	counter := 0
	for _, name := range selected {
		recs, total, err := s.listAllRecords(ctx, tenantID, name)
		if err != nil {
			return nil, nil, err
		}
		if total > exportRecordCap {
			warn("samples %q: cắt tại %d/%d bản ghi (trần exportRecordCap — pack là seed/demo, không phải bulk backup)", name, len(recs), total)
		}
		for i := range recs {
			counter++
			idToRef[recs[i].ID] = fmt.Sprintf("@r%d", counter)
		}
		grouped = append(grouped, entityRecords{entity: name, records: recs})
	}

	var samples []PackSample
	for _, er := range grouped {
		computedSet := computedByEntity[er.entity]
		relationSet := relationByEntity[er.entity]
		polymorphicSet := polymorphicByEntity[er.entity]
		if len(polymorphicSet) > 0 && len(er.records) > 0 {
			warn("samples %q: %d trường polymorphic không được đóng gói — giá trị {type,id} trỏ bản ghi tenant nguồn (leak + fail cài lại); tenant đích tự nối lại sau", er.entity, len(polymorphicSet))
		}
		relDropped := 0
		ps := PackSample{Entity: er.entity, Records: make([]PackRecord, 0, len(er.records))}
		for i := range er.records {
			rec := er.records[i]
			data := make(map[string]any, len(rec.Data))
			for k, v := range rec.Data {
				if computedSet[k] {
					continue // never seed a computed/server-assigned value (trap B9/G1)
				}
				if polymorphicSet[k] {
					continue // strip {type,id}: a raw source-tenant record UUID (cross-tenant leak + 422 on re-install)
				}
				if relationSet[k] {
					idStr, ok := v.(string)
					if !ok || strings.TrimSpace(idStr) == "" {
						continue // no/blank relation value → omit the key
					}
					ref, found := idToRef[idStr]
					if !found {
						relDropped++
						continue // target not exported → drop rather than dangle a raw UUID
					}
					data[k] = ref
					continue
				}
				// Non-relation value: a genuine literal that happens to start with "@" (a note
				// "@followup", or a code literally equal to a minted ref like "@r5") must NOT be
				// resolved as a relation ref on install. Escape a leading "@" by doubling it;
				// resolveRefs unescapes exactly one. Relation refs above are single-"@", unescaped.
				data[k] = escapeRefLiteral(v)
			}
			ps.Records = append(ps.Records, PackRecord{Ref: idToRef[rec.ID], Data: data})
		}
		if relDropped > 0 {
			warn("samples %q: %d giá trị quan hệ bị bỏ — bản ghi đích không nằm trong tập export (ngoài selection, đã xoá, hoặc quá trần); bản ghi con sẽ cài với quan hệ để trống", er.entity, relDropped)
		}
		if len(ps.Records) > 0 {
			samples = append(samples, ps)
		}
	}
	return samples, idToRef, nil
}

// listAllRecords pages through every non-deleted record of one entity for the tenant (admin scope,
// no owner/team narrowing) up to exportRecordCap. Ordered by created_at ASC for a stable-ish dump.
// The second return is the entity's TOTAL live-record count so the caller can warn on truncation.
func (s *Service) listAllRecords(ctx context.Context, tenantID, entity string) ([]dynrepo.Record, int, error) {
	scope := dynrepo.RowScope{Level: dynrepo.AccessAll, IsAdmin: true}
	var out []dynrepo.Record
	var total int
	for page := 1; ; page++ {
		recs, tot, err := s.recordRd.ListRecords(ctx, entity, tenantID, scope, dynrepo.ListParams{
			Page:  page,
			Limit: exportRecordPageLimit,
			Sort:  "created_at",
			Order: "asc",
		})
		if err != nil {
			return nil, 0, fmt.Errorf("vertical_pack.Export: list records of %q: %w", entity, err)
		}
		total = tot
		out = append(out, recs...)
		if len(recs) == 0 || len(out) >= total || len(out) >= exportRecordCap {
			break
		}
	}
	return out, total, nil
}

// decodeOptions turns a stored sys_fields.options JSON-string pointer into a map. A nil pointer
// (NULL column) or empty string yields a nil map (no options), mirroring how the field service
// stores "no options" as NULL.
func decodeOptions(raw *string) (map[string]any, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(*raw), &out); err != nil {
		return nil, fmt.Errorf("unmarshal options: %w", err)
	}
	return out, nil
}

// escapeRefLiteral doubles a leading "@" on a NON-relation string value so a real literal
// ("@followup", "@r5") survives the export→install round-trip without being resolved as a
// relation ref. Relation values are emitted as single-"@" refs and never pass through here.
// resolveRefs performs the inverse (a "@@"-prefixed value drops exactly one "@").
func escapeRefLiteral(v any) any {
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, "@") {
		return v
	}
	return "@" + s
}

// scopeRoleActions keeps only the grants that belong to the entities being exported.
//
// Security-critical (MỘT XƯỞNG RỔ-4 P0): Nexus builds many customers' apps inside ONE tenant, so
// exporting a role's FULL grant list ships other customers' access model into this customer's
// workspace. A wildcard grant ("*") is kept — it is meaningful for whatever schema is handed over.
// Pure function so the rule is testable without standing up the whole export service.
//
// The wildcard exemption is the one hole in this filter, and it is deliberate but NOT harmless: a
// role whose grants are ONLY wildcards always survives with len(Actions) > 0, so the droppedRoles
// guard never fires for it and no `skipped` warning is emitted — the operator sees a clean export.
// ACLService.Authorize resolves '*' as every entity, so such a role means "read everything in the
// destination workspace", including entities from unrelated later deliveries. Two things now stand
// between that and a customer: countWildcardActions makes the export say so out loud (caller
// below), and reconcileRoleGrants refuses to append a wildcard to a role the customer already has.
func scopeRoleActions(acts []auth.RoleAction, selected map[string]bool) (kept []PackRoleAction, skipped int) {
	kept = make([]PackRoleAction, 0, len(acts))
	for _, a := range acts {
		if a.EntityName != "*" && !selected[a.EntityName] {
			skipped++
			continue
		}
		kept = append(kept, PackRoleAction{
			EntityName:  a.EntityName,
			Action:      a.Action,
			AccessLevel: a.AccessLevel,
		})
	}
	return kept, skipped
}

// countWildcardActions reports how many of the kept grants are wildcards, so the export can name
// them instead of letting a "read everything" grant leave silently.
func countWildcardActions(acts []PackRoleAction) int {
	n := 0
	for _, a := range acts {
		if strings.TrimSpace(a.EntityName) == wildcardEntityName {
			n++
		}
	}
	return n
}

// stripWorkflowCredentials returns a copy of pw whose outbound_webhook actions carry no live
// credential — no HMAC signing Secret and no custom Headers — plus the number of actions it
// stripped. URL survives, so the rule's shape installs and the destination only has to put its own
// key in the vault and pick it (specs/050 S3a — a key on the rule itself is refused now).
//
// This is exportHooks' rule (see its doc below: "Dropping the secret is the whole point, not an
// omission") applied to the OUTBOUND direction, which had none of it. It is ONE function rather
// than a strip at each site because the credential was being persisted TWICE per install: once by
// toWorkflowInput → CreateWorkflow into the target tenant's sys_workflows.config (plaintext, no
// redaction on read — GET /admin/workflows returns it), and once by recordProvenance →
// provenanceItems into that tenant's sys_pack_items.content, which
// GET /admin/verticals/installs/:slug/diff reads back out. A different customer's admin could read
// Nexus's production Bearer token from either. Export, provenance and install therefore all call
// this, so a pack authored before this change — or one already sitting in a provenance row —
// cannot smuggle a credential through whichever path was missed.
//
// Headers go whole rather than by name allowlist: a bearer token is exactly as much a credential
// under "X-Company-Key" as under "Authorization", and nothing here can tell them apart.
func stripWorkflowCredentials(pw PackWorkflow) (PackWorkflow, int) {
	n := 0
	for _, a := range pw.Actions {
		if strings.TrimSpace(a.Type) == "outbound_webhook" && (a.Secret != "" || len(a.Headers) > 0) {
			n++
		}
	}
	if n == 0 {
		return pw, 0
	}
	// Copy before mutating: callers share the Actions backing array (provenance iterates the very
	// slice install is about to consume), so stripping in place would be a spooky side effect.
	acts := make([]PackWorkflowAction, len(pw.Actions))
	copy(acts, pw.Actions)
	for i := range acts {
		if strings.TrimSpace(acts[i].Type) != "outbound_webhook" {
			continue
		}
		acts[i].Secret = ""
		acts[i].Headers = nil
	}
	pw.Actions = acts
	return pw, n
}

// exportHooks serializes the tenant's inbound webhook hooks whose target entity is in the selected
// set into pack.InboundHooks — WITHOUT their secrets.
//
// Dropping the secret is the whole point, not an omission: it is a live signing credential, so an
// exported pack carrying it would hand whoever installs it a working key for THIS tenant's
// endpoint, and provenance would persist a second copy into sys_pack_items. Install mints a fresh
// one per tenant (Pass 8e), so a hook travels by regeneration.
//
// Two references decide whether a hook is portable, and they can disagree: EntityName (where each
// delivery lands) and PaymentMatch.TargetEntity (the record whose status flips). A hook whose
// LANDING entity is outside the selection is not exported at all — it would install pointing at a
// table the pack does not carry. A hook whose landing entity is in but whose payment TARGET is
// out is exported WITH a warning: the pack is still useful (deliveries are recorded), but the
// matching half will not resolve on install and would otherwise fail silently at the first
// transfer — exactly the class of quiet half-working this codebase keeps paying for.
func (s *Service) exportHooks(ctx context.Context, tenantID string, selectedSet map[string]bool, pack *Pack, warn func(string, ...any)) error {
	if s.hookWr == nil {
		return nil
	}
	hooks, err := s.hookWr.ListAdmin(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("vertical_pack.exportHooks: %w", err)
	}
	dropped := 0
	for _, h := range hooks {
		if !selectedSet[h.EntityName] {
			dropped++
			continue
		}
		ph := PackInboundHook{
			Name:       h.Name,
			EntityName: h.EntityName,
		}
		if h.MatchField != nil {
			ph.MatchField = *h.MatchField
		}
		for _, m := range h.Mapping {
			ph.Mapping = append(ph.Mapping, PackHookMapping{Source: m.Source, Field: m.Field})
		}
		if pm := h.PaymentMatch; pm != nil {
			if !selectedSet[pm.TargetEntity] {
				warn("Hook %q: bước đối soát trỏ tới %q không nằm trong các đối tượng được chọn — pack vẫn ghi nhận tín hiệu nhưng KHÔNG tự khớp; chọn thêm đối tượng đó hoặc dựng lại bước đối soát sau khi cài.", h.Name, pm.TargetEntity)
			}
			ph.PaymentMatch = &PackHookPaymentMatch{
				TargetEntity: pm.TargetEntity,
				RefField:     pm.RefField,
				RefPattern:   pm.RefPattern,
				MemoSource:   pm.MemoSource,
				StatusField:  pm.StatusField,
				StatusValue:  pm.StatusValue,
				AmountSource: pm.AmountSource,
				AmountField:  pm.AmountField,
			}
		}
		// IsActive travels as-is: a hook the admin deliberately paused should not come back live
		// at the customer, and an active one should not arrive silently switched off.
		active := h.IsActive
		ph.IsActive = &active
		pack.InboundHooks = append(pack.InboundHooks, ph)
	}
	if dropped > 0 {
		warn("Bỏ %d hook nhận tín hiệu vì đối tượng đích không nằm trong lựa chọn.", dropped)
	}
	// Stable order so two exports of the same tenant serialize identically.
	sort.SliceStable(pack.InboundHooks, func(i, j int) bool { return pack.InboundHooks[i].Name < pack.InboundHooks[j].Name })
	return nil
}

// exportViews serializes the SHARED saved views of the selected entities into pack.Views (queue
// #720). userID "" makes ListForUser return shared views only — a personal view belongs to a person
// of the source tenant and never travels. Silent when no ViewStore is wired (nothing was installable
// either).
func (s *Service) exportViews(ctx context.Context, tenantID string, selectedSet map[string]bool, pack *Pack) error {
	if s.viewWr == nil {
		return nil
	}
	names := make([]string, 0, len(selectedSet))
	for n, ok := range selectedSet {
		if ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, entity := range names {
		recs, err := s.viewWr.List(ctx, tenantID, "", entity)
		if err != nil {
			return fmt.Errorf("vertical_pack.Export: list views of %q: %w", entity, err)
		}
		for _, v := range recs {
			if !v.Shared {
				continue
			}
			pack.Views = append(pack.Views, PackView{EntityName: v.EntityName, Name: v.Name, Config: v.Config})
		}
	}
	sort.SliceStable(pack.Views, func(i, j int) bool {
		if pack.Views[i].EntityName != pack.Views[j].EntityName {
			return pack.Views[i].EntityName < pack.Views[j].EntityName
		}
		return pack.Views[i].Name < pack.Views[j].Name
	})
	return nil
}

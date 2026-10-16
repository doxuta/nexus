// Pack upgrade adapters (../specs/069-pack-apply-upgrade/plan.md §5). One adapter per item type
// writes through the admin service the Studio itself uses, so an upgrade can never skip a guard a
// hand-made save honours. "Ready" is a type assertion on the writer already wired into the service
// (D12): a build that lacks the update method simply offers "keep" for that type (ADAPTER_NOT_READY).
//
// The workflow (automation rule) adapter lives here; report / dashboard / layout are in
// upgrade_adapters_docs.go and the role family in upgrade_adapters_acl.go.
package vertical_pack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// workflowUpdater is the update half of the workflow admin service. The compile-time pin below
// breaks the build, not an upgrade at runtime, when the service changes a signature.
type workflowUpdater interface {
	GetWorkflow(ctx context.Context, tenantID, id string) (*workflow.WorkflowRecord, error)
	UpdateWorkflow(ctx context.Context, tenantID, id string, in services.UpdateWorkflowInput) (*workflow.WorkflowRecord, error)
	DeleteWorkflow(ctx context.Context, tenantID, id string) error
}

var _ workflowUpdater = (*services.WorkflowService)(nil)

// applyEnv is the per-run context handed to adapters. Name→id maps are built lazily, once.
type applyEnv struct {
	s         *Service
	tenantID  string
	userID    string
	slug      string
	toVersion string
	theirs    map[string]json.RawMessage // "type/key" → secret-stripped new-pack content

	teamIDs    map[string]string
	processIDs map[string]string

	// Set by bind (upgrade_adapters_acl.go): the plan rows and the effective decision per row, so a
	// role can be written as one unit; book / run are the bookkeeping and the bounded write contexts.
	plan     map[string]*UpgradeItem
	act      map[string]string
	book     context.Context
	run      context.Context
	roleIDs  map[string]string
	roleRuns map[string]*roleRun
	baseRows map[string]StoredPackItem

	// Schema adapters (upgrade_adapters_schema.go): per-table create result (nil error = created),
	// the tables created in this run in order, field-id cache, and the family members that
	// reached the pack's content (feeds the composed table baseline).
	entRuns  map[string]error
	created  []string
	fieldIDs map[string]map[string]string
	done     map[string]bool

	// notes: info-level caveats of a successful write ("type/key" → code, message), see note().
	notes map[string][2]string

	// draftRun: the run is on a delivered project (L7); items whose class is "draft" land in drafts.
	draftRun bool
}

func newApplyEnv(s *Service, tenantID, userID, slug, toVersion string, art *planArtifacts) *applyEnv {
	return &applyEnv{s: s, tenantID: tenantID, userID: userID, slug: slug, toVersion: toVersion, theirs: art.theirs}
}

// teamMap: team name → id (the install's teamNameToID, read from the live tenant).
func (e *applyEnv) teamMap(ctx context.Context) (map[string]string, error) {
	if e.teamIDs != nil {
		return e.teamIDs, nil
	}
	e.teamIDs = map[string]string{}
	if e.s.teamWr == nil {
		return e.teamIDs, nil
	}
	ts, err := e.s.teamWr.ListTeams(ctx, e.tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.applyEnv.teamMap: %w", err)
	}
	for _, t := range ts {
		e.teamIDs[strings.TrimSpace(t.Name)] = t.ID
	}
	return e.teamIDs, nil
}

// processMap: (entity, name) → approval process id, for start_approval actions.
func (e *applyEnv) processMap(ctx context.Context) (map[string]string, error) {
	if e.processIDs != nil {
		return e.processIDs, nil
	}
	e.processIDs = map[string]string{}
	if e.s.approvalWr == nil {
		return e.processIDs, nil
	}
	ps, err := e.s.approvalWr.ListProcesses(ctx, e.tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.applyEnv.processMap: %w", err)
	}
	for _, p := range ps {
		e.processIDs[approvalDupKey(strings.TrimSpace(p.EntityName), strings.TrimSpace(p.Name))] = p.ID
	}
	return e.processIDs, nil
}

// upgradeAdapter writes one item type. remove == nil ⇒ "delete" is never offered for the type.
type upgradeAdapter struct {
	ready  func(s *Service) bool
	write  func(ctx context.Context, e *applyEnv, it *UpgradeItem) (liveID string, err error)
	remove func(ctx context.Context, e *applyEnv, it *UpgradeItem) error
}

var upgradeAdapters = map[string]upgradeAdapter{
	ItemWorkflow: {
		ready: func(s *Service) bool { _, ok := s.workflowWr.(workflowUpdater); return ok },
		write: writeWorkflow,
		remove: func(ctx context.Context, e *applyEnv, it *UpgradeItem) error {
			u, _ := e.s.workflowWr.(workflowUpdater)
			if it.LiveID == "" {
				return invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết luật nào trong workspace ứng với mục này")
			}
			if err := u.DeleteWorkflow(ctx, e.tenantID, it.LiveID); err != nil {
				return fmt.Errorf("vertical_pack.upgrade.deleteWorkflow: %w", err)
			}
			return nil
		},
	},
}

// adapterReady is the planner's view: is there a wired adapter for the type?
func (s *Service) adapterReady(typ string) bool {
	ad, ok := upgradeAdapters[typ]
	return ok && ad.ready(s)
}

// writeWorkflow creates the rule (item absent in the tenant) or updates the live one in place
// (id from the baseline/probe, so a renamed rule still lands on the right row). Everything goes
// through toWorkflowInput + Create/UpdateWorkflow: the same validation as install and the screen.
// An update keeps what the pack does not carry: the cadence (intervalSeconds) of a scheduled rule.
// A signing key / headers on the live rule cannot be carried (the service refuses inline keys since
// specs/050 S3a); the plan flags that rule WEBHOOK_SECRET_NOT_CARRIED and the service's own refusal
// (WEBHOOK_LEGACY_SECRET_PENDING) is the item's error if the builder chooses Apply anyway.
func writeWorkflow(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	raw, ok := e.theirs[ItemWorkflow+"/"+it.ItemKey]
	if !ok {
		return "", invalid("UPGRADE_PACK_ITEM_MISSING", "Bản mới của gói không còn luật này")
	}
	var pw PackWorkflow
	if err := json.Unmarshal(raw, &pw); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.writeWorkflow: decode %q: %w", it.ItemKey, err)
	}
	teams, err := e.teamMap(ctx)
	if err != nil {
		return "", err
	}
	procs, err := e.processMap(ctx)
	if err != nil {
		return "", err
	}
	in, err := toWorkflowInput(pw, e.userID, teams, procs, nil)
	if err != nil {
		return "", err
	}
	u, _ := e.s.workflowWr.(workflowUpdater)
	if needsCreate(it) {
		rec, err := e.s.workflowWr.CreateWorkflow(ctx, e.tenantID, in)
		if err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.createWorkflow: %w", err)
		}
		if rec == nil {
			return "", nil
		}
		return rec.ID, nil
	}
	if it.LiveID == "" {
		return "", invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết luật nào trong workspace ứng với mục này")
	}
	cur, err := u.GetWorkflow(ctx, e.tenantID, it.LiveID)
	if err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.writeWorkflow: read live %q: %w", it.ItemKey, err)
	}
	upd := services.UpdateWorkflowInput{
		Name: in.Name, EntityName: in.EntityName, Trigger: in.Trigger, Match: in.Match, Rules: in.Rules,
		Timer: in.Timer, SaveOn: in.SaveOn, Actions: in.Actions, IsActive: in.IsActive,
		IntervalSeconds: cur.IntervalSeconds, ActorID: e.userID,
	}
	if _, err := u.UpdateWorkflow(ctx, e.tenantID, it.LiveID, upd); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.updateWorkflow: %w", err)
	}
	return it.LiveID, nil
}

// useMerged makes the merged document the "new pack content" of this one item for the duration of
// the write, and returns the func that puts the pack's own content back (idempotent). A field lives
// inside its table's content, so the table's field entry is swapped there too.
func (e *applyEnv) useMerged(it *UpgradeItem) (restore func(), err error) {
	if len(it.Merged) == 0 {
		return func() {}, invalid("UPGRADE_MERGE_UNAVAILABLE", "Mục này không có bản gộp")
	}
	key := it.ItemType + "/" + it.ItemKey
	oldItem := it.Theirs
	it.Theirs = it.Merged
	if it.ItemType != ItemField {
		old, had := e.theirs[key]
		e.theirs[key] = it.Merged
		return func() {
			it.Theirs = oldItem
			if had {
				e.theirs[key] = old
			}
		}, nil
	}
	tkey := ItemEntity + "/" + it.ParentKey
	raw, ok := e.theirs[tkey]
	if !ok {
		return func() { it.Theirs = oldItem }, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return func() { it.Theirs = oldItem }, fmt.Errorf("vertical_pack.useMerged: decode table %q: %w", it.ParentKey, err)
	}
	var fields []json.RawMessage
	if err := json.Unmarshal(m["fields"], &fields); err != nil {
		return func() { it.Theirs = oldItem }, fmt.Errorf("vertical_pack.useMerged: decode fields of %q: %w", it.ParentKey, err)
	}
	name := strings.TrimPrefix(it.ItemKey, it.ParentKey+".")
	for i, f := range fields {
		var probe struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(f, &probe) == nil && strings.TrimSpace(probe.Name) == name {
			fields[i] = it.Merged
		}
	}
	if m["fields"], err = json.Marshal(fields); err != nil {
		return func() { it.Theirs = oldItem }, fmt.Errorf("vertical_pack.useMerged: encode fields: %w", err)
	}
	merged, err := json.Marshal(m)
	if err != nil {
		return func() { it.Theirs = oldItem }, fmt.Errorf("vertical_pack.useMerged: encode table: %w", err)
	}
	e.theirs[tkey] = merged
	return func() {
		it.Theirs = oldItem
		e.theirs[tkey] = raw
	}, nil
}

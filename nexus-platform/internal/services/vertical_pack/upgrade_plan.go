// Pack upgrade, slice L1 (../specs/069-pack-apply-upgrade/plan.md §2-§4, §9): the READ-ONLY 3-way
// plan. For every item the pack tracks it compares three sides — the BASE (the content the
// install that last wrote the item recorded in sys_pack_items), THEIRS (the new pack, walked by the
// SAME provenanceItems the install uses) and LIVE (a re-export of the tenant plus existence probes)
// — with the same canonicalize + diff the "Đối chiếu khuôn" report already trusts, then labels the
// item (added / update_safe / kept_custom / conflict / …) and the actions the Studio may offer.
//
// L1 writes NOTHING: no provenance row, no run row, no lock. No upgrade adapter exists yet, so
// every status that would need an action offers only "keep" (reason ADAPTER_NOT_READY); later
// slices register adapters and widen the action set.
package vertical_pack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadiff"
	"nexus/platform/internal/metaversion"
)

// Upgrade item statuses (FR-012 table). Wire strings, shared with the FE.
const (
	StatusAdded                 = "added"
	StatusUpdateSafe            = "update_safe"
	StatusUnchanged             = "unchanged"
	StatusInSync                = "in_sync"
	StatusKeptCustom            = "kept_custom"
	StatusConflict              = "conflict"
	StatusConflictNoBase        = "conflict_no_base"
	StatusDeletedByBuilder      = "deleted_by_builder"
	StatusConflictDeleted       = "conflict_deleted"
	StatusRemovedInPack         = "removed_in_pack"
	StatusRemovedInPackModified = "removed_in_pack_modified"
	StatusProtected             = "protected"
	StatusUndiffable            = "undiffable"
	StatusAmbiguousName         = "ambiguous_name"
	StatusUnsupported           = "unsupported"
)

// Sub-item types produced by expanding a table into fields and a role into grants (D3).
const (
	ItemField     = "field"
	ItemRoleGrant = "role_grant"
)

// Actions and reasons. L1 only ever emits ActionKeep.
const (
	ActionKeep = "keep"

	ReasonAdapterNotReady  = "ADAPTER_NOT_READY"
	ReasonUndiffable       = "EXPORT_UNDIFFABLE"
	ReasonAmbiguousName    = "AMBIGUOUS_NAME"
	ReasonBaseExpectedOnly = "BASE_EXPECTED_ONLY"
	ReasonFieldTypeChange  = "FIELD_TYPE_CHANGE"
	ReasonFieldUnique      = "FIELD_UNIQUE_CHANGE"
	ReasonNNUpdate         = "NN_UPDATE_UNSUPPORTED"
	ReasonRoleWildcard     = "ROLE_WILDCARD_REFUSED"
	ReasonHardDeleteKind   = "HARD_DELETE_KIND"
	ReasonRoleReserved     = "ROLE_NAME_RESERVED"
)

// UpgradeWarning is a per-item caveat. Level "confirm" means the item is never pre-selected;
// "info" is display-only.
type UpgradeWarning struct {
	Code  string `json:"code"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

// UpgradeItem is one row of the plan. base/theirs/live are only present for items that need a
// decision, and are secret-stripped (FR-019).
type UpgradeItem struct {
	ItemType      string   `json:"itemType"`
	ItemKey       string   `json:"itemKey"`
	Label         string   `json:"label"`
	ParentKey     string   `json:"parentKey,omitempty"`
	Status        string   `json:"status"`
	Actions       []string `json:"actions"`
	DefaultAction string   `json:"defaultAction"`
	Adopt         bool     `json:"adopt,omitempty"`
	TheirsPaths   []string `json:"theirsPaths"`
	OursPaths     []string `json:"oursPaths"`
	Overlap       []string `json:"overlap"`
	// Merged / TheirsTaken (L6): the three-way merge of a both-sides-changed item and the paths that
	// came from the pack. Present only when ActionMerge is offered.
	Merged      json.RawMessage  `json:"merged,omitempty"`
	TheirsTaken []string         `json:"theirsTakenPaths,omitempty"`
	Base        json.RawMessage  `json:"base,omitempty"`
	Theirs      json.RawMessage  `json:"theirs,omitempty"`
	Live        json.RawMessage  `json:"live,omitempty"`
	LiveID      string           `json:"liveId,omitempty"`
	RenamedTo   string           `json:"renamedTo,omitempty"`
	SharedWith  []string         `json:"sharedWith"`
	Warnings    []UpgradeWarning `json:"warnings"`
	Reason      string           `json:"reason,omitempty"`
	// ApplyMode (L7, delivered projects only): draft | immediate_traced | immediate_untraced | immediate_hidden.
	ApplyMode string `json:"applyMode,omitempty"`

	hashes [3]string // sha256 of the canonical base/theirs/live — feeds planHash only
}

// UpgradePlan is the POST …/upgrade-plan payload.
type UpgradePlan struct {
	Slug         string         `json:"slug"`
	FromVersions []string       `json:"fromVersions"`
	ToVersion    string         `json:"toVersion"`
	Source       string         `json:"source"` // builtin | upload
	Untracked    bool           `json:"untracked"`
	Delivered    bool           `json:"delivered"`
	PlanHash     string         `json:"planHash"`
	Summary      map[string]int `json:"summary"`
	Warnings     []string       `json:"warnings"`
	Items        []UpgradeItem  `json:"items"`
}

// itemPathsCap bounds the paths echoed per item; overlap is computed on the full lists.
const itemPathsCap = 50

// liveState is what the live tenant says about one item.
type liveState int

const (
	liveHere       liveState = iota // present and readable
	liveAbsent                      // confirmed gone
	liveUnreadable                  // exists but the export cannot round-trip it
	liveAmbiguous                   // several live records carry the name and no id disambiguates
)

type liveRes struct {
	raw       json.RawMessage
	state     liveState
	id        string
	renamedTo string
	idCount   int
}

// existenceProbed are the types Export FILTERS (it cannot round-trip some live records), so an
// absent export entry is only "gone" when the authoritative list agrees (FR-014).
var existenceProbed = map[string]bool{
	ItemWorkflow: true, ItemReport: true, ItemDashboard: true, ItemTeam: true, ItemWorkQueue: true, ItemApproval: true,
	ItemInboundHook: true,
}

// createsWithID are the install passes whose create call returns an id, so a base row WITHOUT
// live_id for them means "the install skipped an existing item" — the base is only what the pack
// expected (FR-010a). layout / deferred_field / nn_relationship return none.
var createsWithID = map[string]bool{
	ItemEntity: true, ItemRole: true, ItemTeam: true, ItemDashboard: true, ItemWorkflow: true,
	ItemReport: true, ItemWorkQueue: true, ItemApproval: true, ItemInboundHook: true,
}

// softDeleteTypes: only these have a soft-delete admin path, so only they may ever offer "Xoá".
var softDeleteTypes = map[string]bool{
	ItemWorkflow: true, ItemReport: true, ItemDashboard: true, ItemWorkQueue: true, ItemApproval: true,
	// FR-022 exceptions: revoking a grant / lifting a field restriction is a config row, never data;
	// both are only ever offered, never pre-selected, and carry a confirm warning.
	ItemRoleGrant: true, ItemFieldACL: true,
}

// idIndex is the id side of the live probes: name→ids and id→name per item type.
type idIndex struct {
	ids   map[string]map[string][]string
	names map[string]map[string]string
	ok    map[string]bool // probe succeeded for the type
	// keyedHook marks live workflow ids whose outbound_webhook action still carries a signing key or
	// headers of its own (saved before specs/050 S3a) — a save cannot carry those over (FR-036).
	keyedHook map[string]bool
}

func newIDIndex() *idIndex {
	return &idIndex{ids: map[string]map[string][]string{}, names: map[string]map[string]string{}, ok: map[string]bool{}, keyedHook: map[string]bool{}}
}

func (x *idIndex) add(typ, name, id string) {
	name = strings.TrimSpace(name)
	if x.ids[typ] == nil {
		x.ids[typ] = map[string][]string{}
		x.names[typ] = map[string]string{}
	}
	x.ids[typ][name] = append(x.ids[typ][name], id)
	x.names[typ][id] = name
}

// buildIDIndex runs ONE list per probed type (never per item — hot-loop rule).
func (s *Service) buildIDIndex(ctx context.Context, tenantID string) *idIndex {
	x := newIDIndex()
	if s.workflowWr != nil {
		if rs, err := s.workflowWr.ListWorkflows(ctx, tenantID); err == nil {
			x.ok[ItemWorkflow] = true
			for _, r := range rs {
				x.add(ItemWorkflow, r.Name, r.ID)
				if hasKeyedWebhook(r.Actions) {
					x.keyedHook[r.ID] = true
				}
			}
		}
	}
	if s.reportWr != nil {
		if rs, err := s.reportWr.ListReports(ctx, tenantID); err == nil {
			x.ok[ItemReport] = true
			for _, r := range rs {
				x.add(ItemReport, r.Name, r.ID)
			}
		}
	}
	if s.dashboardWr != nil {
		if rs, err := s.dashboardWr.ListDashboards(ctx, tenantID); err == nil {
			x.ok[ItemDashboard] = true
			for _, r := range rs {
				x.add(ItemDashboard, r.Name, r.ID)
			}
		}
	}
	if s.workQueueWr != nil {
		if rs, err := s.workQueueWr.ListAdmin(ctx, tenantID); err == nil {
			x.ok[ItemWorkQueue] = true
			for _, r := range rs {
				x.add(ItemWorkQueue, r.Name, r.ID)
			}
		}
	}
	if s.approvalWr != nil {
		if rs, err := s.approvalWr.ListProcesses(ctx, tenantID); err == nil {
			x.ok[ItemApproval] = true
			for _, r := range rs {
				x.add(ItemApproval, strings.TrimSpace(r.EntityName)+":"+strings.TrimSpace(r.Name), r.ID)
			}
		}
	}
	if s.hookWr != nil {
		if rs, err := s.hookWr.ListAdmin(ctx, tenantID); err == nil {
			x.ok[ItemInboundHook] = true
			for _, r := range rs {
				x.add(ItemInboundHook, r.Name, r.ID)
			}
		}
	}
	if s.teamWr != nil {
		if rs, err := s.teamWr.ListTeams(ctx, tenantID); err == nil {
			x.ok[ItemTeam] = true
			for _, r := range rs {
				x.add(ItemTeam, r.Name, r.ID)
			}
		}
	}
	if s.roleRd != nil {
		if rs, err := s.roleRd.ListRoles(ctx, tenantID); err == nil {
			x.ok[ItemRole] = true
			for _, r := range rs {
				x.add(ItemRole, r.Name, r.ID)
			}
		}
	}
	if s.entityRd != nil {
		if rs, err := s.entityRd.ListEntities(tenantID); err == nil {
			x.ok[ItemEntity] = true
			for _, r := range rs {
				x.add(ItemEntity, r.Name, r.ID)
			}
		}
	}
	return x
}

// ── semver ──────────────────────────────────────────────────────────────────────────────────

// parseSemver reads MAJOR.MINOR.PATCH. // ponytail: three integers only — pre-release / build
// suffixes are ignored; upgrade to a semver lib if a pack ever ships "1.2.0-rc1" vs "1.2.0".
func parseSemver(v string) ([3]int, bool) {
	m := semverPattern.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func cmpSemver(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ── plan ────────────────────────────────────────────────────────────────────────────────────

// planCtx carries the per-call state of one PlanUpgrade.
type planCtx struct {
	ready   func(typ string) bool // upgrade adapter wired for the type (nil ⇒ none: keep-only plan)
	liveIdx map[string]json.RawMessage
	idx     *idIndex
	shared  map[string][]string // "type/key" → other slugs
	// hasRecords: does the table hold ≥ 1 record? nil ⇒ unknown, the data warnings stay silent.
	hasRecords func(entity string) bool
}

// hasKeyedWebhook: does a stored actions blob hold an outbound_webhook with its own key/headers?
func hasKeyedWebhook(actions json.RawMessage) bool {
	if !bytes.Contains(actions, []byte("outbound_webhook")) {
		return false
	}
	var acts []struct {
		Type    string            `json:"type"`
		Secret  string            `json:"secret"`
		Headers map[string]string `json:"headers"`
	}
	if json.Unmarshal(actions, &acts) != nil {
		return false
	}
	for _, a := range acts {
		if a.Type == "outbound_webhook" && (a.Secret != "" || len(a.Headers) > 0) {
			return true
		}
	}
	return false
}

// itemMeta is what the item builder needs beyond the three contents.
type itemMeta struct {
	protected        bool
	baseExpectedOnly bool
	liveID           string
	idCount          int
	renamedTo        string
	parentKey        string
	sharedWith       []string
	keyedHook        bool
}

// PlanUpgrade builds the read-only 3-way upgrade plan for one installed pack against `pack` (a
// builtin or an uploaded file, already decoded). source is "builtin" | "upload". It writes nothing.
func (s *Service) PlanUpgrade(ctx context.Context, tenantID, slug string, pack *Pack, source string) (*UpgradePlan, error) {
	plan, _, err := s.planUpgrade(ctx, tenantID, slug, pack, source)
	return plan, err
}

// planArtifacts is what ApplyUpgrade needs besides the plan: the secret-stripped new-pack content
// per item (the only thing ever written) — recomputed server-side, never taken from the caller.
type planArtifacts struct {
	theirs map[string]json.RawMessage
	idx    *idIndex
}

func (s *Service) planUpgrade(ctx context.Context, tenantID, slug string, pack *Pack, source string) (*UpgradePlan, *planArtifacts, error) {
	slug = strings.TrimSpace(slug)
	// FR-101: the plan reads the RUNNING configuration, never the builder's draft overlay.
	ctx = metaversion.WithLiveRead(ctx)
	if s.items == nil {
		return nil, nil, fmt.Errorf("vertical_pack.PlanUpgrade: provenance store not wired")
	}
	if pack == nil {
		return nil, nil, invalid("VERTICAL_PACK_REQUIRED", "Cần cung cấp `pack` (nội dung) hoặc `builtin` (tên pack có sẵn)")
	}
	if pack.Manifest == nil || strings.TrimSpace(pack.Manifest.Slug) == "" {
		return nil, nil, invalid("PACK_UPGRADE_NO_SLUG", "Gói không có manifest.slug — không biết nâng cấp cho gói nào")
	}
	if got := strings.TrimSpace(pack.Manifest.Slug); got != slug {
		return nil, nil, invalid("PACK_UPGRADE_SLUG_MISMATCH", "Mã gói trong tệp (%s) khác gói đang nâng cấp (%s)", got, slug)
	}
	toVersion := strings.TrimSpace(pack.Manifest.Version)
	toSem, ok := parseSemver(toVersion)
	if !ok {
		return nil, nil, invalid("VERTICAL_MANIFEST_INVALID_VERSION", "manifest.version %q không phải semver hợp lệ (định dạng MAJOR.MINOR.PATCH)", toVersion)
	}

	// Same shape gate install runs: an approval carrying graph/options/unknown keys would be
	// flattened by the decoder and the plan would claim "unchanged" about a file it did not read.
	if err := approvalShapeGate(pack.Approvals); err != nil {
		return nil, nil, err
	}

	rows, err := s.items.ListItems(ctx, tenantID, slug)
	if err != nil {
		return nil, nil, fmt.Errorf("vertical_pack.PlanUpgrade: list items: %w", err)
	}
	var ledger []InstallRecord
	if len(rows) == 0 && s.installs != nil {
		if ledger, err = s.installs.ListInstalls(ctx, tenantID); err != nil {
			return nil, nil, fmt.Errorf("vertical_pack.PlanUpgrade: list installs: %w", err)
		}
	}
	fromVersions, untracked, err := resolveVersions(slug, toVersion, toSem, rows, ledger)
	if err != nil {
		return nil, nil, err
	}

	liveIdx, err := s.liveIndex(ctx, tenantID)
	if err != nil {
		return nil, nil, err
	}
	in := planInput{
		tenantID: tenantID, slug: slug, toVersion: toVersion, source: source,
		fromVersions: fromVersions, untracked: untracked,
		theirs: s.packContentIndex(pack), live: liveIdx, idx: s.buildIDIndex(ctx, tenantID),
		shared: map[string][]string{}, base: map[string]StoredPackItem{}, ready: s.adapterReady,
	}
	in.hasRecords = s.recordProbe(ctx, tenantID)
	if s.delivery != nil {
		if in.delivered, err = s.delivery.Delivered(ctx, tenantID); err != nil {
			return nil, nil, fmt.Errorf("vertical_pack.PlanUpgrade: delivered check: %w", err)
		}
		if in.delivered {
			if in.drafts, err = s.loadDraftIndex(ctx, tenantID); err != nil {
				return nil, nil, err
			}
		}
	}
	dropUnresolvableFieldACLs(&in, pack)
	if w := s.packCalendarWarning(ctx, tenantID, pack); w != "" {
		in.warnings = append(in.warnings, w)
	}
	// FR-016: a type whose install writer is not wired is excluded from ALL three sides, so a
	// never-installed type cannot show up as "Gói đã bỏ".
	excluded := s.unwiredTypes()
	baseHadType := map[string]bool{}
	for _, r := range rows {
		if excluded[r.Type] {
			baseHadType[r.Type] = true
			continue
		}
		in.base[r.Type+"/"+r.Key] = r
	}
	for _, typ := range sortedKeys(excluded) {
		if baseHadType[typ] || packCarries(pack, typ) {
			in.warnings = append(in.warnings, fmt.Sprintf("Loại %q không được nối ở máy chủ này — bỏ qua khi nâng cấp", typ))
		}
	}
	if ol, ok := s.items.(otherSlugLister); ok {
		others, oerr := ol.ListItemKeysOtherSlugs(ctx, tenantID, slug)
		if oerr != nil {
			return nil, nil, fmt.Errorf("vertical_pack.PlanUpgrade: list other-slug items: %w", oerr)
		}
		for _, o := range others {
			k := o.Type + "/" + o.Key
			if !slices.Contains(in.shared[k], o.Slug) {
				in.shared[k] = append(in.shared[k], o.Slug)
			}
		}
	}
	plan, err := computePlan(in)
	if err != nil {
		return nil, nil, err
	}
	return plan, &planArtifacts{theirs: in.theirs, idx: in.idx}, nil
}

// resolveVersions derives the "from" versions and the downgrade guard (FR-003). Baseline = the
// per-item pack_version of the provenance rows; with no rows at all it falls back to the install
// ledger (untracked: installed before provenance existed); neither ⇒ ErrInstallNotFound.
func resolveVersions(slug, toVersion string, toSem [3]int, rows []StoredPackItem, ledger []InstallRecord) ([]string, bool, error) {
	versions := map[string]bool{}
	untracked := len(rows) == 0
	for _, r := range rows {
		versions[r.PackVersion] = true
	}
	if untracked {
		for _, r := range ledger {
			if strings.TrimSpace(r.Slug) == slug {
				versions[strings.TrimSpace(r.Version)] = true
			}
		}
		if len(versions) == 0 {
			return nil, true, ErrInstallNotFound
		}
	}
	from := make([]string, 0, len(versions))
	var maxSem [3]int
	haveMax := false
	for v := range versions {
		from = append(from, v)
		if sv, ok := parseSemver(v); ok && (!haveMax || cmpSemver(sv, maxSem) > 0) {
			maxSem, haveMax = sv, true
		}
	}
	sort.Slice(from, func(i, j int) bool {
		a, aok := parseSemver(from[i])
		b, bok := parseSemver(from[j])
		if aok && bok {
			return cmpSemver(a, b) < 0
		}
		return from[i] < from[j]
	})
	if haveMax && cmpSemver(toSem, maxSem) < 0 {
		return nil, untracked, invalid("PACK_UPGRADE_DOWNGRADE", "Phiên bản mới (%s) thấp hơn phiên bản đã cài (%d.%d.%d)", toVersion, maxSem[0], maxSem[1], maxSem[2])
	}
	return from, untracked, nil
}

// planInput is everything the pure 3-way core needs — gathered by PlanUpgrade (DB + export +
// probes), fabricated directly by unit tests.
type planInput struct {
	tenantID, slug, toVersion, source string
	fromVersions                      []string
	untracked                         bool
	base                              map[string]StoredPackItem  // "type/key" → baseline row
	theirs, live                      map[string]json.RawMessage // "type/key" → content
	idx                               *idIndex
	shared                            map[string][]string // "type/key" → other slugs
	warnings                          []string
	ready                             func(typ string) bool
	hasRecords                        func(entity string) bool
	delivered                         bool        // L7: the tenant is a delivered project
	drafts                            *draftIndex // L7: pending drafts (nil off a delivered project)
}

// computePlan is the pure core: classify every item of (base ∪ theirs) and hash the result.
func computePlan(in planInput) (*UpgradePlan, error) {
	if in.idx == nil {
		in.idx = newIDIndex()
	}
	c := &planCtx{liveIdx: in.live, idx: in.idx, shared: in.shared, ready: in.ready, hasRecords: in.hasRecords}

	// Key set = base ∪ theirs. live is only ever LOOKED UP, never walked: items the builder
	// added themselves are not part of the pack, never enter the plan or its hash (FR-010).
	keySet := map[string]bool{}
	for k := range in.base {
		keySet[k] = true
	}
	for k := range in.theirs {
		keySet[k] = true
	}
	var items []UpgradeItem
	for _, tk := range sortedKeys(keySet) {
		typ, key, _ := strings.Cut(tk, "/")
		b, hasB := in.base[tk]
		traw, hasT := in.theirs[tk]
		var bp *StoredPackItem
		if hasB {
			bp = &b
		}
		lv := c.resolveLive(typ, key, bp)
		meta := itemMeta{
			liveID: lv.id, idCount: lv.idCount, renamedTo: lv.renamedTo, sharedWith: c.shared[tk],
			protected:        hasB && b.Protected,
			baseExpectedOnly: hasB && b.LiveID == "" && createsWithID[typ],
			keyedHook:        typ == ItemWorkflow && lv.id != "" && c.idx.keyedHook[lv.id],
		}
		if meta.liveID == "" && hasB {
			meta.liveID = b.LiveID
		}
		if (typ == ItemEntity || typ == ItemRole) && hasT && lv.state == liveHere {
			if sub, ok := c.expand(typ, key, bp, traw, lv, meta); ok {
				items = append(items, sub...)
				continue
			}
		}
		items = append(items, c.item(typ, key, bp != nil, rawOf(bp), traw, hasT, lv.raw, lv.state, meta))
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].ItemType != items[j].ItemType {
			return items[i].ItemType < items[j].ItemType
		}
		return items[i].ItemKey < items[j].ItemKey
	})

	if in.delivered {
		overlayDelivered(items, in.drafts)
	}

	summary := map[string]int{"writes": 0, "adoptable": 0}
	for _, it := range items {
		summary[it.Status]++
		if it.DefaultAction == "apply" || it.DefaultAction == "restore" || it.DefaultAction == "delete" {
			summary["writes"]++
		}
		if it.Adopt {
			summary["adoptable"]++
		}
	}
	plan := &UpgradePlan{
		Slug: in.slug, FromVersions: in.fromVersions, ToVersion: in.toVersion, Source: in.source, Untracked: in.untracked, Delivered: in.delivered,
		Summary: summary, Warnings: in.warnings, Items: items,
	}
	if plan.FromVersions == nil {
		plan.FromVersions = []string{}
	}
	if plan.Warnings == nil {
		plan.Warnings = []string{}
	}
	h, err := planHash(in.tenantID, plan)
	if err != nil {
		return nil, err
	}
	plan.PlanHash = h
	return plan, nil
}

// recordProbe answers "does this table hold at least one record?" with a LIMIT 1 read through the
// record port (admin scope, tenant-scoped), memoised per plan. When it cannot tell (no reader, read
// error) it says yes: a warning that fires too often is safe, one that stays silent is not.
func (s *Service) recordProbe(ctx context.Context, tenantID string) func(string) bool {
	memo := map[string]bool{}
	return func(entity string) bool {
		if v, ok := memo[entity]; ok {
			return v
		}
		has := true
		if s.recordRd != nil {
			scope := dynrepo.RowScope{Level: dynrepo.AccessAll, IsAdmin: true}
			_, total, err := s.recordRd.ListRecords(ctx, entity, tenantID, scope, dynrepo.ListParams{Page: 1, Limit: 1, Sort: "created_at", Order: "asc"})
			if err != nil {
				s.logger.Warn("vertical_pack.recordProbe: cannot count records — assuming some", zap.String("tenant_id", tenantID), zap.String("entity_name", entity), zap.Error(err))
			} else {
				has = total > 0
			}
		}
		memo[entity] = has
		return has
	}
}

// liveIndex re-exports the tenant (no samples) and indexes it exactly like the pack side.
func (s *Service) liveIndex(ctx context.Context, tenantID string) (map[string]json.RawMessage, error) {
	live, _, err := s.Export(ctx, tenantID, nil, false, nil)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.PlanUpgrade: export live state: %w", err)
	}
	return s.packContentIndex(live), nil
}

// unwiredTypes lists provenance types whose install writer is not wired on this service.
func (s *Service) unwiredTypes() map[string]bool {
	has := s.writerAvailability()
	out := map[string]bool{}
	if !has.Workflows {
		out[ItemWorkflow] = true
	}
	if !has.Reports {
		out[ItemReport] = true
	}
	if !has.Dashboards {
		out[ItemDashboard] = true
	}
	if !has.Teams {
		out[ItemTeam] = true
	}
	if !has.WorkQueues {
		out[ItemWorkQueue] = true
	}
	if !has.Approvals {
		out[ItemApproval] = true
	}
	if !has.FieldACLs {
		out[ItemFieldACL] = true
	}
	if !has.InboundHooks {
		out[ItemInboundHook] = true
	}
	return out
}

func packCarries(p *Pack, typ string) bool {
	switch typ {
	case ItemWorkflow:
		return len(p.Workflows) > 0
	case ItemReport:
		return len(p.Reports) > 0
	case ItemDashboard:
		return len(p.Dashboards) > 0
	case ItemTeam:
		return len(p.Teams) > 0
	case ItemWorkQueue:
		return len(p.WorkQueues) > 0
	case ItemApproval:
		return len(p.Approvals) > 0
	case ItemFieldACL:
		return len(p.FieldACLs) > 0
	case ItemInboundHook:
		return len(p.InboundHooks) > 0
	}
	return false
}

// teamLive is the live content of a team that exists (nil for any other type).
func teamLive(typ, name string) json.RawMessage {
	if typ != ItemTeam {
		return nil
	}
	raw, err := json.Marshal(PackTeam{Name: name})
	if err != nil {
		return nil
	}
	return raw
}

func rawOf(b *StoredPackItem) json.RawMessage {
	if b == nil {
		return nil
	}
	return b.Content
}

// resolveLive answers "what does the tenant hold for this item" (plan.md §2 resolution order).
func (c *planCtx) resolveLive(typ, key string, b *StoredPackItem) liveRes {
	if b != nil && b.LiveID != "" {
		if n, ok := c.idx.names[typ][b.LiveID]; ok {
			res := liveRes{id: b.LiveID, idCount: 1}
			if n != key {
				res.renamedTo = n
			}
			if raw, in := c.liveIdx[typ+"/"+n]; in {
				res.raw, res.state = raw, liveHere
			} else if raw := teamLive(typ, n); raw != nil {
				res.raw, res.state = raw, liveHere
			} else {
				res.state = liveUnreadable
			}
			return res
		}
	}
	ids := c.idx.ids[typ][key]
	if len(ids) > 1 {
		return liveRes{state: liveAmbiguous, idCount: len(ids)}
	}
	res := liveRes{idCount: len(ids)}
	if len(ids) == 1 {
		res.id = ids[0]
	}
	if raw, in := c.liveIdx[typ+"/"+key]; in {
		res.raw, res.state = raw, liveHere
		return res
	}
	// A team is nothing but its name, and Export only emits the teams a workflow references: the
	// id index already proves the live team, so its content is read off the name.
	if len(ids) == 1 {
		if raw := teamLive(typ, key); raw != nil {
			res.raw, res.state = raw, liveHere
			return res
		}
	}
	// Absent from the export — Export is fidelity-FILTERED, so absence alone proves nothing for
	// the probed types: consult the id index (the same lists classifyAbsent walks, fetched once).
	switch {
	case existenceProbed[typ] && !c.idx.ok[typ]:
		res.state = liveUnreadable
	case existenceProbed[typ] && len(ids) > 0:
		res.state = liveUnreadable
	default:
		res.state = liveAbsent
	}
	return res
}

// expand splits a table into entity attrs + fields, a role into role attrs + grants (D3), so a
// decision is made per field/grant instead of per whole table. ok=false ⇒ caller falls back to a
// whole item (a side failed to decode).
func (c *planCtx) expand(typ, key string, b *StoredPackItem, theirs json.RawMessage, lv liveRes, meta itemMeta) ([]UpgradeItem, bool) {
	tParent, tKids, err := splitParent(typ, key, theirs)
	if err != nil {
		return nil, false
	}
	lParent, lKids, err := splitParent(typ, key, lv.raw)
	if err != nil {
		return nil, false
	}
	var bParent json.RawMessage
	var bKids map[string]json.RawMessage
	hasB := b != nil
	if hasB {
		if bParent, bKids, err = splitParent(typ, key, b.Content); err != nil {
			return nil, false
		}
	}
	out := []UpgradeItem{c.item(typ, key, hasB, bParent, tParent, true, lParent, liveHere, meta)}

	kidType := ItemField
	if typ == ItemRole {
		kidType = ItemRoleGrant
	}
	kidKeys := map[string]bool{}
	for k := range bKids {
		kidKeys[k] = true
	}
	for k := range tKids {
		kidKeys[k] = true
	}
	kmeta := itemMeta{baseExpectedOnly: meta.baseExpectedOnly, parentKey: key, sharedWith: meta.sharedWith}
	for _, k := range sortedKeys(kidKeys) {
		bk, hasBK := bKids[k]
		tk, hasTK := tKids[k]
		lk, hasLK := lKids[k]
		st := liveAbsent
		if hasLK {
			st = liveHere
		}
		out = append(out, c.item(kidType, k, hasBK, bk, tk, hasTK, lk, st, kmeta))
	}
	return out, true
}

// splitParent separates the whole-table/role JSON into the parent attributes and a map of child
// key → child JSON. Fields key as <table>.<field>, grants as <role>/<table>/<action>.
func splitParent(typ, key string, raw json.RawMessage) (json.RawMessage, map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("vertical_pack.splitParent: %w", err)
	}
	listKey := "fields"
	if typ == ItemRole {
		listKey = "actions"
	}
	list := m[listKey]
	delete(m, listKey)
	var elems []json.RawMessage
	if len(list) > 0 && string(list) != "null" {
		if err := json.Unmarshal(list, &elems); err != nil {
			return nil, nil, fmt.Errorf("vertical_pack.splitParent: %s: %w", listKey, err)
		}
	}
	kids := make(map[string]json.RawMessage, len(elems))
	for _, e := range elems {
		var probe struct {
			Name       string `json:"name"`
			EntityName string `json:"entityName"`
			Action     string `json:"action"`
		}
		if err := json.Unmarshal(e, &probe); err != nil {
			return nil, nil, fmt.Errorf("vertical_pack.splitParent: element: %w", err)
		}
		if typ == ItemRole {
			kids[key+"/"+strings.TrimSpace(probe.EntityName)+"/"+strings.TrimSpace(probe.Action)] = e
		} else {
			kids[key+"."+strings.TrimSpace(probe.Name)] = e
		}
	}
	parent, err := json.Marshal(m)
	if err != nil {
		return nil, nil, fmt.Errorf("vertical_pack.splitParent: marshal parent: %w", err)
	}
	return parent, kids, nil
}

// item classifies ONE item from its three sides and assembles the plan row.
func (c *planCtx) item(typ, key string, hasB bool, braw json.RawMessage, traw json.RawMessage, hasT bool, lraw json.RawMessage, ls liveState, meta itemMeta) UpgradeItem {
	it := UpgradeItem{
		ItemType: typ, ItemKey: key, Label: key, ParentKey: meta.parentKey,
		TheirsPaths: []string{}, OursPaths: []string{}, Overlap: []string{},
		SharedWith: []string{}, Warnings: []UpgradeWarning{}, LiveID: meta.liveID, RenamedTo: meta.renamedTo,
	}
	if len(meta.sharedWith) > 0 {
		it.SharedWith = append(it.SharedWith, meta.sharedWith...)
		it.Warnings = append(it.Warnings, UpgradeWarning{
			Code: "SHARED_WITH_PACK", Level: "info",
			Text: "Dùng chung với gói " + strings.Join(meta.sharedWith, ", "),
		})
	}
	if meta.keyedHook && hasT && bytes.Contains(traw, []byte("outbound_webhook")) {
		it.Warnings = append(it.Warnings, UpgradeWarning{
			Code: "WEBHOOK_SECRET_NOT_CARRIED", Level: "confirm",
			Text: "Luật này còn khoá ký/header riêng cho webhook ra ngoài — lưu lại không chép được khoá. Dời khoá vào Kho khoá kết nối (Bác sĩ hệ thống) trước khi áp.",
		})
	}
	if ls == liveAmbiguous {
		return c.finish(it, StatusAmbiguousName, ReasonAmbiguousName, nil)
	}

	// Decode + canonicalize each present side once (defence in depth: strip webhook credentials
	// from every side before anything can reach a response, a hash or a log).
	var cb, ct, cl any
	var err error
	if hasB {
		braw = redactItem(typ, braw)
		cb, err = decodeCanonical(typ, braw)
	}
	if err == nil && hasT {
		traw = redactItem(typ, traw)
		ct, err = decodeCanonical(typ, traw)
	}
	if err == nil && ls == liveHere {
		lraw = redactItem(typ, lraw)
		cl, err = decodeCanonical(typ, lraw)
	}
	if err != nil || ls == liveUnreadable {
		return c.finish(it, StatusUndiffable, ReasonUndiffable, nil)
	}
	liveHereOK := ls == liveHere
	eq := func(a, b any) bool { return len(diffPaths(a, b)) == 0 }
	var eqBT, eqBL, eqTL bool
	if hasB && hasT {
		eqBT = eq(cb, ct)
	}
	if hasB && liveHereOK {
		eqBL = eq(cb, cl)
	}
	if hasT && liveHereOK {
		eqTL = eq(ct, cl)
	}

	status := classifyThreeWay(hasB, hasT, liveHereOK, eqBT, eqBL, eqTL)
	reason := ""
	if meta.baseExpectedOnly && (status == StatusKeptCustom || status == StatusConflict) {
		status, reason = StatusConflictNoBase, ReasonBaseExpectedOnly
	}
	if r := unsupportedReason(typ, status, ct, cl, hasT, liveHereOK); r != "" {
		status, reason = StatusUnsupported, r
	}
	if meta.protected && protectable[status] {
		status = StatusProtected
	}
	// The customer-owner role is out of reach of every upgrade, whatever the 3-way says (plan.md §3,
	// last override — it wins even over unchanged/in_sync, so it can never be adopted either).
	if reservedRoleItem(typ, key) {
		status, reason = StatusProtected, ReasonRoleReserved
	}
	if typ == ItemInboundHook && hasT && !liveHereOK && (status == StatusAdded || status == StatusDeletedByBuilder || status == StatusConflictDeleted) {
		it.Warnings = append(it.Warnings, UpgradeWarning{Code: "NEW_HOOK_SECRET", Level: "info",
			Text: "Webhook nhận này sẽ tạo với khoá ký MỚI — lấy khoá trong Cài đặt rồi mới báo đối tác gửi"})
	}
	it.Warnings = append(it.Warnings, aclWarnings(typ, hasT, liveHereOK, ct, cl)...)
	it.Warnings = append(it.Warnings, c.schemaWarnings(typ, status, hasT, ct, cl, meta.parentKey)...)

	// Paths (full lists for overlap, capped copies on the wire).
	var tp, op []string
	switch {
	case hasB && hasT:
		tp = diffPaths(cb, ct)
	case hasT && liveHereOK:
		tp = diffPaths(cl, ct) // no base: show where the pack and the tenant differ
	}
	if hasB && liveHereOK {
		op = diffPaths(cb, cl)
	}
	it.TheirsPaths, it.OursPaths = capPaths(tp), capPaths(op)
	it.Overlap = capPaths(overlapPaths(op, tp))
	if status == StatusConflict && reason == "" && mergeTypes[typ] && hasB && hasT && liveHereOK {
		it.Merged, it.TheirsTaken = mergeItem(typ, cb, cl, ct)
	}

	// Adoption: only when the baseline row would actually change content (or gain its live id).
	if status == StatusInSync || status == StatusUnchanged {
		switch {
		case !hasB:
			it.Adopt = true
		case !hasT:
			it.Adopt = true
		case !eqBT:
			it.Adopt = true
		case meta.baseExpectedOnly && meta.parentKey == "" && meta.idCount == 1:
			it.Adopt = true
		}
	}

	if status == StatusRemovedInPack || status == StatusRemovedInPackModified {
		if !softDeleteTypes[typ] && reason == "" {
			reason = ReasonHardDeleteKind
		}
	}
	needsAction := status != StatusUnchanged && status != StatusInSync
	if needsAction {
		it.Base, it.Theirs, it.Live = rawIf(hasB, braw), rawIf(hasT, traw), rawIf(liveHereOK, lraw)
	}
	var cbHash, ctHash, clHash string
	if hasB {
		cbHash = hashCanon(cb)
	}
	if hasT {
		ctHash = hashCanon(ct)
	}
	if liveHereOK {
		clHash = hashCanon(cl)
	}
	return c.finish(it, status, reason, &[3]string{cbHash, ctHash, clHash})
}

// protectable are the statuses where a "Luôn giữ" flag turns the row into `protected`.
var protectable = map[string]bool{
	StatusUpdateSafe: true, StatusConflict: true, StatusConflictNoBase: true, StatusKeptCustom: true, StatusConflictDeleted: true,
}

// finish fills status, the offered actions and the pre-selected one (plan.md §4, FR-012/020/021).
// A status that needs a decision offers more than "keep" only when the type's adapter is wired.
// hashes feeds planHash only.
func (c *planCtx) finish(it UpgradeItem, status, reason string, hashes *[3]string) UpgradeItem {
	it.Status = status
	if status == StatusUnchanged || status == StatusInSync {
		it.Actions, it.DefaultAction = []string{}, "none"
	} else {
		write, canRemove := c.writable(it.ItemType)
		acts := []string{ActionKeep}
		if write && (reason == "" || reason == ReasonBaseExpectedOnly) {
			switch status {
			case StatusAdded, StatusUpdateSafe, StatusConflict, StatusConflictNoBase, StatusProtected:
				acts = []string{ActionApply, ActionKeep}
			case StatusKeptCustom, StatusDeletedByBuilder, StatusConflictDeleted:
				acts = []string{ActionKeep, ActionRestore}
			case StatusRemovedInPack, StatusRemovedInPackModified:
				if canRemove {
					acts = []string{ActionKeep, ActionDelete}
				}
			}
		}
		if reason == "" && len(acts) == 1 {
			reason = ReasonAdapterNotReady
		}
		// "Luôn giữ" (FR-024): only an item with its own baseline row can carry the flag, and only
		// where the plan would otherwise offer to overwrite it (adapter wired); never a table's
		// field or a role's grant.
		// "Gộp" (L6): both sides changed, at places that do not collide; never the default.
		if write && status == StatusConflict && len(it.Merged) > 0 && (reason == "" || reason == ReasonBaseExpectedOnly) {
			acts = append(acts, ActionMerge)
		}
		if write && protectable[status] && keepAlwaysTypes(it.ItemType) && (reason == "" || reason == ReasonBaseExpectedOnly) {
			acts = append(acts, ActionKeepAlways)
		}
		def := ActionKeep
		if (status == StatusAdded || status == StatusUpdateSafe) && acts[0] == ActionApply && !hasConfirm(it.Warnings) {
			def = ActionApply
		}
		it.Actions, it.DefaultAction, it.Reason = acts, def, reason
	}
	if hashes != nil {
		it.hashes = *hashes
	}
	return it
}

// mergeTypes are the item types whose adapter writes the new-pack content (env.theirs / it.Theirs),
// so a merged document can stand in for it. role / entity / team families are not offered yet.
var mergeTypes = map[string]bool{
	ItemWorkflow: true, ItemReport: true, ItemDashboard: true, ItemLayout: true, ItemWorkQueue: true,
	ItemApproval: true, ItemField: true,
}

// mergeItem runs the three-way merge on the canonical sides; nil results ⇒ no merge offered.
func mergeItem(typ string, base, ours, theirs any) (json.RawMessage, []string) {
	m, taken, ok := metadiff.Merge3(typ, base, ours, theirs)
	if !ok {
		return nil, nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, nil
	}
	return raw, capPaths(taken)
}

// keepAlwaysTypes: the item types that own a sys_pack_items row (SetProtected refuses the rest).
// role is excluded too: its apply path neither clears the flag nor re-creates a protected-but-missing role
// (spec 069 section 21 debt) - offer it once ensureRole/saveRoleBaseline own the flag lifecycle.
func keepAlwaysTypes(typ string) bool {
	return typ != ItemField && typ != ItemRoleGrant && typ != ItemRole
}

// writable: can this item type be written (and soft-deleted) by an upgrade adapter right now?
func (c *planCtx) writable(typ string) (write, remove bool) {
	if c.ready == nil || !c.ready(typ) {
		return false, false
	}
	return true, upgradeAdapters[typ].remove != nil
}

func hasConfirm(ws []UpgradeWarning) bool {
	for _, w := range ws {
		if w.Level == "confirm" {
			return true
		}
	}
	return false
}

// classifyThreeWay is the plan.md §3 table. live: present+readable. L? and ambiguity are handled
// by the caller before this point.
func classifyThreeWay(hasB, hasT, live, eqBT, eqBL, eqTL bool) string {
	switch {
	case !hasB && hasT:
		if !live {
			return StatusAdded
		}
		if eqTL {
			return StatusInSync
		}
		return StatusConflictNoBase
	case hasB && hasT:
		if live {
			switch {
			case eqBT && eqBL:
				return StatusUnchanged
			case eqBL:
				return StatusUpdateSafe
			case eqBT:
				return StatusKeptCustom
			case eqTL:
				return StatusInSync
			default:
				return StatusConflict
			}
		}
		if eqBT {
			return StatusDeletedByBuilder
		}
		return StatusConflictDeleted
	default: // hasB && !hasT
		if !live {
			return StatusInSync // both sides dropped it
		}
		if eqBL {
			return StatusRemovedInPack
		}
		return StatusRemovedInPackModified
	}
}

// unsupportedReason names why an item the pack changed can never be applied automatically.
func unsupportedReason(typ, status string, theirs, live any, hasT, liveOK bool) string {
	actionable := status == StatusUpdateSafe || status == StatusConflict || status == StatusConflictNoBase || status == StatusKeptCustom || status == StatusAdded
	if !actionable {
		return ""
	}
	switch typ {
	case ItemField:
		tm, _ := theirs.(map[string]any)
		lm, _ := live.(map[string]any)
		if hasT && liveOK && tm != nil && lm != nil {
			if tm["fieldType"] != lm["fieldType"] {
				return ReasonFieldTypeChange
			}
			tu, _ := tm["isUnique"].(bool)
			lu, _ := lm["isUnique"].(bool)
			if tu != lu {
				return ReasonFieldUnique
			}
		}
	case ItemNN:
		if status != StatusAdded {
			return ReasonNNUpdate
		}
	case ItemRoleGrant:
		if tm, _ := theirs.(map[string]any); tm != nil && tm["entityName"] == "*" {
			return ReasonRoleWildcard
		}
	}
	return ""
}

// redactItem strips outbound-webhook credentials from a workflow's JSON (no-op for other types
// and for workflows that carry none). Provenance + export already strip; this is the last line.
func redactItem(typ string, raw json.RawMessage) json.RawMessage {
	if typ != ItemWorkflow || !bytes.Contains(raw, []byte("outbound_webhook")) {
		return raw
	}
	var pw PackWorkflow
	if err := json.Unmarshal(raw, &pw); err != nil {
		return nil // fail closed: never emit a payload we could not scrub
	}
	sw, n := stripWorkflowCredentials(pw)
	if n == 0 {
		return raw
	}
	out, err := json.Marshal(sw)
	if err != nil {
		return nil // fail closed
	}
	return out
}

func rawIf(ok bool, r json.RawMessage) json.RawMessage {
	if !ok {
		return nil
	}
	return r
}

func hashCanon(v any) string {
	b, _ := json.Marshal(v) // map[string]any of decoded JSON: cannot fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func capPaths(p []string) []string {
	if p == nil {
		return []string{}
	}
	if len(p) > itemPathsCap {
		return p[:itemPathsCap]
	}
	return p
}

// overlapPaths returns the paths where a builder edit and a pack edit touch the same spot: one
// path is the other (or an ancestor of it on a . or [ boundary). The shorter path is reported.
func overlapPaths(ours, theirs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range ours {
		for _, t := range theirs {
			short := ""
			switch {
			case pathWithin(t, o):
				short = o
			case pathWithin(o, t):
				short = t
			}
			if short != "" && !seen[short] {
				seen[short] = true
				out = append(out, short)
			}
		}
	}
	sort.Strings(out)
	return out
}

// pathWithin: child == parent, or child starts with parent followed by '.' or '['.
func pathWithin(child, parent string) bool {
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+".") || strings.HasPrefix(child, parent+"[")
}

// planHash is sha256 over the canonical JSON of everything a decision depends on (plan.md §9):
// any change on any side of any item, or in the offered actions / default / confirm warnings,
// yields a different hash. Items are sorted; no timestamps, no map iteration order.
func planHash(tenantID string, p *UpgradePlan) (string, error) {
	type hashItem struct {
		T  string   `json:"t"`
		K  string   `json:"k"`
		S  string   `json:"s"`
		A  []string `json:"a"`
		D  string   `json:"d"`
		C  []string `json:"c"`
		Ad bool     `json:"ad"`
		HB string   `json:"hb"`
		HT string   `json:"ht"`
		HL string   `json:"hl"`
	}
	hi := make([]hashItem, 0, len(p.Items))
	for _, it := range p.Items {
		var confirms []string
		for _, w := range it.Warnings {
			if w.Level == "confirm" {
				confirms = append(confirms, w.Code)
			}
		}
		sort.Strings(confirms)
		hi = append(hi, hashItem{it.ItemType, it.ItemKey, it.Status, it.Actions, it.DefaultAction, confirms, it.Adopt, it.hashes[0], it.hashes[1], it.hashes[2]})
	}
	sort.Slice(hi, func(i, j int) bool {
		if hi[i].T != hi[j].T {
			return hi[i].T < hi[j].T
		}
		return hi[i].K < hi[j].K
	})
	b, err := json.Marshal(struct {
		Slug    string     `json:"slug"`
		Version string     `json:"version"`
		Tenant  string     `json:"tenant"`
		Items   []hashItem `json:"items"`
	}{p.Slug, p.ToVersion, tenantID, hi})
	if err != nil {
		return "", fmt.Errorf("vertical_pack.planHash: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reservedRoleItem: is this role / grant / field restriction the customer-owner role's? The key
// shapes are role "<r>", grant "<r>/<table>/<action>", restriction "<r>:<table>.<field>".
func reservedRoleItem(typ, key string) bool {
	switch typ {
	case ItemRole:
		return key == auth.OwnerRoleName
	case ItemRoleGrant:
		return strings.HasPrefix(key, auth.OwnerRoleName+"/")
	case ItemFieldACL:
		return strings.HasPrefix(key, auth.OwnerRoleName+":")
	}
	return false
}

// accessRank orders row scopes by reach. field:<user field> reaches like "owner" (records tied to
// the user), the narrowest non-empty scope.
func accessRank(level string) int {
	switch level {
	case "none":
		return 0
	case "owner":
		return 1
	case "team":
		return 2
	case "all", "":
		return 3
	}
	return 1
}

// aclWarnings flags the two steps that WIDEN or NARROW access: a grant the pack would lower or drop
// (GRANT_NARROWING) and a field restriction the pack would lift (FIELD_ACL_OPENING). Both are
// level "confirm" — never pre-selected (plan.md §4).
func aclWarnings(typ string, hasT, liveOK bool, theirs, live any) []UpgradeWarning {
	if !liveOK {
		return nil
	}
	tm, _ := theirs.(map[string]any)
	lm, _ := live.(map[string]any)
	switch typ {
	case ItemRoleGrant:
		ll, _ := lm["accessLevel"].(string)
		tl, _ := tm["accessLevel"].(string)
		if !hasT || accessRank(tl) < accessRank(ll) {
			return []UpgradeWarning{{Code: "GRANT_NARROWING", Level: "confirm",
				Text: "Áp bước này sẽ thu hẹp quyền đang chạy của vai — nhân viên có thể mất truy cập."}}
		}
	case ItemFieldACL:
		lv, _ := lm["canView"].(bool)
		le, _ := lm["canEdit"].(bool)
		tv, _ := tm["canView"].(bool)
		te, _ := tm["canEdit"].(bool)
		if !hasT || (tv && !lv) || (te && !le) {
			return []UpgradeWarning{{Code: "FIELD_ACL_OPENING", Level: "confirm",
				Text: "Áp bước này sẽ mở lại một trường đang bị ẩn hoặc khoá với vai — nhân viên sẽ thấy hoặc sửa được."}}
		}
	}
	return nil
}

// schemaWarnings: the two confirm-level cautions of a field write (plan.md §4). Both need a LIVE
// table: kids only exist for a table the tenant already has (c.expand), so `parent` is that table.
func (c *planCtx) schemaWarnings(typ, status string, hasT bool, theirs, live any, parent string) []UpgradeWarning {
	if typ != ItemField || !hasT || parent == "" || status == StatusUnchanged || status == StatusInSync || status == StatusUnsupported {
		return nil
	}
	tm, _ := theirs.(map[string]any)
	lm, _ := live.(map[string]any)
	var out []UpgradeWarning
	if ft, _ := tm["fieldType"].(string); status == StatusAdded && (ft == "relation" || ft == "lookup") {
		out = append(out, UpgradeWarning{Code: "NEW_RELATION_ON_EXISTING_TABLE", Level: "confirm",
			Text: "Trường quan hệ/tra cứu mới trên bảng đang có dữ liệu — hồ sơ cũ sẽ để trống ở trường này."})
	}
	req, _ := tm["isRequired"].(bool)
	wasReq, _ := lm["isRequired"].(bool)
	if req && !wasReq && c.hasRecords != nil && c.hasRecords(parent) {
		out = append(out, UpgradeWarning{Code: "REQUIRED_ON_EXISTING_DATA", Level: "confirm",
			Text: "Bảng đã có hồ sơ — bật 'bắt buộc' khiến hồ sơ cũ để trống trường này không lưu lại được cho tới khi điền."})
	}
	return out
}

// dropUnresolvableFieldACLs removes, from the new-pack side, restrictions naming a role that exists
// neither in the tenant nor in the pack: install skips them too, so they would read "added" forever.
func dropUnresolvableFieldACLs(in *planInput, pack *Pack) {
	inPack := map[string]bool{}
	for _, r := range pack.Roles {
		inPack[strings.TrimSpace(r.Name)] = true
	}
	for _, fa := range pack.FieldACLs {
		rn := strings.TrimSpace(fa.RoleName)
		if inPack[rn] || len(in.idx.ids[ItemRole][rn]) > 0 {
			continue
		}
		delete(in.theirs, ItemFieldACL+"/"+fieldACLKey(fa))
	}
}

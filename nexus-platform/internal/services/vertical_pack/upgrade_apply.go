// Pack upgrade, slice L2a (../specs/069-pack-apply-upgrade/plan.md §1, §6-§8): the APPLY step.
//
// ApplyUpgrade takes the run lock FIRST, recomputes the 3-way plan under it, refuses a stale
// plan hash, then walks the items in install-pass order and writes each chosen one through the
// SAME admin service the Studio uses (an upgradeAdapter), recording the new baseline row right
// after every item so a crash mid-run never leaves an applied item looking like a customisation.
// Nothing here issues SQL on a sys_* config table: configuration goes through adapters, the
// baseline through the item store, and the run row through the upgrade store.
package vertical_pack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// Run statuses and item outcomes (wire strings, shared with the FE).
const (
	RunRunning     = "running"
	RunDone        = "done"
	RunPartial     = "partial"
	RunFailed      = "failed"
	RunInterrupted = "interrupted"

	OutcomeApplied  = "applied"
	OutcomeRestored = "restored"
	OutcomeMerged   = "merged"
	OutcomeDeleted  = "deleted"
	OutcomeAdopted  = "adopted"
	OutcomeKept     = "kept"
	OutcomeFailed   = "failed"
	OutcomeBlocked  = "blocked" // needs a table this run did not create (plan.md §6)
	OutcomeNotRun   = "not_run"

	ActionApply   = "apply"
	ActionRestore = "restore"
	ActionMerge   = "merge" // L6: write the three-way merge instead of the pack version
	ActionDelete  = "delete"
	// ActionKeepAlways flags the item protected: later plans default it to keep (FR-024).
	ActionKeepAlways = "keep_always"
	OutcomeProtected = "protected"

	// bookBudget: bookkeeping gets its own short context so a run that hits the cap can still
	// record where it stopped.
	bookBudget = 10 * time.Second
)

// upgradeRunBudget caps the config-writing part of one run. A var only so a test can shrink it.
var upgradeRunBudget = 2 * time.Minute

// upgradeTodo is one planned write (or adoption) of a run.
type upgradeTodo struct {
	it     *UpgradeItem
	action string
}

// UpgradeDecision is one caller choice. Items without one use the plan's defaultAction.
type UpgradeDecision struct {
	ItemType string `json:"itemType"`
	ItemKey  string `json:"itemKey"`
	Action   string `json:"action"`
}

// UpgradeResult is the per-item outcome. It never carries configuration content or secrets.
type UpgradeResult struct {
	ItemType string `json:"itemType"`
	ItemKey  string `json:"itemKey"`
	Action   string `json:"action"`
	Outcome  string `json:"outcome"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
	LiveID   string `json:"liveId,omitempty"`
}

// UpgradeRun is one sys_pack_upgrades row (the run lock and the history in one).
type UpgradeRun struct {
	ID           string            `json:"id"`
	TenantID     string            `json:"-"`
	Slug         string            `json:"slug"`
	FromVersions string            `json:"fromVersions"`
	ToVersion    string            `json:"toVersion"`
	Source       string            `json:"source"`
	Mode         string            `json:"mode"`
	PlanHash     string            `json:"planHash"`
	Status       string            `json:"status"`
	Decisions    []UpgradeDecision `json:"decisions"`
	Results      []UpgradeResult   `json:"results"`
	Counts       map[string]int    `json:"counts"`
	CreatedBy    string            `json:"createdBy"`
	StartedAt    time.Time         `json:"startedAt"`
	FinishedAt   *time.Time        `json:"finishedAt,omitempty"`
	// ReplicaLagSeconds is the MetaCache TTL other server instances may still serve stale config
	// for (FR-039). Only set on the apply response.
	ReplicaLagSeconds int `json:"replicaLagSeconds,omitempty"`
}

// ErrRunInProgress is returned by ClaimRun when another run holds the lock for (tenant, slug).
type ErrRunInProgress struct {
	RunID     string
	StartedAt time.Time
}

func (e *ErrRunInProgress) Error() string { return "vertical_pack: pack upgrade already running" }

// upgradeStore is the run lock + history. Optional capability of the item store (type-asserted,
// plan.md D12) so fakes of itemStore need no new method. Satisfied by *packinstall.Repository.
type upgradeStore interface {
	// ClaimRun takes the lock (after retiring a run whose heartbeat is older than the lease) or
	// returns *ErrRunInProgress.
	ClaimRun(ctx context.Context, run UpgradeRun) error
	// AbortRun cancels a run that was rejected after it took the lock: lock released, row hidden.
	AbortRun(ctx context.Context, tenantID, id string) error
	BeginRun(ctx context.Context, tenantID, id, fromVersions string, decisions []UpgradeDecision) error
	AppendResult(ctx context.Context, tenantID, id string, r UpgradeResult) error
	FinishRun(ctx context.Context, tenantID, id, status string, counts map[string]int) error
	ListRuns(ctx context.Context, tenantID, slug string, limit int) ([]UpgradeRun, error)
	// RunningRun returns the run currently holding the lock (within its lease), or nil.
	RunningRun(ctx context.Context, tenantID, slug string) (*ErrRunInProgress, error)
	DeleteItems(ctx context.Context, tenantID, slug string, keys [][2]string) error
}

func (s *Service) upgradeStore() upgradeStore {
	if s.items == nil {
		return nil
	}
	st, _ := s.items.(upgradeStore)
	return st
}

// typeRank is the install-pass order (plan.md §6); types not listed sort last.
var typeRank = map[string]int{
	ItemEntity: 0, ItemNN: 1, ItemDeferredField: 2, ItemField: 3, ItemLayout: 4, ItemRole: 5, ItemRoleGrant: 5,
	ItemFieldACL: 6, ItemTeam: 7, ItemDashboard: 8, ItemWorkflow: 9, ItemReport: 10, ItemWorkQueue: 11, ItemApproval: 12, ItemInboundHook: 13,
}

func rankOf(typ string) int {
	if r, ok := typeRank[typ]; ok {
		return r
	}
	return 99
}

// entityCreateOrder: position of each table in Install's dependency order (a relation's target is
// created before the table that points at it); an undecodable or cyclic set falls back to key order.
func entityCreateOrder(theirs map[string]json.RawMessage, keys []string) map[string]int {
	var ents []PackEntity
	for _, k := range keys {
		var pe PackEntity
		if raw, ok := theirs[ItemEntity+"/"+k]; ok && json.Unmarshal(raw, &pe) == nil {
			ents = append(ents, pe)
		}
	}
	out := map[string]int{}
	if ord, err := orderEntities(ents); err == nil {
		for i, pe := range ord {
			out[strings.TrimSpace(pe.Name)] = i
		}
	}
	return out
}

func stale(hash string) error {
	return &services.ValidationError{Code: "PACK_UPGRADE_PLAN_STALE",
		Msg:     "Kế hoạch đã cũ — workspace hoặc gói đã đổi từ lúc bạn xem. Xem lại kế hoạch rồi áp.",
		Details: map[string]any{"planHash": hash}}
}

// ApplyUpgrade applies the plan the caller reviewed. Order is load-bearing (FR-030): delivered
// gate → ClaimRun → PlanUpgrade under the lock → hash/decision checks (each failure releases the
// lock via AbortRun) → writes. Computing the plan BEFORE claiming leaves a window in which a
// request that planned before another run finished would re-apply a stale plan and duplicate
// every `added` item (rule/report names are not unique).
func (s *Service) ApplyUpgrade(ctx context.Context, tenantID, userID, slug string, pack *Pack, source, planHash string, decisions []UpgradeDecision) (*UpgradeRun, error) {
	slug = strings.TrimSpace(slug)
	st := s.upgradeStore()
	if st == nil {
		return nil, fmt.Errorf("vertical_pack.ApplyUpgrade: upgrade store not wired")
	}
	// Mode: "draft" on a delivered project (the builder in its own build session; checkDeliveredCaller
	// refuses every other caller BEFORE the lock), "live" otherwise.
	mode := "live"
	if s.delivery != nil {
		delivered, err := s.delivery.Delivered(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.ApplyUpgrade: delivered check: %w", err)
		}
		if delivered {
			if err := checkDeliveredCaller(ctx, tenantID); err != nil {
				return nil, err
			}
			mode = "draft"
		}
	}
	if pack == nil || pack.Manifest == nil {
		return nil, invalid("PACK_UPGRADE_NO_SLUG", "Gói không có manifest.slug — không biết nâng cấp cho gói nào")
	}
	// Column widths of sys_pack_upgrades: an over-long client value must be a 422 here, not a
	// MySQL 1406 (500) after the lock row was already inserted.
	if len(planHash) > 64 || len(slug) > 150 || len(pack.Manifest.Version) > 50 {
		return nil, invalid("PACK_UPGRADE_INPUT_INVALID", "planHash, slug hoặc version vượt độ dài cho phép")
	}
	if decisions == nil {
		decisions = []UpgradeDecision{}
	}
	run := UpgradeRun{
		ID: uuid.NewString(), TenantID: tenantID, Slug: slug, ToVersion: strings.TrimSpace(pack.Manifest.Version),
		Source: source, Mode: mode, PlanHash: planHash, Status: RunRunning, Decisions: decisions,
		CreatedBy: userID, StartedAt: time.Now().UTC(),
	}
	if err := st.ClaimRun(ctx, run); err != nil {
		var busy *ErrRunInProgress
		if errors.As(err, &busy) {
			return nil, &services.ValidationError{Code: "PACK_UPGRADE_IN_PROGRESS",
				Msg:     "Một lần nâng cấp gói này đang chạy — đợi nó xong rồi thử lại.",
				Details: map[string]any{"runId": busy.RunID, "startedAt": busy.StartedAt.UTC().Format(time.RFC3339)}}
		}
		return nil, fmt.Errorf("vertical_pack.ApplyUpgrade: claim: %w", err)
	}
	// Anything that returns before BeginRun must hand the lock back.
	began := false
	abort := func() {
		if began {
			return
		}
		bctx, cancel := bookCtx(ctx)
		defer cancel()
		if err := st.AbortRun(bctx, tenantID, run.ID); err != nil {
			s.logger.Error("vertical_pack.ApplyUpgrade: abort run", zap.String("tenant_id", tenantID), zap.String("slug", slug), zap.Error(err))
		}
	}
	defer abort()

	plan, art, err := s.planUpgrade(ctx, tenantID, slug, pack, source)
	if err != nil {
		return nil, err
	}
	if plan.PlanHash != planHash {
		return nil, stale(plan.PlanHash)
	}

	byKey := make(map[string]*UpgradeItem, len(plan.Items))
	for i := range plan.Items {
		byKey[plan.Items[i].ItemType+"/"+plan.Items[i].ItemKey] = &plan.Items[i]
	}
	chosen := map[string]string{}
	for _, d := range decisions {
		it, ok := byKey[d.ItemType+"/"+d.ItemKey]
		if !ok || !contains(it.Actions, d.Action) {
			return nil, &services.ValidationError{Code: "PACK_UPGRADE_DECISION_INVALID",
				Msg:     "Quyết định không nằm trong các hành động được phép của mục này",
				Details: map[string]any{"itemType": d.ItemType, "itemKey": d.ItemKey, "action": d.Action}}
		}
		chosen[d.ItemType+"/"+d.ItemKey] = d.Action
	}

	var work []upgradeTodo
	nWrites, nAdopt := 0, 0
	for i := range plan.Items {
		it := &plan.Items[i]
		a, ok := chosen[it.ItemType+"/"+it.ItemKey]
		if !ok {
			a = it.DefaultAction
		}
		switch {
		case a == ActionApply || a == ActionRestore || a == ActionMerge || a == ActionDelete || a == ActionKeepAlways:
			nWrites++
			work = append(work, upgradeTodo{it, a})
		case it.Adopt:
			nAdopt++
			work = append(work, upgradeTodo{it, "adopt"})
		}
	}
	if nWrites+nAdopt == 0 {
		return nil, invalid("PACK_UPGRADE_NOTHING_TO_APPLY", "Không có gì để áp: mọi mục đang chọn Giữ và không có mục nào để nhận nuôi")
	}
	// L7: a change that reaches the customer at once with no trace needs the builder's tick.
	if mode == "draft" && !ImmediateConfirmed(ctx) {
		if hit := needsImmediateConfirm(work); len(hit) > 0 {
			return nil, &services.ValidationError{Code: "PACK_UPGRADE_IMMEDIATE_UNCONFIRMED",
				Msg:     "Có thay đổi áp ngay cho khách, không qua bản nháp — cần xác nhận \"Áp ngay cho khách\"",
				Details: map[string]any{"items": hit}}
		}
	}
	var entKeys []string
	for _, w := range work {
		if w.it.ItemType == ItemEntity {
			entKeys = append(entKeys, w.it.ItemKey)
		}
	}
	entOrder := entityCreateOrder(art.theirs, entKeys)
	sort.SliceStable(work, func(i, j int) bool {
		ri, rj := rankOf(work[i].it.ItemType), rankOf(work[j].it.ItemType)
		if ri != rj {
			return ri < rj
		}
		if ri == rankOf(ItemEntity) {
			if oi, oj := entOrder[work[i].it.ItemKey], entOrder[work[j].it.ItemKey]; oi != oj {
				return oi < oj
			}
		}
		return work[i].it.ItemKey < work[j].it.ItemKey
	})

	eff := make([]UpgradeDecision, 0, len(work))
	for _, w := range work {
		eff = append(eff, UpgradeDecision{ItemType: w.it.ItemType, ItemKey: w.it.ItemKey, Action: w.action})
	}
	run.FromVersions = strings.Join(plan.FromVersions, ",")
	run.Decisions = eff
	{
		bctx, cancel := bookCtx(ctx)
		err := st.BeginRun(bctx, tenantID, run.ID, run.FromVersions, eff)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.ApplyUpgrade: begin run: %w", err)
		}
	}
	began = true

	// From here the run finishes even when the caller disconnects (FR-052).
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upgradeRunBudget)
	defer cancel()
	env := newApplyEnv(s, tenantID, userID, slug, run.ToVersion, art)
	env.draftRun = mode == "draft"
	env.bind(ctx, rctx, plan.Items, eff)
	counts := map[string]int{}
	var results []UpgradeResult
	record := func(res UpgradeResult) {
		counts[res.Outcome]++
		results = append(results, res)
		bctx, cancelB := bookCtx(ctx)
		defer cancelB()
		if err := st.AppendResult(bctx, tenantID, run.ID, res); err != nil {
			s.logger.Error("vertical_pack.ApplyUpgrade: append result", zap.String("tenant_id", tenantID), zap.String("run_id", run.ID),
				zap.String("item_type", res.ItemType), zap.String("item_key", res.ItemKey), zap.Error(err))
		}
	}
	for _, w := range work {
		res := UpgradeResult{ItemType: w.it.ItemType, ItemKey: w.it.ItemKey, Action: w.action}
		switch {
		case rctx.Err() != nil:
			res.Outcome, res.Code, res.Message = OutcomeNotRun, "UPGRADE_DEADLINE", "Hết hạn 2 phút trước khi tới mục này — áp lại để làm tiếp"
		case w.action == "adopt":
			s.adoptItem(ctx, env, w.it, &res)
		default:
			s.applyItem(ctx, rctx, env, w.it, w.action, &res)
		}
		record(res)
	}
	if rctx.Err() == nil {
		if res, ok := s.appendNav(rctx, env); ok {
			if mode == "draft" && res.Outcome == OutcomeApplied {
				res.Outcome = OutcomeDrafted // the app is a draft document: the new menu entries wait for Publish
			}
			record(res)
		}
	}
	counts[OutcomeKept] = countKept(plan, chosen)

	bad := counts[OutcomeFailed] + counts[OutcomeNotRun]
	good := counts[OutcomeDrafted] + counts[OutcomeProtected] + counts[OutcomeApplied] + counts[OutcomeMerged] + counts[OutcomeRestored] + counts[OutcomeDeleted] + counts[OutcomeAdopted]
	switch {
	case bad == 0:
		run.Status = RunDone
	case good > 0:
		run.Status = RunPartial
	default:
		run.Status = RunFailed
	}
	bctx, cancelB := bookCtx(ctx)
	defer cancelB()
	if err := st.FinishRun(bctx, tenantID, run.ID, run.Status, counts); err != nil {
		// The row stays "running" until its lease lapses — say so loudly; the work itself is done.
		s.logger.Error("vertical_pack.ApplyUpgrade: finish run", zap.String("tenant_id", tenantID), zap.String("run_id", run.ID), zap.Error(err))
	}
	if run.Status == RunDone && s.installs != nil {
		rec := InstallRecord{ID: newInstallID(), TenantID: tenantID, Slug: slug, Version: run.ToVersion, InstalledAt: nowUTC(),
			Result: map[string]any{"kind": "upgrade", "runId": run.ID, "installedBy": userID}}
		if err := s.installs.RecordInstall(bctx, rec); err != nil {
			s.logger.Warn("vertical_pack.ApplyUpgrade: record install ledger", zap.String("tenant_id", tenantID), zap.String("slug", slug), zap.Error(err))
		}
	}
	now := time.Now().UTC()
	run.Results, run.Counts, run.FinishedAt, run.ReplicaLagSeconds = results, counts, &now, 60
	return &run, nil
}

// refuseWhileUpgrading is Install's side of the run lock: same slug mid-upgrade ⇒ 409.
func (s *Service) refuseWhileUpgrading(ctx context.Context, tenantID string, pack *Pack) error {
	st := s.upgradeStore()
	if st == nil || pack == nil || pack.Manifest == nil {
		return nil
	}
	slug := strings.TrimSpace(pack.Manifest.Slug)
	if slug == "" {
		return nil
	}
	busy, err := st.RunningRun(ctx, tenantID, slug)
	if err != nil {
		return fmt.Errorf("vertical_pack.Install: check running upgrade: %w", err)
	}
	if busy == nil {
		return nil
	}
	return &services.ValidationError{Code: "PACK_UPGRADE_IN_PROGRESS",
		Msg:     "Một lần nâng cấp gói này đang chạy — đợi nó xong rồi cài.",
		Details: map[string]any{"runId": busy.RunID, "startedAt": busy.StartedAt.UTC().Format(time.RFC3339)}}
}

func bookCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bookBudget)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// countKept counts items that offered a real choice and ended on Keep (display only).
func countKept(plan *UpgradePlan, chosen map[string]string) int {
	n := 0
	for _, it := range plan.Items {
		if len(it.Actions) == 0 {
			continue
		}
		a, ok := chosen[it.ItemType+"/"+it.ItemKey]
		if !ok {
			a = it.DefaultAction
		}
		if a == ActionKeep {
			n++
		}
	}
	return n
}

// applyItem writes one item through its adapter and records the new baseline right after.
// adoptCtx is the request context's detached twin used for bookkeeping; rctx bounds the write.
func (s *Service) applyItem(ctx, rctx context.Context, env *applyEnv, it *UpgradeItem, action string, res *UpgradeResult) {
	if action == ActionKeepAlways {
		s.protectItem(ctx, env, it, res) // flag only: no adapter, nothing live is written
		return
	}
	ad, ok := upgradeAdapters[it.ItemType]
	if !ok || !ad.ready(s) {
		res.Outcome, res.Code, res.Message = OutcomeFailed, "ADAPTER_NOT_READY", "Chưa hỗ trợ nâng cấp loại này"
		return
	}
	if msg := env.blockedBy(it); msg != "" {
		res.Outcome, res.Code, res.Message = OutcomeBlocked, "BLOCKED_BY", msg
		return
	}
	restore := func() {}
	if action == ActionMerge {
		var rerr error
		// The merge is only what gets WRITTEN; the baseline stays the pack's version (FR-040), so
		// the builder's own edits keep reading as customisations in the next plan.
		if restore, rerr = env.useMerged(it); rerr != nil {
			s.failItem(env, it, rerr, res)
			return
		}
		defer restore()
	}
	var err error
	switch action {
	case ActionDelete:
		if ad.remove == nil {
			res.Outcome, res.Code, res.Message = OutcomeFailed, "ADAPTER_NOT_READY", "Loại này không xoá được qua nâng cấp"
			return
		}
		err = ad.remove(rctx, env, it)
	default:
		res.LiveID, err = ad.write(rctx, env, it)
	}
	if err != nil {
		s.failItem(env, it, err, res)
		return
	}
	if n, ok := env.notes[it.ItemType+"/"+it.ItemKey]; ok {
		res.Code, res.Message = n[0], n[1]
	}
	restore()
	// L7 (FR-105): a change that went into the draft layer is NOT the customer's running config yet,
	// so the baseline stays: the next plan reads it as "Đang có nháp", and after Publish as in_sync,
	// which adopts it.
	if env.draftRun && it.ApplyMode == ApplyModeDraft {
		res.Outcome = OutcomeDrafted
		return
	}
	switch action {
	case ActionDelete:
		res.Outcome = OutcomeDeleted
		s.dropBaseline(ctx, env, it, res)
	case ActionRestore:
		res.Outcome = OutcomeRestored
		s.saveBaseline(ctx, env, it, res.LiveID, res)
	case ActionMerge:
		res.Outcome = OutcomeMerged
		s.saveBaseline(ctx, env, it, res.LiveID, res)
	default:
		res.Outcome = OutcomeApplied
		s.saveBaseline(ctx, env, it, res.LiveID, res)
	}
}

// itemProtector is the optional "Luôn giữ" capability of the item store (type-asserted, D12).
type itemProtector interface {
	SetProtected(ctx context.Context, tenantID, slug, itemType, itemKey string, protected bool) error
}

// protectItem flags the item's baseline row protected; nothing live is written. An item with no
// baseline row yet (conflict_no_base) first gets one = the pack version, so the flag has a row.
func (s *Service) protectItem(ctx context.Context, env *applyEnv, it *UpgradeItem, res *UpgradeResult) {
	pr, ok := s.items.(itemProtector)
	if !ok || !keepAlwaysTypes(it.ItemType) {
		res.Outcome, res.Code, res.Message = OutcomeFailed, "ADAPTER_NOT_READY", "Chưa hỗ trợ Luôn giữ cho mục này"
		return
	}
	bctx, cancel := bookCtx(ctx)
	defer cancel()
	if len(it.Base) == 0 {
		raw, has := env.theirs[it.ItemType+"/"+it.ItemKey]
		if !has {
			res.Outcome, res.Code, res.Message = OutcomeFailed, "UPGRADE_PACK_ITEM_MISSING", "Bản mới của gói không còn mục này"
			return
		}
		if err := s.items.SaveItems(bctx, env.tenantID, env.slug, env.toVersion, []PackItem{{
			Type: it.ItemType, Key: it.ItemKey, Content: raw, Wrote: true, LiveID: it.LiveID}}); err != nil {
			s.failItem(env, it, fmt.Errorf("vertical_pack.protectItem: baseline: %w", err), res)
			return
		}
	}
	if err := pr.SetProtected(bctx, env.tenantID, env.slug, it.ItemType, it.ItemKey, true); err != nil {
		s.failItem(env, it, fmt.Errorf("vertical_pack.protectItem: %w", err), res)
		return
	}
	res.Outcome = OutcomeProtected
}

// failItem turns an adapter error into the item's failed outcome (a validation error keeps its code).
func (s *Service) failItem(env *applyEnv, it *UpgradeItem, err error, res *UpgradeResult) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		res.Outcome, res.Code, res.Message = OutcomeFailed, ve.Code, ve.Msg
		return
	}
	res.Outcome, res.Code, res.Message = OutcomeFailed, "UPGRADE_ITEM_FAILED", "Không ghi được mục này"
	s.logger.Error("vertical_pack.applyItem", zap.String("tenant_id", env.tenantID),
		zap.String("item_type", it.ItemType), zap.String("item_key", it.ItemKey), zap.Error(err))
}

// adoptItem records the pack version as the baseline of an item the tenant already matches.
func (s *Service) adoptItem(ctx context.Context, env *applyEnv, it *UpgradeItem, res *UpgradeResult) {
	res.Outcome = OutcomeAdopted
	if isRoleFamily(it.ItemType) { // the role's baseline is composed once from all its members
		id, err := env.ensureRole(roleNameOf(it))
		if err != nil {
			s.failItem(env, it, err, res)
			return
		}
		res.LiveID = id
		return
	}
	if env.isEntityFamily(it) {
		env.markDone(it)
		return
	}
	if _, has := env.theirs[it.ItemType+"/"+it.ItemKey]; !has {
		s.dropBaseline(ctx, env, it, res) // both sides dropped it
		return
	}
	s.saveBaseline(ctx, env, it, it.LiveID, res)
}

// saveBaseline upserts the item's baseline = the new pack content (already credential-stripped by
// provenanceItems) — never the input just written, which may carry live keys (FR-036).
func (s *Service) saveBaseline(ctx context.Context, env *applyEnv, it *UpgradeItem, liveID string, res *UpgradeResult) {
	if isRoleFamily(it.ItemType) {
		return // recorded by ensureRole
	}
	if env.isEntityFamily(it) {
		env.markDone(it) // composed from the family's outcomes
		return
	}
	raw, ok := env.theirs[it.ItemType+"/"+it.ItemKey]
	if !ok {
		return
	}
	bctx, cancel := bookCtx(ctx)
	defer cancel()
	err := s.items.SaveItems(bctx, env.tenantID, env.slug, env.toVersion, []PackItem{{
		Type: it.ItemType, Key: it.ItemKey, Content: raw, Wrote: true, LiveID: liveID,
	}})
	if err != nil {
		// The config is written; the next plan reads this item as in_sync and adopts it.
		s.logger.Error("vertical_pack.saveBaseline", zap.String("tenant_id", env.tenantID),
			zap.String("item_type", it.ItemType), zap.String("item_key", it.ItemKey), zap.Error(err))
		res.Code, res.Message = "BASELINE_NOT_RECORDED", "Đã ghi nhưng chưa lưu được bản gốc — lần nâng cấp sau sẽ tự nhận nuôi"
		return
	}
	// The builder took the pack's version, so the "Luôn giữ" flag has done its job.
	if it.Status == StatusProtected {
		if pr, ok := s.items.(itemProtector); ok {
			if err := pr.SetProtected(bctx, env.tenantID, env.slug, it.ItemType, it.ItemKey, false); err != nil {
				s.logger.Error("vertical_pack.saveBaseline: clear protected", zap.String("tenant_id", env.tenantID),
					zap.String("item_type", it.ItemType), zap.String("item_key", it.ItemKey), zap.Error(err))
			}
		}
	}
}

func (s *Service) dropBaseline(ctx context.Context, env *applyEnv, it *UpgradeItem, res *UpgradeResult) {
	if isRoleFamily(it.ItemType) {
		return // recorded by ensureRole
	}
	st := s.upgradeStore()
	bctx, cancel := bookCtx(ctx)
	defer cancel()
	if err := st.DeleteItems(bctx, env.tenantID, env.slug, [][2]string{{it.ItemType, it.ItemKey}}); err != nil {
		s.logger.Error("vertical_pack.dropBaseline", zap.String("tenant_id", env.tenantID),
			zap.String("item_type", it.ItemType), zap.String("item_key", it.ItemKey), zap.Error(err))
		res.Code, res.Message = "BASELINE_NOT_RECORDED", "Đã xử lý nhưng chưa xoá được bản gốc — lần nâng cấp sau sẽ tự nhận nuôi"
	}
}

// ListUpgrades returns the newest-first upgrade history of one pack in the tenant (limit 1..50).
func (s *Service) ListUpgrades(ctx context.Context, tenantID, slug string, limit int) ([]UpgradeRun, error) {
	st := s.upgradeStore()
	if st == nil {
		return []UpgradeRun{}, nil
	}
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	runs, err := st.ListRuns(ctx, tenantID, strings.TrimSpace(slug), limit)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.ListUpgrades: %w", err)
	}
	if runs == nil {
		runs = []UpgradeRun{}
	}
	return runs, nil
}

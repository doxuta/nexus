package vertical_pack

// Unit tests for ApplyUpgrade (specs/069 L2a) over in-memory fakes: lock order, stale plan hash,
// decision validation, nothing-to-apply, delivered gate, run survives a cancelled request context,
// secrets never reach the baseline, Install refuses while a run holds the lock. The DB-level lock
// (UNIQUE + CHECK + lease) is proven in internal/packinstall against real MySQL.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// upgStore is an in-memory itemStore + upgradeStore. calls logs the order of lock operations.
type upgStore struct {
	mu      sync.Mutex
	rows    map[string]StoredPackItem
	calls   []string
	running *ErrRunInProgress
	runs    []UpgradeRun
	results []UpgradeResult
	deleted [][2]string
}

func newUpgStore() *upgStore { return &upgStore{rows: map[string]StoredPackItem{}} }

func (f *upgStore) log(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *upgStore) SaveItems(_ context.Context, tenantID, slug, version string, items []PackItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range items {
		k := tenantID + "|" + slug + "|" + it.Type + "|" + it.Key
		if _, exists := f.rows[k]; exists && !it.Wrote {
			continue // insert-if-absent, like the SQL
		}
		f.rows[k] = StoredPackItem{Type: it.Type, Key: it.Key, PackVersion: version,
			Content: append(json.RawMessage(nil), it.Content...), LiveID: it.LiveID}
	}
	return nil
}

func (f *upgStore) ListItems(_ context.Context, tenantID, slug string) ([]StoredPackItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StoredPackItem
	for k, v := range f.rows {
		if strings.HasPrefix(k, tenantID+"|"+slug+"|") {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *upgStore) ClaimRun(_ context.Context, run UpgradeRun) error {
	f.log("claim")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running != nil {
		return f.running
	}
	f.running = &ErrRunInProgress{RunID: run.ID, StartedAt: run.StartedAt}
	f.runs = append(f.runs, run)
	return nil
}
func (f *upgStore) AbortRun(context.Context, string, string) error {
	f.log("abort")
	f.mu.Lock()
	f.running = nil
	f.mu.Unlock()
	return nil
}
func (f *upgStore) BeginRun(context.Context, string, string, string, []UpgradeDecision) error {
	f.log("begin")
	return nil
}
func (f *upgStore) AppendResult(_ context.Context, _, _ string, r UpgradeResult) error {
	f.mu.Lock()
	f.results = append(f.results, r)
	f.mu.Unlock()
	return nil
}
func (f *upgStore) FinishRun(context.Context, string, string, string, map[string]int) error {
	f.log("finish")
	f.mu.Lock()
	f.running = nil
	f.mu.Unlock()
	return nil
}
func (f *upgStore) ListRuns(context.Context, string, string, int) ([]UpgradeRun, error) {
	return f.runs, nil
}
func (f *upgStore) RunningRun(_ context.Context, _, slug string) (*ErrRunInProgress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if slug != "p" { // the fake only ever locks pack "p"
		return nil, nil
	}
	return f.running, nil
}
func (f *upgStore) DeleteItems(_ context.Context, _, _ string, keys [][2]string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, keys...)
	f.mu.Unlock()
	return nil
}
func (f *upgStore) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

// upgWF is a workflow writer that also satisfies workflowUpdater.
type upgWF struct {
	mu      sync.Mutex
	seq     int
	created []services.CreateWorkflowInput
	updated []services.UpdateWorkflowInput
	deleted []string
	onMake  func()          // runs inside CreateWorkflow (e.g. cancel the request context)
	failFor map[string]bool // rule names whose CreateWorkflow errors
}

func (f *upgWF) ListWorkflows(context.Context, string) ([]workflow.WorkflowRecord, error) {
	return nil, nil
}
func (f *upgWF) CreateWorkflow(_ context.Context, tenantID string, in services.CreateWorkflowInput) (*workflow.WorkflowRecord, error) {
	f.mu.Lock()
	f.seq++
	f.created = append(f.created, in)
	id := fmt.Sprintf("wf-%d", f.seq)
	hook := f.onMake
	fail := f.failFor[in.Name]
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if fail {
		return nil, fmt.Errorf("boom %s", in.Name)
	}
	return &workflow.WorkflowRecord{ID: id, TenantID: tenantID, Name: in.Name}, nil
}
func (f *upgWF) GetWorkflow(_ context.Context, _, id string) (*workflow.WorkflowRecord, error) {
	return &workflow.WorkflowRecord{ID: id, IntervalSeconds: 600}, nil
}
func (f *upgWF) UpdateWorkflow(_ context.Context, _, id string, in services.UpdateWorkflowInput) (*workflow.WorkflowRecord, error) {
	f.mu.Lock()
	f.updated = append(f.updated, in)
	f.mu.Unlock()
	return &workflow.WorkflowRecord{ID: id}, nil
}
func (f *upgWF) DeleteWorkflow(_ context.Context, _, id string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, id)
	f.mu.Unlock()
	return nil
}

type gateFn func(context.Context, string) (bool, error)

func (g gateFn) Delivered(ctx context.Context, tenantID string) (bool, error) {
	return g(ctx, tenantID)
}

const upgTenant = "t-upg"

func upgWFJSON(name string, withSecret bool) json.RawMessage {
	act := `{"type":"field_update","field":"x","value":"1"}`
	if withSecret {
		act = `{"type":"outbound_webhook","url":"https://x.test/h","secret":"SECRET-MARKER-9","headers":{"Authorization":"MARKER-9"}}`
	}
	return json.RawMessage(fmt.Sprintf(`{"name":%q,"entityName":"e","trigger":"after_create","actions":[%s]}`, name, act))
}

func upgPack(version string, names ...string) *Pack {
	p := &Pack{Manifest: &Manifest{Slug: "p", Version: version}}
	names = append([]string{"seed"}, names...) // keeps the seeded baseline row "present in the pack" (no adoption noise)
	for _, n := range names {
		var pw PackWorkflow
		_ = json.Unmarshal(upgWFJSON(n, strings.HasPrefix(n, "hook")), &pw)
		p.Workflows = append(p.Workflows, pw)
	}
	return p
}

// newUpgSvc: a tenant tracked at 1.0.0 (one baseline row for an old rule the pack dropped is NOT
// used — a single `kept` base row would make the plan non-empty), so every pack rule is "added".
func newUpgSvc(t *testing.T) (*Service, *upgStore, *upgWF) {
	t.Helper()
	st := newUpgStore()
	st.rows[upgTenant+"|p|workflow|seed"] = StoredPackItem{Type: ItemWorkflow, Key: "seed", PackVersion: "1.0.0", Content: upgWFJSON("seed", false)}
	wf := &upgWF{}
	svc := NewService(Deps{EntityReader: &fakeMintMeta{}, LayoutReader: fakeApLayouts{}, RoleReader: fakeApRoleReader{}, WorkflowWriter: wf}).WithItemStore(st)
	return svc, st, wf
}

func planHashOf(t *testing.T, svc *Service, pack *Pack) string {
	t.Helper()
	p, err := svc.PlanUpgrade(context.Background(), upgTenant, "p", pack, "upload")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p.PlanHash
}

func vcode(err error) string {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

func TestApplyUpgrade_AppliesAddedRuleAndRecordsBaselineWithoutSecret(t *testing.T) {
	svc, st, wf := newUpgSvc(t)
	pack := upgPack("1.1.0", "hook rule", "plain rule")
	hash := planHashOf(t, svc, pack)

	run, err := svc.ApplyUpgrade(context.Background(), upgTenant, "u1", "p", pack, "upload", hash, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if run.Status != RunDone || run.Counts[OutcomeApplied] != 2 {
		t.Fatalf("run = %s counts=%v; want done with 2 applied (results %+v)", run.Status, run.Counts, run.Results)
	}
	if len(wf.created) != 2 {
		t.Fatalf("created %d rules, want 2", len(wf.created))
	}
	for _, in := range wf.created { // install-side strip: no credential ever goes to the service
		for _, a := range in.Actions {
			if a.Secret != "" || len(a.Headers) > 0 {
				t.Errorf("rule %q written with credentials", in.Name)
			}
		}
	}
	row, ok := st.rows[upgTenant+"|p|workflow|hook rule"]
	if !ok || row.PackVersion != "1.1.0" || row.LiveID == "" {
		t.Fatalf("baseline row = %+v ok=%v; want 1.1.0 with live id", row, ok)
	}
	if strings.Contains(string(row.Content), "SECRET-MARKER") || strings.Contains(string(row.Content), "MARKER-9") {
		t.Fatalf("baseline carries a credential: %s", row.Content)
	}
	b, _ := json.Marshal(run)
	if strings.Contains(string(b), "SECRET-MARKER") {
		t.Fatalf("run payload carries a credential: %s", b)
	}
	// Lock order (FR-030): claim → begin → finish; and nothing left running.
	if got := strings.Join(st.calls, ","); got != "claim,begin,finish" {
		t.Errorf("store call order = %s; want claim,begin,finish", got)
	}
}

func TestApplyUpgrade_StalePlanHashWritesNothingAndReleasesLock(t *testing.T) {
	svc, st, wf := newUpgSvc(t)
	pack := upgPack("1.1.0", "plain rule")
	_, err := svc.ApplyUpgrade(context.Background(), upgTenant, "u1", "p", pack, "upload", "deadbeef", nil)
	if vcode(err) != "PACK_UPGRADE_PLAN_STALE" {
		t.Fatalf("err = %v; want PACK_UPGRADE_PLAN_STALE", err)
	}
	if len(wf.created) != 0 || st.count("abort") != 1 || st.running != nil {
		t.Fatalf("created=%d abort=%d running=%v; want 0, 1, nil", len(wf.created), st.count("abort"), st.running)
	}
}

func TestApplyUpgrade_DecisionOutsideActionsIsRefused(t *testing.T) {
	svc, st, wf := newUpgSvc(t)
	pack := upgPack("1.1.0", "plain rule")
	hash := planHashOf(t, svc, pack)
	for name, d := range map[string]UpgradeDecision{
		"unknown action": {ItemType: ItemWorkflow, ItemKey: "plain rule", Action: "delete"},
		"unknown item":   {ItemType: ItemWorkflow, ItemKey: "nope", Action: "apply"},
	} {
		_, err := svc.ApplyUpgrade(context.Background(), upgTenant, "u1", "p", pack, "upload", hash, []UpgradeDecision{d})
		if vcode(err) != "PACK_UPGRADE_DECISION_INVALID" {
			t.Errorf("%s: err = %v; want PACK_UPGRADE_DECISION_INVALID", name, err)
		}
	}
	if len(wf.created) != 0 || st.running != nil {
		t.Fatalf("created=%d running=%v after refused decisions", len(wf.created), st.running)
	}
}

func TestApplyUpgrade_AllKeepIsNothingToApply(t *testing.T) {
	svc, st, wf := newUpgSvc(t)
	pack := upgPack("1.1.0", "plain rule")
	hash := planHashOf(t, svc, pack)
	_, err := svc.ApplyUpgrade(context.Background(), upgTenant, "u1", "p", pack, "upload", hash,
		[]UpgradeDecision{{ItemType: ItemWorkflow, ItemKey: "plain rule", Action: ActionKeep}})
	if vcode(err) != "PACK_UPGRADE_NOTHING_TO_APPLY" {
		t.Fatalf("err = %v; want PACK_UPGRADE_NOTHING_TO_APPLY", err)
	}
	if len(wf.created) != 0 || st.count("begin") != 0 || st.running != nil {
		t.Fatalf("created=%d begin=%d running=%v", len(wf.created), st.count("begin"), st.running)
	}
}

func TestApplyUpgrade_DeliveredWorkspaceIsRefusedBeforeTheLock(t *testing.T) {
	svc, st, wf := newUpgSvc(t)
	svc.WithDeliveryGate(gateFn(func(context.Context, string) (bool, error) { return true, nil }))
	_, err := svc.ApplyUpgrade(context.Background(), upgTenant, "u1", "p", upgPack("1.1.0", "r"), "upload", "x", nil)
	if vcode(err) != "PROJECT_DELIVERED_INSTALL_BLOCKED" {
		t.Fatalf("err = %v; want PROJECT_DELIVERED_INSTALL_BLOCKED", err)
	}
	if st.count("claim") != 0 || len(wf.created) != 0 {
		t.Fatalf("claim=%d created=%d; a delivered workspace must be refused before the lock", st.count("claim"), len(wf.created))
	}
}

func TestApplyUpgrade_SecondRunAndInstallSeeTheLock(t *testing.T) {
	svc, st, _ := newUpgSvc(t)
	st.running = &ErrRunInProgress{RunID: "r-1", StartedAt: time.Now()}
	_, err := svc.ApplyUpgrade(context.Background(), upgTenant, "u1", "p", upgPack("1.1.0", "r"), "upload", "x", nil)
	if vcode(err) != "PACK_UPGRADE_IN_PROGRESS" {
		t.Fatalf("apply err = %v; want PACK_UPGRADE_IN_PROGRESS", err)
	}
	_, err = svc.Install(context.Background(), upgTenant, "u1", upgPack("1.1.0", "r"))
	if vcode(err) != "PACK_UPGRADE_IN_PROGRESS" {
		t.Fatalf("install err = %v; want PACK_UPGRADE_IN_PROGRESS", err)
	}
	// ... but another slug installs (the lock is per pack).
	other := upgPack("1.0.0")
	other.Manifest.Slug = "other"
	if err := svc.refuseWhileUpgrading(context.Background(), upgTenant, other); err != nil {
		t.Fatalf("another slug must not see the lock: %v", err)
	}
}

func TestApplyUpgrade_RunFinishesAfterTheRequestContextIsCancelled(t *testing.T) {
	svc, st, wf := newUpgSvc(t)
	pack := upgPack("1.1.0", "rule a", "rule b", "rule c")
	hash := planHashOf(t, svc, pack)
	ctx, cancel := context.WithCancel(context.Background())
	wf.onMake = cancel // the client "hangs up" while the first rule is being written
	run, err := svc.ApplyUpgrade(ctx, upgTenant, "u1", "p", pack, "upload", hash, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if run.Status != RunDone || len(wf.created) != 3 || st.running != nil {
		t.Fatalf("status=%s created=%d running=%v; a disconnect must not cut the run", run.Status, len(wf.created), st.running)
	}
}

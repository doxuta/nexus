// Package doctor is the check-registry framework of the Nexus Doctor
// (../specs/008-nexus-doctor/spec.md §2.1) — the invariant scanner + targeted fixer distilled from
// Nexus Quick Repair & Rebuild. It holds ONLY the generic machinery (Check, Finding, Runner);
// the concrete checks are built in the service layer where the existing helpers they must
// reuse live (validator-parity discipline: a fix never re-implements a repair that already
// exists as a helper).
//
// Hard conventions the framework enforces or documents (spec §2.1):
//   - Detect is READ-ONLY, absolutely. The anti-lesson is Nexus's rebuildAuditTables running
//     DDL in display-only mode (QuickRepairAndRebuild.php:650-656) — a Nexus scan must never
//     mutate anything, so the Runner's Scan path only ever calls Detect.
//   - Detect is LIMIT-bounded: implementations query with MaxFindingsPerCheck+1 and the
//     Runner truncates to MaxFindingsPerCheck, reporting Truncated honestly.
//   - Fix re-detects its single item before mutating (recovery model = re-run, like QRR's
//     idempotent diff): an item that is already clean returns ErrAlreadyClean and is counted
//     as skipped, never double-fixed.
//   - Per-item failure is recorded and the run CONTINUES (re-run resumes what's left).
//   - Every check is tenant-scoped: Detect/Fix receive the tenantID and every query they
//     issue pins it.
package doctor

import (
	"context"
	"errors"
	"fmt"
)

// MaxFindingsPerCheck caps the findings a single check reports per scan (and fixes per fix
// batch). Detect implementations should query LIMIT MaxFindingsPerCheck+1 so the Runner can
// detect (and honestly report) truncation. A truncated fix batch is fine: fixes shrink the
// drift, so re-running the doctor resumes the remainder (QRR recovery model).
const MaxFindingsPerCheck = 200

// Severity levels (spec §2.1).
const (
	SeverityInfo     = "info"
	SeverityWarn     = "warn"
	SeverityCritical = "critical"
)

// ErrAlreadyClean is returned by a Check's Fix when the per-item re-detect shows the finding
// no longer holds (fixed concurrently, or stale). The Runner counts it as skipped.
var ErrAlreadyClean = errors.New("finding already clean")

// ErrUnknownCheck is returned (wrapped, with the offending id) when a fix request names a
// check id that is not in the registry. The service maps it to 422 DOCTOR_CHECK_UNKNOWN.
var ErrUnknownCheck = errors.New("checkId không tồn tại")

// ErrConfirmRequired is returned (wrapped, with the offending id) when a fix request names a
// needs-confirm check (AutoFixSafe=false) without confirm=true. The service maps it to 422
// DOCTOR_CONFIRM_REQUIRED. Registered needs-confirm checks today: A6/B4 (DOCTOR-SCHEMA) + C4
// (DOCTOR-OPS).
var ErrConfirmRequired = errors.New("check này cần confirm=true")

// ErrReportOnly is returned (wrapped, with the offending id) when a fix request names a
// report-only check (DOCTOR-OPS A3/B5/C8). Report-only findings have NO automated fix by
// design — the "fix" is a human decision (drop touches co-tenants, the grace-window is a
// feature, the DeleteField flow owns the cascade) — so the request is refused before any
// mutation, exactly like an unknown id. The service maps it to 422 DOCTOR_REPORT_ONLY.
var ErrReportOnly = errors.New("check chỉ báo cáo — không có fix tự động, xử lý tay")

// Finding is one detected drift item. Key is the stable identifier Fix uses to re-resolve
// the item (a sys_* row id, or a composite like "entity:<refId>" for grouped findings);
// Detail is the human-readable description shown in the FE findings table.
type Finding struct {
	Key    string `json:"key"`
	Detail string `json:"detail"`
}

// Check is one registered invariant: a read-only detector plus its targeted, idempotent
// fixer (spec §2.1). AutoFixSafe=false marks a needs-confirm check (destructive family) that
// the fix endpoint refuses without confirm=true. ReportOnly=true marks a check that carries
// NO fixer at all (Fix may be nil — the runner refuses the request before ever calling it);
// such checks always set AutoFixSafe=false so no batch path can select them either.
type Check struct {
	ID          string // e.g. "B1_PHANTOM_RELATIONSHIPS"
	Severity    string // SeverityInfo | SeverityWarn | SeverityCritical
	AutoFixSafe bool   // true = eligible for "Sửa tất cả (an toàn)"
	ReportOnly  bool   // true = no automated fix exists; fix requests are refused (ErrReportOnly)
	// Detect returns the current findings for the tenant. READ-ONLY, LIMIT-bounded
	// (MaxFindingsPerCheck+1 so the Runner can report truncation).
	Detect func(ctx context.Context, tenantID string) ([]Finding, error)
	// Fix repairs ONE finding: re-detect the single item first (return ErrAlreadyClean when
	// it no longer holds), then apply the idempotent repair via an existing helper.
	Fix func(ctx context.Context, tenantID string, f Finding) error
}

// CheckResult is one check's scan outcome — the wire shape of the scan endpoint's
// findings[] entries (camelCase JSON per api-conventions). ReportOnly tells the FE to render
// a "chỉ báo cáo, sửa tay" tag instead of a fix button.
type CheckResult struct {
	CheckID     string    `json:"checkId"`
	Severity    string    `json:"severity"`
	AutoFixSafe bool      `json:"autoFixSafe"`
	ReportOnly  bool      `json:"reportOnly"`
	Count       int       `json:"count"`
	Items       []Finding `json:"items"`
	Truncated   bool      `json:"truncated"`
}

// FixItemError is one failed item's RAW error, kept alongside the wire string so the
// orchestrating service can inspect the error chain (errors.As on the typed public-verdict
// sentinel) instead of substring-matching a marker inside the rendered message. Key is the
// finding key, or "detect" for a detect-phase failure. Never serialized.
type FixItemError struct {
	Key string
	Err error
}

// FixResult is one check's fix outcome — the wire shape of the fix endpoint's results[]
// entries. Errors carries one "key: message" string per failed item; ItemErrors carries the
// raw errors 1:1 with Errors (same order) for the service-layer sanitizer.
type FixResult struct {
	CheckID    string         `json:"checkId"`
	Fixed      int            `json:"fixed"`
	Skipped    int            `json:"skipped"`
	Failed     int            `json:"failed"`
	Errors     []string       `json:"errors"`
	ItemErrors []FixItemError `json:"-"`
}

// Runner drives the registry: Scan runs every check's Detect; Fix re-detects then repairs
// the requested checks. It is deliberately free of DB/log/audit dependencies — orchestration
// concerns (audit rows, zap, cache invalidation) live in the service that builds the checks.
type Runner struct {
	checks []Check
	byID   map[string]Check
}

// NewRunner builds a runner over the registered checks. Check IDs are authored constants
// and must be unique; the registry is tiny and wired at boot, so this is a programmer
// contract rather than a runtime validation.
func NewRunner(checks []Check) *Runner {
	byID := make(map[string]Check, len(checks))
	for _, ck := range checks {
		byID[ck.ID] = ck
	}
	return &Runner{checks: checks, byID: byID}
}

// Scan runs every registered check's Detect (read-only) and returns one CheckResult per
// check, in registration order — zero-count checks included, so the FE can render the full
// checklist ("Hệ thống đồng bộ ✓" when every count is 0). A single check erroring fails the
// whole scan (fail-loud: a scan that silently omits a broken check would report false health).
func (r *Runner) Scan(ctx context.Context, tenantID string) ([]CheckResult, error) {
	out := make([]CheckResult, 0, len(r.checks))
	for _, ck := range r.checks {
		findings, err := ck.Detect(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("doctor.Scan %s: %w", ck.ID, err)
		}
		truncated := false
		if len(findings) > MaxFindingsPerCheck {
			findings = findings[:MaxFindingsPerCheck]
			truncated = true
		}
		if findings == nil {
			findings = []Finding{} // marshal as [] never null
		}
		out = append(out, CheckResult{
			CheckID:     ck.ID,
			Severity:    ck.Severity,
			AutoFixSafe: ck.AutoFixSafe,
			ReportOnly:  ck.ReportOnly,
			Count:       len(findings),
			Items:       findings,
			Truncated:   truncated,
		})
	}
	return out, nil
}

// Fix repairs the requested checks. The whole request is validated FIRST — an unknown id or
// a needs-confirm check without confirm rejects the request before any mutation. Then, per
// check (duplicates collapsed, first occurrence order): re-run Detect fresh (never trust a
// stale scan report), fix each finding, record per-item failures and continue. A Detect
// error during the fix phase is recorded on that check's result and the run continues to
// the next check (re-run resumes).
func (r *Runner) Fix(ctx context.Context, tenantID string, checkIDs []string, confirm bool) ([]FixResult, error) {
	seen := make(map[string]bool, len(checkIDs))
	selected := make([]Check, 0, len(checkIDs))
	for _, id := range checkIDs {
		ck, ok := r.byID[id]
		if !ok {
			return nil, fmt.Errorf("doctor.Fix: %w: %q", ErrUnknownCheck, id)
		}
		if ck.ReportOnly {
			return nil, fmt.Errorf("doctor.Fix: %w: %q", ErrReportOnly, id)
		}
		if !ck.AutoFixSafe && !confirm {
			return nil, fmt.Errorf("doctor.Fix: %w: %q", ErrConfirmRequired, id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		selected = append(selected, ck)
	}

	results := make([]FixResult, 0, len(selected))
	for _, ck := range selected {
		res := FixResult{CheckID: ck.ID, Errors: []string{}}
		findings, err := ck.Detect(ctx, tenantID)
		if err != nil {
			// Can't even enumerate this check — record and continue with the others.
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("detect: %v", err))
			res.ItemErrors = append(res.ItemErrors, FixItemError{Key: "detect", Err: err})
			results = append(results, res)
			continue
		}
		if len(findings) > MaxFindingsPerCheck {
			// Bounded batch — the remainder surfaces on the next run (QRR recovery model).
			findings = findings[:MaxFindingsPerCheck]
		}
		for _, f := range findings {
			switch err := ck.Fix(ctx, tenantID, f); {
			case err == nil:
				res.Fixed++
			case errors.Is(err, ErrAlreadyClean):
				res.Skipped++
			default:
				res.Failed++
				res.Errors = append(res.Errors, f.Key+": "+err.Error())
				res.ItemErrors = append(res.ItemErrors, FixItemError{Key: f.Key, Err: err})
			}
		}
		results = append(results, res)
	}
	return results, nil
}

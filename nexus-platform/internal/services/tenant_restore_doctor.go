package services

import (
	"context"
	"fmt"

	"nexus/platform/internal/doctor"
)

// restoreDoctorChecks are the checks a tenant restore repairs afterwards: the generated columns
// and mandatory DDL of obj_ tables whose data just came back, and the obj_/rel_ tables themselves
// (specs/068-tenant-backup-restore, Khôi phục bước 6).
var restoreDoctorChecks = []string{"A1_MISSING_GEN_COLUMN", "A5_MISSING_MANDATORY_DDL", "A6_MISSING_OBJ_TABLE", "B4_MISSING_JOIN_TABLE"}

// TenantRestoreDoctor adapts DoctorService to tenantbackup.RestoreDoctor (that package imports
// neither services nor doctor).
type TenantRestoreDoctor struct{ d *DoctorService }

// NewTenantRestoreDoctor wraps the shared DoctorService.
func NewTenantRestoreDoctor(d *DoctorService) *TenantRestoreDoctor { return &TenantRestoreDoctor{d: d} }

// Repair runs Fix on the restore checks, then a full Scan, and returns what was fixed and how many
// critical findings remain.
func (t *TenantRestoreDoctor) Repair(ctx context.Context, tenantID string) (fixed, remainingCritical int, err error) {
	results, err := t.d.Fix(ctx, tenantID, restoreDoctorChecks, true)
	if err != nil {
		return 0, 0, fmt.Errorf("services.TenantRestoreDoctor.Repair: fix: %w", err)
	}
	for _, r := range results {
		fixed += r.Fixed
	}
	scan, err := t.d.Scan(ctx, tenantID)
	if err != nil {
		return fixed, 0, fmt.Errorf("services.TenantRestoreDoctor.Repair: scan: %w", err)
	}
	for _, c := range scan {
		if c.Severity == doctor.SeverityCritical {
			remainingCritical += c.Count
		}
	}
	return fixed, remainingCritical, nil
}

// FlushCaches drops the in-process metadata / ACL / workflow / user caches.
func (t *TenantRestoreDoctor) FlushCaches(ctx context.Context, tenantID string) {
	t.d.FlushCaches(ctx, tenantID)
}

// doctor_bpm_duplicate_check.go — D4 (specs/043 S9a, T263): records that hold two or more PENDING
// approvals. The old start-up race could leave such a pair; Migration B (uq_ai_open, one open
// approval per record) refuses to migrate over it and never deletes data on its own.
//
// RUNBOOK (before deploying Migration B): run this check on a COPY of the production database;
// for each pair Nexus decides which one stays and withdraws the other by hand (Thu hồi on the
// record, or cancel the row), then re-scans until it reads 0, then migrates. REPORT-ONLY: choosing
// which approval of a pair is the real one is a business decision, not a repair a script may make.
package services

import (
	"context"
	"fmt"
	"strings"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/doctor"
)

// DoctorCheckApprovalDuplicatePending is D4.
const DoctorCheckApprovalDuplicatePending = "D4_APPROVAL_DUPLICATE_PENDING"

// doctorApprovalDuplicates is the slice of *bpm.SQLRepository D4 reads (tenant-pinned, read-only).
type doctorApprovalDuplicates interface {
	ListDuplicatePending(ctx context.Context, tenantID string, limit int) ([]bpm.DuplicatePendingRow, error)
}

// WithApprovalDuplicates registers D4 over the BPM repository. Chainable.
func (s *DoctorService) WithApprovalDuplicates(r doctorApprovalDuplicates) *DoctorService {
	s.approvalDuplicates = r
	s.runner = doctor.NewRunner(s.checks())
	return s
}

func (s *DoctorService) checkApprovalDuplicatePending() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckApprovalDuplicatePending,
		Severity:    doctor.SeverityWarn, // blocks Migration B until a person has settled each pair
		AutoFixSafe: false,
		ReportOnly:  true, // which approval of a pair is the real one is Nexus's call
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			rows, err := s.approvalDuplicates.ListDuplicatePending(ctx, tenantID, doctor.MaxFindingsPerCheck+1)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.D4 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0, len(rows))
			for _, r := range rows {
				findings = append(findings, doctor.Finding{
					Key: r.EntityName + "/" + r.RecordID,
					Detail: fmt.Sprintf("Bản ghi %s/%s có %d hồ sơ duyệt cùng đang chờ (%s) — giữ một, thu hồi các hồ sơ còn lại bằng tay trước khi chạy Migration B; hệ thống không tự huỷ hồ sơ nào.",
						r.EntityName, r.RecordID, len(r.InstanceIDs), strings.Join(r.InstanceIDs, ", ")),
				})
			}
			return findings, nil
		},
	}
}

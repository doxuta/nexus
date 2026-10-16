// doctor_bpm_seats_check.go — D3 (specs/043 S3a2, T063): pending approvals whose inbox still rests
// on the projection fallback of bpm.AwaitsActor — no open work seat at all, or open seats none of
// which agrees with the instance (a pre-S3a binary moved it without touching seats: rolling deploy,
// or a binary rollback on schema 000070). The inbox still shows these rows through
// current_approver_*, so nothing is broken for a user; the count exists so Nexus knows when the
// fallback can be removed (specs/043 "Nợ còn mở" 9: at zero on every tenant).
//
// REPORT-ONLY: the repair is the engine's — the next decision/recall/delegation on the instance
// writes the right seat (S3a heals on touch; S3b creates the seat inside the locked transaction).
// A Doctor write here would be a second seat writer outside the instance's lock.
package services

import (
	"context"
	"fmt"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/doctor"
)

// DoctorCheckApprovalSeatDrift is D3 — the D-class holds record-data checks; approval instances are
// runtime data of the same kind.
const DoctorCheckApprovalSeatDrift = "D3_APPROVAL_SEAT_DRIFT"

// doctorApprovalSeats is the slice of *bpm.SQLRepository D3 reads (tenant-pinned, read-only).
type doctorApprovalSeats interface {
	ListSeatDrift(ctx context.Context, tenantID string, limit int) ([]bpm.SeatDriftRow, error)
}

// WithApprovalSeats registers D3 over the BPM repository. Chainable.
func (s *DoctorService) WithApprovalSeats(r doctorApprovalSeats) *DoctorService {
	s.approvalSeats = r
	s.runner = doctor.NewRunner(s.checks())
	return s
}

func (s *DoctorService) checkApprovalSeatDrift() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckApprovalSeatDrift,
		Severity:    doctor.SeverityInfo, // the inbox still shows every one of them (projection fallback)
		AutoFixSafe: false,
		ReportOnly:  true, // the engine heals a seat on the instance's next write; the Doctor never writes one
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			rows, err := s.approvalSeats.ListSeatDrift(ctx, tenantID, doctor.MaxFindingsPerCheck+1)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.D3 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0, len(rows))
			for _, r := range rows {
				why := "chưa có ghế việc nào"
				if r.Drift == bpm.SeatDriftMismatch {
					why = "ghế việc đang mở lệch với hồ sơ (khác bước hoặc khác người duyệt)"
				}
				findings = append(findings, doctor.Finding{
					Key: r.InstanceID,
					Detail: fmt.Sprintf("Hồ sơ duyệt %s (%s/%s) đang chờ nhưng %s — hộp duyệt vẫn hiện nó qua người duyệt ghi trên hồ sơ; lần duyệt/uỷ quyền/thu hồi kế tiếp tự sửa ghế.",
						r.InstanceID, r.EntityName, r.RecordID, why),
				})
			}
			return findings, nil
		},
	}
}

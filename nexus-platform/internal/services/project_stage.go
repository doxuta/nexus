package services

import "time"

// Project stages — machine values of GET /admin/projects `stage` (specs/062-customer-workspace
// FR-026). The word "maintenance" is deliberately absent: it belongs to the write freeze of
// specs/068-tenant-backup-restore (maintenanceUntil, TENANT_MAINTENANCE), and the two flags mean
// different things on the same card.
const (
	StageBuilding       = "building"        // Đang dựng
	StageTrial          = "trial"           // Chạy thử
	StageLive           = "live"            // Đang vận hành
	StageNeedsAttention = "needs_attention" // Cần bảo trì
)

// DeriveStage is the single place a project's stage is decided. Pure on purpose: the stage is never
// stored, so it cannot go stale or be set by hand.
//
//   - delivered && health critical => needs_attention
//   - delivered                    => live (the trial switch is irrelevant once delivered)
//   - trial mark set               => trial
//   - otherwise                    => building
//
// health is the stored health_status only ("ok" | "warn" | "critical" | "unknown"); the optional
// health.ops operating signal is never an input (it would feed itself).
func DeriveStage(deliveredAt, trialSince *time.Time, health string) string {
	switch {
	case deliveredAt != nil && health == "critical":
		return StageNeedsAttention
	case deliveredAt != nil:
		return StageLive
	case trialSince != nil:
		return StageTrial
	}
	return StageBuilding
}

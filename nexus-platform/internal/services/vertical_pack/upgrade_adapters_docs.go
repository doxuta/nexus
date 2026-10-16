// Pack upgrade adapters for the "document" item types — reports, dashboards and screen layouts
// (../specs/069-pack-apply-upgrade/plan.md §5, slice L2b). Same rule as the workflow adapter: write
// through the admin service the Studio uses, never around it. A layout is never deleted (an upgrade
// that dropped a screen would strand its users); reports and dashboards soft-delete like the screens.
package vertical_pack

import (
	"context"
	"encoding/json"
	"fmt"

	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

// reportUpdater / dashboardUpdater are the update halves of the admin services; the compile-time
// pins break the build, not an upgrade at runtime, when a service changes a signature.
type reportUpdater interface {
	UpdateReport(ctx context.Context, tenantID, id string, in services.UpdateReportInput) (*report.Record, error)
	DeleteReport(ctx context.Context, tenantID, id string) error
}

type dashboardUpdater interface {
	UpdateDashboard(ctx context.Context, tenantID, id string, in services.UpdateDashboardInput) (*dashboard.Record, error)
	DeleteDashboard(ctx context.Context, tenantID, id string) error
}

var (
	_ reportUpdater    = (*services.ReportService)(nil)
	_ dashboardUpdater = (*services.DashboardService)(nil)
)

func init() {
	upgradeAdapters[ItemReport] = upgradeAdapter{
		ready: func(s *Service) bool { _, ok := s.reportWr.(reportUpdater); return ok },
		write: writeReport,
		remove: func(ctx context.Context, e *applyEnv, it *UpgradeItem) error {
			u, _ := e.s.reportWr.(reportUpdater)
			if it.LiveID == "" {
				return invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết báo cáo nào trong workspace ứng với mục này")
			}
			if err := u.DeleteReport(ctx, e.tenantID, it.LiveID); err != nil {
				return fmt.Errorf("vertical_pack.upgrade.deleteReport: %w", err)
			}
			return nil
		},
	}
	upgradeAdapters[ItemDashboard] = upgradeAdapter{
		ready: func(s *Service) bool { _, ok := s.dashboardWr.(dashboardUpdater); return ok },
		write: writeDashboard,
		remove: func(ctx context.Context, e *applyEnv, it *UpgradeItem) error {
			u, _ := e.s.dashboardWr.(dashboardUpdater)
			if it.LiveID == "" {
				return invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết dashboard nào trong workspace ứng với mục này")
			}
			if err := u.DeleteDashboard(ctx, e.tenantID, it.LiveID); err != nil {
				return fmt.Errorf("vertical_pack.upgrade.deleteDashboard: %w", err)
			}
			return nil
		},
	}
	// remove == nil ⇒ the plan never offers "Xoá" for a layout.
	upgradeAdapters[ItemLayout] = upgradeAdapter{
		ready: func(s *Service) bool { return s.layoutWr != nil },
		write: writeLayout,
	}
}

// needsCreate: is the live row absent, so the write must create rather than update in place? A
// "protected" row lost its original status to the flag, so the live side decides.
func needsCreate(it *UpgradeItem) bool {
	switch it.Status {
	case StatusAdded, StatusDeletedByBuilder, StatusConflictDeleted:
		return true
	case StatusProtected:
		return len(it.Live) == 0
	}
	return false
}

// packItem decodes the secret-stripped new-pack content of one item.
func (e *applyEnv) packItem(typ, key string, into any) error {
	raw, ok := e.theirs[typ+"/"+key]
	if !ok {
		return invalid("UPGRADE_PACK_ITEM_MISSING", "Bản mới của gói không còn mục này")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("vertical_pack.upgrade.packItem: decode %s/%q: %w", typ, key, err)
	}
	return nil
}

func writeReport(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	var pr PackReport
	if err := e.packItem(ItemReport, it.ItemKey, &pr); err != nil {
		return "", err
	}
	in := toReportInput(pr, e.userID)
	if needsCreate(it) {
		rec, err := e.s.reportWr.CreateReport(ctx, e.tenantID, in)
		if err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.createReport: %w", err)
		}
		if rec == nil {
			return "", nil
		}
		return rec.ID, nil
	}
	if it.LiveID == "" {
		return "", invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết báo cáo nào trong workspace ứng với mục này")
	}
	u, _ := e.s.reportWr.(reportUpdater)
	if _, err := u.UpdateReport(ctx, e.tenantID, it.LiveID, services.UpdateReportInput{
		Name: in.Name, EntityName: in.EntityName, Type: in.Type, Config: in.Config, IsActive: in.IsActive, ActorID: in.ActorID,
	}); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.updateReport: %w", err)
	}
	return it.LiveID, nil
}

func writeDashboard(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	var pd PackDashboard
	if err := e.packItem(ItemDashboard, it.ItemKey, &pd); err != nil {
		return "", err
	}
	in := toDashboardInput(pd, e.userID)
	if needsCreate(it) {
		rec, err := e.s.dashboardWr.CreateDashboard(ctx, e.tenantID, in)
		if err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.createDashboard: %w", err)
		}
		if rec == nil {
			return "", nil
		}
		return rec.ID, nil
	}
	if it.LiveID == "" {
		return "", invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết dashboard nào trong workspace ứng với mục này")
	}
	u, _ := e.s.dashboardWr.(dashboardUpdater)
	if _, err := u.UpdateDashboard(ctx, e.tenantID, it.LiveID, services.UpdateDashboardInput{
		Name: in.Name, Description: in.Description, Widgets: in.Widgets,
	}); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.updateDashboard: %w", err)
	}
	return it.LiveID, nil
}

// writeLayout upserts the screen (SaveLayout is keyed by tenant + entity + view, so there is no
// live id to track and nothing to create-versus-update).
func writeLayout(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	var pl PackLayout
	if err := e.packItem(ItemLayout, it.ItemKey, &pl); err != nil {
		return "", err
	}
	if _, err := e.s.layoutWr.SaveLayout(ctx, e.tenantID, services.SaveLayoutInput{
		EntityName: pl.EntityName, ViewType: pl.ViewType, LayoutJSON: pl.LayoutJSON,
	}); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.saveLayout: %w", err)
	}
	return "", nil
}

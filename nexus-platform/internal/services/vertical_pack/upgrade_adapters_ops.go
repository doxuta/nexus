// Pack upgrade adapters for the "operations" item types — work queues, approval processes, teams
// and inbound webhook hooks (../specs/069-pack-apply-upgrade/plan.md §5, slice L5). Same rule as
// the other adapters: write through the admin service the Studio uses, never around it.
//
// Three promises that matter more than the writes themselves:
//   - an approval edit never touches a request already waiting (the engine snapshots the steps per
//     request, so UpdateProcess only changes what NEW requests see);
//   - a team is only ever created (its members are the customer's);
//   - an inbound hook is never deleted and never re-keyed: Update leaves the signing key alone, so a
//     partner already sending keeps working; Create mints a fresh key and the result says so.
package vertical_pack

import (
	"context"
	"fmt"
	"strings"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workqueue"
)

type workQueueUpdater interface {
	Update(ctx context.Context, tenantID, id string, in services.WorkQueueInput) (*workqueue.Queue, error)
	Delete(ctx context.Context, tenantID, id string) error
}

type approvalUpdater interface {
	UpdateProcess(ctx context.Context, tenantID, id string, in services.UpdateApprovalProcessInput) (*bpm.ProcessRecord, error)
	DeleteProcess(ctx context.Context, tenantID, id string) error
}

type hookUpdater interface {
	Update(ctx context.Context, tenantID, id string, in services.InboundHookInput) (*inboundhook.Hook, error)
}

var (
	_ workQueueUpdater = (*services.WorkQueueService)(nil)
	_ approvalUpdater  = (*services.ApprovalService)(nil)
	_ hookUpdater      = (*services.InboundHookService)(nil)
)

func init() {
	upgradeAdapters[ItemWorkQueue] = upgradeAdapter{
		ready: func(s *Service) bool { _, ok := s.workQueueWr.(workQueueUpdater); return ok },
		write: writeWorkQueue,
		remove: func(ctx context.Context, e *applyEnv, it *UpgradeItem) error {
			u, _ := e.s.workQueueWr.(workQueueUpdater)
			if it.LiveID == "" {
				return invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết hàng đợi nào trong workspace ứng với mục này")
			}
			if err := u.Delete(ctx, e.tenantID, it.LiveID); err != nil {
				return fmt.Errorf("vertical_pack.upgrade.deleteWorkQueue: %w", err)
			}
			return nil
		},
	}
	upgradeAdapters[ItemApproval] = upgradeAdapter{
		ready: func(s *Service) bool { _, ok := s.approvalWr.(approvalUpdater); return ok },
		write: writeApproval,
		remove: func(ctx context.Context, e *applyEnv, it *UpgradeItem) error {
			u, _ := e.s.approvalWr.(approvalUpdater)
			if it.LiveID == "" {
				return invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết quy trình duyệt nào trong workspace ứng với mục này")
			}
			if err := u.DeleteProcess(ctx, e.tenantID, it.LiveID); err != nil {
				return fmt.Errorf("vertical_pack.upgrade.deleteApproval: %w", err)
			}
			return nil
		},
	}
	// remove == nil for both below ⇒ the plan never offers "Xoá".
	upgradeAdapters[ItemTeam] = upgradeAdapter{
		ready: func(s *Service) bool { return s.teamWr != nil },
		write: writeTeam,
	}
	upgradeAdapters[ItemInboundHook] = upgradeAdapter{
		ready: func(s *Service) bool { _, ok := s.hookWr.(hookUpdater); return ok },
		write: writeInboundHook,
	}
}

func writeWorkQueue(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	var pq PackWorkQueue
	if err := e.packItem(ItemWorkQueue, it.ItemKey, &pq); err != nil {
		return "", err
	}
	roleID := ""
	if rn := strings.TrimSpace(pq.RoleName); rn != "" {
		roles, err := e.roleIDByName(ctx)
		if err != nil {
			return "", err
		}
		if roleID = roles[rn]; roleID == "" {
			return "", invalid("WORKQUEUE_ROLE_NOT_FOUND", "hàng đợi %q: vai %q không có trong workspace", pq.Name, rn)
		}
	}
	in := toWorkQueueInput(pq, roleID)
	if needsCreate(it) {
		rec, err := e.s.workQueueWr.Create(ctx, e.tenantID, in)
		if err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.createWorkQueue: %w", err)
		}
		if rec == nil {
			return "", nil
		}
		return rec.ID, nil
	}
	if it.LiveID == "" {
		return "", invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết hàng đợi nào trong workspace ứng với mục này")
	}
	u, _ := e.s.workQueueWr.(workQueueUpdater)
	if _, err := u.Update(ctx, e.tenantID, it.LiveID, in); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.updateWorkQueue: %w", err)
	}
	return it.LiveID, nil
}

// queueMap: queue name → id, read from the live tenant (a queue made earlier in this run is there).
func (e *applyEnv) queueMap(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if e.s.workQueueWr == nil {
		return out, nil
	}
	qs, err := e.s.workQueueWr.ListAdmin(ctx, e.tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.applyEnv.queueMap: %w", err)
	}
	for _, q := range qs {
		out[strings.TrimSpace(q.Name)] = q.ID
	}
	return out, nil
}

func writeApproval(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	var pa PackApproval
	if err := e.packItem(ItemApproval, it.ItemKey, &pa); err != nil {
		return "", err
	}
	roles, err := e.roleIDByName(ctx)
	if err != nil {
		return "", err
	}
	teams, err := e.teamMap(ctx)
	if err != nil {
		return "", err
	}
	queues, err := e.queueMap(ctx)
	if err != nil {
		return "", err
	}
	in, unresolved, kind := toApprovalInput(pa, e.userID, approvalRefs{
		role:  func(ref string) string { return roles[strings.TrimSpace(ref)] },
		team:  func(ref string) string { return teams[strings.TrimSpace(ref)] },
		queue: func(ref string) string { return queues[strings.TrimSpace(ref)] },
	})
	if unresolved == approvalUserApprover {
		return "", invalid("APPROVAL_APPROVER_NOT_PORTABLE", "quy trình duyệt %q: người duyệt chỉ định đích danh không nâng cấp được — gán lại thủ công", pa.Name)
	}
	if unresolved != "" {
		return "", invalid("APPROVAL_APPROVER_NOT_FOUND", "quy trình duyệt %q: %s %q không có trong workspace", pa.Name, kind, unresolved)
	}
	if needsCreate(it) {
		rec, err := e.s.approvalWr.CreateProcess(ctx, e.tenantID, in)
		if err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.createApproval: %w", err)
		}
		if rec == nil {
			return "", nil
		}
		e.processMapAdd(approvalDupKey(strings.TrimSpace(pa.EntityName), strings.TrimSpace(pa.Name)), rec.ID)
		return rec.ID, nil
	}
	if it.LiveID == "" {
		return "", invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết quy trình duyệt nào trong workspace ứng với mục này")
	}
	u, _ := e.s.approvalWr.(approvalUpdater)
	if _, err := u.UpdateProcess(ctx, e.tenantID, it.LiveID, services.UpdateApprovalProcessInput{
		Name: in.Name, EntityName: in.EntityName, Trigger: in.Trigger, Match: in.Match, Rules: in.Rules,
		Steps: in.Steps, IsActive: in.IsActive, ActorID: in.ActorID, Outcome: in.Outcome,
	}); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.updateApproval: %w", err)
	}
	return it.LiveID, nil
}

// processMapAdd keeps the lazily built process map in step with a process made in this run.
func (e *applyEnv) processMapAdd(key, id string) {
	if e.processIDs != nil {
		e.processIDs[key] = id
	}
}

// writeTeam creates the team and seeds the actor as its first member. A team that already exists is
// left exactly as it is: a team is a name, and who sits in it belongs to the customer.
func writeTeam(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	var pt PackTeam
	if err := e.packItem(ItemTeam, it.ItemKey, &pt); err != nil {
		return "", err
	}
	if !needsCreate(it) {
		return it.LiveID, nil
	}
	team := &auth.Team{ID: newInstallID(), TenantID: e.tenantID, Name: strings.TrimSpace(pt.Name)}
	if err := e.s.teamWr.CreateTeam(ctx, team); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.createTeam: %w", err)
	}
	if e.teamIDs != nil {
		e.teamIDs[team.Name] = team.ID
	}
	// A convenience, never a reason to fail (same as install): the builder is often not a member of
	// the customer's tenant. Say so instead.
	if err := e.s.teamWr.AddMember(ctx, e.tenantID, team.ID, e.userID, newInstallID()); err != nil {
		e.note(it, "TEAM_NO_MEMBERS", fmt.Sprintf("Đội %q đã tạo nhưng chưa có thành viên — thêm người vào đội trước khi bật định tuyến vòng tròn", team.Name))
	}
	return team.ID, nil
}

// writeInboundHook creates the receiver (fresh signing key) or updates its config in place. Update
// goes through InboundHookService.Update, which never touches the key.
func writeInboundHook(ctx context.Context, e *applyEnv, it *UpgradeItem) (string, error) {
	var ph PackInboundHook
	if err := e.packItem(ItemInboundHook, it.ItemKey, &ph); err != nil {
		return "", err
	}
	in := toInboundHookInput(ph)
	if needsCreate(it) {
		h, err := e.s.hookWr.Create(ctx, e.tenantID, e.userID, in)
		if err != nil {
			return "", fmt.Errorf("vertical_pack.upgrade.createInboundHook: %w", err)
		}
		e.note(it, "NEW_HOOK_SECRET", fmt.Sprintf("Webhook nhận %q đã tạo với khoá ký MỚI — lấy khoá trong Cài đặt rồi mới báo đối tác gửi", ph.Name))
		if h == nil {
			return "", nil
		}
		return h.ID, nil
	}
	if it.LiveID == "" {
		return "", invalid("UPGRADE_LIVE_ID_UNKNOWN", "Không biết webhook nhận nào trong workspace ứng với mục này")
	}
	u, _ := e.s.hookWr.(hookUpdater)
	if _, err := u.Update(ctx, e.tenantID, it.LiveID, in); err != nil {
		return "", fmt.Errorf("vertical_pack.upgrade.updateInboundHook: %w", err)
	}
	return it.LiveID, nil
}

// note records an info-level caveat of a successful write; applyItem copies it onto the result.
func (e *applyEnv) note(it *UpgradeItem, code, msg string) {
	if e.notes == nil {
		e.notes = map[string][2]string{}
	}
	e.notes[it.ItemType+"/"+it.ItemKey] = [2]string{code, msg}
}

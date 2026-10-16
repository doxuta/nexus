package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/notify"
)

// notifyFakeNotifier records every SecurityEvent enqueued so a test can assert WHICH alert
// fired (type/tenant/user/email) and HOW MANY times. It can be programmed to panic to prove
// the notifier is fail-open (a misbehaving notifier must never break the business flow).
// notify-prefixed to avoid colliding with other fakes in the services test package.
type notifyFakeNotifier struct {
	events    []notify.SecurityEvent
	panicMsg  string // when non-empty, EnqueueSecurityAlert panics with it (fail-open probe)
	callCount int
}

func (f *notifyFakeNotifier) EnqueueSecurityAlert(_ context.Context, evt notify.SecurityEvent) {
	f.callCount++
	if f.panicMsg != "" {
		panic(f.panicMsg)
	}
	f.events = append(f.events, evt)
}

// newNotifyUserSvc builds a user service wired to a recording notifier (and a no-op revoker so
// the W1 path is inert and does not interfere with the notify assertions). Returns the service,
// the repo to seed, and the notifier to inspect.
func newNotifyUserSvc(n *notifyFakeNotifier) (*UserService, *fakeUserCRUDRepo) {
	repo := newFakeUserCRUDRepo()
	svc := NewUserService(repo).WithNotifier(n)
	i := 0
	svc.newID = func() string {
		i++
		return "ntf-id-" + string(rune('0'+i))
	}
	return svc, repo
}

// E1: changing a user's password emits exactly one password_changed alert for the right user.
func TestNotify_UpdateUserPasswordChangeEmitsE1(t *testing.T) {
	n := &notifyFakeNotifier{}
	svc, repo := newNotifyUserSvc(n)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	newPass := "Str0ngNewP@ss!"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &newPass}); err != nil {
		t.Fatalf("password change: %v", err)
	}
	if len(n.events) != 1 {
		t.Fatalf("emitted %d events; want exactly 1 on password change", len(n.events))
	}
	e := n.events[0]
	if e.Type != notify.AlertPasswordChanged {
		t.Fatalf("event type = %q; want %q", e.Type, notify.AlertPasswordChanged)
	}
	if e.TenantID != "t1" || e.UserID != "u1" {
		t.Fatalf("event tenant/user = %s/%s; want t1/u1", e.TenantID, e.UserID)
	}
	if e.Email != "u1@acme.test" {
		t.Fatalf("event email = %q; want the user's email snapshot", e.Email)
	}
}

// E2: changing role_id emits role_changed.
func TestNotify_UpdateUserRoleChangeEmitsE2(t *testing.T) {
	n := &notifyFakeNotifier{}
	svc, repo := newNotifyUserSvc(n)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	role := "role-x"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{RoleID: &role}); err != nil {
		t.Fatalf("role change: %v", err)
	}
	if len(n.events) != 1 || n.events[0].Type != notify.AlertRoleChanged {
		t.Fatalf("events = %+v; want exactly one role_changed", n.events)
	}
}

// E2: toggling is_admin is a privilege change → role_changed (the §2 'đổi quyền' event).
func TestNotify_UpdateUserIsAdminChangeEmitsE2(t *testing.T) {
	n := &notifyFakeNotifier{}
	svc, repo := newNotifyUserSvc(n)
	w1Seed(repo, "t1", "admin-1", true, "active", nil)
	w1Seed(repo, "t1", "admin-2", true, "active", nil) // sibling admin so demote is allowed

	no := false
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "admin-1", UpdateUserInput{IsAdmin: &no}); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if len(n.events) != 1 || n.events[0].Type != notify.AlertRoleChanged {
		t.Fatalf("events = %+v; want exactly one role_changed on is_admin toggle", n.events)
	}
}

// E3: disabling an account (status leaving active) emits account_disabled.
func TestNotify_UpdateUserDisableEmitsE3(t *testing.T) {
	n := &notifyFakeNotifier{}
	svc, repo := newNotifyUserSvc(n)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	disabled := "disabled"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Status: &disabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if len(n.events) != 1 || n.events[0].Type != notify.AlertAccountDisabled {
		t.Fatalf("events = %+v; want exactly one account_disabled", n.events)
	}
}

// E4: deleting an account emits account_deleted for that user.
func TestNotify_DeleteUserEmitsE4(t *testing.T) {
	n := &notifyFakeNotifier{}
	svc, repo := newNotifyUserSvc(n)
	w1Seed(repo, "t1", "admin-1", true, "active", nil) // sole admin survives
	w1Seed(repo, "t1", "member-1", false, "active", nil)

	if err := svc.DeleteUser(context.Background(), "t1", "admin-1", true, "member-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(n.events) != 1 {
		t.Fatalf("emitted %d events; want exactly 1 on delete", len(n.events))
	}
	e := n.events[0]
	if e.Type != notify.AlertAccountDeleted || e.TenantID != "t1" || e.UserID != "member-1" {
		t.Fatalf("event = %+v; want account_deleted for t1/member-1", e)
	}
}

// Control: an update that changes NOTHING sensitive (only full_name) emits NO alert.
func TestNotify_UpdateUserNonSensitiveEmitsNothing(t *testing.T) {
	n := &notifyFakeNotifier{}
	svc, repo := newNotifyUserSvc(n)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	name := "Renamed"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{FullName: &name}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(n.events) != 0 {
		t.Fatalf("emitted %+v on a benign profile edit; want none", n.events)
	}
}

// Control: re-enabling a user (status back TO active) is not a disable → no alert.
func TestNotify_UpdateUserEnableEmitsNothing(t *testing.T) {
	n := &notifyFakeNotifier{}
	svc, repo := newNotifyUserSvc(n)
	w1Seed(repo, "t1", "u1", false, "disabled", nil)

	active := "active"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Status: &active}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if len(n.events) != 0 {
		t.Fatalf("emitted %+v on re-enable; want none", n.events)
	}
}

// Combined password + role change: spec §2 lists these as two distinct events (E1 + E2), so a
// combined update emits BOTH (one password_changed and one role_changed). This differs from the
// W1 revoke which fires exactly once — alerting is per distinct sensitive event.
func TestNotify_UpdateUserPasswordAndRoleEmitsBoth(t *testing.T) {
	n := &notifyFakeNotifier{}
	svc, repo := newNotifyUserSvc(n)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	pass := "Str0ngNewP@ss!"
	role := "role-x"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &pass, RoleID: &role}); err != nil {
		t.Fatalf("password+role: %v", err)
	}
	types := map[string]int{}
	for _, e := range n.events {
		types[e.Type]++
	}
	if types[notify.AlertPasswordChanged] != 1 || types[notify.AlertRoleChanged] != 1 {
		t.Fatalf("event types = %v; want one password_changed and one role_changed", types)
	}
}

// Fail-open: a notifier that PANICS must NOT break UpdateUser — the password change still
// succeeds (spec §1 'lỗi gửi email KHÔNG được làm hỏng nghiệp vụ chính'). The service must
// enqueue defensively (recover) so a misbehaving notifier cannot bubble up.
func TestNotify_UpdateUserNotifierPanicIsFailOpen(t *testing.T) {
	n := &notifyFakeNotifier{panicMsg: "notifier exploded"}
	svc, repo := newNotifyUserSvc(n)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	pass := "Str0ngNewP@ss!"
	v, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &pass})
	if err != nil {
		t.Fatalf("UpdateUser failed because the notifier panicked (%v); alerting must be fail-open", err)
	}
	if v == nil {
		t.Fatalf("UpdateUser returned nil view despite success")
	}
	if n.callCount == 0 {
		t.Fatalf("notifier was never invoked; the E1 enqueue must still be attempted")
	}
}

// Fail-open: deleting with a panicking notifier still deletes (E4 emission is best-effort).
func TestNotify_DeleteUserNotifierPanicIsFailOpen(t *testing.T) {
	n := &notifyFakeNotifier{panicMsg: "boom"}
	svc, repo := newNotifyUserSvc(n)
	w1Seed(repo, "t1", "admin-1", true, "active", nil)
	w1Seed(repo, "t1", "member-1", false, "active", nil)

	if err := svc.DeleteUser(context.Background(), "t1", "admin-1", true, "member-1"); err != nil {
		t.Fatalf("DeleteUser failed because the notifier panicked (%v); must be fail-open", err)
	}
}

// Default: a user service built WITHOUT a notifier (the noop default) does not blow up and does
// not attempt to notify — the alert path is inert until WithNotifier is wired.
func TestNotify_NoNotifierWiredIsSafe(t *testing.T) {
	repo := newFakeUserCRUDRepo()
	svc := NewUserService(repo) // no WithNotifier → noopNotifier
	w1Seed(repo, "t1", "u1", false, "active", nil)

	pass := "Str0ngNewP@ss!"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &pass}); err != nil {
		t.Fatalf("update with no notifier should be safe: %v", err)
	}
}

// Gate ordering: a non-admin actor is refused before ANY alert is emitted (the alert must not
// leak that an update was attempted; the gate fronts notification just as it fronts the write).
func TestNotify_UpdateUserNonAdminEmitsNothing(t *testing.T) {
	n := &notifyFakeNotifier{}
	svc, repo := newNotifyUserSvc(n)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	pass := "Str0ngNewP@ss!"
	_, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", false, "u1", UpdateUserInput{Password: &pass})
	if !errors.Is(err, ErrUserForbidden) {
		t.Fatalf("err = %v; want ErrUserForbidden for non-admin", err)
	}
	if n.callCount != 0 {
		t.Fatalf("notifier invoked %d times despite forbidden update; want 0", n.callCount)
	}
}

package services

import (
	"context"
	"encoding/json"
	"testing"
)

// specs/065 D-12: only GET /auth/me reports canManageUsers (an explicit true/false); Login keeps its
// exact pre-065 key set so existing clients parse the same bytes.
func TestMe_CanManageUsers_OnlyMeCarriesIt(t *testing.T) {
	ctx := context.Background()
	svc, password := newTestService(t)
	repo := svc.users.(*fakeUserRepo)
	alice := repo.users["t-1\x00alice@acme.test"]
	alice.IsAdmin = false
	alice.CanManageUsers = true

	out, err := svc.Login(ctx, LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	raw, _ := json.Marshal(out)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if u, _ := m["user"].(map[string]any); u == nil {
		t.Fatalf("login response has no user: %s", raw)
	} else if _, has := u["canManageUsers"]; has {
		t.Errorf("Login response gained canManageUsers: %s", raw)
	}

	me, err := svc.Me(ctx, out.AccessToken)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if me.CanManageUsers == nil || !*me.CanManageUsers {
		t.Fatalf("Me canManageUsers = %v, want explicit true", me.CanManageUsers)
	}

	alice.CanManageUsers = false
	me, err = svc.Me(ctx, out.AccessToken)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if me.CanManageUsers == nil || *me.CanManageUsers {
		t.Fatalf("Me canManageUsers = %v, want explicit false (read fresh)", me.CanManageUsers)
	}
}

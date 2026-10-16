package tenantbackup

import (
	"encoding/json"
	"strings"
	"testing"
)

func snapWithRole(role string) *AccessSnapshot {
	s := NewAccessSnapshot()
	s.AddRole(role)
	return s
}

// One row per kind of FR-035a. b = the backup, c = the live data.
func TestDiffAccess_EachKind(t *testing.T) {
	tests := []struct {
		name  string
		setup func() (b, c *AccessSnapshot)
		check func(t *testing.T, ac AccessChanges)
	}{
		{"nothing narrowed", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := snapWithRole("r"), snapWithRole("r")
			b.AddGrant("r", "accounts", "view", "all")
			c.AddGrant("r", "accounts", "view", "all")
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.Total != 0 || len(ac.OutboundHosts) != 0 {
				t.Errorf("want zero, got %+v", ac)
			}
		}},
		{"role lost its delete action", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := snapWithRole("r"), snapWithRole("r")
			b.AddGrant("r", "accounts", "delete", "all")
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.RoleGrantsWidened != 1 || ac.Total != 1 {
				t.Errorf("got %+v", ac)
			}
		}},
		{"owner now, all in the backup: same row count, still widened", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := snapWithRole("r"), snapWithRole("r")
			b.AddGrant("r", "accounts", "view", "all")
			c.AddGrant("r", "accounts", "view", "owner")
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.RoleGrantsWidened != 1 {
				t.Errorf("owner -> all must count, got %+v", ac)
			}
		}},
		{"live is wider than the backup: not counted", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := snapWithRole("r"), snapWithRole("r")
			b.AddGrant("r", "accounts", "view", "owner")
			c.AddGrant("r", "accounts", "view", "all")
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.Total != 0 {
				t.Errorf("got %+v", ac)
			}
		}},
		{"role only in the backup is not counted", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := snapWithRole("old"), NewAccessSnapshot()
			b.AddGrant("old", "accounts", "view", "all")
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.Total != 0 {
				t.Errorf("got %+v", ac)
			}
		}},
		{"field hidden now, open (no row) in the backup", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := snapWithRole("r"), snapWithRole("r")
			c.AddFieldACL("r", "accounts", "salary", false, true)
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.FieldACLReopened != 1 {
				t.Errorf("got %+v", ac)
			}
		}},
		{"field hidden in both: unchanged", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := snapWithRole("r"), snapWithRole("r")
			b.AddFieldACL("r", "accounts", "salary", false, true)
			c.AddFieldACL("r", "accounts", "salary", false, true)
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.FieldACLReopened != 0 {
				t.Errorf("got %+v", ac)
			}
		}},
		{"share revoked since the backup", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := NewAccessSnapshot(), NewAccessSnapshot()
			b.AddShare("s1", "u1", false)
			c.AddShare("s1", "u1", true) // soft-deleted now
			c.AddUser("u1", "", "active", false)
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.SharesRegranted != 1 || ac.UsersAffected != 1 {
				t.Errorf("got %+v", ac)
			}
		}},
		{"share already revoked in the backup: not counted", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := NewAccessSnapshot(), NewAccessSnapshot()
			b.AddShare("s1", "u1", true)
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.Total != 0 {
				t.Errorf("got %+v", ac)
			}
		}},
		{"removed from a team; the user is still in the customer", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := NewAccessSnapshot(), NewAccessSnapshot()
			b.AddMember("t1", "u1")
			b.AddMember("t1", "gone")
			c.AddUser("u1", "", "active", false)
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.MembershipsReadded != 1 || ac.UsersAffected != 1 {
				t.Errorf("a user no longer in the customer must not count: %+v", ac)
			}
		}},
		{"field marked sensitive now, not in the backup", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := NewAccessSnapshot(), NewAccessSnapshot()
			b.AddField("f1", json.RawMessage(`{}`), false)
			c.AddField("f1", json.RawMessage(`{"sensitive":true}`), false)
			c.AddField("f2", json.RawMessage(`{"sensitive":true}`), false) // not in the backup at all
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.SensitiveFieldsCleared != 1 {
				t.Errorf("got %+v", ac)
			}
		}},
		{"rule off now, on in the backup, with an outbound webhook (URL carries a token)", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := NewAccessSnapshot(), NewAccessSnapshot()
			acts := json.RawMessage(`[{"type":"outbound_webhook","url":"https://partner.example/hook?token=SECRET123"},{"type":"email_alert"}]`)
			b.AddWorkflow("w1", true, false, acts)
			c.AddWorkflow("w1", false, false, acts)
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.WorkflowsReenabled != 1 || ac.OutboundDestinationsReenabled != 1 {
				t.Fatalf("got %+v", ac)
			}
			if len(ac.OutboundHosts) != 1 || ac.OutboundHosts[0] != "partner.example" {
				t.Errorf("hosts = %v", ac.OutboundHosts)
			}
			if strings.Contains(strings.Join(ac.OutboundHosts, ","), "SECRET123") {
				t.Errorf("a URL query leaked into the result")
			}
		}},
		{"rule deleted now (soft), on in the backup", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := NewAccessSnapshot(), NewAccessSnapshot()
			b.AddWorkflow("w1", true, false, nil)
			c.AddWorkflow("w1", true, true, nil)
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.WorkflowsReenabled != 1 || ac.OutboundDestinationsReenabled != 0 {
				t.Errorf("got %+v", ac)
			}
		}},
		{"destination already live in another rule: not new", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := NewAccessSnapshot(), NewAccessSnapshot()
			acts := json.RawMessage(`[{"type":"outbound_webhook","url":"https://p.example/x"}]`)
			b.AddWorkflow("w1", true, false, acts)
			c.AddWorkflow("w2", true, false, acts)
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.OutboundDestinationsReenabled != 0 {
				t.Errorf("got %+v", ac)
			}
		}},
		{"credential destination shows the key name, not an URL", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := NewAccessSnapshot(), NewAccessSnapshot()
			b.AddWorkflow("w1", true, false, json.RawMessage(`[{"type":"outbound_webhook","credentialId":"c1","path":"/v1"}]`))
			c.CredNames["c1"] = "Cổng thanh toán"
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if len(ac.OutboundHosts) != 1 || ac.OutboundHosts[0] != "khoá kết nối Cổng thanh toán" {
				t.Errorf("hosts = %v", ac.OutboundHosts)
			}
		}},
		{"users and API keys holding a widened role", func() (*AccessSnapshot, *AccessSnapshot) {
			b, c := snapWithRole("r"), snapWithRole("r")
			b.AddGrant("r", "accounts", "delete", "all")
			c.AddUser("u1", "r", "active", false)
			c.AddUser("u2", "r", "inactive", false)
			c.AddUser("u3", "other", "active", false)
			c.AddAPIKey("r")
			c.AddAPIKey("other")
			return b, c
		}, func(t *testing.T, ac AccessChanges) {
			if ac.UsersAffected != 1 || ac.APIKeysAffected != 1 {
				t.Errorf("got %+v", ac)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, c := tc.setup()
			tc.check(t, DiffAccess(b, c))
		})
	}
}

func TestDiffAccess_FieldLevel(t *testing.T) {
	tests := []struct {
		name, backup, cur string
		want              int
	}{
		{"none to field", "field:assignedTo", "none", 1},
		{"owner to field", "field:assignedTo", "owner", 1},
		{"owner backup over field live narrows", "owner", "field:assignedTo", 0},
		{"team backup over field live widens", "team", "field:assignedTo", 1},
		{"field to field same", "field:assignedTo", "field:assignedTo", 0},
		{"field live, owner backup", "owner", "field:x", 0},
		{"field backup, team live", "field:x", "team", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, c := snapWithRole("r"), snapWithRole("r")
			b.AddGrant("r", "accounts", "view", tc.backup)
			c.AddGrant("r", "accounts", "view", tc.cur)
			if got := DiffAccess(b, c).RoleGrantsWidened; got != tc.want {
				t.Errorf("got %d want %d", got, tc.want)
			}
		})
	}
}

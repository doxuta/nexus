package tenantbackup

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
)

// AccessSnapshot is everything of ONE customer's configuration that decides who may see or do
// what, read either from a backup's JSONL or from the live database (specs/068 D-24). DiffAccess
// compares two of them; it is pure so the whole rule table is unit-tested without a database.
type AccessSnapshot struct {
	Roles     map[string]bool
	Grants    map[string]int // role|entity|action -> level (absent = none)
	FieldACL  map[string]FieldACLRow
	Shares    map[string]ShareRow // live shares only, by id
	Members   map[string]bool     // team|user
	Sensitive map[string]bool     // live field id -> options.sensitive
	Workflows map[string]WorkflowRow
	Users     map[string]UserRow // users of the customer that are not deleted
	APIKeys   []string           // role id of every unrevoked key
	CredNames map[string]string  // credential id -> name (live side only; labels hosts)
}

// FieldACLRow is one sys_field_acl row.
type FieldACLRow struct{ View, Edit bool }

// ShareRow is one live record share.
type ShareRow struct{ UserID string }

// WorkflowRow is a rule: live-and-switched-on, plus the outbound destinations it posts to.
type WorkflowRow struct {
	Active bool
	Dests  []string
}

// UserRow is a user of the customer.
type UserRow struct {
	RoleID string
	Active bool
}

// AccessChanges is the part of a restore that would widen access again (FR-035a).
type AccessChanges struct {
	Total                         int      `json:"total"`
	RoleGrantsWidened             int      `json:"roleGrantsWidened"`
	FieldACLReopened              int      `json:"fieldAclReopened"`
	SharesRegranted               int      `json:"sharesRegranted"`
	MembershipsReadded            int      `json:"membershipsReadded"`
	SensitiveFieldsCleared        int      `json:"sensitiveFieldsCleared"`
	WorkflowsReenabled            int      `json:"workflowsReenabled"`
	OutboundDestinationsReenabled int      `json:"outboundDestinationsReenabled"`
	OutboundHosts                 []string `json:"outboundHosts"`
	UsersAffected                 int      `json:"usersAffected"`
	APIKeysAffected               int      `json:"apiKeysAffected"`
}

const keySep = "\x00"

// NewAccessSnapshot returns an empty snapshot.
func NewAccessSnapshot() *AccessSnapshot {
	return &AccessSnapshot{
		Roles: map[string]bool{}, Grants: map[string]int{}, FieldACL: map[string]FieldACLRow{},
		Shares: map[string]ShareRow{}, Members: map[string]bool{}, Sensitive: map[string]bool{},
		Workflows: map[string]WorkflowRow{}, Users: map[string]UserRow{}, CredNames: map[string]string{},
	}
}

// accessLevel orders row-level access: none < owner < field:<x> < team < all. "field:<x>" is owner
// plus rows pointing at the user, so it ranks just above owner. An unknown word is none.
// ponytail: field:x vs field:y rank equal (the name is not kept); add a name compare if it matters.
func accessLevel(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "owner":
		return 1
	case strings.HasPrefix(s, "field:"):
		return 2
	case s == "team":
		return 3
	case s == "all":
		return 4
	}
	return 0
}

// AddRole records a role id.
func (a *AccessSnapshot) AddRole(id string) { a.Roles[id] = true }

// AddGrant records one acl_role_actions row.
func (a *AccessSnapshot) AddGrant(role, entity, action, level string) {
	a.Grants[role+keySep+entity+keySep+action] = accessLevel(level)
}

// AddFieldACL records one sys_field_acl row.
func (a *AccessSnapshot) AddFieldACL(role, entity, field string, view, edit bool) {
	a.FieldACL[role+keySep+entity+keySep+field] = FieldACLRow{View: view, Edit: edit}
}

// AddShare records a share; one that is soft-deleted is not in force and is not recorded.
func (a *AccessSnapshot) AddShare(id, userID string, deleted bool) {
	if !deleted {
		a.Shares[id] = ShareRow{UserID: userID}
	}
}

// AddMember records one team membership.
func (a *AccessSnapshot) AddMember(team, user string) { a.Members[team+keySep+user] = true }

// AddField records options.sensitive of a live field (a soft-deleted one is skipped).
func (a *AccessSnapshot) AddField(id string, options json.RawMessage, deleted bool) {
	if deleted {
		return
	}
	var o struct {
		Sensitive bool `json:"sensitive"`
	}
	if len(options) > 0 {
		_ = json.Unmarshal(options, &o) // unreadable options = not sensitive
	}
	a.Sensitive[id] = o.Sensitive
}

// AddWorkflow records a rule; a soft-deleted or switched-off one is not Active.
func (a *AccessSnapshot) AddWorkflow(id string, isActive, deleted bool, actions json.RawMessage) {
	w := WorkflowRow{Active: isActive && !deleted}
	var acts []struct {
		Type         string `json:"type"`
		URL          string `json:"url"`
		CredentialID string `json:"credentialId"`
		Path         string `json:"path"`
	}
	if len(actions) > 0 && json.Unmarshal(actions, &acts) == nil {
		for _, x := range acts {
			if strings.TrimSpace(x.Type) != "outbound_webhook" {
				continue
			}
			switch {
			case strings.TrimSpace(x.URL) != "":
				w.Dests = append(w.Dests, "url:"+strings.TrimSpace(x.URL))
			case x.CredentialID != "":
				w.Dests = append(w.Dests, "cred:"+x.CredentialID+keySep+x.Path)
			}
		}
	}
	a.Workflows[id] = w
}

// AddUser records a user that is not deleted.
func (a *AccessSnapshot) AddUser(id, roleID, status string, deleted bool) {
	if !deleted {
		a.Users[id] = UserRow{RoleID: roleID, Active: status == "active"}
	}
}

// AddAPIKey records the role of one unrevoked API key.
func (a *AccessSnapshot) AddAPIKey(roleID string) {
	if roleID != "" {
		a.APIKeys = append(a.APIKeys, roleID)
	}
}

func roleOfKey(k string) string { return k[:strings.Index(k, keySep)] }

const maxOutboundHosts = 20

// DiffAccess counts what restoring `backup` over `cur` would widen again. Only roles present on
// BOTH sides count for role/field grants (a role on one side only has no holder whose access
// changes). Pure: no clock, no database.
func DiffAccess(backup, cur *AccessSnapshot) AccessChanges {
	var ac AccessChanges
	widened := map[string]bool{}
	bothRoles := func(role string) bool { return backup.Roles[role] && cur.Roles[role] }

	for k, bl := range backup.Grants {
		role := roleOfKey(k)
		if bothRoles(role) && bl > cur.Grants[k] {
			ac.RoleGrantsWidened++
			widened[role] = true
		}
	}
	for k, cr := range cur.FieldACL {
		role := roleOfKey(k)
		if !bothRoles(role) {
			continue
		}
		br, ok := backup.FieldACL[k]
		if !ok {
			br = FieldACLRow{View: true, Edit: true} // no row = open
		}
		if (!cr.View && br.View) || (!cr.Edit && br.Edit) {
			ac.FieldACLReopened++
			widened[role] = true
		}
	}

	extra := map[string]bool{} // users made newly reachable by a share / membership
	for id, s := range backup.Shares {
		if _, ok := cur.Shares[id]; !ok {
			ac.SharesRegranted++
			extra[s.UserID] = true
		}
	}
	for k := range backup.Members {
		if cur.Members[k] {
			continue
		}
		user := k[strings.Index(k, keySep)+1:]
		if _, ok := cur.Users[user]; ok {
			ac.MembershipsReadded++
			extra[user] = true
		}
	}
	for id, was := range cur.Sensitive {
		if now, ok := backup.Sensitive[id]; was && ok && !now {
			ac.SensitiveFieldsCleared++
		}
	}

	curDests := map[string]bool{}
	for _, w := range cur.Workflows {
		if w.Active {
			for _, d := range w.Dests {
				curDests[d] = true
			}
		}
	}
	newDests := map[string]bool{}
	for id, bw := range backup.Workflows {
		if !bw.Active {
			continue
		}
		if cw, ok := cur.Workflows[id]; !ok || !cw.Active {
			ac.WorkflowsReenabled++
		}
		for _, d := range bw.Dests {
			if !curDests[d] {
				newDests[d] = true
			}
		}
	}
	ac.OutboundDestinationsReenabled = len(newDests)
	hosts := map[string]bool{}
	for d := range newDests {
		hosts[destHost(d, cur.CredNames)] = true
	}
	ac.OutboundHosts = make([]string, 0, len(hosts))
	for h := range hosts {
		ac.OutboundHosts = append(ac.OutboundHosts, h)
	}
	sort.Strings(ac.OutboundHosts)
	if len(ac.OutboundHosts) > maxOutboundHosts {
		ac.OutboundHosts = ac.OutboundHosts[:maxOutboundHosts]
	}

	users := map[string]bool{}
	for id, u := range cur.Users {
		if u.Active && (widened[u.RoleID] || extra[id]) {
			users[id] = true
		}
	}
	ac.UsersAffected = len(users)
	for _, role := range cur.APIKeys {
		if widened[role] {
			ac.APIKeysAffected++
		}
	}
	ac.Total = ac.RoleGrantsWidened + ac.FieldACLReopened + ac.SharesRegranted + ac.MembershipsReadded +
		ac.SensitiveFieldsCleared + ac.WorkflowsReenabled + ac.OutboundDestinationsReenabled
	return ac
}

// destHost is what the operator may read about a destination: a host name or a credential name,
// never the URL (it can carry a key in its query string).
func destHost(dest string, creds map[string]string) string {
	if raw, ok := strings.CutPrefix(dest, "url:"); ok {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
		return "địa chỉ không đọc được"
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(dest, "cred:"), keySep)
	if n := creds[id]; n != "" {
		return "khoá kết nối " + n
	}
	return "khoá kết nối"
}

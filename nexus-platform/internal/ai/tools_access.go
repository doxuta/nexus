package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// propose_access (issue #2): the agent says who may use what it has just built. A user holds ONE role
// (users.role_id), so moving the rep onto an app role would strip their CRM role: the agent grants on
// roles that already exist instead (list_roles), and only on what it built — a table proposed in this
// same turn, or a live one born after the last publish. It never grants delete, a field-level or team
// scope or a record share, and never touches a reserved role or one an admin or a platform operator
// holds. The grant is additive (mergeAccessGrants). Nothing is written here: the builder applies the
// card through PUT /admin/roles/:id, which leaves a traced "role" version entry.

// accessActionOrder is the closed action vocabulary, in the order a card reads it.
var accessActionOrder = []string{"view", "create", "edit"}

// maxAccessGrants bounds one call, like maxProposeRecordRows does for sample data.
const maxAccessGrants = 20

var (
	accessArgKeys   = []string{"grants"}
	accessGrantKeys = []string{"role", "entity", "actions", "scope"}
)

// DraftAccess is a proposed set of grants for ONE existing role (kind "access"). FE APPLY CONTRACT
// (lib/aiApply.ts): resolve Role to an id (GET /admin/roles), read the role (GET /admin/roles/:id),
// merge Grants into its grants with mergeAccessGrants, write the result (PUT /admin/roles/:id).
type DraftAccess struct {
	Role      string                    `json:"role"`      // canonical stored role name
	RoleLabel string                    `json:"roleLabel"` // what the card calls the role
	Grants    []services.RoleActionView `json:"grants"`    // to ADD: action view|create|edit, accessLevel all|owner
	Labels    map[string]string         `json:"labels"`    // table name -> label, for the card
}

// mergeAccessGrants is the ONE additive rule of propose_access (its FE twin is mergeAccessGrants in
// lib/accessLevels.ts; testdata/access_merge_cases.json pins both): every grant of the role stays as it
// is, a proposed (table, action) is added or raised, never lowered, and not added at all when the
// role's "*" grant already gives as much.
func mergeAccessGrants(current, add []services.RoleActionView) []services.RoleActionView {
	out := slices.Clone(current)
	find := func(entity, action string) int {
		return slices.IndexFunc(out, func(g services.RoleActionView) bool { return g.EntityName == entity && g.Action == action })
	}
	for _, g := range add {
		if i := find(g.EntityName, g.Action); i >= 0 {
			if !accessCovers(out[i].AccessLevel, g.AccessLevel) {
				out[i].AccessLevel = g.AccessLevel
			}
			continue
		}
		if i := find("*", g.Action); i >= 0 && accessCovers(out[i].AccessLevel, g.AccessLevel) {
			continue
		}
		out = append(out, g)
	}
	return out
}

// accessCovers: a role at level have already reaches every row level want does (want is all or owner).
// owner is the narrowest grant that sees anything; team and field:<user field> both include it.
func accessCovers(have, want string) bool {
	return have == "all" || (want == "owner" && have != "none" && have != "")
}

func (e *toolExecutor) proposeAccess(ctx context.Context, argsJSON string) string {
	var probes *AccessProbes
	if c, ok := e.reader.(AccessProbeCarrier); ok {
		probes = c.AccessProbes()
	}
	if probes == nil || probes.Roles == nil || probes.Users == nil {
		return toolErr("ACCESS_PROPOSE_UNAVAILABLE: server chưa nối bộ đọc vai cho AI Builder — cấp quyền thủ công ở màn Người dùng & quyền")
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return toolErr("INVALID_ARGS: %v", err)
	}
	if bad := unknownKey(args, accessArgKeys); bad != "" {
		return toolErr("ACCESS_GRANT_KEY_UNSUPPORTED: khoá %q không có — chỉ grants: [{role, entity, actions, scope}]", bad)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(args["grants"], &raw); err != nil && len(args["grants"]) > 0 {
		return toolErr("INVALID_ARGS: grants: %v", err)
	}
	if len(raw) == 0 {
		return toolErr("ACCESS_GRANTS_REQUIRED: grants cần ít nhất 1 phần tử {role, entity, actions, scope}")
	}
	if len(raw) > maxAccessGrants {
		return toolErr("ACCESS_TOO_MANY_GRANTS: %d grant, tối đa %d mỗi lần", len(raw), maxAccessGrants)
	}

	roles, err := probes.Roles.ListRoles(ctx, e.tenantID)
	if err != nil {
		return toolErr("ACCESS_PROPOSE_FAILED: %v", err)
	}
	// One query for the admins and operators holding a role this call names (issue #23): a refused
	// grant is still judged in order below, so a role that does not resolve is simply not asked about.
	var named []string
	for _, g := range raw {
		var ref string
		if json.Unmarshal(g["role"], &ref) != nil {
			continue // not a string: refused as INVALID_ARGS below
		}
		if r, msg := resolveAccessRole(roles, ref, 0); msg == "" {
			named = append(named, r.ID)
		}
	}
	users, err := probes.Users.PrivilegedHolders(ctx, e.tenantID, named)
	if err != nil {
		return toolErr("ACCESS_PROPOSE_FAILED: %v", err)
	}
	drafted := e.draftedEntities()

	type pending struct {
		role   auth.Role
		grants []services.RoleActionView
		labels map[string]string
	}
	var order []*pending
	byRole := map[string]*pending{}
	// Every grant is judged before anything is buffered: a refused grant leaves the draft as it was.
	for i, g := range raw {
		n := i + 1
		if bad := unknownKey(g, accessGrantKeys); bad != "" {
			return toolErr("ACCESS_GRANT_KEY_UNSUPPORTED: grant %d: khoá %q không có — chỉ role, entity, actions, scope (không quyền theo trường, không chia sẻ bản ghi)", n, bad)
		}
		var in struct {
			Role    string   `json:"role"`
			Entity  string   `json:"entity"`
			Actions []string `json:"actions"`
			Scope   string   `json:"scope"`
		}
		if b, _ := json.Marshal(g); json.Unmarshal(b, &in) != nil {
			return toolErr("INVALID_ARGS: grant %d: role/entity/scope là chuỗi, actions là mảng chuỗi", n)
		}

		role, msg := resolveAccessRole(roles, in.Role, n)
		if msg != "" {
			return msg
		}
		if auth.IsOwnerRoleName(role.Name) {
			return toolErr("ACCESS_ROLE_RESERVED: grant %d: vai %q là vai dành riêng của chủ khách — không cấp qua trợ lý", n, role.Name)
		}
		if who := privilegedHolder(users, role.ID); who != "" {
			return toolErr("ACCESS_ROLE_ADMIN: grant %d: vai %q đang được %s giữ — không cấp quyền cho vai của quản trị viên qua trợ lý", n, role.Name, who)
		}

		entity, label, msg := e.resolveAccessEntity(ctx, probes, drafted, in.Entity, n)
		if msg != "" {
			return msg
		}

		if len(in.Actions) == 0 {
			return toolErr("ACCESS_ACTIONS_REQUIRED: grant %d: actions cần ít nhất một trong view, create, edit", n)
		}
		for _, a := range in.Actions {
			if !slices.Contains(accessActionOrder, a) {
				return toolErr("ACCESS_ACTION_UNSUPPORTED: grant %d: hành động %q không cấp được qua trợ lý — chỉ view, create, edit (xoá do người duyệt tự cấp)", n, a)
			}
		}
		if in.Scope != "all" && in.Scope != "owner" {
			return toolErr("ACCESS_SCOPE_UNSUPPORTED: grant %d: scope %q — chỉ 'all' (mọi bản ghi) hoặc 'owner' (bản ghi của chính họ)", n, in.Scope)
		}

		p := byRole[role.ID]
		if p == nil {
			p = &pending{role: role, labels: map[string]string{}}
			byRole[role.ID] = p
			order = append(order, p)
		}
		p.labels[entity] = label
		for _, a := range accessActionOrder {
			if !slices.Contains(in.Actions, a) {
				continue
			}
			add := services.RoleActionView{EntityName: entity, Action: a, AccessLevel: in.Scope}
			// The same (table, action) twice in one call keeps the wider scope.
			p.grants = mergeAccessGrants(p.grants, []services.RoleActionView{add})
		}
	}

	var buffered, unchanged []string
	from := len(e.draft)
	for _, p := range order {
		acts, err := probes.Roles.ListActionsForRole(ctx, e.tenantID, p.role.ID)
		if err != nil {
			e.draft = e.draft[:from]
			return toolErr("ACCESS_PROPOSE_FAILED: %v", err)
		}
		before := grantViews(acts)
		after := mergeAccessGrants(before, p.grants)
		if slices.Equal(after, before) {
			unchanged = append(unchanged, p.role.Name)
			continue
		}
		label := p.role.Label
		if strings.TrimSpace(label) == "" {
			label = p.role.Name
		}
		e.draft = append(e.draft, DraftItem{
			Kind:   "access",
			Access: &DraftAccess{Role: p.role.Name, RoleLabel: label, Grants: p.grants, Labels: p.labels},
			Change: accessChange(p.role, label, before, after, p.grants, p.labels),
		})
		buffered = append(buffered, fmt.Sprintf("vai %q: %s", p.role.Name, grantsText(p.grants)))
	}
	if len(buffered) == 0 {
		return toolErr("ACCESS_NOTHING_TO_ADD: vai %s đã có đủ các quyền này — không cần đề xuất", strings.Join(unchanged, ", "))
	}
	out := map[string]any{"buffered": "quyền cho " + strings.Join(buffered, "; ")}
	if len(unchanged) > 0 {
		out["unchanged"] = unchanged // already had it all: no card for these
	}
	return toolOK(out)
}

// list_roles {grants: true} output caps (issue #67), like snapshot_model's: a safety ceiling, not a
// working limit (the CRM pack's widest role holds about 40 grants). Worst case 40 × 60 grant rows, about
// 160 KB with short table names; the issue's tenant (20 roles × 100 tables × 4 actions) was 550 KB in
// one result. Grants come sorted by table and "*" sorts first, so every-table grants are the last cut.
const (
	maxListedRoles         = 40
	maxListedGrantsPerRole = 60
)

// listRoleGrants is list_roles {grants: true} (issue #23): every role of the tenant with its current
// grants, so "who can edit contacts?" gets a whole answer. A read: the tenant is the executor's,
// nothing is buffered or written, which is why MCP offers it too.
func (e *toolExecutor) listRoleGrants(ctx context.Context) string {
	var probes *AccessProbes
	if c, ok := e.reader.(AccessProbeCarrier); ok {
		probes = c.AccessProbes()
	}
	if probes == nil || probes.Roles == nil {
		return toolErr("ROLES_GRANTS_UNAVAILABLE: server chưa nối bộ đọc vai cho AI Builder — gọi list_roles không kèm grants")
	}
	// One snapshot of every role with its grants: a role deleted meanwhile is absent, not a failed read.
	roles, err := probes.Roles.ListRoleGrants(ctx, e.tenantID)
	if err != nil {
		return toolErr("ROLES_READ_FAILED: %v", err)
	}
	res := map[string]any{}
	cut := len(roles) > maxListedRoles
	if cut {
		res["rolesTruncated"], res["rolesTotal"] = true, len(roles)
		roles = roles[:maxListedRoles]
	}
	out := make([]map[string]any, 0, len(roles))
	for _, r := range roles {
		row := map[string]any{"name": r.Role.Name, "label": r.Role.Label}
		acts := r.Actions
		if len(acts) > maxListedGrantsPerRole {
			row["grantsTruncated"], row["grantsTotal"] = true, len(acts)
			acts, cut = acts[:maxListedGrantsPerRole], true
		}
		row["grants"] = grantViews(acts)
		out = append(out, row)
	}
	res["roles"] = out
	if cut {
		res["note"] = fmt.Sprintf("đã cắt bớt: tối đa %d vai, %d quyền mỗi vai (rolesTotal / grantsTotal là số thật) — quyền của một người trên một bảng cụ thể: hỏi explain_access", maxListedRoles, maxListedGrantsPerRole)
	}
	return toolOK(res)
}

// grantViews is a role's grant rows as the API and the access card show them; never nil.
func grantViews(acts []auth.RoleAction) []services.RoleActionView {
	out := make([]services.RoleActionView, len(acts))
	for i, a := range acts {
		out[i] = services.RoleActionView{EntityName: a.EntityName, Action: a.Action, AccessLevel: a.AccessLevel}
	}
	return out
}

// resolveAccessRole finds the role by its name, else by its label when exactly one role carries it
// (the builder may say "Nhân viên kinh doanh"). roles is tenant-pinned, so another tenant's role is
// simply not there. The second value is the tool error ("" = found).
func resolveAccessRole(roles []auth.Role, ref string, n int) (auth.Role, string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return auth.Role{}, toolErr("ACCESS_ROLE_REQUIRED: grant %d: role là bắt buộc — tên vai từ list_roles", n)
	}
	var byLabel []auth.Role
	for _, r := range roles {
		if strings.EqualFold(strings.TrimSpace(r.Name), ref) {
			return r, ""
		}
		if strings.EqualFold(strings.TrimSpace(r.Label), ref) {
			byLabel = append(byLabel, r)
		}
	}
	if len(byLabel) == 1 {
		return byLabel[0], ""
	}
	return auth.Role{}, toolErr("ACCESS_ROLE_NOT_FOUND: grant %d: không có vai %q trong tổ chức — gọi list_roles để lấy đúng tên vai đang có", n, ref)
}

// privilegedHolder says who makes the role privileged ("" = nobody): an admin or a platform operator
// holding it (users = PrivilegedHolders). They bypass access control anyway, and the assistant stays
// off their roles.
func privilegedHolder(users []auth.User, roleID string) string {
	for _, u := range users {
		if u.RoleID == nil || *u.RoleID != roleID {
			continue
		}
		if u.IsPlatformOperator {
			return "một người vận hành nền tảng"
		}
		if u.IsAdmin {
			return "một quản trị viên"
		}
	}
	return ""
}

// resolveAccessEntity: the registry first — a live table is judged by its own created_at against the
// last publish and named by its own label, never by a draft of the same name (verifier round 1 of #2:
// propose_entity{name: crm_contracts, label: "Phiếu hỗ trợ"} then propose_access on crm_contracts);
// only a name the registry does not know may be a table proposed in this same turn. Returns its
// canonical name and label, or the tool error.
func (e *toolExecutor) resolveAccessEntity(ctx context.Context, probes *AccessProbes, drafted map[string]*DraftEntity, raw string, n int) (string, string, string) {
	ent, err := e.reader.GetEntityByName(ctx, e.tenantID, canonicalEntityName(raw))
	switch {
	case err == nil:
		if msg := e.liveTableRefusal(ctx, probes, ent, n); msg != "" {
			return "", "", msg
		}
		label := ent.Label
		if strings.TrimSpace(label) == "" {
			label = ent.Name
		}
		return ent.Name, label, ""
	case !errors.Is(err, metadata.ErrEntityNotFound):
		return "", "", toolErr("ACCESS_PROPOSE_FAILED: grant %d: %v", n, err)
	}
	if d, ok := drafted[canonicalEntityName(raw)]; ok {
		return d.Name, d.Label, ""
	}
	return "", "", toolErr("ENTITY_NOT_FOUND: grant %d: bảng %q không có và cũng chưa được propose_entity trong CÙNG lượt này", n, raw)
}

// liveTableRefusal: a live table takes a grant only while the customer cannot see it yet — born after
// the last publish (still hidden, publish_hidden_new.go). Never delivered (no cutoff): staff may already
// use every live table (the demo's rep exists before the first publish), so none qualifies and the
// builder ticks it by hand. No cutoff reader wired: fail closed. "" = the grant may go ahead.
func (e *toolExecutor) liveTableRefusal(ctx context.Context, probes *AccessProbes, ent *metadata.EntityDefinition, n int) string {
	published := toolErr("ACCESS_ENTITY_PUBLISHED: grant %d: bảng %q khách đã dùng — trợ lý chỉ cấp quyền trên bảng vừa dựng; quyền trên bảng này người duyệt chỉnh ở màn Người dùng & quyền", n, ent.Name)
	if probes.Published == nil {
		return published
	}
	at, err := probes.Published.HeadCutoff(ctx, e.tenantID) // cached per tenant by the DraftGate
	if err != nil {
		return toolErr("ACCESS_PROPOSE_FAILED: grant %d: %v", n, err)
	}
	if at == nil {
		return toolErr("ACCESS_ENTITY_EXISTS: grant %d: bảng %q đã có sẵn và dự án chưa xuất bản lần nào — nhân viên có thể đang dùng nó; trợ lý chỉ cấp quyền trên bảng propose_entity trong CÙNG lượt này, quyền trên bảng có sẵn người duyệt tự chọn ở màn Người dùng & quyền", n, ent.Name)
	}
	if !ent.CreatedAt.After(*at) {
		return published
	}
	return ""
}

// accessChange is the propose_access envelope: a "role" document (the version doc the apply leaves),
// with the role's grants on the tables it touches, plus its "*" rows those tables follow, before and
// after the merge. No changedPaths: the card says the grant in words (lib/aiProposalView.ts).
func accessChange(role auth.Role, label string, before, after, add []services.RoleActionView, labels map[string]string) *DraftChange {
	keep := func(gs []services.RoleActionView) []services.RoleActionView {
		out := []services.RoleActionView{}
		for _, g := range gs {
			if _, ok := labels[g.EntityName]; ok || g.EntityName == "*" {
				out = append(out, g)
			}
		}
		return out
	}
	return &DraftChange{
		DocType: "role", DocKey: role.ID, Op: "update", Label: fmt.Sprintf("Vai %q", label),
		Target: DraftChangeTarget{Name: role.Name, Entity: add[0].EntityName},
		Before: marshalProjection(map[string]any{"grants": keep(before), "labels": labels}),
		After:  marshalProjection(map[string]any{"grants": keep(after), "labels": labels}),
	}
}

// grantsText is the tool result's one-line summary: tickets: view(all), create(all), edit(owner).
func grantsText(gs []services.RoleActionView) string {
	var parts []string
	last := ""
	for _, g := range gs {
		item := fmt.Sprintf("%s(%s)", g.Action, g.AccessLevel)
		if g.EntityName != last {
			item = g.EntityName + ": " + item
			last = g.EntityName
		}
		parts = append(parts, item)
	}
	return strings.Join(parts, ", ")
}

// unknownKey is the first key of m outside allowed ("" = none), in sorted order so the answer is stable.
func unknownKey(m map[string]json.RawMessage, allowed []string) string {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(allowed, k) {
			return k
		}
	}
	return ""
}

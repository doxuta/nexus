package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// teamRepo is the subset of the team repository the team-management service needs. It is declared
// consumer-side so the service is unit-testable with an in-memory fake and does not import a
// concrete DB type. EVERY method is tenant-scoped in the real implementation (teams.tenant_id and,
// for memberships, a tenant guard on both team and user), so no use case can read or mutate another
// tenant's team or smuggle a cross-tenant user onto a team (R2/R3).
type teamRepo interface {
	ListTeams(ctx context.Context, tenantID string) ([]auth.Team, error)
	GetTeamByID(ctx context.Context, tenantID, id string) (*auth.Team, error)
	CreateTeam(ctx context.Context, team *auth.Team) error
	UpdateTeam(ctx context.Context, tenantID, id string, fields map[string]any) (*auth.Team, error)
	DeleteTeam(ctx context.Context, tenantID, id string) error
	ListMembers(ctx context.Context, tenantID, teamID string) ([]auth.User, error)
	AddMember(ctx context.Context, tenantID, teamID, userID, membershipID string) error
	RemoveMember(ctx context.Context, tenantID, teamID, userID string) error
}

// teamSetReader is the READ-ONLY team-set slice of the repository the W7 sharing screens need
// (sets are engine-derived — resolved on record write from the creator's memberships — so the
// admin surface only lists/inspects them). Declared consumer-side; the real implementation is
// auth.TeamRepository. It is auto-wired by type assertion in NewTeamService (W5 seam pattern)
// so existing unit-test fakes that don't implement it keep compiling unchanged.
type teamSetReader interface {
	ListTeamSets(ctx context.Context, tenantID string) ([]auth.TeamSetWithTeams, error)
	GetTeamSetByID(ctx context.Context, tenantID, id string) (*auth.TeamSetWithTeams, error)
}

// TeamService implements the A5-extra team-management use cases (CRUD over /admin/teams plus
// member add/remove over /admin/teams/:id/members). Every method takes the verified tenant id and
// pins all repository calls to it, so no use case can touch another tenant's team and a membership
// can never link a team to a user outside the caller's tenant. It is HTTP-agnostic (the handler
// maps its typed errors to status codes) and holds no per-request state.
type TeamService struct {
	repo  teamRepo
	sets  teamSetReader // nil when the repo cannot read team sets (older fakes)
	newID func() string // injectable id generator; defaults to uuid
	// auditor records committed team mutations (RM-19), async + fail-open.
	// Defaults to a noop so existing wiring/tests run unchanged; injected via WithAuditor.
	auditor audit.Auditor
}

// NewTeamService wires the team service over a team repository. When the repository also
// implements the team-set reads (the production auth.TeamRepository does), they are auto-wired.
func NewTeamService(repo teamRepo) *TeamService {
	s := &TeamService{repo: repo, newID: uuid.NewString, auditor: audit.NewNoopAuditor()}
	if r, ok := repo.(teamSetReader); ok {
		s.sets = r
	}
	return s
}

// WithAuditor injects the RM-19 audit dispatcher so committed team mutations emit an
// append-only __team__ audit row (async, fail-open). The actor user id is read from the
// request context (audit.WithActor, set by the admin handler). Chainable; a nil auditor is
// ignored.
func (s *TeamService) WithAuditor(a audit.Auditor) *TeamService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// teamMemberAudit is the __team__ membership-change audit payload: an add emits it as the
// after side (member:true appears), a remove as the before side (member:true disappears) —
// both under ActionUpdate, since a membership change mutates the team, not creates/deletes it.
type teamMemberAudit struct {
	TeamID string `json:"teamId"`
	UserID string `json:"userId"`
	Member bool   `json:"member"`
}

// TeamView is the safe public projection of a team.
type TeamView struct {
	ID       string  `json:"id"`
	TenantID string  `json:"tenantId"`
	Name     string  `json:"name"`
	ParentID *string `json:"parentId,omitempty"`
}

// MemberView is the safe public projection of a team member (a user). password_hash never appears.
type MemberView struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	FullName string `json:"fullName,omitempty"`
	IsAdmin  bool   `json:"isAdmin"`
}

// CreateTeamInput is the payload to create a team.
type CreateTeamInput struct {
	Name     string
	ParentID *string
}

// UpdateTeamInput carries the mutable team attributes with apply-when-non-nil semantics: an omitted
// (nil) field leaves the stored value untouched. ParentID is a double pointer so the caller can
// distinguish "leave parent" (nil) from "set/clear parent" (non-nil, pointing at an id or "").
type UpdateTeamInput struct {
	Name     *string
	ParentID *string
}

// ListTeams returns the safe view of every team in the tenant.
func (s *TeamService) ListTeams(ctx context.Context, tenantID string) ([]TeamView, error) {
	rows, err := s.repo.ListTeams(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.TeamService.ListTeams: %w", err)
	}
	views := make([]TeamView, len(rows))
	for i := range rows {
		views[i] = teamToView(&rows[i])
	}
	return views, nil
}

// GetTeam returns one tenant-scoped team, or a TEAM_NOT_FOUND ValidationError for an unknown/
// cross-tenant id.
func (s *TeamService) GetTeam(ctx context.Context, tenantID, id string) (*TeamView, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("TEAM_ID_REQUIRED", "id team là bắt buộc")
	}
	team, err := s.repo.GetTeamByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrTeamNotFound) {
			return nil, invalid("TEAM_NOT_FOUND", "không tìm thấy team")
		}
		return nil, fmt.Errorf("services.TeamService.GetTeam: %w", err)
	}
	v := teamToView(team)
	return &v, nil
}

// CreateTeam registers a new team in the tenant. The name is required and unique within the tenant
// (409 TEAM_ALREADY_EXISTS). A parent_id, when supplied, must reference a live team in the same
// tenant (404 TEAM_NOT_FOUND). The created team's safe view is returned.
func (s *TeamService) CreateTeam(ctx context.Context, tenantID string, in CreateTeamInput) (*TeamView, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, invalid("TEAM_NAME_REQUIRED", "name team là bắt buộc")
	}

	team := &auth.Team{
		ID:       s.newID(),
		TenantID: tenantID,
		Name:     name,
	}
	if in.ParentID != nil {
		if pid := strings.TrimSpace(*in.ParentID); pid != "" {
			team.ParentID = &pid
		}
	}

	if err := s.repo.CreateTeam(ctx, team); err != nil {
		if errors.Is(err, auth.ErrTeamAlreadyExists) {
			return nil, invalid("TEAM_ALREADY_EXISTS", "tên team đã tồn tại trong tổ chức")
		}
		if errors.Is(err, auth.ErrTeamNotFound) {
			return nil, invalid("TEAM_PARENT_NOT_FOUND", "team cha không tồn tại trong tổ chức")
		}
		return nil, fmt.Errorf("services.TeamService.CreateTeam: %w", err)
	}
	v := teamToView(team)
	// RM-19: audit the committed team create (after-only diff). Async + fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.TeamMeta, audit.ActionCreate, nil, v)
	return &v, nil
}

// UpdateTeam applies the supplied non-nil fields to a tenant-scoped team. An unknown/cross-tenant
// id is TEAM_NOT_FOUND; a name collision is 409; a parent_id that is missing/cross-tenant is 404;
// a parent_id that is the team itself or any of its descendants is 422 TEAM_PARENT_CYCLE (the
// repository walks the real ancestor chain — queue #21). The refreshed safe view is returned.
func (s *TeamService) UpdateTeam(ctx context.Context, tenantID, id string, in UpdateTeamInput) (*TeamView, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("TEAM_ID_REQUIRED", "id team là bắt buộc")
	}

	// Snapshot the before-image (best-effort) so the RM-19 audit diff reflects what changed.
	// A read failure must not block the update — the audit then records an after-only diff
	// (fail-open).
	var beforeView any
	if prior, gerr := s.repo.GetTeamByID(ctx, tenantID, id); gerr == nil && prior != nil {
		beforeView = teamToView(prior)
	}

	fields := map[string]any{}
	if in.Name != nil {
		nm := strings.TrimSpace(*in.Name)
		if nm == "" {
			return nil, invalid("TEAM_NAME_REQUIRED", "name team không được rỗng")
		}
		fields["name"] = nm
	}
	if in.ParentID != nil {
		pid := strings.TrimSpace(*in.ParentID)
		if pid == "" {
			fields["parent_id"] = nil // explicit clear (detach from hierarchy)
		} else {
			fields["parent_id"] = pid
		}
	}

	team, err := s.repo.UpdateTeam(ctx, tenantID, id, fields)
	if err != nil {
		if errors.Is(err, auth.ErrTeamNotFound) {
			return nil, invalid("TEAM_NOT_FOUND", "không tìm thấy team")
		}
		if errors.Is(err, auth.ErrTeamAlreadyExists) {
			return nil, invalid("TEAM_ALREADY_EXISTS", "tên team đã tồn tại trong tổ chức")
		}
		if errors.Is(err, auth.ErrTeamParentCycle) {
			return nil, invalid("TEAM_PARENT_CYCLE",
				"không thể đặt cấp trên: nhóm được chọn chính là nhóm này hoặc đang nằm dưới nó — sẽ tạo thành vòng tròn")
		}
		return nil, fmt.Errorf("services.TeamService.UpdateTeam: %w", err)
	}
	v := teamToView(team)
	// RM-19: audit the committed team update. Async + fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.TeamMeta, audit.ActionUpdate, beforeView, v)
	return &v, nil
}

// DeleteTeam removes a tenant-scoped team. The DB FKs cascade its memberships (team_memberships)
// and set compositions (team_sets_teams); its child teams are re-homed to its parent (repository
// tx) so the subtree stays visible to ancestors. An unknown/cross-tenant id is TEAM_NOT_FOUND.
func (s *TeamService) DeleteTeam(ctx context.Context, tenantID, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return invalid("TEAM_ID_REQUIRED", "id team là bắt buộc")
	}
	// Snapshot the before-image (best-effort) for a delete = before-only audit diff.
	var beforeView any
	if prior, gerr := s.repo.GetTeamByID(ctx, tenantID, id); gerr == nil && prior != nil {
		beforeView = teamToView(prior)
	}
	if err := s.repo.DeleteTeam(ctx, tenantID, id); err != nil {
		if errors.Is(err, auth.ErrTeamNotFound) {
			return invalid("TEAM_NOT_FOUND", "không tìm thấy team")
		}
		return fmt.Errorf("services.TeamService.DeleteTeam: %w", err)
	}
	// RM-19: audit the committed team delete (before-only diff). Async + fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.TeamMeta, audit.ActionDelete, beforeView, nil)
	return nil
}

// ListMembers returns the safe member views for a tenant-scoped team. Unknown/cross-tenant team id
// is TEAM_NOT_FOUND.
func (s *TeamService) ListMembers(ctx context.Context, tenantID, teamID string) ([]MemberView, error) {
	teamID = strings.TrimSpace(teamID)
	if teamID == "" {
		return nil, invalid("TEAM_ID_REQUIRED", "id team là bắt buộc")
	}
	users, err := s.repo.ListMembers(ctx, tenantID, teamID)
	if err != nil {
		if errors.Is(err, auth.ErrTeamNotFound) {
			return nil, invalid("TEAM_NOT_FOUND", "không tìm thấy team")
		}
		return nil, fmt.Errorf("services.TeamService.ListMembers: %w", err)
	}
	views := make([]MemberView, len(users))
	for i := range users {
		views[i] = MemberView{ID: users[i].ID, Email: users[i].Email, FullName: users[i].FullName, IsAdmin: users[i].IsAdmin}
	}
	return views, nil
}

// AddMember adds a user to a tenant-scoped team. Both the team and the user must be in the caller's
// tenant: an unknown/cross-tenant team is TEAM_NOT_FOUND, an unknown/cross-tenant user is
// MEMBER_USER_NOT_FOUND. A re-add is idempotent (no error). userID is required.
func (s *TeamService) AddMember(ctx context.Context, tenantID, teamID, userID string) error {
	teamID = strings.TrimSpace(teamID)
	if teamID == "" {
		return invalid("TEAM_ID_REQUIRED", "id team là bắt buộc")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return invalid("MEMBER_USER_REQUIRED", "userId là bắt buộc")
	}
	if err := s.repo.AddMember(ctx, tenantID, teamID, userID, s.newID()); err != nil {
		if errors.Is(err, auth.ErrTeamNotFound) {
			return invalid("TEAM_NOT_FOUND", "không tìm thấy team")
		}
		if errors.Is(err, auth.ErrMemberUserNotFound) {
			return invalid("MEMBER_USER_NOT_FOUND", "user không tồn tại trong tổ chức")
		}
		return fmt.Errorf("services.TeamService.AddMember: %w", err)
	}
	// RM-19: a membership change mutates the team → ActionUpdate with an after-only diff
	// ({teamId,userId,member:true} appears). Async + fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.TeamMeta, audit.ActionUpdate, nil,
		teamMemberAudit{TeamID: teamID, UserID: userID, Member: true})
	return nil
}

// RemoveMember removes a user from a tenant-scoped team. Unknown/cross-tenant team is
// TEAM_NOT_FOUND; a user who is not a member is MEMBERSHIP_NOT_FOUND. userID is required.
func (s *TeamService) RemoveMember(ctx context.Context, tenantID, teamID, userID string) error {
	teamID = strings.TrimSpace(teamID)
	if teamID == "" {
		return invalid("TEAM_ID_REQUIRED", "id team là bắt buộc")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return invalid("MEMBER_USER_REQUIRED", "userId là bắt buộc")
	}
	if err := s.repo.RemoveMember(ctx, tenantID, teamID, userID); err != nil {
		if errors.Is(err, auth.ErrTeamNotFound) {
			return invalid("TEAM_NOT_FOUND", "không tìm thấy team")
		}
		if errors.Is(err, auth.ErrMembershipNotFound) {
			return invalid("MEMBERSHIP_NOT_FOUND", "user không phải thành viên của team")
		}
		return fmt.Errorf("services.TeamService.RemoveMember: %w", err)
	}
	// RM-19: the mirror of AddMember — ActionUpdate with a before-only diff (the membership
	// disappears). Async + fail-open.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.TeamMeta, audit.ActionUpdate,
		teamMemberAudit{TeamID: teamID, UserID: userID, Member: true}, nil)
	return nil
}

// teamToView projects a team to the safe public view.
func teamToView(t *auth.Team) TeamView {
	return TeamView{
		ID:       t.ID,
		TenantID: t.TenantID,
		Name:     t.Name,
		ParentID: t.ParentID,
	}
}

// --- W7 team-set reads (sharing screens) ------------------------------------------------------

// TeamSetView is the safe public projection of one team set: its id, the optional primary
// team, when it was derived, and the member teams (id + name) the record-visibility resolves
// through. team_md5 (an internal dedupe key) is deliberately not exposed.
type TeamSetView struct {
	ID            string           `json:"id"`
	PrimaryTeamID *string          `json:"primaryTeamId"`
	Teams         []TeamSetTeamRef `json:"teams"`
	CreatedAt     string           `json:"createdAt"`
}

// TeamSetTeamRef mirrors auth.TeamSetTeamRef (id + display name) at the service boundary.
type TeamSetTeamRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListTeamSets returns every team set of the tenant with member teams (W7 read API — the
// "who sees this record and why" data for the sharing panel). Read-only by design: sets are
// engine-derived on record writes (dynrepo.resolveOrCreateTeamSet), never hand-edited here.
func (s *TeamService) ListTeamSets(ctx context.Context, tenantID string) ([]TeamSetView, error) {
	if s.sets == nil {
		return nil, fmt.Errorf("services.TeamService.ListTeamSets: team-set reader not configured")
	}
	rows, err := s.sets.ListTeamSets(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.TeamService.ListTeamSets: %w", err)
	}
	out := make([]TeamSetView, 0, len(rows))
	for i := range rows {
		out = append(out, teamSetToView(&rows[i]))
	}
	return out, nil
}

// GetTeamSet returns one tenant-scoped team set, or TEAM_SET_NOT_FOUND (404) for an unknown or
// cross-tenant id — indistinguishable by design.
func (s *TeamService) GetTeamSet(ctx context.Context, tenantID, id string) (*TeamSetView, error) {
	if s.sets == nil {
		return nil, fmt.Errorf("services.TeamService.GetTeamSet: team-set reader not configured")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("TEAM_SET_NOT_FOUND", "không tìm thấy team set")
	}
	row, err := s.sets.GetTeamSetByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrTeamSetNotFound) {
			return nil, invalid("TEAM_SET_NOT_FOUND", "không tìm thấy team set")
		}
		return nil, fmt.Errorf("services.TeamService.GetTeamSet: %w", err)
	}
	v := teamSetToView(row)
	return &v, nil
}

// teamSetToView projects a repository team-set row to the API view (RFC3339 UTC timestamps).
func teamSetToView(r *auth.TeamSetWithTeams) TeamSetView {
	teams := make([]TeamSetTeamRef, 0, len(r.Teams))
	for _, t := range r.Teams {
		teams = append(teams, TeamSetTeamRef{ID: t.ID, Name: t.Name})
	}
	return TeamSetView{
		ID:            r.ID,
		PrimaryTeamID: r.PrimaryTeamID,
		Teams:         teams,
		CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339),
	}
}

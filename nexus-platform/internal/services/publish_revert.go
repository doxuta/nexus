package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
)

// Revert (specs/063 US9, plan §10): undo ONE applied-at-once ("direct") history entry - a role, its
// grants, field permissions, an app created or deleted. Unlike Rollback it does not go through the
// publish path: each document type is put back through its own service, which leaves a fresh direct
// entry of its own and drops its caches. The live document must still equal the entry's "after"
// (else REVERT_STALE) so a second click, or a later edit, is never overwritten.

// revertTargets is the live system a revert reads and writes. Every method is tenant-pinned.
type revertTargets interface {
	RoleView(ctx context.Context, tenantID, id string) (*RoleView, bool, error)
	UpdateRole(ctx context.Context, tenantID, id string, before RoleView) error
	DeleteRole(ctx context.Context, tenantID, id string) error
	RecreateRole(ctx context.Context, tenantID, id string, before RoleView, fieldACL []auth.FieldACL, userIDs []string) (int, error)
	FieldRows(ctx context.Context, tenantID, roleID string) ([]auth.FieldACL, error)
	RestoreFieldRows(ctx context.Context, tenantID, roleID string, rows []auth.FieldACL) error
	AppSnap(ctx context.Context, tenantID, id string) (*appAuditDoc, error)
	DeleteApp(ctx context.Context, tenantID, id string) error
	RestoreApp(ctx context.Context, tenantID, id string, snap appAuditDoc) error
	// HasLaterDoc: a newer history entry touched the same document (a "delete" entry is stale then).
	HasLaterDoc(ctx context.Context, tenantID, docType, docKey string, afterSeq uint) (bool, error)
}

// liveRevertTargets wires revertTargets over the real services.
type liveRevertTargets struct {
	roles *RoleService
	facl  *FieldACLAdminService
	apps  *AppService
	foot  *sqlRoleFootprint
	vers  *metaversion.VersionRepo
}

func (t liveRevertTargets) HasLaterDoc(ctx context.Context, tenantID, docType, docKey string, afterSeq uint) (bool, error) {
	return t.vers.HasLaterDoc(ctx, tenantID, docType, docKey, afterSeq)
}

func (t liveRevertTargets) RoleView(ctx context.Context, tenantID, id string) (*RoleView, bool, error) {
	v, err := t.roles.GetRole(ctx, tenantID, id)
	var ve *ValidationError
	if errors.As(err, &ve) && ve.Code == "ROLE_NOT_FOUND" {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func (t liveRevertTargets) UpdateRole(ctx context.Context, tenantID, id string, before RoleView) error {
	name, label := before.Name, before.Label
	_, err := t.roles.UpdateRole(ctx, tenantID, id, UpdateRoleInput{Name: &name, Label: &label, Actions: actionInputs(before.Actions)})
	return err
}

func (t liveRevertTargets) DeleteRole(ctx context.Context, tenantID, id string) error {
	return t.roles.DeleteRole(ctx, tenantID, id)
}

func (t liveRevertTargets) RecreateRole(ctx context.Context, tenantID, id string, before RoleView, fieldACL []auth.FieldACL, userIDs []string) (int, error) {
	return t.roles.Recreate(ctx, tenantID, id, before, fieldACL, userIDs)
}

func (t liveRevertTargets) FieldRows(ctx context.Context, tenantID, roleID string) ([]auth.FieldACL, error) {
	return t.facl.RoleRows(ctx, tenantID, roleID)
}

func (t liveRevertTargets) RestoreFieldRows(ctx context.Context, tenantID, roleID string, rows []auth.FieldACL) error {
	return t.facl.RestoreRole(ctx, tenantID, roleID, rows)
}

func (t liveRevertTargets) AppSnap(ctx context.Context, tenantID, id string) (*appAuditDoc, error) {
	a, err := t.apps.repo.GetByID(metaversion.WithLiveRead(ctx), tenantID, id) // the live row, never a draft
	if errors.Is(err, appmeta.ErrAppNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load app: %w", err)
	}
	d, _ := appAuditSnapshot(a).(*appAuditDoc)
	return d, nil
}

func (t liveRevertTargets) DeleteApp(ctx context.Context, tenantID, id string) error {
	return t.apps.DeleteApp(ctx, tenantID, id)
}

func (t liveRevertTargets) RestoreApp(ctx context.Context, tenantID, id string, snap appAuditDoc) error {
	return t.apps.RestoreApp(ctx, tenantID, id, snap)
}

// WithReverters arms Revert over the role, field-permission and app services. Chainable.
func (s *PublishService) WithReverters(roles *RoleService, facl *FieldACLAdminService, apps *AppService) *PublishService {
	s.revert = liveRevertTargets{roles: roles, facl: facl, apps: apps, vers: s.versions}
	return s
}

// revertStale is the 409 every "the live document moved since" case answers with.
func revertStale(what string) error {
	return &ValidationError{Code: "REVERT_STALE", Msg: "Đã có thay đổi mới hơn trên " + what + ": không hoàn tác được"}
}

func revertUnsupported(docType string) error {
	return invalid("REVERT_UNSUPPORTED", "Chưa hoàn tác được mục loại %q từ lịch sử", docType)
}

// roleSig / faclSig are the order-free identities two images are compared by (ids and timestamps do
// not count).
func roleSig(v RoleView) string {
	acts := make([]string, len(v.Actions))
	for i, a := range v.Actions {
		acts[i] = a.EntityName + "|" + a.Action + "|" + a.AccessLevel
	}
	sort.Strings(acts)
	return v.Name + "\x00" + v.Label + "\x00" + strings.Join(acts, ",")
}

func faclSig(rows []auth.FieldACL) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("%s|%s|%t|%t", r.EntityName, r.FieldName, r.CanView, r.CanEdit)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func decodeImage(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return errors.New("empty image")
	}
	return json.Unmarshal(raw, into)
}

// revertStep is one document's undo, already checked against the live state; apply writes it.
type revertStep func(ctx context.Context) (reassigned int, err error)

// planRevert checks one history document against the live state and returns the step that undoes it.
// REVERT_STALE: the live document is not what the entry left behind. Nothing is written here.
func planRevert(ctx context.Context, t revertTargets, tenantID string, verSeq uint, d metaversion.VersionDoc) (revertStep, error) {
	switch d.DocType {
	case "role":
		return planRevertRole(ctx, t, tenantID, verSeq, d)
	case "field_acl":
		return planRevertFieldACL(ctx, t, tenantID, d)
	case "app":
		return planRevertApp(ctx, t, tenantID, verSeq, d)
	}
	return nil, revertUnsupported(d.DocType)
}

// staleIfSuperseded refuses a "delete" entry that is not the latest word on its document: the live
// state being absent proves nothing when the document was brought back and deleted again since, and
// the older image would overwrite the newer (tighter) one.
func staleIfSuperseded(ctx context.Context, t revertTargets, tenantID string, verSeq uint, d metaversion.VersionDoc, what string) error {
	later, err := t.HasLaterDoc(ctx, tenantID, d.DocType, d.DocKey, verSeq)
	if err != nil {
		return fmt.Errorf("services.planRevert: %s %s: %w", d.DocType, d.DocKey, err)
	}
	if later {
		return revertStale(what)
	}
	return nil
}

func planRevertRole(ctx context.Context, t revertTargets, tenantID string, verSeq uint, d metaversion.VersionDoc) (revertStep, error) {
	live, exists, err := t.RoleView(ctx, tenantID, d.DocKey)
	if err != nil {
		return nil, fmt.Errorf("services.planRevert: role %s: %w", d.DocKey, err)
	}
	switch d.Op {
	case "delete":
		if exists {
			return nil, revertStale("vai này")
		}
		if err := staleIfSuperseded(ctx, t, tenantID, verSeq, d, "vai này"); err != nil {
			return nil, err
		}
		var before RoleView
		if err := decodeImage(d.BeforeJSON, &before); err != nil {
			return nil, fmt.Errorf("services.planRevert: role %s before: %w", d.DocKey, err)
		}
		// fail closed: without the recorded restrictions/holders the role would come back looser than it left.
		var keys map[string]json.RawMessage
		if json.Unmarshal(d.Meta, &keys) != nil || keys["fieldAcl"] == nil || keys["assignedUserIds"] == nil {
			return nil, revertUnsupported("role/delete")
		}
		var meta struct {
			AssignedUserIDs []string        `json:"assignedUserIds"`
			FieldACL        []auth.FieldACL `json:"fieldAcl"`
		}
		if err := json.Unmarshal(d.Meta, &meta); err != nil {
			return nil, fmt.Errorf("services.planRevert: role %s meta: %w", d.DocKey, err)
		}
		return func(ctx context.Context) (int, error) {
			return t.RecreateRole(ctx, tenantID, d.DocKey, before, meta.FieldACL, meta.AssignedUserIDs)
		}, nil
	case "create", "update":
		var after RoleView
		if err := decodeImage(d.AfterJSON, &after); err != nil {
			return nil, fmt.Errorf("services.planRevert: role %s after: %w", d.DocKey, err)
		}
		if !exists || roleSig(*live) != roleSig(after) {
			return nil, revertStale("vai này")
		}
		if d.Op == "create" {
			return func(ctx context.Context) (int, error) { return 0, t.DeleteRole(ctx, tenantID, d.DocKey) }, nil
		}
		var before RoleView
		if err := decodeImage(d.BeforeJSON, &before); err != nil {
			return nil, fmt.Errorf("services.planRevert: role %s before: %w", d.DocKey, err)
		}
		return func(ctx context.Context) (int, error) { return 0, t.UpdateRole(ctx, tenantID, d.DocKey, before) }, nil
	}
	return nil, revertUnsupported(d.DocType + "/" + d.Op)
}

func planRevertFieldACL(ctx context.Context, t revertTargets, tenantID string, d metaversion.VersionDoc) (revertStep, error) {
	var before, after []auth.FieldACL
	if err := decodeImage(d.AfterJSON, &after); err != nil {
		return nil, fmt.Errorf("services.planRevert: field_acl %s after: %w", d.DocKey, err)
	}
	if len(d.BeforeJSON) > 0 {
		if err := json.Unmarshal(d.BeforeJSON, &before); err != nil {
			return nil, fmt.Errorf("services.planRevert: field_acl %s before: %w", d.DocKey, err)
		}
	}
	live, err := t.FieldRows(ctx, tenantID, d.DocKey)
	if err != nil {
		return nil, fmt.Errorf("services.planRevert: field_acl %s: %w", d.DocKey, err)
	}
	if faclSig(live) != faclSig(after) {
		return nil, revertStale("quyền theo trường của vai này")
	}
	return func(ctx context.Context) (int, error) { return 0, t.RestoreFieldRows(ctx, tenantID, d.DocKey, before) }, nil
}

func planRevertApp(ctx context.Context, t revertTargets, tenantID string, verSeq uint, d metaversion.VersionDoc) (revertStep, error) {
	live, err := t.AppSnap(ctx, tenantID, d.DocKey)
	if err != nil {
		return nil, fmt.Errorf("services.planRevert: app %s: %w", d.DocKey, err)
	}
	switch d.Op {
	case "create":
		var after appAuditDoc
		if err := decodeImage(d.AfterJSON, &after); err != nil {
			return nil, fmt.Errorf("services.planRevert: app %s after: %w", d.DocKey, err)
		}
		if live == nil || *live != after {
			return nil, revertStale("app này")
		}
		return func(ctx context.Context) (int, error) { return 0, t.DeleteApp(ctx, tenantID, d.DocKey) }, nil
	case "delete":
		if live != nil {
			return nil, revertStale("app này")
		}
		if err := staleIfSuperseded(ctx, t, tenantID, verSeq, d, "app này"); err != nil {
			return nil, err
		}
		var before appAuditDoc
		if err := decodeImage(d.BeforeJSON, &before); err != nil {
			return nil, fmt.Errorf("services.planRevert: app %s before: %w", d.DocKey, err)
		}
		return func(ctx context.Context) (int, error) { return 0, t.RestoreApp(ctx, tenantID, d.DocKey, before) }, nil
	}
	return nil, revertUnsupported(d.DocType + "/" + d.Op)
}

// RevertResult is the answer of POST /admin/publish/versions/:id/revert.
type RevertResult struct {
	RevertedID string `json:"revertedId"`
	// Entry is the new direct entry the undo left behind; nil when it could not be written (the undo still applied).
	Entry      *VersionSummary `json:"entry"`
	Reassigned int             `json:"reassigned"`
}

// Revert undoes one direct entry. Order: builder -> tenant-pinned lookup (an unknown, malformed or
// other tenant's id is the same 404) -> kind check -> stale check of EVERY document -> apply.
// ponytail: documents of one entry are applied one by one, each atomic on its own; only a field-permission
// save over several roles has more than one, and a failure on a later one leaves the earlier ones undone.
func (s *PublishService) Revert(ctx context.Context, tenantID, id string) (*RevertResult, error) {
	caller, err := s.builder(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	notFound := invalid("VERSION_NOT_FOUND", "Không tìm thấy phiên bản này")
	if _, err := uuid.Parse(id); err != nil {
		return nil, notFound
	}
	v, err := s.versions.GetVersion(ctx, tenantID, id)
	if errors.Is(err, metaversion.ErrVersionNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.Revert: load version: %w", err)
	}
	if v.VersionNo != nil || v.Kind != metaversion.KindDirect {
		return nil, invalid("REVERT_USE_ROLLBACK", "Đây là phiên bản đã xuất bản: dùng Quay lại, không dùng Hoàn tác")
	}
	if s.revert == nil {
		return nil, errors.New("services.PublishService.Revert: reverters not wired")
	}
	docs, err := s.versions.ListDocs(ctx, tenantID, v.ID)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.Revert: %w", err)
	}
	if len(docs) == 0 {
		return nil, revertUnsupported("")
	}
	steps := make([]revertStep, 0, len(docs))
	for _, d := range docs {
		st, err := planRevert(ctx, s.revert, tenantID, v.Seq, d)
		if err != nil {
			return nil, s.fail("Revert", err)
		}
		steps = append(steps, st)
	}
	actx := audit.WithActor(ctx, caller.UserID)
	reassigned := 0
	for _, st := range steps {
		n, err := st(actx)
		if err != nil {
			return nil, s.fail("Revert", err)
		}
		reassigned += n
	}
	res := &RevertResult{RevertedID: v.ID, Reassigned: reassigned}
	if latest, _, lerr := s.versions.ListVersions(ctx, tenantID, 1, 0); lerr == nil && len(latest) == 1 && latest[0].Seq > v.Seq {
		sum := summaryOf(latest[0])
		res.Entry = &sum
	}
	return res, nil
}

// ReassignUser is one person a role undo would give the role back to (FR-056).
type ReassignUser struct {
	UserID        string `json:"userId" db:"id"`
	Label         string `json:"label" db:"label"`
	Status        string `json:"status" db:"status"`
	CurrentRoleID string `json:"currentRoleId" db:"role_id"`
}

// reassignOf lists, for a "role deleted" document, the users recorded as holding the role - read from
// users of THIS tenant only, with the role each holds now, so the builder sees who would get access
// back before confirming. A user who has since left, or holds another role, shows as such.
func (s *PublishService) reassignOf(ctx context.Context, q sqlx.QueryerContext, tenantID string, meta json.RawMessage) ([]ReassignUser, error) {
	var m struct {
		AssignedUserIDs []string `json:"assignedUserIds"`
	}
	out := []ReassignUser{}
	if len(meta) == 0 || json.Unmarshal(meta, &m) != nil || len(m.AssignedUserIDs) == 0 {
		return out, nil
	}
	query, args, err := sqlx.In(`SELECT id, COALESCE(NULLIF(full_name, ''), email) AS label, status, COALESCE(role_id, '') AS role_id
		FROM users WHERE tenant_id = ? AND id IN (?) ORDER BY label, id`, tenantID, m.AssignedUserIDs)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.reassignOf: %w", err)
	}
	if err := sqlx.SelectContext(ctx, q, &out, query, args...); err != nil {
		return nil, fmt.Errorf("services.PublishService.reassignOf: %w", err)
	}
	return out, nil
}

package notes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

// MaxBodyRunes is the maximum note body length (rune count, matching the api-conventions.md
// convention that string caps count runes not bytes — RECORD_TOO_LONG for record text fields does
// the same). A hard, non-configurable constant: no other slice of the engine needed this
// per-tenant tunable, so it is not wired to an env var (ponytail — add one only when a real need
// shows up).
const MaxBodyRunes = 5000

// RecordGuard is the slice of *services.RecordService the notes subsystem needs: authorize the
// caller against the PARENT record's view grant + tenant + A5 row scope, regardless of which HTTP
// method/action the route's own ACL middleware resolved (record_note.go's ResolveNoteTarget
// explains why). Declared here (consumer-side) so Service stays testable with a fake and does not
// hard-depend on the concrete record service beyond this one call — mirrors files.RecordGuard.
type RecordGuard interface {
	ResolveNoteTarget(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID string) (canonicalEntity string, viewScope services.RecordScope, err error)
	// ResolveWriteTarget is the write-side gate of Create: the record under the caller's re-resolved
	// grant for action, shares OFF, a miss answered like every record write (#43).
	ResolveWriteTarget(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, action string) (canonicalEntity string, err error)
}

// EntityAuthorizer resolves whether a role may perform an action on an entity. Satisfied by
// *services.ACLService (the SAME decider RequireACL/AuthorizeView use). Used ONLY by Delete's
// "note author OR entity delete-grant" override — List/Create rely entirely on RecordGuard. A nil
// EntityAuthorizer makes Delete fail closed to author-only (see Delete).
type EntityAuthorizer interface {
	Authorize(ctx context.Context, tenantID, roleID, entity, action string) (allowed bool, accessLevel string, err error)
}

// Service is the record-notes business logic: it authorizes every operation against the parent
// record (RecordGuard), validates the note body, and delegates storage to Repository. It knows
// nothing about HTTP.
type Service struct {
	repo  *Repository
	guard RecordGuard
	authz EntityAuthorizer
	newID func() string
}

// NewService wires the notes service. authz may be nil (Delete then falls back to author-only —
// see Delete); production always wires *services.ACLService.
func NewService(repo *Repository, guard RecordGuard, authz EntityAuthorizer) *Service {
	return &Service{repo: repo, guard: guard, authz: authz, newID: uuid.NewString}
}

// List returns a page of live notes on (entityName, recordID), newest first, plus the total count
// for the offset-pagination envelope. Authorizes via RecordGuard first — a caller who may not view
// the parent record gets that EXACT error (ErrEntityNotFound / ErrRecordNotFound), never a note.
func (s *Service) List(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID string, page, limit int) ([]Note, int, error) {
	canonical, _, err := s.guard.ResolveNoteTarget(ctx, scope, tenantID, entityName, recordID)
	if err != nil {
		return nil, 0, err
	}
	page, limit = dynrepo.ClampPageLimit(page, limit)
	offset := (page - 1) * limit
	rows, err := s.repo.ListByRecord(ctx, tenantID, canonical, recordID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("notes.Service.List: %w", err)
	}
	total, err := s.repo.CountByRecord(ctx, tenantID, canonical, recordID)
	if err != nil {
		return nil, 0, fmt.Errorf("notes.Service.List: %w", err)
	}
	return rows, total, nil
}

// Create authorizes against the parent record (RecordGuard — same "ACL trước business logic"
// order as everywhere else), then validates the body, then inserts. created_by is ALWAYS
// scope.UserID — the verified authenticated caller — never a client-supplied value (mirrors the
// A5 invariant "created_by là user THẬT").
func (s *Service) Create(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, body string) (*Note, error) {
	// specs/060 (#349): the floor is "view" — whoever can read the record can comment on it. A manual
	// share is read-only (specs/051) and a note is a write on the record's conversation, so the record
	// must be reachable under the view grant WITHOUT the share. A miss gets the shared write-miss answer
	// (#43): 403 RECORD_SHARED_READ_ONLY for a share-only viewer, the 404 of a missing record otherwise.
	canonical, err := s.guard.ResolveWriteTarget(ctx, scope, tenantID, entityName, recordID, "view")
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil, &services.ValidationError{
			Code: "RECORD_NOTE_EMPTY", Msg: "Nội dung ghi chú không được để trống", Details: map[string]any{},
		}
	}
	if n := utf8.RuneCountInString(trimmed); n > MaxBodyRunes {
		return nil, &services.ValidationError{
			Code: "RECORD_NOTE_TOO_LONG",
			Msg:  fmt.Sprintf("Nội dung ghi chú vượt quá %d ký tự", MaxBodyRunes),
			Details: map[string]any{
				"max": MaxBodyRunes,
			},
		}
	}
	now := time.Now().UTC()
	n := Note{
		ID:         s.newID(),
		TenantID:   tenantID,
		EntityName: canonical,
		RecordID:   recordID,
		Body:       trimmed,
		CreatedBy:  scope.UserID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.repo.Insert(ctx, n); err != nil {
		return nil, fmt.Errorf("notes.Service.Create: %w", err)
	}
	return &n, nil
}

// Delete authorizes against the parent record (RecordGuard), fetches the note SCOPED to that
// exact (entity, record) pair (so a noteId belonging to a different record 404s rather than
// leaking cross-record), then applies the delete rule: the note's own author may always delete
// it; anyone else needs the entity's "delete" ACL grant (checked via EntityAuthorizer — the SAME
// decider RequireACL uses, just invoked directly since the route's own middleware floor is only
// "view"); an admin caller always bypasses, exactly like every other ACL surface in this engine.
//
// EntityAuthorizer==nil (not wired) fails CLOSED to author-only — never silently widens to
// "anyone may delete anyone's note".
func (s *Service) Delete(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, noteID string) error {
	canonical, _, err := s.guard.ResolveNoteTarget(ctx, scope, tenantID, entityName, recordID)
	if err != nil {
		return err
	}
	n, err := s.repo.GetForRecord(ctx, tenantID, canonical, recordID, noteID)
	if err != nil {
		return err // ErrNoteNotFound, or a repo error already wrapped with its own method name
	}
	if !scope.IsAdmin && n.CreatedBy != scope.UserID {
		// specs/051-record-manual-share: this route is pinned to "view", so a caller who sees the
		// record only through a manual share got past the guard above. A share is read-only and
		// removing someone else's note is a write on the record's conversation — the record must
		// also be reachable WITHOUT the share.
		if scope.IncludeShares {
			own := scope
			own.IncludeShares = false
			if _, _, err := s.guard.ResolveNoteTarget(ctx, own, tenantID, entityName, recordID); err != nil {
				if errors.Is(err, services.ErrRecordNotFound) {
					return services.ErrRecordSharedReadOnly
				}
				return err
			}
		}
		allowed := false
		if s.authz != nil {
			// Only the boolean matters here: "có quyền delete trên entity đó" is a plain
			// allow/deny on the "delete" action. The resolved row-level access_level
			// (owner/team/all) that Authorize also returns is deliberately ignored — a note is
			// not itself row-scoped (its PARENT record already was, via ResolveNoteTarget above);
			// holding the entity's delete grant at all is the bar, matching the plain reading of
			// "người có quyền delete trên entity đó" (the seeded "Quản lý" role is delete:all
			// anyway, so this never under-authorizes in practice).
			var aerr error
			allowed, _, aerr = s.authz.Authorize(ctx, tenantID, scope.RoleID, canonical, "delete")
			if aerr != nil {
				return fmt.Errorf("notes.Service.Delete: authorize: %w", aerr)
			}
		}
		if !allowed {
			return &services.ValidationError{
				Code: "FORBIDDEN", Msg: "Bạn không có quyền xoá ghi chú này", Details: map[string]any{},
			}
		}
	}
	if err := s.repo.SoftDelete(ctx, tenantID, noteID); err != nil {
		return fmt.Errorf("notes.Service.Delete: %w", err)
	}
	return nil
}

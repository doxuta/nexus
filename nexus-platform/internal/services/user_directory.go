package services

import (
	"context"
	"fmt"
	"math"

	"nexus/platform/internal/auth"
)

// directoryRepo is the consumer-side slice of auth.UserRepository the directory service needs —
// declared deliberately narrow (ONE method) so this non-admin-gated read can never grow into the
// privileged userCRUDRepo surface UserService depends on. Keeping the two interfaces separate is
// the point: a future field added to userCRUDRepo (or to UserView) does not silently become
// reachable from GET /users/directory just because both happen to read the same table.
type directoryRepo interface {
	ListDirectory(ctx context.Context, tenantID, q string, limit, offset int) ([]auth.DirectoryUser, int64, error)
}

// UserDirectoryService implements GET /users/directory (docs/NEXUS-MOT-XUONG-2026-07-29.md §0b —
// closing the owner-change FE-gap: PUT /:entity/:id/owner permits any manager holding "edit all"
// (or an admin), but the only existing listing, /admin/users, is admin-gated, so a non-admin
// manager could open the reassign dialog and never populate it). ANY authenticated user of the
// tenant may call ListDirectory — it returns only {id, fullName, email} for ACTIVE users of the
// caller's OWN tenant (auth.DirectoryUser's SQL never selects password_hash / is_admin / role_id /
// status / MFA columns in the first place). This is intentionally its own service, not a method on
// UserService, so the admin CRUD surface never has to grow a method a non-admin path depends on.
type UserDirectoryService struct {
	repo directoryRepo
}

// NewUserDirectoryService wires the service over the directory repository.
func NewUserDirectoryService(repo directoryRepo) *UserDirectoryService {
	return &UserDirectoryService{repo: repo}
}

// DirectoryResult is the paginated result returned by ListDirectory.
type DirectoryResult struct {
	Items      []auth.DirectoryUser
	Page       int
	Limit      int
	Total      int
	TotalPages int
}

// ListDirectory returns a page of the caller's tenant's active users (safe projection),
// optionally filtered by q (substring match on full name or email). page/limit default to 1/20
// and limit clamps to max 100 — the same offset-pagination defaults as every other list endpoint
// (api-conventions.md), mirroring NotificationService.ListNotifications.
func (s *UserDirectoryService) ListDirectory(ctx context.Context, tenantID, q string, page, limit int) (*DirectoryResult, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset := (page - 1) * limit

	rows, total, err := s.repo.ListDirectory(ctx, tenantID, q, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("services.UserDirectoryService.ListDirectory: %w", err)
	}
	totalPages := int(math.Ceil(float64(total) / float64(limit)))
	if totalPages < 1 {
		totalPages = 1
	}
	return &DirectoryResult{
		Items:      rows,
		Page:       page,
		Limit:      limit,
		Total:      int(total),
		TotalPages: totalPages,
	}, nil
}

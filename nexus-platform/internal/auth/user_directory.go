package auth

import (
	"context"
	"fmt"
	"strings"
)

// DirectoryUser is the minimal, safe projection ListDirectory returns — TRỤ 2 mục 2 FE-gap close
// (docs/NEXUS-MOT-XUONG-2026-07-29.md §0b): a manager holding "edit all" (or an admin) may reassign
// a record's owner (services.RecordService.SetOwner), but the ONLY existing user listing,
// GET /admin/users, is admin-gated — so a non-admin manager could reach the action but never load
// anyone to assign it to. This is a DIFFERENT, DELIBERATELY NARROWER shape than auth.User /
// services.UserView: it carries ONLY id + full_name + email, and the SQL in ListDirectory selects
// exactly those three columns — password_hash, is_admin, role_id, status, failed_login_count,
// locked_until never leave the database for this path, so there is no sensitive field left in
// memory for a future change to forget to strip further up the stack.
type DirectoryUser struct {
	ID       string `gorm:"column:id"        json:"id"`
	FullName string `gorm:"column:full_name" json:"fullName"`
	Email    string `gorm:"column:email"     json:"email"`
}

// escapeLikeWildcards escapes the LIKE wildcard characters (%, _, \) in a user-supplied search
// term so they are treated as literals when used with LIKE ? ESCAPE '\\'. This is a
// match-correctness measure (the value still travels as a `?` placeholder — it is NOT an
// injection defense). Mirrors dynrepo's helper of the same name; duplicated here (four lines)
// rather than exported across packages — auth sits below dynrepo in the dependency graph and a
// shared micro-package for one string helper is not worth the indirection (ponytail).
func escapeLikeWildcards(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// ListDirectory returns a page of ACTIVE users in tenantID plus the total matching count, for the
// org-wide "who can I assign this to" picker. Every authenticated user of the tenant may call the
// service/handler built on this — it is NOT gated to admins (unlike ListUsers/GetUserByID above).
//
// q, when non-empty (after trimming), filters to rows whose full_name OR email contains it. No
// explicit COLLATE is needed (unlike the JSON-extraction paths in package dynrepo): the `users`
// table is CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci (migration 000003). EMPIRICALLY CONFIRMED
// (TestUserDirectoryIntegration_TenantIsolation_DisabledFilter_QSearch, against a real migrated
// MySQL 8, not just inferred from the collation name): this collation is BOTH case-insensitive
// ("bob"/"BOB" match "Bob Tran") AND accent-folding for Vietnamese diacritics ("nguyen" with no
// diacritics matches full_name "Nguyễn Văn A") — the common real-world search a manager types.
//
// full_name is nullable in the schema (VARCHAR(150) DEFAULT NULL) even though every write path
// today supplies at least an empty string, never SQL NULL — COALESCE guards the theoretical case
// so a NULL can never fail the Go string scan. Rows are ordered by email (NOT NULL, unique per
// tenant) then id, a deterministic tie-break that does not depend on full_name being set — sorting
// by a nullable column would put every blank-name row first (MySQL sorts NULL as the lowest value
// in ASC order), which reads as a page of broken-looking blank rows.
//
// limit/offset are the caller's already-clamped page window (see services.UserDirectoryService).
func (r *UserRepository) ListDirectory(ctx context.Context, tenantID, q string, limit, offset int) ([]DirectoryUser, int64, error) {
	// deleted_at IS NULL is explicit because Table("users") gets NO GORM soft-delete scope, and an
	// admin delete (SoftDeleteUser) leaves status='active'. Same predicate as the record-time user
	// check (dynrepo.BatchActiveUserExists) — the picker must never offer someone Save refuses.
	where := "tenant_id = ? AND status = ? AND deleted_at IS NULL"
	args := []any{tenantID, "active"}

	q = strings.TrimSpace(q)
	if q != "" {
		like := "%" + escapeLikeWildcards(q) + "%"
		where += " AND (full_name LIKE ? ESCAPE '\\\\' OR email LIKE ? ESCAPE '\\\\')"
		args = append(args, like, like)
	}

	var total int64
	if err := r.db.WithContext(ctx).Table("users").Where(where, args...).Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("auth.UserRepository.ListDirectory: count: %w", err)
	}

	var rows []DirectoryUser
	err := r.db.WithContext(ctx).Table("users").Where(where, args...).
		Select("id, COALESCE(full_name, '') AS full_name, email").
		Order("email ASC, id ASC").
		Limit(limit).Offset(offset).
		Find(&rows).Error
	if err != nil {
		return nil, 0, fmt.Errorf("auth.UserRepository.ListDirectory: %w", err)
	}
	return rows, total, nil
}

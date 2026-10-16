package services

import (
	"context"
	"fmt"
	"strings"

	"nexus/platform/internal/dynrepo"
)

// `user` cells of a CSV import (queue #357, specs/061 §7): a customer's file names the person in
// charge the way people do — "an@example.com" or "Nguyễn Văn An" — not by UUID. A cell is matched, in
// order, as the user's id, then e-mail, then full name, all within the tenant and only against ACTIVE
// users (the same set a record save accepts, checkUserTargets). A name shared by several users is an
// error of that row — never a silent pick. A cell that names nobody reads exactly like a disabled
// user or another tenant's id: the one RECORD_USER_INVALID of the record routes (anti-enumeration).

// userLoginMatcher finds ACTIVE users by e-mail or full name (dynrepo.BatchActiveUsersByLogin). Read
// off the wired RelationBatchChecker — production's *dynrepo.Repository is both — like relationValueMatcher.
type userLoginMatcher interface {
	BatchActiveUsersByLogin(ctx context.Context, tenantID string, values []string) ([]dynrepo.UserLoginMatch, error)
}

// A typo'd method name would silently disarm the lookup (the type assertion just fails): fail the build.
var _ userLoginMatcher = (*dynrepo.Repository)(nil)

// resolveUsers turns every `user` cell of rows into the id of an active user of the tenant. A row
// survives only if all its user cells resolved; each cell that did not is reported on its own
// (nobody: RECORD_USER_INVALID; several users of that name: IMPORT_USER_AMBIGUOUS; lookup failure:
// IMPORT_ERROR; lookup not wired: RECORD_USER_UNAVAILABLE, fail closed as checkUserTargets does).
// Two batched queries for the whole file: one id probe, one e-mail/name lookup for the rest.
func (s *ImportService) resolveUsers(ctx context.Context, tenantID string, specs []FieldSpec, rows []validRow) (kept []validRow, errs []ImportRowError) {
	type cell struct {
		row   int // index into rows
		field string
		value string // trimmed cell
	}
	var cells []cell
	distinct := make(map[string]struct{})
	for i, vr := range rows {
		for _, f := range specs {
			if f.FieldType != "user" {
				continue
			}
			raw, _ := vr.payload[f.Name].(string)
			if v := strings.TrimSpace(raw); v != "" {
				cells = append(cells, cell{row: i, field: f.Name, value: v})
				distinct[v] = struct{}{}
			}
		}
	}
	if len(cells) == 0 {
		return rows, nil
	}

	bad := make(map[int]bool)
	failAll := func(code, format string, args ...any) {
		for _, c := range cells {
			bad[c.row] = true
			errs = append(errs, ImportRowError{Row: rows[c.row].rowNum, Field: c.field, Code: code, Message: fmt.Sprintf(format, args...)})
		}
	}
	exister, canProbe := s.relChecker.(userExister)
	matcher, canMatch := s.relChecker.(userLoginMatcher)
	if !canProbe || !canMatch {
		failAll("RECORD_USER_UNAVAILABLE", "kiểm tra field kiểu user hiện không khả dụng — vui lòng thử lại sau")
		return keepOK(rows, bad), errs
	}

	values := make([]string, 0, len(distinct))
	for v := range distinct {
		values = append(values, v)
	}
	isID, err := exister.BatchActiveUserExists(ctx, tenantID, values)
	if err != nil {
		failAll("IMPORT_ERROR", "không kiểm tra được người dùng: %s", err.Error())
		return keepOK(rows, bad), errs
	}
	var logins []string // lower-cased cells that are not ids
	for _, v := range values {
		if !isID[v] {
			logins = append(logins, strings.ToLower(v))
		}
	}
	byEmail := make(map[string][]string)
	byName := make(map[string][]string)
	if len(logins) > 0 {
		matches, err := matcher.BatchActiveUsersByLogin(ctx, tenantID, logins)
		if err != nil {
			failAll("IMPORT_ERROR", "không tra được người dùng: %s", err.Error())
			return keepOK(rows, bad), errs
		}
		for _, m := range matches {
			byEmail[m.Email] = append(byEmail[m.Email], m.ID)
			if m.Name != "" {
				byName[m.Name] = append(byName[m.Name], m.ID)
			}
		}
	}

	for _, c := range cells {
		fail := func(code, format string, args ...any) {
			bad[c.row] = true
			errs = append(errs, ImportRowError{Row: rows[c.row].rowNum, Field: c.field, Code: code, Message: fmt.Sprintf(format, args...)})
		}
		key := strings.ToLower(c.value)
		switch {
		case isID[c.value]:
			rows[c.row].payload[c.field] = c.value
		case len(byEmail[key]) == 1:
			rows[c.row].payload[c.field] = byEmail[key][0]
		case len(byEmail[key]) == 0 && len(byName[key]) == 1:
			rows[c.row].payload[c.field] = byName[key][0]
		case len(byEmail[key]) == 0 && len(byName[key]) > 1:
			fail("IMPORT_USER_AMBIGUOUS", "%q trùng tên của %d người dùng — ghi email của người đó", c.value, len(byName[key]))
		default:
			fail("RECORD_USER_INVALID", "field %q phải là email, tên hoặc id của một người dùng đang hoạt động trong tổ chức", c.field)
		}
	}
	return keepOK(rows, bad), errs
}

// keepOK is rows without the indexes marked bad.
func keepOK(rows []validRow, bad map[int]bool) []validRow {
	kept := make([]validRow, 0, len(rows))
	for i, vr := range rows {
		if !bad[i] {
			kept = append(kept, vr)
		}
	}
	return kept
}

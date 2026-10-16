// doctor_locked_required_check.go — queue #143: C9, the report-only list of field-ACL rows that
// leave a role unable to create anything: a hidden or read-only row (sys_field_acl) on a required
// field the engine cannot fill (requiredEngineCannotFill — the predicate the PutGrants and EditField
// guards refuse on). Those guards stop NEW rows of this shape; C9 lists the ones that got there
// before them.
//
// It is the admin-side half of an anti-oracle rule: the create path answers a role that hits a
// HIDDEN required field without naming it (api-conventions §4.4b), and points the admin here.
// NO automated fix by design: the exits (turn "bắt buộc" off, set a default, lift the row) are
// product decisions, and lifting a hidden row exposes a field — never the Doctor's call.
package services

import (
	"context"
	"errors"
	"fmt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/metadata"
)

// DoctorCheckLockedRequiredFields is the C9 check id (C = soft metadata/config drift).
const DoctorCheckLockedRequiredFields = "C9_LOCKED_REQUIRED_FIELDS"

// doctorFieldRestrictions is the slice of *auth.FieldACLRepository C9 reads (tenant-pinned,
// read-only): every restrictive row of the tenant joined to its role.
type doctorFieldRestrictions interface {
	ListFieldRestrictions(ctx context.Context, tenantID, entity, field string) ([]auth.FieldACLRestriction, error)
}

// WithFieldRestrictions registers C9 over the field-ACL repository. Chainable.
func (s *DoctorService) WithFieldRestrictions(r doctorFieldRestrictions) *DoctorService {
	s.fieldRestrictions = r
	s.runner = doctor.NewRunner(s.checks())
	return s
}

func (s *DoctorService) checkLockedRequiredFields() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckLockedRequiredFields,
		Severity:    doctor.SeverityWarn, // a role cannot create any record of the entity
		AutoFixSafe: false,
		ReportOnly:  true, // every exit is a product decision; lifting a hidden row exposes a field
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			// ponytail: one unbounded read of the tenant's restrictive rows — at most roles x
			// fields, a few hundred in practice; findings are still capped below.
			rows, err := s.fieldRestrictions.ListFieldRestrictions(ctx, tenantID, "", "")
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.C9 detect: %w", err)
			}
			defsByEntity := map[string]map[string]metadata.FieldDefinition{}
			findings := make([]doctor.Finding, 0)
			for _, r := range rows {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				defs, seen := defsByEntity[r.EntityName]
				if !seen {
					defs, err = s.doctorFieldDefs(ctx, tenantID, r.EntityName)
					if err != nil {
						return nil, fmt.Errorf("services.Doctor.C9 detect: %w", err)
					}
					defsByEntity[r.EntityName] = defs
				}
				def, ok := defs[r.FieldName]
				if !ok {
					continue // a row on a gone field/entity restricts nothing a create needs
				}
				deadEnd, err := requiredEngineCannotFill(def)
				if err != nil {
					return nil, fmt.Errorf("services.Doctor.C9 detect: %w", err)
				}
				if !deadEnd {
					continue
				}
				role, how, label := r.RoleLabel, "chỉ đọc", def.Label
				if role == "" {
					role = r.RoleName
				}
				if !r.CanView {
					how = "ẩn"
				}
				if label == "" {
					label = def.Name
				}
				findings = append(findings, doctor.Finding{
					Key: r.EntityName + "." + r.FieldName + ":" + r.RoleID,
					Detail: fmt.Sprintf("Vai %q đang %s trường bắt buộc %s.%s (%q) mà máy không tự điền — vai này không tạo được bản ghi %s nào (form, \"Tạo nhiều dòng\", nhập CSV). Cách gỡ: %s",
						role, how, r.EntityName, r.FieldName, label, r.EntityName, lockedRequiredRemedy(def.FieldType)),
				})
			}
			return findings, nil
		},
	}
}

// doctorFieldDefs maps an entity's live fields by name; nil for an entity that no longer exists.
func (s *DoctorService) doctorFieldDefs(ctx context.Context, tenantID, entity string) (map[string]metadata.FieldDefinition, error) {
	ent, err := s.meta.GetEntityByName(ctx, tenantID, entity)
	if errors.Is(err, metadata.ErrEntityNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("entity %s: %w", entity, err)
	}
	fields, err := s.meta.ListFields(ctx, tenantID, ent.ID)
	if err != nil {
		return nil, fmt.Errorf("fields %s: %w", entity, err)
	}
	defs := make(map[string]metadata.FieldDefinition, len(fields))
	for _, f := range fields {
		defs[f.Name] = f
	}
	return defs, nil
}

package vertical_pack

import "testing"

// DAILY-QUEUE #243: lms_enrollments.assigned_advisor must be a `user` field on a fresh install (so the
// enrolment screen shows a name, like CRM/Ticketing/PM) and the pack version must be bumped so
// existing tenants see an upgrade. Existing tenants keep `text` (install never retypes a live field).
func TestLMSPack_AssignedAdvisorIsUserField(t *testing.T) {
	pack, ok := BuiltinPack("lms")
	if !ok {
		t.Fatal("builtin lms pack not found")
	}
	if pack.Manifest.Version == "1.3.1" {
		t.Errorf("lms version still 1.3.1, want bumped")
	}
	found := false
	for _, e := range pack.Entities {
		if e.Name != "lms_enrollments" {
			continue
		}
		for _, f := range e.Fields {
			if f.Name == "assigned_advisor" {
				found = true
				if f.FieldType != "user" {
					t.Errorf("assigned_advisor fieldType = %q, want user", f.FieldType)
				}
			}
		}
	}
	if !found {
		t.Fatal("lms_enrollments.assigned_advisor not found")
	}
}

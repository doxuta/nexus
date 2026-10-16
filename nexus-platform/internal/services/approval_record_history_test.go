package services

import (
	"context"
	"testing"

	"nexus/platform/internal/bpm"
)

// TestGetInstancesForRecord_PublicHistoryForNonAdmin: the record's approvals tab reads the same
// instance the Nexus team debugs; a non-admin never gets the machine's lines, an admin does.
func TestGetInstancesForRecord_PublicHistoryForNonAdmin(t *testing.T) {
	inst := routedInstance(t)
	inst.EntityName, inst.RecordID = "accounts", "rec-1"
	key := "accounts\x00rec-1"
	seed := map[string][]bpm.Instance{key: {*inst}}
	svc := NewApprovalService(&fakeApprovalProcessRepo{}, &fakeApprovalInstanceRepo{byRecord: seed}, &fakeApprovalDecider{}).
		WithRequestFlow(nil, &fakeApprovalRecordReader{authzLevel: "all"})

	rows, err := svc.GetInstancesForRecord(context.Background(), RecordScope{UserID: "creator-1", AccessLevel: "all"}, "t1", "accounts", "rec-1")
	if err != nil || len(rows) != 1 || len(rows[0].History) != 1 {
		t.Fatalf("non-admin tab = %+v (%v); want one hồ sơ with only the submit line", rows, err)
	}
	rows, err = svc.GetInstancesForRecord(context.Background(), RecordScope{IsAdmin: true}, "t1", "accounts", "rec-1")
	if err != nil || len(rows) != 1 || len(rows[0].History) != 5 {
		t.Fatalf("admin tab = %+v (%v); want all 5 lines", rows, err)
	}
	if len(seed[key][0].History) != 5 {
		t.Fatal("redaction wrote through to the stored history")
	}
}

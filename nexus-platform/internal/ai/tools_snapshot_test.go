package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// snapshotResult decodes a snapshot_model tool result into its typed shape for assertions.
type snapshotResult struct {
	OK     bool `json:"ok"`
	Result struct {
		Entities []struct {
			Name   string `json:"name"`
			Label  string `json:"label"`
			Fields []struct {
				Name      string `json:"name"`
				Label     string `json:"label"`
				FieldType string `json:"fieldType"`
			} `json:"fields"`
			FieldsTruncated bool `json:"fieldsTruncated"`
			FieldsTotal     int  `json:"fieldsTotal"`
		} `json:"entities"`
		EntitiesTruncated bool `json:"entitiesTruncated"`
		EntitiesTotal     int  `json:"entitiesTotal"`
	} `json:"result"`
	Error string `json:"error"`
}

func parseSnapshot(t *testing.T, raw string) snapshotResult {
	t.Helper()
	var s snapshotResult
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("snapshot result not JSON: %v (%s)", err, raw)
	}
	return s
}

// TestTools_SnapshotModel_TenantIsolation proves the snapshot is composed ONLY from the current
// tenant's reader view: tenant A's snapshot carries its own entity+fields and none of tenant B's.
func TestTools_SnapshotModel_TenantIsolation(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	raw := e.execute(context.Background(), "snapshot_model", "{}")

	if strings.Contains(raw, "invoices") || strings.Contains(raw, "Hóa đơn") || strings.Contains(raw, `"total"`) {
		t.Fatalf("tenant A's snapshot leaked tenant B data: %s", raw)
	}

	s := parseSnapshot(t, raw)
	if !s.OK {
		t.Fatalf("snapshot_model should succeed: %s", raw)
	}
	if len(s.Result.Entities) != 1 || s.Result.Entities[0].Name != "patients" {
		t.Fatalf("tenant A's snapshot missing its own entity: %s", raw)
	}
	got := s.Result.Entities[0]
	if len(got.Fields) != 1 || got.Fields[0].Name != "full_name" || got.Fields[0].FieldType != "text" {
		t.Fatalf("tenant A's snapshot missing its own fields: %+v", got.Fields)
	}
	// Read-only: a snapshot never touches the draft buffer.
	if len(e.draft) != 0 {
		t.Fatalf("snapshot_model must not mutate the draft buffer, got %+v", e.draft)
	}
}

// TestTools_SnapshotModel_EmptyTenant proves an entity-less tenant yields a VALID empty snapshot
// ({"entities":[]}), not an error — "nothing built yet" is a normal Discovery starting point.
func TestTools_SnapshotModel_EmptyTenant(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-EMPTY"}
	raw := e.execute(context.Background(), "snapshot_model", "{}")

	s := parseSnapshot(t, raw)
	if !s.OK {
		t.Fatalf("snapshot_model on an empty tenant must succeed, got %s", raw)
	}
	if len(s.Result.Entities) != 0 {
		t.Fatalf("empty tenant should have an empty snapshot, got %s", raw)
	}
	// The entities key must be an explicit [] (never null) so the model reads "empty model", not
	// a missing field.
	if !strings.Contains(raw, `"entities":[]`) {
		t.Fatalf(`expected explicit "entities":[] in %s`, raw)
	}
}

// TestTools_SnapshotModel_CompactShape pins the per-field summary to EXACTLY {name, label,
// fieldType} — the compact contract (spec §17). A field row growing extra keys (options, required)
// would multiply the token cost of every Discovery turn and must be a deliberate decision.
func TestTools_SnapshotModel_CompactShape(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	raw := e.execute(context.Background(), "snapshot_model", "{}")

	var loose struct {
		Result struct {
			Entities []struct {
				Fields []map[string]any `json:"fields"`
			} `json:"entities"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &loose); err != nil {
		t.Fatalf("snapshot result not JSON: %v", err)
	}
	if len(loose.Result.Entities) == 0 || len(loose.Result.Entities[0].Fields) == 0 {
		t.Fatalf("expected at least one entity with fields: %s", raw)
	}
	row := loose.Result.Entities[0].Fields[0]
	if len(row) != 3 {
		t.Fatalf("compact field row must carry exactly name/label/fieldType, got %d keys: %v", len(row), row)
	}
	for _, k := range []string{"name", "label", "fieldType"} {
		if _, ok := row[k]; !ok {
			t.Fatalf("compact field row missing key %q: %v", k, row)
		}
	}
}

// TestTools_SnapshotModel_CapsRespected proves both output bounds: a tenant exceeding the entity
// cap and the per-entity field cap gets a TRIMMED snapshot that says so honestly (truncated flags
// + true totals) instead of an unbounded blob or a silent lie.
func TestTools_SnapshotModel_CapsRespected(t *testing.T) {
	const (
		extraEntities = 5
		extraFields   = 3
	)
	f := &fakeMetaReader{
		entities: map[string][]metadata.EntityDefinition{},
		fields:   map[string]map[string][]metadata.FieldDefinition{"tenant-BIG": {}},
	}
	for i := 0; i < maxSnapshotEntities+extraEntities; i++ {
		id := fmt.Sprintf("e%d", i)
		f.entities["tenant-BIG"] = append(f.entities["tenant-BIG"], metadata.EntityDefinition{
			ID: id, TenantID: "tenant-BIG", Name: fmt.Sprintf("entity_%d", i), Label: fmt.Sprintf("Entity %d", i),
		})
		var fields []metadata.FieldDefinition
		for j := 0; j < maxSnapshotFieldsPerEntity+extraFields; j++ {
			fields = append(fields, metadata.FieldDefinition{
				Name: fmt.Sprintf("field_%d", j), Label: fmt.Sprintf("Field %d", j), FieldType: "text",
			})
		}
		f.fields["tenant-BIG"][id] = fields
	}

	e := &toolExecutor{reader: f, tenantID: "tenant-BIG"}
	s := parseSnapshot(t, e.execute(context.Background(), "snapshot_model", "{}"))
	if !s.OK {
		t.Fatal("snapshot_model on a huge tenant must still succeed (trimmed)")
	}
	if len(s.Result.Entities) != maxSnapshotEntities {
		t.Fatalf("expected snapshot trimmed to %d entities, got %d", maxSnapshotEntities, len(s.Result.Entities))
	}
	if !s.Result.EntitiesTruncated || s.Result.EntitiesTotal != maxSnapshotEntities+extraEntities {
		t.Fatalf("expected entitiesTruncated=true with entitiesTotal=%d, got truncated=%v total=%d",
			maxSnapshotEntities+extraEntities, s.Result.EntitiesTruncated, s.Result.EntitiesTotal)
	}
	first := s.Result.Entities[0]
	if len(first.Fields) != maxSnapshotFieldsPerEntity {
		t.Fatalf("expected fields trimmed to %d, got %d", maxSnapshotFieldsPerEntity, len(first.Fields))
	}
	if !first.FieldsTruncated || first.FieldsTotal != maxSnapshotFieldsPerEntity+extraFields {
		t.Fatalf("expected fieldsTruncated=true with fieldsTotal=%d, got truncated=%v total=%d",
			maxSnapshotFieldsPerEntity+extraFields, first.FieldsTruncated, first.FieldsTotal)
	}
}

// TestTools_SnapshotModel_UntruncatedOmitsFlags pins the flag hygiene: a small (normal Discovery)
// tenant's snapshot must NOT carry truncation keys at all — noise-free for the model.
func TestTools_SnapshotModel_UntruncatedOmitsFlags(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	raw := e.execute(context.Background(), "snapshot_model", "{}")
	if strings.Contains(raw, "Truncated") || strings.Contains(raw, "Total") {
		t.Fatalf("untruncated snapshot must omit truncation keys entirely: %s", raw)
	}
}

// erroringFieldsReader wraps the fake to fail ListFields — the snapshot must fail WHOLE, never
// return a partial model that silently omits an entity's fields (that would corrupt coverage).
type erroringFieldsReader struct{ *fakeMetaReader }

func (r *erroringFieldsReader) ListFields(context.Context, string, string) ([]metadata.FieldDefinition, error) {
	return nil, errors.New("boom")
}

func TestTools_SnapshotModel_ReaderError_FailsWhole(t *testing.T) {
	e := &toolExecutor{reader: &erroringFieldsReader{twoTenantReader()}, tenantID: "tenant-A"}
	ok, m := parseToolResult(t, e.execute(context.Background(), "snapshot_model", "{}"))
	if ok {
		t.Fatal("snapshot_model must fail when a reader call fails (no partial snapshot)")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "SNAPSHOT_FAILED") {
		t.Fatalf("expected SNAPSHOT_FAILED, got %q", errStr)
	}
}

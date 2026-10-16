package services

import (
	"errors"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
)

// m8Code unwraps the *ValidationError code from a validator error (fails the test otherwise).
func m8Code(t *testing.T, err error) string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %v", err)
	}
	return ve.Code
}

// TestValidateReportConfig_DepthModes pins the M8 contract of the exported wrapper:
//   - nil liveFields (the apply path — CreateReport/UpdateReport): SHAPE-ONLY. Deep semantic
//     violations (a bucket on a non-date groupBy) deliberately PASS here because they are
//     re-validated at run time against live metadata.
//   - non-nil liveFields (the AI Builder's propose_report): the SAME config is rejected with the
//     RUN-TIME code, because the deep check runs through resolveAggregatePlan — the one body the
//     run-time Aggregate engine executes.
//   - shape violations reject identically in BOTH modes (one shape body).
func TestValidateReportConfig_DepthModes(t *testing.T) {
	live := []metadata.FieldDefinition{
		{Name: "price", FieldType: "number"},
		{Name: "visit_date", FieldType: "date", Options: strPtr(`{"indexed":true}`)},
	}

	// Deep divergence: bucket over the (non-date) id column.
	cfgBucketOnID := ReportConfig{GroupBy: "id", Metric: "count", Bucket: "month"}
	if _, _, err := ValidateReportConfig("summary", cfgBucketOnID, nil); err != nil {
		t.Fatalf("shape-only mode must defer deep checks to run time, got %v", err)
	}
	_, _, err := ValidateReportConfig("summary", cfgBucketOnID, live)
	if got := m8Code(t, err); got != "AGGREGATE_BUCKET_FIELD_NOT_DATE" {
		t.Fatalf("deep mode must surface the run-time code, got %s", got)
	}

	// Shape violations: identical rejection in both modes.
	for _, mode := range []struct {
		name       string
		liveFields []metadata.FieldDefinition
	}{{"shape-only", nil}, {"deep", live}} {
		_, _, err := ValidateReportConfig("summary", ReportConfig{Metric: "count"}, mode.liveFields)
		if got := m8Code(t, err); got != "REPORT_CONFIG_INVALID" {
			t.Fatalf("%s: summary without groupBy must be REPORT_CONFIG_INVALID, got %s", mode.name, got)
		}
		_, _, err = ValidateReportConfig("pivot", ReportConfig{}, mode.liveFields)
		if got := m8Code(t, err); got != "REPORT_TYPE_INVALID" {
			t.Fatalf("%s: unknown type must be REPORT_TYPE_INVALID, got %s", mode.name, got)
		}
	}

	// Deep filter check: an unknown filter field surfaces the run-time list code.
	_, _, err = ValidateReportConfig("summary", ReportConfig{
		GroupBy: "created_at", Metric: "count",
		Filters: []ReportFilterInput{{Field: "ghost", Op: "eq", Values: []string{"x"}}},
	}, live)
	if got := m8Code(t, err); got != "LIST_UNKNOWN_FILTER" {
		t.Fatalf("deep mode must validate filters with the run body, got %s", got)
	}

	// A deep-valid config passes deep mode, normalizing type/metric like the apply path.
	typ, blob, err := ValidateReportConfig(" Summary ", ReportConfig{
		GroupBy: "visit_date", Metric: "SUM", Field: "price", Bucket: "month",
	}, live)
	if err != nil {
		t.Fatalf("deep-valid config must pass, got %v", err)
	}
	if typ != report.TypeSummary {
		t.Fatalf("expected normalized type summary, got %q", typ)
	}
	if len(blob) == 0 {
		t.Fatal("expected the marshaled config JSON")
	}
}

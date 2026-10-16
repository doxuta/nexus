package services

// W4 unit tests — run-overlay parsing/translation and time-series bucket validation, over the
// in-memory fakes (fakeRecordCatalog/fakeRecordStore from record_list_test.go). The MySQL-truth
// counterparts live in w4_overlay_bucket_integration_test.go (-tags=integration).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// w4CodeOf extracts the ValidationError code, or "" for nil / non-validation errors.
func w4CodeOf(err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

// --- ParseRunOverlay ---------------------------------------------------------------------------

func TestParseRunOverlay(t *testing.T) {
	tests := []struct {
		name                      string
		dateFrom, dateTo, ownerID string
		wantNil                   bool
		wantCode                  string
	}{
		{name: "tất cả rỗng → nil overlay (backward compatible)", wantNil: true},
		{name: "chỉ dateFrom hợp lệ", dateFrom: "2026-01-01"},
		{name: "chỉ dateTo hợp lệ", dateTo: "2026-03-31"},
		{name: "chỉ ownerId", ownerID: "u-123"},
		{name: "range hợp lệ", dateFrom: "2026-01-01", dateTo: "2026-03-31"},
		{name: "from == to hợp lệ (một ngày)", dateFrom: "2026-01-15", dateTo: "2026-01-15"},
		{name: "dateFrom sai định dạng", dateFrom: "2026-1-1", wantCode: "RUN_DATE_INVALID"},
		{name: "dateFrom là datetime", dateFrom: "2026-01-01T00:00:00Z", wantCode: "RUN_DATE_INVALID"},
		{name: "dateTo sai định dạng", dateTo: "31/01/2026", wantCode: "RUN_DATE_INVALID"},
		{name: "ngày không tồn tại", dateFrom: "2026-02-30", wantCode: "RUN_DATE_INVALID"},
		{name: "from > to", dateFrom: "2026-02-01", dateTo: "2026-01-01", wantCode: "RUN_DATE_RANGE_INVALID"},
		{name: "rác thuần", dateFrom: "abc", wantCode: "RUN_DATE_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := ParseRunOverlay(tt.dateFrom, tt.dateTo, tt.ownerID)
			if tt.wantCode != "" {
				if got := w4CodeOf(err); got != tt.wantCode {
					t.Fatalf("code = %q (err=%v); want %q", got, err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantNil {
				if o != nil {
					t.Fatalf("overlay = %+v; want nil", o)
				}
				return
			}
			if o == nil {
				t.Fatal("overlay = nil; want non-nil")
			}
		})
	}
}

// ParseRunOverlay trims whitespace so " 2026-01-01 " and padded ownerId behave like clean input.
func TestParseRunOverlay_Trims(t *testing.T) {
	o, err := ParseRunOverlay(" 2026-01-01 ", "", "  u-9  ")
	if err != nil {
		t.Fatalf("ParseRunOverlay: %v", err)
	}
	if o.DateFrom != "2026-01-01" || o.OwnerID != "u-9" {
		t.Errorf("overlay = %+v; want trimmed values", o)
	}
}

// --- overlayFilters ----------------------------------------------------------------------------

func TestOverlayFilters_Translation(t *testing.T) {
	o := &RunOverlay{DateFrom: "2026-01-01", DateTo: "2026-01-31", OwnerID: "u-owner"}
	fs := overlayFilters(o)
	if len(fs) != 3 {
		t.Fatalf("filters len = %d; want 3", len(fs))
	}
	// dateFrom → created_at >= '2026-01-01 00:00:00' on the PHYSICAL column, no CAST.
	if fs[0].PhysicalColumn != "created_at" || fs[0].Operator != dynrepo.FilterGte ||
		fs[0].Values[0] != "2026-01-01 00:00:00" || fs[0].CastDatetime || fs[0].CastNumeric {
		t.Errorf("dateFrom filter = %+v; want created_at gte '2026-01-01 00:00:00' uncast", fs[0])
	}
	// dateTo → created_at < dateTo+1d (month boundary: 2026-01-31 → 2026-02-01).
	if fs[1].PhysicalColumn != "created_at" || fs[1].Operator != dynrepo.FilterLt ||
		fs[1].Values[0] != "2026-02-01 00:00:00" {
		t.Errorf("dateTo filter = %+v; want created_at lt '2026-02-01 00:00:00'", fs[1])
	}
	// ownerId → created_by = ? exact (never case-insensitive).
	if fs[2].PhysicalColumn != "created_by" || fs[2].Operator != dynrepo.FilterEq ||
		fs[2].Values[0] != "u-owner" || fs[2].CaseInsensitive {
		t.Errorf("owner filter = %+v; want created_by eq 'u-owner' exact", fs[2])
	}
}

func TestOverlayFilters_YearBoundaryAndNil(t *testing.T) {
	if got := overlayFilters(nil); got != nil {
		t.Errorf("overlayFilters(nil) = %v; want nil", got)
	}
	// Year boundary: dateTo 2025-12-31 → upper bound 2026-01-01.
	fs := overlayFilters(&RunOverlay{DateTo: "2025-12-31"})
	if len(fs) != 1 || fs[0].Values[0] != "2026-01-01 00:00:00" {
		t.Errorf("year-boundary dateTo = %+v; want single lt '2026-01-01 00:00:00'", fs)
	}
}

// --- Aggregate: bucket validation + overlay pass-through ----------------------------------------

// w4AggregateFixtureService builds a RecordService over fakes with: score (number, indexed),
// closed_on (date, indexed), note (text, plain — not groupable).
func w4AggregateFixtureService() (*RecordService, *fakeRecordStore) {
	indexed := `{"indexed":true}`
	store := &fakeRecordStore{}
	cat := &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-w4", TenantID: "tenant-1", Name: "deals", DataTable: "obj_deals"},
		fields: []metadata.FieldDefinition{
			{Name: "score", FieldType: "number", Options: &indexed},
			{Name: "closed_on", FieldType: "date", Options: &indexed},
			{Name: "note", FieldType: "text"},
		},
	}
	return NewRecordService(cat, store), store
}

func TestAggregate_BucketValidation(t *testing.T) {
	scope := RecordScope{UserID: "u1", AccessLevel: "all"}
	tests := []struct {
		name     string
		q        AggregateQuery
		wantCode string
		// wantBucket asserts the repo params when the call must succeed.
		wantBucket dynrepo.AggregateBucket
	}{
		{
			name:     "bucket token lạ → AGGREGATE_BUCKET_INVALID",
			q:        AggregateQuery{Metric: AggMetricCount, GroupBy: "created_at", Bucket: "decade"},
			wantCode: "AGGREGATE_BUCKET_INVALID",
		},
		{
			name:     "bucket không có groupBy → AGGREGATE_BUCKET_REQUIRES_GROUPBY",
			q:        AggregateQuery{Metric: AggMetricCount, Bucket: "month"},
			wantCode: "AGGREGATE_BUCKET_REQUIRES_GROUPBY",
		},
		{
			name:     "bucket trên field số (indexed) → AGGREGATE_BUCKET_FIELD_NOT_DATE",
			q:        AggregateQuery{Metric: AggMetricCount, GroupBy: "score", Bucket: "month"},
			wantCode: "AGGREGATE_BUCKET_FIELD_NOT_DATE",
		},
		{
			name:     "bucket trên cột id → AGGREGATE_BUCKET_FIELD_NOT_DATE",
			q:        AggregateQuery{Metric: AggMetricCount, GroupBy: "id", Bucket: "day"},
			wantCode: "AGGREGATE_BUCKET_FIELD_NOT_DATE",
		},
		{
			name:     "bucket trên date field KHÔNG indexed → AGGREGATE_REQUIRES_INDEXED thắng trước",
			q:        AggregateQuery{Metric: AggMetricCount, GroupBy: "note", Bucket: "month"},
			wantCode: "AGGREGATE_REQUIRES_INDEXED",
		},
		{
			name:       "bucket month trên created_at hợp lệ",
			q:          AggregateQuery{Metric: AggMetricCount, GroupBy: "created_at", Bucket: "month"},
			wantBucket: dynrepo.BucketMonth,
		},
		{
			name:       "bucket QUARTER viết hoa được normalize",
			q:          AggregateQuery{Metric: AggMetricCount, GroupBy: "updated_at", Bucket: "QUARTER"},
			wantBucket: dynrepo.BucketQuarter,
		},
		{
			name:       "bucket week trên date field indexed hợp lệ",
			q:          AggregateQuery{Metric: AggMetricCount, GroupBy: "closed_on", Bucket: "week"},
			wantBucket: dynrepo.BucketWeek,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, store := w4AggregateFixtureService()
			_, err := svc.Aggregate(context.Background(), scope, "tenant-1", "deals", tt.q)
			if tt.wantCode != "" {
				if got := w4CodeOf(err); got != tt.wantCode {
					t.Fatalf("code = %q (err=%v); want %q", got, err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("Aggregate: %v", err)
			}
			if store.gotAggParams.GroupByBucket != tt.wantBucket {
				t.Errorf("GroupByBucket = %q; want %q", store.gotAggParams.GroupByBucket, tt.wantBucket)
			}
		})
	}
}

// The overlay's system-column predicates are appended AFTER the declared-field filters, on the
// physical columns, for both Aggregate and List — so every widget/report path shares one shape.
func TestAggregateAndList_OverlayAppended(t *testing.T) {
	scope := RecordScope{UserID: "u1", AccessLevel: "all"}
	overlay := &RunOverlay{DateFrom: "2026-01-01", DateTo: "2026-01-31", OwnerID: "u-owner"}

	svc, store := w4AggregateFixtureService()
	_, err := svc.Aggregate(context.Background(), scope, "tenant-1", "deals", AggregateQuery{
		Metric:  AggMetricCount,
		Filters: []FilterInput{{Field: "note", Values: []string{"x"}}},
		Overlay: overlay,
	})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(store.gotAggParams.Filters) != 4 {
		t.Fatalf("aggregate filters len = %d; want 4 (1 declared + 3 overlay)", len(store.gotAggParams.Filters))
	}
	if store.gotAggParams.Filters[0].Column != "note" {
		t.Errorf("declared filter must come first, got %+v", store.gotAggParams.Filters[0])
	}
	if store.gotAggParams.Filters[3].PhysicalColumn != "created_by" {
		t.Errorf("overlay owner filter must be appended last, got %+v", store.gotAggParams.Filters[3])
	}

	svc2, store2 := w4AggregateFixtureService()
	_, err = svc2.List(context.Background(), scope, "tenant-1", "deals", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "note", Values: []string{"x"}}},
		Overlay: overlay,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(store2.gotParams.Filters) != 4 {
		t.Fatalf("list filters len = %d; want 4 (1 declared + 3 overlay)", len(store2.gotParams.Filters))
	}
	if store2.gotParams.Filters[1].PhysicalColumn != "created_at" || store2.gotParams.Filters[1].Operator != dynrepo.FilterGte {
		t.Errorf("list overlay dateFrom = %+v; want created_at gte", store2.gotParams.Filters[1])
	}

	// nil overlay → identical to pre-W4 (no extra filters).
	svc3, store3 := w4AggregateFixtureService()
	if _, err := svc3.List(context.Background(), scope, "tenant-1", "deals", ListQuery{Page: 1, Limit: 20}); err != nil {
		t.Fatalf("List nil overlay: %v", err)
	}
	if len(store3.gotParams.Filters) != 0 {
		t.Errorf("nil overlay must add no filters, got %d", len(store3.gotParams.Filters))
	}
}

// --- Dashboard widget config-time bucket validation (B6 parity) --------------------------------

func TestValidateWidgets_Bucket(t *testing.T) {
	base := WidgetConfig{ID: "w1", Title: "t", Type: "bar", Entity: "deals", Metric: "count"}
	tests := []struct {
		name     string
		mutate   func(w *WidgetConfig)
		wantCode string
	}{
		{
			name:   "bucket hợp lệ + groupBy",
			mutate: func(w *WidgetConfig) { w.GroupBy = "created_at"; w.Bucket = "month" },
		},
		{
			name:   "bucket viết hoa vẫn hợp lệ (normalize khi chạy)",
			mutate: func(w *WidgetConfig) { w.GroupBy = "created_at"; w.Bucket = "Week" },
		},
		{
			name:     "bucket token lạ → WIDGET_BUCKET_INVALID",
			mutate:   func(w *WidgetConfig) { w.GroupBy = "created_at"; w.Bucket = "fortnight" },
			wantCode: "WIDGET_BUCKET_INVALID",
		},
		{
			name:     "bucket không groupBy → WIDGET_BUCKET_REQUIRES_GROUPBY",
			mutate:   func(w *WidgetConfig) { w.Bucket = "day" },
			wantCode: "WIDGET_BUCKET_REQUIRES_GROUPBY",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := base
			tt.mutate(&w)
			_, err := validateAndMarshalWidgets([]WidgetConfig{w})
			if got := w4CodeOf(err); got != tt.wantCode {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tt.wantCode)
			}
		})
	}
}

// --- Report config-time bucket validation (B6 parity) ------------------------------------------

func TestValidateReportConfig_Bucket(t *testing.T) {
	tests := []struct {
		name     string
		typ      string
		cfg      ReportConfig
		wantCode string
		wantErr  string // substring of the error message when code alone is ambiguous
	}{
		{
			name: "summary + bucket hợp lệ (normalize lowercase)",
			typ:  "summary",
			cfg:  ReportConfig{GroupBy: "created_at", Metric: "count", Bucket: "Month"},
		},
		{
			name:     "summary + bucket token lạ → REPORT_CONFIG_INVALID",
			typ:      "summary",
			cfg:      ReportConfig{GroupBy: "created_at", Metric: "count", Bucket: "decade"},
			wantCode: "REPORT_CONFIG_INVALID",
			wantErr:  "bucket",
		},
		{
			name:     "tabular + bucket → REPORT_CONFIG_INVALID (bucket chỉ cho summary/matrix)",
			typ:      "tabular",
			cfg:      ReportConfig{Columns: []string{"a"}, Bucket: "month"},
			wantCode: "REPORT_CONFIG_INVALID",
			wantErr:  "bucket",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typ, terr := validateReportType(tt.typ)
			if terr != nil {
				t.Fatalf("validateReportType: %v", terr)
			}
			raw, err := validateReportConfig(typ, tt.cfg)
			if tt.wantCode != "" {
				if got := w4CodeOf(err); got != tt.wantCode {
					t.Fatalf("code = %q (err=%v); want %q", got, err, tt.wantCode)
				}
				if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q must mention %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateReportConfig: %v", err)
			}
			if !strings.Contains(string(raw), `"bucket":"month"`) {
				t.Errorf("stored config %s must carry normalized bucket", raw)
			}
		})
	}
}

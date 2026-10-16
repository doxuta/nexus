package services

import (
	"testing"

	"nexus/platform/internal/dynrepo"
)

// #359/#361: what Xuất CSV writes, Nhập CSV reads back to the same stored value.
func TestExportRowRoundTripsThroughImport(t *testing.T) {
	pick := FieldSpec{Name: "stage", FieldType: "picklist", Options: map[string]any{
		"values":      []any{"new", "won", "dup1", "dup2"},
		"valueLabels": map[string]any{"new": "Mới", "won": "Thắng", "dup1": "Trùng", "dup2": "Trùng"},
	}}
	// "A" is labelled "B", which is also a code: the label would be read back as code B.
	clash := FieldSpec{Name: "c", FieldType: "picklist", Options: map[string]any{
		"values": []any{"A", "B"}, "valueLabels": map[string]any{"A": "B"},
	}}
	num := FieldSpec{Name: "n", FieldType: "number"}
	specs := map[string]FieldSpec{"stage": pick, "c": clash, "n": num}
	tests := []struct {
		name      string
		spec      FieldSpec
		value     any
		want      string // exact cell
		roundTrip bool   // the cell must import back to value
	}{
		{"label instead of code", pick, "new", "Mới", true},
		{"shared label keeps the code", pick, "dup1", "dup1", true},
		{"label that is another code keeps the code", clash, "A", "A", true},
		{"unknown stored value verbatim", pick, "legacy", "legacy", false},
		{"three decimals not read as thousands", num, 12.345, "12.3450", true},
		{"negative three decimals", num, -123.456, "-123.4560", true},
		{"whole number untouched", num, float64(1500), "1500", true},
		{"four digit head is a decimal already", num, 1234.567, "1234.567", true},
		{"small decimal untouched", num, 0.125, "0.125", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &dynrepo.Record{ID: "1", Data: map[string]any{tc.spec.Name: tc.value}}
			row := buildCSVRow(rec, []string{tc.spec.Name}, false, specs)
			if row[0] != tc.want {
				t.Fatalf("cell = %q; want %q", row[0], tc.want)
			}
			if !tc.roundTrip {
				return
			}
			got, err := coerceCell(tc.spec, row[0])
			if err != nil || got != tc.value {
				t.Fatalf("import of %q = %v, %v; want %v", row[0], got, err, tc.value)
			}
		})
	}
}

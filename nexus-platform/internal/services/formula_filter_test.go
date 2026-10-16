package services

import (
	"testing"

	"nexus/platform/internal/dynrepo"
)

// queue #285: a formula's value is stored in data at write time, so it filters as its resultType.
func TestValidateFilterOperator_Formula(t *testing.T) {
	num := FieldSpec{Name: "con_no", FieldType: "formula", Options: map[string]any{"resultType": "number"}}
	dflt := FieldSpec{Name: "con_no", FieldType: "formula", Options: map[string]any{}}
	money := FieldSpec{Name: "con_no", FieldType: "formula", Options: map[string]any{"resultType": "currency"}}

	for name, f := range map[string]FieldSpec{"number": num, "default": dflt} {
		flags, sub, err := validateFilterOperator(f, dynrepo.FilterGt, []string{"0"})
		if err != nil || !flags.CastNumeric || sub != "" {
			t.Fatalf("%s formula gt: flags=%+v sub=%q err=%v", name, flags, sub, err)
		}
	}
	flags, sub, err := validateFilterOperator(money, dynrepo.FilterGt, []string{"0"})
	if err != nil || !flags.CastNumeric || sub != "amount" {
		t.Fatalf("currency formula gt: flags=%+v sub=%q err=%v", flags, sub, err)
	}
}

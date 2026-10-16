package dynrepo

import (
	"strings"
	"testing"
)

// Queue #293: a second FK hop chains off the FIRST hop's alias (FromAlias), each hop keeping its
// own tenant + row-level predicate; the join args stay in statement order.
func TestAggregateFromWhere_TwoHop_ChainsOffPreviousAlias(t *testing.T) {
	p := AggregateParams{
		Metric: MetricCount,
		Joins: []JoinSpec{
			{Alias: "j0", RelatedTable: "obj_appointments", Scope: RowScope{Level: AccessOwner, UserID: "u-mid"}, LeftCol: "appointment_id", RightCol: "id"},
			{Alias: "j1", FromAlias: "j0", RelatedTable: "obj_doctors", Scope: RowScope{Level: AccessAll}, LeftCol: "doctor_id", RightCol: "id"},
		},
		GroupByAlias: "j1",
	}
	from, _, args, err := aggregateFromWhere("obj_invoices", "tenant-A", RowScope{Level: AccessAll}, p)
	if err != nil {
		t.Fatalf("aggregateFromWhere: %v", err)
	}
	for _, want := range []string{
		"INNER JOIN `obj_appointments` `j0` ON `j0`.`id` = `base`.`appointment_id` AND `j0`.`tenant_id` = ?",
		"INNER JOIN `obj_doctors` `j1` ON `j1`.`id` = `j0`.`doctor_id` AND `j1`.`tenant_id` = ?",
	} {
		if !strings.Contains(from, want) {
			t.Fatalf("from missing %q\n got: %s", want, from)
		}
	}
	if strings.Index(from, "`j0` ON") > strings.Index(from, "`j1` ON") {
		t.Fatalf("hop order broken: %s", from)
	}
	// join0 (tenant, mid-hop owner) -> join1 (tenant) -> base tenant.
	want := []any{"tenant-A", "u-mid", "tenant-A", "tenant-A"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %v, want %v (full %v)", i, args[i], want[i], args)
		}
	}
}

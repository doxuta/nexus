package tenantbackup

import (
	"context"
	"fmt"
	"sort"
)

// TableRef is one table a backup reads, with its policy rule.
type TableRef struct {
	Table string
	Rule  Rule
}

// fromWhere is the tenant-pinned FROM/WHERE of a table, aliased `t`. A table with its own
// tenant_id is read directly; a child without tenant_id (acl_role_actions, team_memberships,
// team_sets_teams) is read through its parent, whose tenant_id is the pin. The ONE argument is
// the tenant id. Names are validated identifiers (they come from information_schema + the policy,
// never from a request).
func (r TableRef) fromWhere() (string, error) {
	if err := checkIdent(r.Table); err != nil {
		return "", err
	}
	if r.Rule.Owner == nil {
		return "FROM `" + r.Table + "` t WHERE t.tenant_id = ?", nil
	}
	if err := checkIdent(r.Rule.Owner.Parent); err != nil {
		return "", err
	}
	if err := checkIdent(r.Rule.Owner.Column); err != nil {
		return "", err
	}
	return "FROM `" + r.Table + "` t JOIN `" + r.Rule.Owner.Parent + "` p ON p.id = t.`" + r.Rule.Owner.Column +
		"` WHERE p.tenant_id = ?", nil
}

// deleteQuery is the tenant-pinned DELETE of a restore: the same pin as fromWhere (a child without
// tenant_id goes through its parent's tenant_id). Never TRUNCATE/DROP — obj_* tables are shared by
// every customer.
func (r TableRef) deleteQuery() (string, error) {
	if err := checkIdent(r.Table); err != nil {
		return "", err
	}
	if r.Rule.Owner == nil {
		return "DELETE FROM `" + r.Table + "` WHERE tenant_id = ?", nil
	}
	if err := checkIdent(r.Rule.Owner.Parent); err != nil {
		return "", err
	}
	if err := checkIdent(r.Rule.Owner.Column); err != nil {
		return "", err
	}
	return "DELETE t FROM `" + r.Table + "` t JOIN `" + r.Rule.Owner.Parent + "` p ON p.id = t.`" + r.Rule.Owner.Column +
		"` WHERE p.tenant_id = ?", nil
}

func (r TableRef) countQuery() (string, error) {
	fw, err := r.fromWhere()
	if err != nil {
		return "", err
	}
	return "SELECT COUNT(*) " + fw, nil
}

// exportTables lists the tables that go into a backup: every tenant_id table of the live schema
// whose class is in-file, plus the in-file children that hang off a parent. Sorted by name so the
// file layout is deterministic.
func exportTables(ctx context.Context, q Querier) ([]TableRef, error) {
	tenantTables, _, err := LoadSchema(ctx, q)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(tenantTables))
	var out []TableRef
	for _, t := range tenantTables {
		have[t] = true
		r, ok := RuleFor(t)
		if !ok {
			return nil, &Error{Code: "TENANT_BACKUP_SCHEMA_UNCLASSIFIED",
				Msg: "bảng chưa được phân lớp sao lưu: " + t, Details: map[string]any{"tables": []string{t}}}
		}
		if r.Class.InFile() {
			out = append(out, TableRef{Table: t, Rule: r})
		}
	}
	for t, r := range rules {
		if r.Owner != nil && r.Class.InFile() && have[r.Owner.Parent] {
			out = append(out, TableRef{Table: t, Rule: r})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	return out, nil
}

// TablePlan is a table plus the columns that go into the file (native, minus DropColumns).
type TablePlan struct {
	TableRef
	Cols    []Column
	Dropped []ExcludedColumn
}

// planTable resolves a table's columns through q.
func planTable(ctx context.Context, q Querier, ref TableRef) (TablePlan, error) {
	cols, err := NativeColumns(ctx, q, ref.Table)
	if err != nil {
		return TablePlan{}, err
	}
	tp := TablePlan{TableRef: ref}
	for _, c := range cols {
		if reason, drop := ref.Rule.DropColumns[c.Name]; drop {
			tp.Dropped = append(tp.Dropped, ExcludedColumn{Table: ref.Table, Column: c.Name, Reason: reason})
			continue
		}
		tp.Cols = append(tp.Cols, c)
	}
	if len(tp.Cols) == 0 {
		return TablePlan{}, fmt.Errorf("tenantbackup.planTable %s: no exportable columns", ref.Table)
	}
	return tp, nil
}

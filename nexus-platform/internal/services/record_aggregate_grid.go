package services

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"nexus/platform/internal/dynrepo"
)

// Report grid service (specs/049-report-grouping-pivot, slice S2a report-grid-service): the
// multi-level tree with sub-totals and the two-axis matrix, over dynrepo.AggregateGrid (one
// GROUP BY … WITH ROLLUP; every sub-total and total is computed by MySQL over the base rows).
// Every guard is the SAME aggregatePreflight the single-axis Aggregate runs — one ACL body for
// both paths. This file only adds the axis shape rules, the Q3 cap mapping and the build/sort of
// the result. Wired to /reports/:id/run by S2b (matrix) and S3 (tree); nothing calls it before.

// Result kinds of a grid run.
const (
	GridKindTree   = "tree"
	GridKindMatrix = "matrix"
)

// maxGridLevels: groupBy + at most 2 sub-groups (spec FR-001). A matrix is exactly a row axis and
// a column axis (FR-002).
const maxGridLevels = 3

// GridQuery is a grid request: the metric/field/filters/overlay/join of an AggregateQuery plus
// the axes, outermost first. Matrix makes it a cross-tab: exactly 2 axes, the second being the
// column axis (column totals included); otherwise the axes are the levels of a tree. Having (nil =
// none) keeps only the LEAF groups passing it — matrix cells, the deepest tree level — and every
// total then adds the kept leaves only (specs/049 S5).
type GridQuery struct {
	Metric  AggregateMetric
	Field   string
	Axes    []GridAxisInput
	Matrix  bool
	Filters []FilterInput
	Overlay *RunOverlay
	Joins   []AggregateJoin
	Having  *ReportHavingInput
}

// GridRunResult is a computed grid. A tree fills Groups (level-1 nodes, children nested); a
// matrix fills Rows (row totals), Columns (column totals) and the sparse Cells (only cells that
// hold data; R/C index Rows/Columns). Total is the grand total — {nil, 0} when nothing matched.
// Every list is sorted (FR-013) and is [] rather than nil for its kind.
type GridRunResult struct {
	Kind         string
	Groups       []GridNode
	Rows         []AggregateGroupResult
	Columns      []AggregateGroupResult
	Cells        []GridCell
	Total        AggregateScalarResult
	CurrencyCode string
}

// GridNode is one tree group: its key/value/count (a sub-total over the node's base rows — AVG,
// MIN, MAX mean what they say) and its child groups ([] on the deepest level).
type GridNode struct {
	AggregateGroupResult
	Children []GridNode
}

// GridCell is one matrix cell holding data, addressed by row / column index.
type GridCell struct {
	R, C  int
	Value *string
	Count int64
}

// AggregateGrid computes a tree or a matrix over the entity's rows the caller may read (tenant +
// A5 row scope, per-hop scope on a joined axis). Order, every rejection before any data query:
// axis shape (REPORT_CONFIG_INVALID) → threshold shape (REPORT_HAVING_INVALID; the threshold is then
// judged by MySQL on the SAME scoped rows) → aggregatePreflight (entity, join + per-hop ACL, N-N gate,
// metric, each axis — errors carry details.axis —, filters, field-ACL on metric/EVERY axis/
// filters) → duplicate axis → store. The store's Q3 cap becomes 422 REPORT_MATRIX_TOO_MANY_CELLS
// (matrix) / REPORT_TOO_MANY_GROUPS (tree); a matrix whose dense rows × columns exceed the cap is
// rejected the same way. Never truncated. As for Aggregate, the caller has already resolved the
// scope from the entity-level "view" ACL (RequireACL / ReportService via AuthorizeView).
func (s *RecordService) AggregateGrid(ctx context.Context, scope RecordScope, tenantID, entityName string, q GridQuery) (GridRunResult, error) {
	names, err := gridAxisNames(q)
	if err != nil {
		return GridRunResult{}, err
	}
	// Judged here too, not only when a report is saved: this is a service entry point of its own,
	// and a bound MySQL cannot read as a decimal would be CAST to 0 — a silently wrong filter.
	having, err := normalizeHaving(q.Having)
	if err != nil {
		return GridRunResult{}, err
	}
	pf, err := s.aggregatePreflight(ctx, scope, tenantID, entityName, AggregateQuery{
		Metric: q.Metric, Field: q.Field, Filters: q.Filters, Overlay: q.Overlay, Joins: q.Joins,
	}, q.Axes, names)
	if err != nil {
		return GridRunResult{}, err
	}

	p := dynrepo.GridParams{AggregateParams: pf.params, CrossTotals: q.Matrix, Axes: make([]dynrepo.GridAxis, len(pf.axes))}
	bucketed := make([]bool, len(pf.axes))
	seen := make(map[dynrepo.GridAxis]bool, len(pf.axes))
	for i, ax := range pf.axes {
		ga := dynrepo.GridAxis{Physical: ax.physical, Alias: ax.alias, Bucket: ax.bucket}
		// Compared RESOLVED, so two tokens naming one column (e.g. the owner aliases) are caught
		// too. A repeated level would make GROUPING() ambiguous.
		if seen[ga] {
			return GridRunResult{}, errDuplicateAxis(names, i)
		}
		seen[ga] = true
		p.Axes[i], bucketed[i] = ga, ax.bucket != ""
	}
	if having != nil {
		p.Having = &dynrepo.GridHaving{OnCount: having.On == "count", Op: having.Op, Values: having.Values}
	}

	rows, colTotals, err := s.store.AggregateGrid(ctx, pf.entity, tenantID, scope.toRowScope(), p)
	if err != nil {
		var tl *dynrepo.ErrGridTooLarge
		if errors.As(err, &tl) {
			return GridRunResult{}, gridTooLarge(q.Matrix, tl)
		}
		return GridRunResult{}, fmt.Errorf("services.RecordService.AggregateGrid: %w", err)
	}
	res, err := buildGridResult(q.Matrix, bucketed, rows, colTotals)
	if err != nil {
		return GridRunResult{}, err
	}
	res.CurrencyCode = pf.currencyCode
	return res, nil
}

// gridAxisNames checks the axis count / emptiness of a grid request and names each axis in the
// report-config vocabulary: "groupBy", then "columnGroup" (matrix) or "subGroups[i]" (tree).
func gridAxisNames(q GridQuery) ([]string, error) {
	n := len(q.Axes)
	if q.Matrix && n != 2 {
		return nil, invalid("REPORT_CONFIG_INVALID", "bảng chéo cần đúng một trục hàng và một trục cột (nhận %d trục)", n)
	}
	if n == 0 || n > maxGridLevels {
		return nil, invalid("REPORT_CONFIG_INVALID", "báo cáo nhóm cần từ 1 đến %d cấp nhóm (nhận %d)", maxGridLevels, n)
	}
	names := make([]string, n)
	for i := range names {
		switch {
		case i == 0:
			names[i] = "groupBy"
		case q.Matrix:
			names[i] = "columnGroup"
		default:
			names[i] = fmt.Sprintf("subGroups[%d]", i-1)
		}
		if strings.TrimSpace(q.Axes[i].Field) == "" {
			return nil, tagAxis(invalid("REPORT_CONFIG_INVALID", "cấp nhóm thứ %d chưa chọn trường", i+1), names, i)
		}
	}
	return names, nil
}

// havingOps maps each threshold operator token to its value count; dynrepo owns the SQL for them.
var havingOps = map[string]int{"gt": 1, "gte": 1, "lt": 1, "lte": 1, "eq": 1, "neq": 1, "between": 2}

// havingValueRe is a threshold value: a plain decimal that fits DECIMAL(38,10), the type it is bound
// as — at most 28 integer and 10 fraction digits, "." as the separator, no sign but "-".
var havingValueRe = regexp.MustCompile(`^-?\d{1,28}(\.\d{1,10})?$`)

// normalizeHaving checks a report threshold's shape (specs/049 S5 — the ONE body for the report
// validator and RecordService.AggregateGrid) and returns it normalized: subject and operator
// lower-cased, values trimmed; nil stays nil. `between` bounds are ordered by exact decimal
// comparison (a float would call 0.0000000002 > 0.0000000001 at 19 integer digits equal). Every
// violation is 422 REPORT_HAVING_INVALID {field:"having", reason} — reason ∈ on|op|values|value|
// range — with a sentence that echoes nothing the caller sent.
func normalizeHaving(h *ReportHavingInput) (*ReportHavingInput, error) {
	if h == nil {
		return nil, nil
	}
	out := &ReportHavingInput{
		On: strings.ToLower(strings.TrimSpace(h.On)), Op: strings.ToLower(strings.TrimSpace(h.Op)),
		Values: make([]string, len(h.Values)),
	}
	if out.On != "value" && out.On != "count" {
		return nil, havingInvalid("on", "Ngưỡng chỉ xét được trên Giá trị hoặc Số lượng.")
	}
	want, ok := havingOps[out.Op]
	if !ok {
		return nil, havingInvalid("op", "Toán tử của ngưỡng không hợp lệ — chọn lớn hơn, lớn hơn hoặc bằng, nhỏ hơn, nhỏ hơn hoặc bằng, bằng, khác hoặc trong khoảng.")
	}
	if len(h.Values) != want {
		if want == 2 {
			return nil, havingInvalid("values", "Ngưỡng \"trong khoảng\" cần đúng hai giá trị: cận dưới và cận trên.")
		}
		return nil, havingInvalid("values", "Ngưỡng cần đúng một giá trị.")
	}
	for i, v := range h.Values {
		out.Values[i] = strings.TrimSpace(v)
		if !havingValueRe.MatchString(out.Values[i]) {
			return nil, havingInvalid("value", "Giá trị ngưỡng phải là một số thập phân (dấu chấm ngăn phần lẻ; tối đa 28 chữ số phần nguyên và 10 chữ số phần lẻ).")
		}
	}
	if out.Op == "between" {
		lo, _ := new(big.Rat).SetString(out.Values[0]) // the regexp guarantees both parse
		hi, _ := new(big.Rat).SetString(out.Values[1])
		if lo.Cmp(hi) > 0 {
			return nil, havingInvalid("range", "Cận dưới của ngưỡng đang lớn hơn cận trên.")
		}
	}
	return out, nil
}

func havingInvalid(reason, msg string) error {
	return &ValidationError{Code: "REPORT_HAVING_INVALID", Msg: msg, Field: "having",
		Details: map[string]any{"field": "having", "reason": reason}}
}

// gridTooLarge maps the engine's Q3 cap to its 422. The engine counts LEAF groups; a matrix's
// dense grid has at least as many cells as leaves, so for a matrix the count is always "at least".
func gridTooLarge(matrix bool, tl *dynrepo.ErrGridTooLarge) error {
	if matrix {
		return &ValidationError{
			Code:    "REPORT_MATRIX_TOO_MANY_CELLS",
			Msg:     fmt.Sprintf("Bảng chéo có ít nhất %d ô, vượt giới hạn %d ô — hãy thu hẹp bộ lọc hoặc đổi trục.", tl.Leaves, dynrepo.MaxGridCells),
			Details: map[string]any{"cap": dynrepo.MaxGridCells, "cells": tl.Leaves, "lowerBound": true},
		}
	}
	msg := fmt.Sprintf("Báo cáo có %d nhóm, vượt giới hạn %d nhóm — hãy thu hẹp bộ lọc hoặc bớt cấp nhóm.", tl.Leaves, dynrepo.MaxGridCells)
	if tl.LowerBound {
		msg = fmt.Sprintf("Báo cáo có ít nhất %d nhóm, vượt giới hạn %d nhóm — hãy thu hẹp bộ lọc hoặc bớt cấp nhóm.", tl.Leaves, dynrepo.MaxGridCells)
	}
	return &ValidationError{
		Code:    "REPORT_TOO_MANY_GROUPS",
		Msg:     msg,
		Details: map[string]any{"cap": dynrepo.MaxGridCells, "groups": tl.Leaves, "lowerBound": tl.LowerBound},
	}
}

// buildGridResult turns the ROLLUP rows (+ matrix column totals) into the sorted result. ROLLUP
// only rolls up a SUFFIX of the levels, so a row's depth is its number of leading un-rolled levels:
// depth 0 is the grand total, depth len(bucketed) a leaf, anything between a sub-total. An empty
// set has no row at all (MySQL emits no grand total over nothing): the total stays {nil, 0}.
// Rows are matched on their group identities (GridRow.IDs), never on the key text MySQL shows; a
// group displays the key of its OWN row (a sub-total's own spelling, not a child's).
func buildGridResult(matrix bool, bucketed []bool, rows, colTotals []dynrepo.GridRow) (GridRunResult, error) {
	if matrix {
		return buildGridMatrix(bucketed, rows, colTotals)
	}
	res := GridRunResult{Kind: GridKindTree}
	type treeNode struct {
		g    AggregateGroupResult
		kids []*treeNode
		own  bool // the node's own ROLLUP row was seen
	}
	root := &treeNode{}
	byPath := map[string]*treeNode{}
	// Get-or-create by identity path, parents first — ROLLUP lists a sub-total AFTER its children.
	var node func(r dynrepo.GridRow, d int) *treeNode
	node = func(r dynrepo.GridRow, d int) *treeNode {
		if d == 0 {
			return root
		}
		id := gridPathID(r.IDs[:d])
		if nd, ok := byPath[id]; ok {
			return nd
		}
		nd := &treeNode{g: AggregateGroupResult{Key: r.Keys[d-1]}}
		byPath[id] = nd
		parent := node(r, d-1)
		parent.kids = append(parent.kids, nd)
		return nd
	}
	for _, r := range rows {
		d := gridDepth(r)
		if d == 0 {
			res.Total = AggregateScalarResult{Value: r.Value, Count: r.Count}
			continue
		}
		nd := node(r, d)
		if nd.own {
			return GridRunResult{}, fmt.Errorf("services.buildGridResult: two ROLLUP rows for one group")
		}
		nd.own, nd.g = true, AggregateGroupResult{Key: r.Keys[d-1], Value: r.Value, Count: r.Count}
	}
	// ROLLUP emits a row for every group of every level, so a node known only through a child
	// means the identities disagree — a phantom sibling with a wrong sub-total. Fail loudly.
	for _, nd := range byPath {
		if !nd.own {
			return GridRunResult{}, fmt.Errorf("services.buildGridResult: group without its own ROLLUP row")
		}
	}
	var toNodes func(kids []*treeNode, level int) []GridNode
	toNodes = func(kids []*treeNode, level int) []GridNode {
		out := make([]GridNode, 0, len(kids))
		for _, k := range kids {
			out = append(out, GridNode{AggregateGroupResult: k.g, Children: toNodes(k.kids, level+1)})
		}
		sort.SliceStable(out, func(i, j int) bool {
			return gridGroupLess(out[i].AggregateGroupResult, out[j].AggregateGroupResult, level < len(bucketed) && bucketed[level])
		})
		return out
	}
	res.Groups = toNodes(root.kids, 0)
	return res, nil
}

// buildGridMatrix: leaves (depth 2) are cells, depth-1 rows are the row totals, the separate
// column-total rows head the columns. Rejects a dense grid over the cap with its exact shape.
func buildGridMatrix(bucketed []bool, rows, colTotals []dynrepo.GridRow) (GridRunResult, error) {
	res := GridRunResult{Kind: GridKindMatrix, Rows: []AggregateGroupResult{}, Columns: make([]AggregateGroupResult, 0, len(colTotals))}
	// A header remembers its group identity through the sort: cells find it by identity only.
	type header struct {
		id string
		g  AggregateGroupResult
	}
	var rowHdrs, colHdrs []header
	var leaves []dynrepo.GridRow
	for _, r := range rows {
		switch gridDepth(r) {
		case 0:
			res.Total = AggregateScalarResult{Value: r.Value, Count: r.Count}
		case 1:
			rowHdrs = append(rowHdrs, header{gridKeyID(r.IDs[0]), AggregateGroupResult{Key: r.Keys[0], Value: r.Value, Count: r.Count}})
		default:
			leaves = append(leaves, r)
		}
	}
	for _, c := range colTotals {
		colHdrs = append(colHdrs, header{gridKeyID(c.IDs[0]), AggregateGroupResult{Key: c.Keys[0], Value: c.Value, Count: c.Count}})
	}
	// "Cell" = a cell of the DENSE grid the screen draws (Q3, plan D-5), not a cell holding data.
	if cells := len(rowHdrs) * len(colHdrs); cells > dynrepo.MaxGridCells {
		return GridRunResult{}, &ValidationError{
			Code: "REPORT_MATRIX_TOO_MANY_CELLS",
			Msg: fmt.Sprintf("Bảng chéo có %d×%d = %d ô, vượt giới hạn %d ô — hãy thu hẹp bộ lọc hoặc đổi trục.",
				len(rowHdrs), len(colHdrs), cells, dynrepo.MaxGridCells),
			Details: map[string]any{"cap": dynrepo.MaxGridCells, "cells": cells, "rows": len(rowHdrs),
				"columns": len(colHdrs), "lowerBound": false},
		}
	}
	// Sorts the headers, fills the result list and indexes the positions by identity; two headers
	// with one identity would put two groups' cells under one header — fail loudly instead.
	place := func(hs []header, isBucket bool, out *[]AggregateGroupResult) (map[string]int, error) {
		sort.SliceStable(hs, func(i, j int) bool { return gridGroupLess(hs[i].g, hs[j].g, isBucket) })
		at := make(map[string]int, len(hs))
		for i, h := range hs {
			if _, dup := at[h.id]; dup {
				return nil, fmt.Errorf("services.buildGridMatrix: two totals for one group")
			}
			at[h.id] = i
			*out = append(*out, h.g)
		}
		return at, nil
	}
	rowAt, err := place(rowHdrs, bucketed[0], &res.Rows)
	if err != nil {
		return GridRunResult{}, err
	}
	colAt, err := place(colHdrs, bucketed[1], &res.Columns)
	if err != nil {
		return GridRunResult{}, err
	}
	res.Cells = make([]GridCell, 0, len(leaves))
	for _, l := range leaves {
		ri, okR := rowAt[gridKeyID(l.IDs[0])]
		ci, okC := colAt[gridKeyID(l.IDs[1])]
		if !okR || !okC {
			// Cells, row totals and column totals come from one snapshot (FR-011), so a cell
			// always has both headers; a miss is an engine bug — fail loudly, never drop the cell.
			return GridRunResult{}, fmt.Errorf("services.buildGridMatrix: cell without a row/column total")
		}
		res.Cells = append(res.Cells, GridCell{R: ri, C: ci, Value: l.Value, Count: l.Count})
	}
	sort.Slice(res.Cells, func(i, j int) bool {
		if res.Cells[i].R != res.Cells[j].R {
			return res.Cells[i].R < res.Cells[j].R
		}
		return res.Cells[i].C < res.Cells[j].C
	})
	return res, nil
}

// gridDepth is the number of leading un-rolled levels of a ROLLUP row (0 = grand total).
func gridDepth(r dynrepo.GridRow) int {
	d := 0
	for d < len(r.Rolled) && !r.Rolled[d] {
		d++
	}
	return d
}

// gridKeyID encodes a group identity (nil = the NULL data key) unambiguously; gridPathID a path
// of identities.
func gridKeyID(k *string) string {
	if k == nil {
		return "N"
	}
	return strconv.Quote(*k)
}

func gridPathID(keys []*string) string {
	ids := make([]string, len(keys))
	for i, k := range keys {
		ids[i] = gridKeyID(k)
	}
	return strings.Join(ids, ",")
}

// gridGroupLess is the deterministic sibling order (FR-013): the NULL key always last; a
// time-bucketed axis by key ascending (bucket keys are zero-padded, so text order is calendar
// order); any other axis by value descending — exact decimals, a NULL value after every number —
// with ties by key ascending.
// ponytail: parses the decimals on every comparison; the Q3 cap bounds a level at 1000 groups.
func gridGroupLess(a, b AggregateGroupResult, bucketed bool) bool {
	if (a.Key == nil) != (b.Key == nil) {
		return b.Key == nil
	}
	if !bucketed {
		va, vb := gridRat(a.Value), gridRat(b.Value)
		switch {
		case va != nil && vb != nil:
			if c := va.Cmp(vb); c != 0 {
				return c > 0
			}
		case va != nil || vb != nil:
			return va != nil
		}
	}
	if a.Key == nil {
		return false
	}
	return *a.Key < *b.Key
}

// gridRat parses a DECIMAL result; nil for NULL (MySQL DECIMAL text always parses).
func gridRat(v *string) *big.Rat {
	if v == nil {
		return nil
	}
	r, ok := new(big.Rat).SetString(*v)
	if !ok {
		return nil
	}
	return r
}

// errDuplicateAxis is the one 422 for a level repeating an earlier one — shared by the grid run and
// the config-time check so the two can never word or tag it differently.
func errDuplicateAxis(names []string, i int) error {
	return tagAxis(invalid("REPORT_CONFIG_INVALID",
		"cấp nhóm thứ %d trùng một cấp trước (cùng trường, cùng cách gom)", i+1), names, i)
}

// rejectResolvedDuplicateAxes is the config-time twin of AggregateGrid's duplicate-axis check
// (#261): validateReportConfig compares axis TOKENS, but the run compares the RESOLVED physical
// column + bucket, so groupBy:ownerId + subGroups:[owner_id] saved fine and died 422 on every run.
// Axes that do not resolve here (namespaced onto a join, unknown, not indexed, bad bucket) are
// skipped — those are reported by the deep check / the run with their own codes.
func rejectResolvedDuplicateAxes(axes []GridAxisInput, names []string, specs []FieldSpec, byName map[string]FieldSpec) error {
	type key struct {
		physical string
		bucket   dynrepo.AggregateBucket
	}
	seen := make(map[key]bool, len(axes))
	for i, ax := range axes {
		if _, _, ns := splitJoinedToken(ax.Field); ns {
			continue
		}
		ga, err := resolveGroupAxis(ax.Field, ax.Bucket, specs, byName, nil)
		if err != nil {
			continue
		}
		k := key{ga.physical, ga.bucket}
		if seen[k] {
			return errDuplicateAxis(names, i)
		}
		seen[k] = true
	}
	return nil
}

// ValidateReportAxes runs rejectResolvedDuplicateAxes against the entity's live fields — the save
// path's (CreateReport/UpdateReport) check that needs metadata the shape-only validator lacks.
func (s *RecordService) ValidateReportAxes(ctx context.Context, tenantID, entity string, axes []GridAxisInput, matrix bool) error {
	if len(axes) < 2 {
		return nil
	}
	names, err := gridAxisNames(GridQuery{Axes: axes, Matrix: matrix})
	if err != nil {
		return err
	}
	_, specs, err := s.resolve(ctx, tenantID, entity)
	if err != nil {
		return fmt.Errorf("services.RecordService.ValidateReportAxes: %w", err)
	}
	byName := make(map[string]FieldSpec, len(specs))
	for _, f := range specs {
		byName[f.Name] = f
	}
	return rejectResolvedDuplicateAxes(axes, names, specs, byName)
}

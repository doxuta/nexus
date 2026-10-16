// Package metadiff is the ONE definition of "canonical form" and "where do two stored documents
// differ" shared by the pack provenance diff (vertical_pack), the AI Builder change preview
// (internal/ai) and — later — publish history. Moved from vertical_pack/diff.go (specs/067 L10-4a);
// new noise rules are added HERE, never patched at a call site.
package metadiff

import (
	"fmt"
	"sort"
	"strings"

	"nexus/platform/internal/bpm"
)

// Cap bounds the per-item path list of the capped report shape.
const Cap = 50

// Diff returns every dotted path where a and b differ (unbounded). Arrays of equal length compare
// element-wise; a length mismatch reports the array path itself.
func Diff(a, b any) []string {
	var out []string
	Walk("", a, b, &out, 0)
	return out
}

// Limit trims paths to n, returning the kept paths and how many were dropped.
func Limit(paths []string, n int) ([]string, int) {
	if len(paths) <= n {
		return paths, 0
	}
	return paths[:n], len(paths) - n
}

// Canonicalize normalizes a decoded item in place so write-path defaults and storage-order
// artifacts never read as differences (vet BLOCKER 1). Defaults mirror the WRITE path builders
// (workflow_service buildConditionJSON persists match "and"; absent isActive persists true) —
// extend here whenever the fresh-install gate finds a new artifact, never by special-casing at
// call sites.
func Canonicalize(itemType string, v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	switch itemType {
	case "workflow":
		// isActive absent → true (pack side omits, write path persists true, export re-emits).
		if _, has := m["isActive"]; !has {
			m["isActive"] = true
		}
		// intervalSeconds absent → 0 (an AI proposal never carries the key; a stored rule always does).
		if _, has := m["intervalSeconds"]; !has {
			m["intervalSeconds"] = float64(0)
		}
		// condition null / {} ⇔ absent (match-all).
		if c, has := m["condition"]; has {
			if cm, ok := c.(map[string]any); c == nil || (ok && len(cm) == 0) {
				delete(m, "condition")
			}
		}
		// condition absent stays absent on both sides only if the write path never synthesizes
		// one; when present, match "" / absent → "and" (buildConditionJSON default).
		if c, has := m["condition"].(map[string]any); has {
			if s, _ := c["match"].(string); strings.TrimSpace(s) == "" {
				c["match"] = "and"
			}
		}
	case "layout":
		// GetLayout serves the list view with a per-column `sortable` flag the stored/proposed
		// layout never carries (applyServeTimeSortable) — drop it on both sides.
		dropKeyIn(m["layout"], "columns", "sortable")
		dropKeyIn(m, "columns", "sortable")
	case "report":
		if _, has := m["isActive"]; !has {
			m["isActive"] = true
		}
		if cfg, ok := m["config"].(map[string]any); ok {
			dropEmpty(cfg)
		}
	case "field":
		dropEmpty(m)
		if opts, ok := m["options"].(map[string]any); ok {
			dropEmpty(opts)
			if len(opts) == 0 {
				delete(m, "options")
			}
		}
	case "work_queue":
		// The write path defaults sortOrder→"asc" and actionVerb→"open" (WorkQueueService.validate);
		// a pack that omits them must not read as modified against the export's re-emitted defaults.
		if v, _ := m["sortOrder"].(string); strings.TrimSpace(v) == "" {
			m["sortOrder"] = "asc"
		}
		if v, _ := m["actionVerb"].(string); strings.TrimSpace(v) == "" {
			m["actionVerb"] = "open"
		}
	case "inbound_hook":
		// isActive absent → true (InboundHookInput.IsActive nil = true; export always re-emits it).
		// A mapping-less pack hook and a live one with an empty list are the same hook.
		if _, has := m["isActive"]; !has {
			m["isActive"] = true
		}
		if mp, has := m["mapping"].([]any); has && len(mp) == 0 {
			delete(m, "mapping")
		}
	case "approval":
		// Write-path defaults (ApprovalService.CreateProcess): absent isActive persists true and
		// export re-emits it; criteria WITH rules persists match "and" (buildApprovalCriteriaJSON);
		// an empty rule list persists NULL — export emits no criteria block at all, so a pack-side
		// empty block must not read as an edit.
		if _, has := m["isActive"]; !has {
			m["isActive"] = true
		}
		if c, has := m["criteria"].(map[string]any); has {
			if rules, _ := c["rules"].([]any); len(rules) == 0 {
				delete(m, "criteria")
			} else if s, _ := c["match"].(string); strings.TrimSpace(s) == "" {
				c["match"] = "and"
			}
		}
		// Step ids (specs/043 S0): a pack installed before ids existed has none in provenance,
		// while the live export carries the ids the write path assigned — fill the missing
		// ones with the SAME rule (bpm.NormalizeSteps), so only a real id change reads as edited.
		if steps, has := m["steps"].([]any); has {
			ids := make([]bpm.Step, len(steps))
			for i, st := range steps {
				if sm, ok := st.(map[string]any); ok {
					ids[i].ID, _ = sm["id"].(string)
				}
			}
			for i, s := range bpm.NormalizeSteps(ids) {
				if sm, ok := steps[i].(map[string]any); ok {
					sm["id"] = s.ID
				}
			}
		}
	case "dashboard":
		// Widgets are keyed by id (storage returns them in insertion order); sort by id when EVERY
		// widget carries one, otherwise order stays the meaning (specs/063 S7-6).
		if ws, has := m["widgets"].([]any); has {
			ids := make([]string, len(ws))
			ok := true
			for i, w := range ws {
				wm, isMap := w.(map[string]any)
				id, _ := wm["id"].(string)
				if !isMap || id == "" {
					ok = false
					break
				}
				ids[i] = id
			}
			if ok {
				idx := make([]int, len(ws))
				for i := range idx {
					idx[i] = i
				}
				sort.SliceStable(idx, func(a, b int) bool { return ids[idx[a]] < ids[idx[b]] })
				sorted := make([]any, len(ws))
				for i, j := range idx {
					sorted[i] = ws[j]
				}
				m["widgets"] = sorted
			}
		}
	case "branding":
		dropEmpty(m)
	case "field_acl":
		// Full access is the default of sys_field_acl: an absent flag means "allowed".
		for _, k := range []string{"canView", "canEdit"} {
			if _, has := m[k]; !has {
				m[k] = true
			}
		}
	case "role":
		// Grants are a SET; storage returns them ORDER BY entity_name, action while packs author
		// semantic grouping. Sort both sides the storage way.
		if acts, has := m["actions"].([]any); has {
			sort.SliceStable(acts, func(i, j int) bool {
				ai, _ := acts[i].(map[string]any)
				aj, _ := acts[j].(map[string]any)
				ei, _ := ai["entityName"].(string)
				ej, _ := aj["entityName"].(string)
				if ei != ej {
					return ei < ej
				}
				xi, _ := ai["action"].(string)
				xj, _ := aj["action"].(string)
				return xi < xj
			})
		}
	}
}

// Walk implements the capped and unbounded diffs; limit <= 0 means unbounded.
func Walk(path string, a, b any, out *[]string, limit int) {
	if limit > 0 && len(*out) >= limit {
		return
	}
	push := func(p string) {
		if p == "" {
			p = "(root)"
		}
		if limit <= 0 || len(*out) < limit {
			*out = append(*out, p)
		}
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			push(path)
			return
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		ordered := make([]string, 0, len(keys))
		for k := range keys {
			ordered = append(ordered, k)
		}
		sort.Strings(ordered)
		for _, k := range ordered {
			pa, inA := av[k]
			pb, inB := bv[k]
			sub := k
			if path != "" {
				sub = path + "." + k
			}
			if !inA || !inB {
				push(sub)
				continue
			}
			Walk(sub, pa, pb, out, limit)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			push(path)
			return
		}
		for i := range av {
			Walk(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i], out, limit)
		}
	default:
		if !jsonScalarEqual(a, b) {
			push(path)
		}
	}
}

// jsonScalarEqual compares two decoded JSON scalars (string/float64/bool/nil).
func jsonScalarEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a == b
}

// dropKeyIn deletes `key` from every object of the array at parent[arr] (parent may be any value).
func dropKeyIn(parent any, arr, key string) {
	pm, ok := parent.(map[string]any)
	if !ok {
		return
	}
	list, _ := pm[arr].([]any)
	for _, it := range list {
		if im, ok := it.(map[string]any); ok {
			delete(im, key)
		}
	}
}

// dropEmpty removes null / "" / empty-array values: the write path stores them as absent.
func dropEmpty(m map[string]any) {
	for k, v := range m {
		switch x := v.(type) {
		case nil:
			delete(m, k)
		case string:
			if x == "" {
				delete(m, k)
			}
		case []any:
			if len(x) == 0 {
				delete(m, k)
			}
		}
	}
}

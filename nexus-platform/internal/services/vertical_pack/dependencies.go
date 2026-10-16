package vertical_pack

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// fieldRefs returns the entity names a field cannot work without: a relation's target, a rollup's
// child table, a polymorphic relation's targets. Pure, so Export (warn) and EntityDependencies
// (auto-select) can never disagree about what "depends on" means.
func fieldRefs(fieldType string, opts map[string]any) []string {
	var out []string
	add := func(v any) {
		if n, ok := v.(string); ok && strings.TrimSpace(n) != "" {
			out = append(out, strings.TrimSpace(n))
		}
	}
	switch fieldType {
	case "relation":
		add(opts["relatedEntity"])
	case "rollup":
		add(opts["childEntity"])
	case "polymorphic_relation":
		if arr, ok := opts["targetEntities"].([]any); ok {
			for _, v := range arr {
				add(v)
			}
		}
	}
	return out
}

func entityLabel(all []metadata.EntityDefinition, byName map[string]int, name string) string {
	if i, ok := byName[name]; ok && strings.TrimSpace(all[i].Label) != "" {
		return all[i].Label
	}
	return name
}

// outOfScopeSummary folds the per-kind counts of items skipped because they hang off a table
// outside the selection into one stable line ("" when nothing was skipped).
func outOfScopeSummary(counts map[string]int) string {
	kinds := make([]string, 0, len(counts))
	for k, n := range counts {
		if n > 0 { // a zero count is "nothing skipped", not a warning
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 0 {
		return ""
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return "Bỏ qua " + strings.Join(parts, ", ") + " thuộc bảng ngoài phạm vi xuất (app khác trong cùng tổ chức) — không ảnh hưởng gói này."
}

// EntityDependency is a table the selection needs but did not name.
type EntityDependency struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Reason string `json:"reason"` // human line: which field of which table needs it
}

// EntityDependencies returns the transitive closure of tables the named tables need (relation
// targets, rollup children, polymorphic targets) that are not already named. Tenant-scoped; an
// unknown name is skipped (the export itself rejects it with ENTITY_NOT_FOUND).
func (s *Service) EntityDependencies(ctx context.Context, tenantID string, names []string) ([]EntityDependency, error) {
	ctx = metaversion.WithLiveRead(ctx)
	all, err := s.entityRd.ListEntities(tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.EntityDependencies: list entities: %w", err)
	}
	byName := make(map[string]int, len(all))
	for i := range all {
		byName[all[i].Name] = i
	}
	known := map[string]bool{}
	queue := []string{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if _, ok := byName[n]; ok && !known[n] {
			known[n] = true
			queue = append(queue, n)
		}
	}
	out := []EntityDependency{}
	for len(queue) > 0 {
		cur := all[byName[queue[0]]]
		queue = queue[1:]
		fields, err := s.entityRd.ListFields(ctx, tenantID, cur.ID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.EntityDependencies: list fields of %q: %w", cur.Name, err)
		}
		for _, f := range fields {
			opts, err := decodeOptions(f.Options)
			if err != nil {
				return nil, fmt.Errorf("vertical_pack.EntityDependencies: decode options of %q.%q: %w", cur.Name, f.Name, err)
			}
			for _, ref := range fieldRefs(f.FieldType, opts) {
				if _, ok := byName[ref]; !ok || known[ref] {
					continue
				}
				known[ref] = true
				queue = append(queue, ref)
				out = append(out, EntityDependency{
					Name:   ref,
					Label:  entityLabel(all, byName, ref),
					Reason: fmt.Sprintf("trường %q của bảng %q cần bảng này", f.Label, cur.Label),
				})
			}
		}
	}
	return out, nil
}

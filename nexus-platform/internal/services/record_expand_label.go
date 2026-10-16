package services

import (
	"context"
	"strings"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// withTargetLabelFields points every requested relation spec at its TARGET table's label field
// (#272): the builder's choice on the table (sys_entities.options.labelField) wins, else the
// relation's own options.labelField, else the derived default (metadata.ResolveLabelField) —
// never the blind "name" that left VN tables with empty labels. Returns a copy: the input specs
// come from the shared MetaCache and must not be mutated. One catalog read per distinct target
// (the cache makes repeats free). A nil catalog or an unreadable target keeps the spec as-is
// (pre-fix behaviour) — a label hiccup must not fail the page.
func withTargetLabelFields(ctx context.Context, catalog EntityCatalog, tenantID string, specs []FieldSpec, expand []string) []FieldSpec {
	if catalog == nil || len(expand) == 0 {
		return specs
	}
	want := make(map[string]struct{}, len(expand))
	for _, e := range expand {
		want[e] = struct{}{}
	}
	type resolved struct {
		name     string
		explicit bool
		ok       bool
	}
	byTarget := make(map[string]resolved)
	out := make([]FieldSpec, len(specs))
	copy(out, specs)
	for i, sp := range out {
		if _, asked := want[sp.Name]; !asked || sp.FieldType != "relation" {
			continue
		}
		target, _ := sp.Options["relatedEntity"].(string)
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		r, seen := byTarget[target]
		if !seen {
			if e, err := catalog.GetEntityByName(ctx, tenantID, target); err == nil && e != nil {
				if fields, ferr := catalog.ListFields(ctx, tenantID, e.ID); ferr == nil {
					r.name, r.explicit = metadata.ResolveLabelField(e.Options, fields)
					r.ok = r.name != ""
				}
			}
			byTarget[target] = r
		}
		if !r.ok {
			continue
		}
		if own, _ := sp.Options["labelField"].(string); !r.explicit && strings.TrimSpace(own) != "" {
			continue // no table-level choice: the relation's own explicit labelField stands
		}
		opts := make(map[string]any, len(sp.Options)+1)
		for k, v := range sp.Options {
			opts[k] = v
		}
		opts["labelField"] = r.name
		out[i].Options = opts
	}
	return out
}

// expandRecords is applyExpand with table-level label fields resolved first. Every read path
// (Get, List, cursor List, CSV export, report axis labels) goes through here so they agree.
func (s *RecordService) expandRecords(ctx context.Context, scope RecordScope, tenantID string, records []dynrepo.Record, specs []FieldSpec, expand []string) error {
	return applyExpand(ctx, s.fetcher, s.authz, s.fieldACL, scope, tenantID, records,
		withTargetLabelFields(ctx, s.catalog, tenantID, specs, expand), expand)
}

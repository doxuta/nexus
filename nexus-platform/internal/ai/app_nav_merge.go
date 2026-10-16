package ai

import (
	"context"
	"errors"
	"fmt"

	"nexus/platform/internal/services"
)

// mergeAppNav is the ONE menu-merge rule (specs/067 plan §8; the FE twin is lib/appNav.ts
// mergeAppNavKeys, both pinned by testdata/app_nav_merge_cases.json): keep every existing row
// (any type, custom label, order), append the new tables not yet there, dedupe by (type, name).
func mergeAppNav(before []ContextNavItem, add []string) []ContextNavItem {
	out := append([]ContextNavItem{}, before...)
	seen := make(map[[2]string]bool, len(out)+len(add))
	for _, n := range out {
		seen[[2]string{n.Type, n.Name}] = true
	}
	for _, name := range add {
		if k := [2]string{"entity", name}; !seen[k] {
			seen[k] = true
			out = append(out, ContextNavItem{Type: "entity", Name: name})
		}
	}
	return out
}

// appChange builds the propose_app envelope: slug taken => update (Before = current menu, After =
// merged), unknown => create. The menu holds no secrets, so it needs no redaction.
func (e *toolExecutor) appChange(ctx context.Context, slug string, nav []string) (*DraftChange, string) {
	label, target := fmt.Sprintf("App %q", slug), DraftChangeTarget{Name: slug}
	wrap := func(items []ContextNavItem) map[string]any { return map[string]any{"nav": items} }
	create := func() (*DraftChange, string) {
		return newChange("app", "", "create", label, target, nil, wrap(mergeAppNav(nil, nav))), ""
	}
	if e.docs == nil || e.docs.Apps == nil {
		return create()
	}
	_, items, err := e.docs.Apps.GetNav(ctx, services.RecordScope{IsAdmin: true}, e.tenantID, slug)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) && ve.Code == "APP_NOT_FOUND" {
			return create()
		}
		return nil, previewFailed(err)
	}
	before := make([]ContextNavItem, 0, len(items))
	for _, it := range items {
		before = append(before, ContextNavItem{Type: it.Type, Name: it.RefName, Label: it.Label})
	}
	merged := mergeAppNav(before, nav)
	c := newChange("app", "", "update", label, target, wrap(before), wrap(merged))
	// The shared diff reports a longer array as one place ("nav"); the merge only ever APPENDS,
	// so name each new row instead.
	c.ChangedPaths, c.MoreChanged = nil, 0
	for i := len(before); i < len(merged); i++ {
		if len(c.ChangedPaths) < changePathsMax {
			c.ChangedPaths = append(c.ChangedPaths, fmt.Sprintf("nav[%d]", i))
		} else {
			c.MoreChanged++
		}
	}
	return c, ""
}

package metaversion

import "sort"

// ClassKind says how a configuration document type behaves for a delivered customer workspace.
type ClassKind string

const (
	// Draft: edits go to the draft layer and reach the customer only on publish.
	Draft ClassKind = "draft"
	// DirectTraced: edits apply at once and leave a "direct" version entry (S7-5).
	DirectTraced ClassKind = "direct_traced"
	// DirectUntraced: edits apply at once with no trace (everything not yet covered).
	DirectUntraced ClassKind = "direct_untraced"
)

// Class is the behaviour of one document type.
type Class struct {
	Kind ClassKind
	// NewHidden: a NEW document of this type stays hidden from the customer until publish (S7-8).
	NewHidden bool
}

// draftTypes is the set of types that go through drafts. A slice that adds a type to the registry
// adds it here in the SAME commit (class_test.go ties the two together).
var draftTypes = map[string]Class{
	"workflow":  {Kind: Draft},
	"report":    {Kind: Draft},
	"dashboard": {Kind: Draft},
	"layout":    {Kind: Draft},
	"branding":  {Kind: Draft},
	"app":       {Kind: Draft},
	// S7-8: an edit of a field goes through a draft; a NEW field is hidden from the customer until publish.
	"field": {Kind: Draft, NewHidden: true},
	// S7-9: a table is only ever drafted as "delete" (the customer keeps using it until Publish); a NEW
	// table is hidden from the customer until publish (S7-8).
	"entity": {Kind: Draft, NewHidden: true},
}

// directTracedTypes: types applied at once WITH a version entry. Empty until S7-5.
var directTracedTypes = map[string]Class{
	"role":      {Kind: DirectTraced},
	"field_acl": {Kind: DirectTraced},
}

// newHiddenUntracedTypes: applied at once with no trace, but a NEW one stays hidden from the customer until
// publish. An N-N relation (POST /admin/relationships) is never drafted, yet GET /metadata/relationships drops
// one created after the schema cutoff and the publish change list carries it (services.schemaBornBetween).
var newHiddenUntracedTypes = map[string]Class{
	"relationship": {Kind: DirectUntraced, NewHidden: true},
}

// ClassOf is the ONE source of classification: GET /admin/publish/state builds draftableTypes and
// directTypes from it, and other lanes (pack upgrade applyMode) map from it instead of keeping a
// table of their own. An unknown type is DirectUntraced — fail-safe: never promise "goes through a
// draft" for a type that has no adapter.
func ClassOf(docType string) Class {
	if c, ok := draftTypes[docType]; ok {
		return c
	}
	if c, ok := directTracedTypes[docType]; ok {
		return c
	}
	if c, ok := newHiddenUntracedTypes[docType]; ok {
		return c
	}
	return Class{Kind: DirectUntraced}
}

// TypesOfKind lists the known types of a kind, sorted.
func TypesOfKind(k ClassKind) []string {
	var src map[string]Class
	switch k {
	case Draft:
		src = draftTypes
	case DirectTraced:
		src = directTracedTypes
	default:
		return nil
	}
	out := make([]string, 0, len(src))
	for t := range src {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

package services

import "nexus/platform/internal/metaversion"

// NewMetaRegistry is the one registry of draftable document types. cmd/server wires it, and
// TestClassOf_AgreesWithRegistry checks it against metaversion's class table, so the two cannot drift.
func NewMetaRegistry() *metaversion.Registry {
	return metaversion.NewRegistry(NewLayoutDocAdapter(), NewWorkflowDocAdapter(), NewReportDocAdapter(), NewDashboardDocAdapter(), NewBrandingDocAdapter(), NewAppDocAdapter(), NewFieldDocAdapter(), NewEntityDocAdapter())
}

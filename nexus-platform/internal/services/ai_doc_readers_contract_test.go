package services_test

import (
	"nexus/platform/internal/ai"
	"nexus/platform/internal/services"
)

// Contract (specs/067 T048): the AI reads documents through the real services with no adapter —
// a signature change on any of them breaks compilation here.
var _ = ai.DocReaders{
	Layouts:   (*services.LayoutService)(nil),
	Workflows: (*services.WorkflowService)(nil),
	Reports:   (*services.ReportService)(nil),
	Apps:      (*services.AppService)(nil),
}

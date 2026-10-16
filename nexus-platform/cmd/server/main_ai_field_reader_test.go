package main

// main_ai_field_reader_test.go — specs/063-draft-publish S7-8 T123a/T121a. The AI Builder lists fields
// through the draft layer (so it sees the builder's unpublished labels), and the access probes still
// ride on the OUTERMOST reader: wrapping it the other way round makes explain_access answer
// ACCESS_EXPLAIN_UNAVAILABLE.

import (
	"os"
	"strings"
	"testing"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/services"
)

func TestAIFieldReader_KeepsAccessProbesAndIsWiredInside(t *testing.T) {
	var inner ai.MetadataReader = aiDraftFieldReader{fields: (*services.DraftFieldRepo)(nil)}
	wrapped := ai.ReaderWithAccessProbes(inner, &ai.AccessProbes{})
	carrier, ok := wrapped.(ai.AccessProbeCarrier)
	if !ok || carrier.AccessProbes() == nil {
		t.Fatal("the wrapped draft-field reader lost AccessProbeCarrier: explain_access would answer ACCESS_EXPLAIN_UNAVAILABLE")
	}

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{
		"var aiMeta ai.MetadataReader = aiDraftFieldReader{Repository: metaRepo, fields: draftFieldRepo}",
		"ai.ReaderWithAccessProbes(aiMeta,",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("main.go lost %q — the AI would read live fields only, or its probes would be dropped", want)
		}
	}
}

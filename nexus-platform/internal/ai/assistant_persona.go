package ai

import _ "embed"

// personaNexusAssistant is the system prompt for the ⌘K end-user data assistant (ask-data +
// HITL record drafts). It is a SEPARATE soul from the admin AI-Builder persona: no
// schema-building — "answer questions about the data YOU may see, via tools, never invent
// numbers; record create/update requests become CONFIRM-CARD drafts, never claimed as done".
//
//go:embed persona_nexus_assistant.md
var personaNexusAssistant string

// AssistantPersona returns the embedded data-assistant system prompt. Always non-empty (the
// embed fails the build if the file is missing), mirroring Persona() for the builder.
func AssistantPersona() string { return personaNexusAssistant }

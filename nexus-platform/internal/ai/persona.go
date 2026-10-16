package ai

import _ "embed"

// personaNexusBuilder is the system prompt that gives the agent its "soul" (spec §4): the nexus-*
// voice, the HITL discipline, the 13 field types, the naming rule, and the tool contract. It is
// embedded at build time (never read from disk at runtime) so the binary is self-contained and the
// prompt cannot be swapped out under the running server.
//
//go:embed persona_nexus_builder.md
var personaNexusBuilder string

// Persona returns the embedded nexus-builder system prompt. It is always non-empty (the embed
// directive fails the build if the file is missing/empty), so callers can prepend it as the first
// system message without a nil/empty guard.
func Persona() string { return personaNexusBuilder }

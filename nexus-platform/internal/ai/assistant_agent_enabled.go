package ai

// Enabled reports whether the AI product surface is on (AI_ENABLED). Consumed by the
// GET /ai/health probe so FE surfaces can render an honest on/off state BEFORE the user
// types — previously the only way to learn AI was off was a 503 after composing a message.
func (a *AssistantAgent) Enabled() bool {
	return a.cfg.Enabled
}

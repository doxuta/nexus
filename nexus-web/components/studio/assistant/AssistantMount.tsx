"use client";

// AssistantMount — draws the panel only where the assistant may exist (specs/076 ai3 T061). AssistantProvider decides
// `eligible` (assistantEligible: a phien page + a token that belongs to this project); a page that fails it renders
// nothing here and made no call (aiHealth is asked only after eligibility).
import { AssistantPanel } from "./AssistantPanel";
import { useAssistant } from "./AssistantProvider";

export function AssistantMount() {
  return useAssistant().eligible ? <AssistantPanel /> : null;
}

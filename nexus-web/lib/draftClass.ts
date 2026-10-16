// classOf — how a change of one document type reaches real users (specs/073 FR-113, UI-ARCH 6.4).
// The ONLY owner of this mapping: F2/F3/F4 import it. The lists come from GET /admin/publish/state, never hardcoded here.
import type { PublishState } from "@/lib/api";

export type DraftClass = "draft" | "direct" | "untraced" | "chưa giao";

export function classOf(
  docType: string,
  state: Pick<PublishState, "delivered" | "draftableTypes" | "directTypes"> | undefined,
): DraftClass {
  if (!state?.delivered) return "chưa giao";
  if (state.draftableTypes.includes(docType)) return "draft";
  if (state.directTypes.includes(docType)) return "direct";
  return "untraced";
}

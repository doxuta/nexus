// Test helper (specs/074 lát 34): turns ONE assistant change on inside a ProposalOverlayProvider, like the card's
// "Xem trước" button, so an editor's ghost can be asserted without the whole dock.
import { useEffect, type ReactNode } from "react";
import { PAINTED, ProposalOverlayProvider, useProposalOverlay, type ProposalOp, type ProposedChange } from "@/components/studio/assistant/ProposalOverlay";

export const mkChange = (docType: string, op: ProposalOp, over: Omit<Partial<ProposedChange>, "target"> & { target?: Omit<ProposedChange["target"], "docType"> } = {}): ProposedChange => {
  const { target, ...rest } = over;
  return { proposalKey: `${docType}-${op}`, docType, op, label: docType, target: { docType, ...target }, ...rest };
};

function Switch({ change }: { change: ProposedChange | null }) {
  const o = useProposalOverlay();
  useEffect(() => { if (change) o?.toggle(change); }, []); // eslint-disable-line react-hooks/exhaustive-deps -- once, like one click
  return <button type="button" onClick={() => o?.clear()}>clear-preview</button>;
}

/** Wrap the editor under test; `change` is switched on once after mount. Uses the real PAINTED table. */
export function WithPreview({ change, children }: { change: ProposedChange | null; children: ReactNode }) {
  return (
    <ProposalOverlayProvider slug="demo" navigate={false} painted={PAINTED}>
      <Switch change={change} />
      {children}
    </ProposalOverlayProvider>
  );
}

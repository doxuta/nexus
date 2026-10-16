// Test helper (specs/074 lát f2-ai-preview-data-rules): mounts the overlay with every kind painted and turns one change on,
// the way the card's "Xem trước" does. The caller mocks next/navigation (the provider reads the router).
import { useEffect, type ReactNode } from "react";
import { ProposalOverlayProvider, useProposalOverlay, type ProposalOp, type ProposedChange } from "@/components/studio/assistant/ProposalOverlay";

const ALL = ["entity", "field", "workflow", "layout"].flatMap((docType) => (["create", "update", "delete"] as ProposalOp[]).map((op) => ({ docType, op })));

function Toggle({ change }: { change: ProposedChange }) {
  const o = useProposalOverlay();
  useEffect(() => o?.toggle(change), []); // eslint-disable-line react-hooks/exhaustive-deps
  return null;
}
export function Off() {
  const o = useProposalOverlay();
  return <button onClick={() => o?.clear()}>ghost-off</button>;
}

export function WithGhost({ change, children }: { change?: ProposedChange; children: ReactNode }) {
  return (
    <ProposalOverlayProvider slug="northwind" painted={ALL} navigate={false}>
      {change && <Toggle change={change} />}
      {children}
    </ProposalOverlayProvider>
  );
}

export const mkChange = (over: Partial<ProposedChange> & Pick<ProposedChange, "docType" | "op" | "label">): ProposedChange => ({
  proposalKey: "k1", target: { docType: over.docType }, ...over,
});

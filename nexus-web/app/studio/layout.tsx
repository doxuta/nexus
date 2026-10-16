"use client";

// Studio route-group layout — the Studio shell on SLDS (specs/038 T038, contracts/shell.md "Shell
// Studio"). components/studio/StudioShell renders the global header and, inside /studio/du-an/**, the project frame
// (its own strip and `main`); the old builder header is gone (specs/073 kh7). Admin-only, fail-closed: RequireAuth
// then RequireOperator.
// StudioPreviewGate (specs/064 S9-2) is outermost: a tab still previewing a role never runs the Studio
// — it ends the preview ("left") and reloads before anything below mounts or requests.
// "Studio KHÔNG nhuộm màu tenant" (contracts/shell.md), two halves: ShellGlobalHeader `studio` shows
// Nexus's name + glyph instead of the tenant's, and `nx-face-studio` is the hook of the
// `html:has(.nx-face-studio)` block that pins every --slds-r-color-brand-* back to the stock SLDS
// literal over lib/branding.ts's inline tenant ramp. It sits on <html>, where SLDS declares the
// --slds-g-color-brand-base-* hooks, so those re-derive stock too. That block is campaign CSS glue,
// applied to globals.css at the campaign merge (the only way globals.css changes); without it the
// class does nothing and the Studio's colours follow the tenant.
import { RequireAuth, RequireOperator } from "@/lib/auth/guards";
import { StudioShell } from "@/components/studio/StudioShell";
import { StudioPreviewGate } from "@/components/studio/StudioPreviewGate";

export default function StudioLayout({ children }: { children: React.ReactNode }) {
  return (
    <StudioPreviewGate>
    <RequireAuth>
      <RequireOperator>
        <StudioShell>{children}</StudioShell>
      </RequireOperator>
    </RequireAuth>
    </StudioPreviewGate>
  );
}

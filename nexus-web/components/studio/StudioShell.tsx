"use client";

// The Studio shell below the guards (specs/073 kh1b1 T155, kh5 T314/T315, kh7 T375): the global header, then either one
// `main` (routes under /studio that are not a project: front page, new project, account, verticals, assistant) or, inside
// /studio/du-an/**, nothing — the project frame brings its own strip and its own `main#nx-main`, so a second `main` and a
// second header never stack. A switch from the header that fails on a route outside a project is drawn here (the frame
// only exists inside one). The global header's switcher is the navigation; the old builder header is gone (kh7).
// Global header slots (FR-052): project switcher, no Setup gear (Setup is the customer face's), and the "Trợ lý" toggle,
// offered only where the token in this tab belongs to the project on screen. It docks/undocks the one assistant panel
// (lib/assistantOpen), the same store the in-page button and ⌘K "Mở trợ lý" use.
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";
import { ShellGlobalHeader } from "@/components/slds/shell/GlobalHeader";
import { toggleAssistant, useAssistantOpen } from "@/lib/assistantOpen";
import { useStudioIdentity, useStudioTransitionFailed } from "@/lib/studioContext";
import { currentProjectSlug, inProjectPath } from "@/lib/studioRoutes";
import { ProjectSwitcher } from "./ProjectSwitcher";
import { StudioSessionBanner } from "./StudioSessionBanner";
import { SwitchFailedPanel } from "./SwitchFailedPanel";

export function StudioShell({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const assistantOpen = useAssistantOpen();
  const project = inProjectPath(pathname);
  const slug = currentProjectSlug(pathname);
  const identity = useStudioIdentity({ enabled: slug !== null });
  const switchFailed = useStudioTransitionFailed();
  const assistant = slug !== null && identity.matches(slug);
  return (
    <div className="nx-face-studio flex h-screen flex-col overflow-hidden">
      <ShellGlobalHeader
        studio
        className="shrink-0 print:hidden"
        switcher={<ProjectSwitcher currentSlug={slug} />}
        hideSetup
        assistantToggle={assistant ? { pressed: assistantOpen, onToggle: toggleAssistant } : undefined}
      />
      <StudioSessionBanner />
      {project ? (
        children
      ) : (
        <main id="nx-main" className="flex min-h-0 flex-1 flex-col overflow-hidden">
          {switchFailed ? <SwitchFailedPanel /> : children}
        </main>
      )}
    </div>
  );
}

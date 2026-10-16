"use client";

// Project switcher in the Studio header (specs/073 FR-051, FR-083). Two modes, by what the TAB is doing:
//   • no build session (the Nexus workshop): "Nội bộ" (the workshop) and "Tất cả dự án" (back to /studio). "Dự án mới"
//     stays out until a project can be created.
//   • inside a build session: GET /admin/projects answers 403 there, so no list of customers is offered. The menu holds
//     the project being built, "Gần đây" (what this operator opened lately, from lib/studioRecentProjects) and the way
//     back. Choosing a recent one is a real session switch (lib/studioContext.switchProject: the work surface goes
//     first, only the close of the old session and the open of the new one reach the network), never a route push.
// Behaviour comes from the SLDS Menu (Radix dropdown): Enter opens, arrows move, Esc closes and returns focus. The
// trigger is the demo's bordered button: the current project's name (or "Tất cả dự án") and a chevron.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, Check } from "lucide-react";
import { useMemo } from "react";
import { Menu, MenuDivider, MenuGroup, MenuItem } from "@/components/slds/Menu";
import { listProjects, type ProjectStage } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { useConfirmLeave, useGuardedRouter } from "@/lib/navigation-blocker";
import { qk } from "@/lib/query/keys";
import { enterProject, exitProject, homeSlug, switchProject, useWorkspaceView } from "@/lib/studioContext";
import { readRecentProjects } from "@/lib/studioRecentProjects";
import { studioHref } from "@/lib/studioRoutes";

// A plain function returning the element, not a component: Radix's Trigger (asChild) puts its ref and handlers on the
// ELEMENT it is given, which a wrapper component would swallow.
function trigger(name: string) {
  return (
    <button
      type="button"
      className="slds-button slds-button_neutral max-w-full min-w-0"
      aria-label={tf("studio.shell.switcher.trigger", { name })}
    >
      <span className="slds-truncate" title={name}>
        {name}
      </span>
      <ChevronDown aria-hidden="true" className="slds-button__icon slds-button__icon_right nx-lucide" />
    </button>
  );
}

const STAGES: { key: ProjectStage; name: I18nKey }[] = [
  { key: "building", name: "studio.stage.building.name" },
  { key: "trial", name: "studio.stage.trial.name" },
  { key: "live", name: "studio.stage.live.name" },
  { key: "needs_attention", name: "studio.stage.needsAttention.name" },
];

export function ProjectSwitcher({ currentSlug }: { currentSlug: string | null }) {
  const router = useGuardedRouter();
  const qc = useQueryClient();
  const confirmLeave = useConfirmLeave();
  const session = useWorkspaceView();
  const home = homeSlug();
  // Read when the session changes, not on every render: the list is only rewritten by opening a session.
  const recent = useMemo(
    () => (session ? readRecentProjects(session.homeUserId).filter((p) => p.slug !== session.tenantSlug) : []),
    [session],
  );

  const projects = useQuery({ queryKey: qk.projects(), queryFn: listProjects, enabled: !session });

  if (session) {
    const deps = { qc, confirmLeave };
    const name = session.tenantName || session.tenantSlug;
    return (
      <Menu size="small" trigger={trigger(name)}>
        <MenuGroup label={t("studio.shell.switcher.building")}>
          <MenuItem label={name} iconRight={Check} current onSelect={(e) => e.preventDefault()} />
        </MenuGroup>
        {recent.length > 0 && (
          <MenuGroup label={t("studio.shell.switcher.recent")}>
            {recent.map((p) => (
              <MenuItem key={p.slug} label={p.name} onSelect={() => void switchProject(p.slug, studioHref(p.slug, "dung/du-lieu"), deps).catch(() => undefined)} />
            ))}
          </MenuGroup>
        )}
        <MenuDivider />
        <MenuItem label={t("studio.newProject.title")} onSelect={() => void exitProject("/studio/moi", deps).catch(() => undefined)} />
        <MenuItem label={t("studio.session.backToProjects")} onSelect={() => void exitProject("/studio", deps).catch(() => undefined)} />
      </Menu>
    );
  }
  const rows = (projects.data ?? []).filter((r) => r.slug !== home);
  // From the home context there is no session to close (open); from another project close it first (switch).
  const pick = (slug: string) => {
    const go = currentSlug === null || currentSlug === home ? enterProject : switchProject;
    void go(slug, studioHref(slug, "dung/du-lieu"), { qc, confirmLeave });
  };
  const current = currentSlug === null ? t("studio.shell.switcher.all") : currentSlug === home ? t("studio.project.home") : currentSlug;
  return (
    <Menu size="small" trigger={trigger(current)}>
      <MenuGroup label={t("studio.shell.switcher.internal")}>
        <MenuItem
          label={t("studio.project.home")}
          iconRight={currentSlug === home ? Check : undefined}
          onSelect={() => router.push(studioHref(home, "dung/du-lieu"))}
        />
      </MenuGroup>
      {STAGES.map((st) => {
        const list = rows.filter((r) => r.stage === st.key);
        return (
          list.length > 0 && (
            <MenuGroup key={st.key} label={t(st.name)}>
              {list.map((r) => (
                <MenuItem key={r.tenantId} label={r.name} iconRight={currentSlug === r.slug ? Check : undefined} onSelect={() => pick(r.slug)} />
              ))}
            </MenuGroup>
          )
        );
      })}
      <MenuDivider />
      <MenuItem label={t("studio.newProject.title")} onSelect={() => router.push("/studio/moi")} />
      <MenuItem
        label={t("studio.shell.switcher.all")}
        iconRight={currentSlug === null ? Check : undefined}
        onSelect={() => router.push("/studio")}
      />
    </Menu>
  );
}

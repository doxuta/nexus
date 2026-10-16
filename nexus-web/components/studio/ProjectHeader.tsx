"use client";

import { MoreHorizontal } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { onOpenPublishReview } from "@/lib/draftEvents";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Menu, MenuItem } from "@/components/slds/Menu";
import { PageHeader } from "@/components/slds/PageHeader";
import { Tabs } from "@/components/slds/Tabs";
import { t, tf } from "@/lib/i18n";
import { useStudioIdentity } from "@/lib/studioContext";
import { studioHref, ZONE_TABS, type RouteId } from "@/lib/studioRoutes";
import { PublishReview } from "./PublishReview";

export type StudioZone = "dung" | "van-hanh" | "bao-tri";

export interface ProjectHeaderProps {
  slug: string;
  identity: { name: string; industry?: string | null; stageLabel?: string | null; originLabel?: string | null };
  zone: StudioZone;
  /** One line about the project's state, only when there is a real source for it. */
  status?: ReactNode;
  /** The Nexus workshop: its own sub-line, and its menu item reads "app của xưởng Nexus". */
  home?: boolean;
  /** Opens the app face for the token in this tab. Offered only when the token belongs to this project (FR-022). */
  openApp?: () => void;
  actions?: ReactNode[];
  /** Delivery state of a customer project (publish/state). Absent = no source (the workshop home, or not read yet). */
  publish?: { delivered: boolean; pendingCount: number; versionNo?: number | null; /** specs/063 S7-9: a published version still has field / table deletes to retry. */ schemaRetry?: boolean };
}

/**
 * Project strip (specs/073 FR-013): the name as the h1 (wraps up to two lines, never cut with an ellipsis: it is the
 * only place that says which customer is being built), one dotted line of facts (only this line is truncated), the
 * three zones as links. This strip reads nothing from the server: facts arrive as props, and the menu asks
 * projectTokenMatches before offering anything that acts on the token's tenant.
 */
export function ProjectHeader({ slug, identity, zone, status, home, openApp, actions = [], publish }: ProjectHeaderProps) {
  const matches = useStudioIdentity().matches(slug);
  const [reviewing, setReviewing] = useState(false);
  // Counted only when the token belongs to this project (FR-022). Heaviest state wins: pending drafts, then the live version, then "not delivered".
  // The workshop has no delivery notion until it is delivered (the one-command demo publishes its baseline): from then on its
  // drafts need the same Publish as a customer project's, or nothing in the Studio could publish them.
  const shown = matches && (!home || publish?.delivered) ? publish : undefined;
  const hasPending = !!shown && shown.delivered && shown.pendingCount > 0;
  const needsRetry = !!shown && shown.delivered && !!shown.schemaRetry;
  const canPublish = hasPending || needsRetry;
  // The assistant's "Mở bản nháp" (specs/076 FR-127) opens this same review; registered only while its button exists.
  useEffect(() => (canPublish ? onOpenPublishReview(() => setReviewing(true)) : undefined), [canPublish]);
  const publishLine = !shown
    ? null
    : !shown.delivered
      ? t("studio.publish.status.undelivered")
      : hasPending
        ? tf("studio.publish.status.pending", { n: shown.pendingCount })
        : needsRetry
          ? t("studio.publish.status.schemaFailed")
          : shown.versionNo
            ? tf("studio.publish.status.live", { n: shown.versionNo })
            : null;
  // Nothing pending but a delete did not finish: the same review opens, with the Thử lại panel in it (specs/063 S7-9).
  const publishBtn = canPublish ? (
    <Button key="publish" variant={hasPending ? "brand" : "neutral"} onClick={() => setReviewing(true)}>
      {t(hasPending ? "studio.publish.open" : "studio.publish.retryOpen")}
    </Button>
  ) : null;
  // Delivered, the workshop is seen by its customers: its line no longer claims they cannot see it.
  const facts = home ? t(shown?.delivered ? "studio.project.homeMetaDelivered" : "studio.project.homeMeta") : [identity.industry, identity.stageLabel, identity.originLabel].filter(Boolean).join(" · ");
  const line = (
    <span className="slds-truncate" title={facts}>
      {facts}
    </span>
  );
  const menu =
    matches && openApp ? (
      <Menu align="right" trigger={<ButtonIcon icon={MoreHorizontal} label={t("studio.project.menu")} variant="border-filled" more />}>
        <MenuItem label={t(home ? "studio.project.openHomeApp" : "studio.project.openApp")} onSelect={openApp} />
      </Menu>
    ) : null;
  const allActions = [
    ...actions,
    ...(publishBtn ? [reviewing ? <PublishReview key="publish" anchor={publishBtn} onClose={() => setReviewing(false)} /> : publishBtn] : []),
    ...(menu ? [menu] : []),
  ];
  // Layout only (FR-013): `xl:` (= 1280px) and not `min-[1280px]:`, which Tailwind emits BEFORE `md:` so md wins at 1440 (measured).
  // One row from 1280px (identity, zones right after it as in demo A, actions at the far end), two rows 768-1279 (identity + actions, then
  // zones), stacked below 768px. The cells are placed by grid so the DOM order stays identity, actions, zones.
  return (
    <div className="slds-theme_default slds-border_bottom grid grid-cols-1 gap-x-4 md:grid-cols-[minmax(0,1fr)_auto] xl:grid-cols-[minmax(0,34rem)_auto_minmax(0,1fr)]" data-testid="project-header">
      <div className="min-w-0 md:col-start-1 md:row-start-1">
        <PageHeader
          className="slds-page-header_joined slds-theme_default border-0"
          title={identity.name}
          wrapTitle
          meta={
            <>
              {line}
              {(status ?? publishLine) && <span className="slds-m-left_small">{status ?? publishLine}</span>}
            </>
          }
        />
      </div>
      {allActions.length > 0 && (
        <div className="slds-p-around_small md:col-start-2 md:row-start-1 xl:col-start-3 xl:justify-self-end">
          <div className="slds-page-header__controls">
            {allActions.map((a, i) => (
              <div key={i} className="slds-page-header__control">
                {a}
              </div>
            ))}
          </div>
        </div>
      )}
      <div className="min-w-0 md:col-span-2 md:row-start-2 xl:col-span-1 xl:col-start-2 xl:row-start-1 xl:self-end">
        <Tabs
          mode="links"
          size="medium"
          label={t("studio.a11y.nav.zones")}
          current={zone}
          items={[
            { value: "dung", label: t("studio.zone.build"), href: studioHref(slug, "dung/du-lieu") },
            { value: "van-hanh", label: t("studio.zone.operate"), href: studioHref(slug, ZONE_TABS["van-hanh"][0].route as RouteId) }, // the zone opens on its first sub tab
            { value: "bao-tri", label: t("studio.zone.maintain"), href: studioHref(slug, "bao-tri/kham") },
          ]}
        />
      </div>
    </div>
  );
}

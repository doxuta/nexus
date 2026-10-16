"use client";

// EntityDirectory — every entity from GET /metadata/entities, on SLDS 2.264.1 (specs/038).
// Design read: the "All Items" page of the customer face — blueprints page-headers/base (inside a
// card, as the object-home template does) + app-launcher/base tiles ("All Apps" grid: initials
// avatar in a custom colour + name) + illustration (empty) + scoped-notifications (error).
// Data-driven: one tile per live entity, each opens /:entity. Zero entities → honest empty state
// (no fake/hardcoded entities).
// Deviation: the whole tile is the link (the blueprint's tile is a draggable div holding an <a>),
// so the old click-anywhere card keeps its target size; the tile's own :hover/:focus rules give
// the link its border and focus ring. No reorder button / description popover — entities carry
// neither an order the user owns nor a description.

import Link from "next/link";
import { studioHref } from "@/lib/studioRoutes";
import { activeProjectSlug } from "@/lib/studioContext";
import { LayoutGrid } from "lucide-react";
import { useEntities } from "@/lib/metadata/hooks";
import { useAuth } from "@/lib/auth/AuthProvider";
import { APP_TONES, appToneIcon } from "@/lib/apps";
import { getHelp } from "@/lib/help-content";
import { t, tf } from "@/lib/i18n";
import { avatarInitials } from "@/components/slds/Avatar";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { EmptyState } from "@/components/slds/EmptyState";
import { PageHeader } from "@/components/slds/PageHeader";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { FieldHelp } from "@/components/fields/widgets";

const entityHelp = getHelp("entity");

export function EntityDirectory() {
  const { data: entities = [], isLoading, isError, error } = useEntities();
  const { user } = useAuth();
  const isAdminUser = !!user?.isAdmin;
  const embedded = useEmbedded();

  const meta = [t("page.entities.desc"), !isLoading && entities.length > 0 && tf("page.entities.count", { n: entities.length })]
    .filter(Boolean)
    .join(" · ");

  let body;
  if (isLoading) {
    body = (
      <div className="slds-is-relative h-16">
        <Spinner size="small" label={t("page.entities.loading")} />
      </div>
    );
  } else if (isError) {
    body = (
      <ScopedNotification theme="error">
        {error instanceof Error ? error.message : t("page.entities.loadError")}
      </ScopedNotification>
    );
  } else if (entities.length === 0) {
    body = (
      <EmptyState
        size="large"
        title={t("entities.empty")}
        description={
          <>
            {/* Studio is for the builder only — anyone else would bounce back to /home, a dead CTA. Gate it. */}
            {t(isAdminUser ? "page.entities.emptyAdmin" : "page.entities.emptyUser")}{" "}
            <Link href="/about">{t("page.entities.aboutLink")}</Link>
          </>
        }
        action={
          isAdminUser && user?.isPlatformOperator ? (
            <Link href={studioHref(activeProjectSlug(), "dung/du-lieu")} className="slds-button slds-button_neutral">
              {t("page.entities.modelLink")}
            </Link>
          ) : undefined
        }
      />
    );
  } else {
    body = (
      <ul className="slds-grid slds-grid_pull-padded slds-wrap">
        {entities.map((entity, idx) => (
          <li key={entity.name} className="slds-p-horizontal_small slds-size_1-of-1 slds-medium-size_1-of-3">
            <Link href={`/${entity.name}`} className="slds-app-launcher__tile slds-text-link_reset">
              {/* Inside the Studio frame the coloured initials tile is dropped (specs/073 FR-005: no avatar chip per row). */}
              {!embedded && (
                <div className="slds-app-launcher__tile-figure" aria-hidden="true">
                  <span className="slds-avatar slds-avatar_large">
                    <abbr
                      className={`slds-avatar__initials slds-icon-custom-${appToneIcon(APP_TONES[idx % APP_TONES.length].key)}`}
                      title={entity.label}
                    >
                      {avatarInitials(entity.label)}
                    </abbr>
                  </span>
                </div>
              )}
              <div className="slds-app-launcher__tile-body slds-grid slds-grid_vertical-align-center min-w-0">
                <span className="slds-text-link slds-truncate" title={entity.label}>{entity.label}</span>
              </div>
            </Link>
          </li>
        ))}
      </ul>
    );
  }

  if (embedded) {
    // The frame is the one card and the page header: this screen is a lede line and the tiles.
    return (
      <div className="slds-p-around_medium">
        <p className="nx-lede slds-m-bottom_medium">{meta}</p>
        {body}
      </div>
    );
  }
  return (
    <div className="slds-p-around_medium flex min-h-0 flex-1 flex-col overflow-y-auto">
      <article className="slds-card">
        <PageHeader
          icon={{ glyph: LayoutGrid, name: "all" }}
          title={t("entities.title")}
          meta={meta}
          actions={entityHelp ? [<FieldHelp key="help" text={entityHelp.short} label={entityHelp.term} />] : undefined}
        />
        <div className="slds-card__body slds-card__body_inner slds-p-top_medium">{body}</div>
      </article>
    </div>
  );
}

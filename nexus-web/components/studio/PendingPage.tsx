import clsx from "clsx";
import Link from "next/link";
import { tf } from "@/lib/i18n";

export interface PendingPageProps {
  /** Name of the feature, already translated ("Cổng nhận dữ liệu"). */
  feature: string;
  /** Where "up" is: the parent page, or the old screen while this one is not built. */
  parent?: { label: string; href: string };
  /** `page` (default) is a whole working surface; `inline` sits inside a page that has its own padding. Same sentence. */
  variant?: "page" | "inline";
}

/**
 * A route that is declared in studioRoutes but not mounted yet (specs/073 FR-016): two left-aligned lines. Deliberately
 * not EmptyState (its 96px glyph, repeated on twenty pages, is a wall of grey pictures), not an icon, not a notification:
 * "not built yet" is a fact about the product, not a message about the user's work.
 */
export function PendingPage({ feature, parent, variant = "page" }: PendingPageProps) {
  return (
    <div className={clsx(variant === "page" && "slds-p-around_large")}>
      <p className="nx-lede">{tf("studio.pending.generic", { feature })}</p>
      {parent && (
        <p className="slds-m-top_small">
          <Link href={parent.href}>{tf("studio.pending.back", { parent: parent.label })}</Link>
        </p>
      )}
    </div>
  );
}

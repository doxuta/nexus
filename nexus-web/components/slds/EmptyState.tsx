// Empty state on the SLDS illustration frame (blueprint `illustration/base`, SLDS 2.264.1):
//   div.slds-illustration.slds-illustration_<small|large> > svg.slds-illustration__svg[aria-hidden]
//     + div.slds-text-longform > h3.slds-text-heading_medium + p.slds-text-body_regular
// The SLDS drawings (lake, desert, fishing…) are CC BY-ND and are NOT copied: the picture is a Lucide
// glyph in a neutral text colour. Its box is the illustration svg's own (SLDS: width 100% up to
// 300/600px); the glyph's `size` sets the height, and absoluteStrokeWidth keeps the line close to the
// SLDS drawings' ~2px stroke instead of scaling with the glyph.
// An optional action (button) goes under the text — SLDS docs: illustrations sit inside other
// components (cards, pages) and are accompanied by text describing the state.
// Embedded (specs/073 FR-005): compact and left-aligned, no 96/160px glyph: h3 16px/600, one 14px weak line, the action.
"use client";

import clsx from "clsx";
import { Inbox, type LucideIcon } from "lucide-react";
import type { ReactNode } from "react";
import { useEmbedded } from "./EmbeddedSurface";

export interface EmptyStateProps {
  title: string;
  description?: ReactNode;
  /** Default Inbox. */
  icon?: LucideIcon;
  /** Default "small" (cards, lists); "large" for a whole page. */
  size?: "small" | "large";
  /** Call to action under the text, e.g. a "Mới" button. */
  action?: ReactNode;
  className?: string;
  /** Studio build pages (demo A `.empty`): no glyph, no illustration frame, left-aligned text block + one action. */
  plain?: boolean;
}

const GLYPH_PX = { small: 96, large: 160 };

export function EmptyState({ title, description, icon: Glyph = Inbox, size = "small", action, className, plain }: EmptyStateProps) {
  const embedded = useEmbedded();
  if (embedded) {
    return (
      <div className={clsx("slds-p-vertical_medium", className)}>
        <h3 className="slds-text-heading_small">{title}</h3>
        {description != null && <p className="slds-text-body_regular slds-text-color_weak slds-m-top_xx-small">{description}</p>}
        {action && <div className="slds-m-top_small">{action}</div>}
      </div>
    );
  }
  if (plain) {
    return (
      <div className={className}>
        <div className="slds-text-longform">
          <h3 className="slds-text-heading_medium">{title}</h3>
          {description != null && <p className="slds-text-body_regular">{description}</p>}
        </div>
        {action && <div className="slds-m-top_small">{action}</div>}
      </div>
    );
  }
  return (
    <div className={clsx("slds-illustration", `slds-illustration_${size}`, className)}>
      <Glyph aria-hidden="true" size={GLYPH_PX[size]} strokeWidth={2} absoluteStrokeWidth className="slds-illustration__svg slds-text-color_weak" />
      <div className="slds-text-longform">
        <h3 className="slds-text-heading_medium">{title}</h3>
        {description != null && <p className="slds-text-body_regular">{description}</p>}
      </div>
      {action}
    </div>
  );
}

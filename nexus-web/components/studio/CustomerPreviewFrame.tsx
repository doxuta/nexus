// "Exactly the screen the customer sees" frame (specs/073 FR-007, UI-ARCH 4.2). `.nx-face-customer` undoes the
// Studio glue inside the subtree; the inline brand scope ALWAYS carries a value (brandScopeStyle(undefined) is
// {} and the frame would then inherit Studio's electric-blue while the real customer app shows #0176d3).
// Customer colour appears only inside this frame.
// The whole frame content sits in EmbeddedSurfaceReset (T104): inside a flattened work surface the preview still
// draws customer-face markup (cards, tablist, badges, avatars, illustrated empty states).
import type { CSSProperties, ReactNode } from "react";
import { EmbeddedSurfaceReset } from "@/components/slds/EmbeddedSurface";
import { brandScopeStyle, safeLogoUrl } from "@/lib/branding";
import { t } from "@/lib/i18n";

export const DEFAULT_CUSTOMER_ACCENT = "#0176d3";

export interface CustomerPreviewFrameProps {
  accentColor?: string;
  /** Omitted for a record frame: the bar then carries only the caption, never an invented app name or logo (specs/078 FR-055). */
  appName?: string;
  logoUrl?: string;
  caption?: string;
  children: ReactNode;
}

export function CustomerPreviewFrame({ accentColor, appName, logoUrl, caption, children }: CustomerPreviewFrameProps) {
  // empty or malformed accent falls back too, not just undefined (brandScopeStyle returns {} for both)
  let style = brandScopeStyle(accentColor);
  if (!Object.keys(style).length) style = brandScopeStyle(DEFAULT_CUSTOMER_ACCENT);
  // `contain: layout` makes the frame the containing block of position:fixed children, so the form template's
  // docked footer sits at the bottom of the frame instead of the viewport.
  style = { ...style, contain: "layout" };
  const logo = safeLogoUrl(logoUrl);
  return (
    <section data-testid="customer-preview-frame" className="nx-face-customer slds-border_top slds-border_right slds-border_bottom slds-border_left" style={style as CSSProperties}
      aria-label={t("studio.preview.frameLabel")}>
      <EmbeddedSurfaceReset>
      {/* Below 768px a nameless bar is dropped (nothing to say there but the caption, which the page already shows). */}
      <div className={`slds-grid slds-grid_vertical-align-center slds-p-horizontal_medium slds-p-vertical_x-small slds-border_bottom slds-theme_default${appName === undefined ? " max-md:hidden" : ""}`}>
        {/* eslint-disable-next-line @next/next/no-img-element -- customer logo is an allowlisted URL/data URI, not optimisable */}
        {logo && <img src={logo} alt="" className="slds-m-right_small" style={{ height: "1.5rem", width: "auto" }} />}
        {appName !== undefined && <span className="slds-text-heading_small slds-truncate">{appName}</span>}
        {caption && <span className={`slds-text-body_small slds-text-color_weak slds-truncate${appName === undefined ? "" : " slds-m-left_small"}`}>{caption}</span>}
      </div>
      <div className={appName === undefined ? "p-4 max-md:px-0" : "slds-p-around_medium"}>{children}</div>
      </EmbeddedSurfaceReset>
    </section>
  );
}

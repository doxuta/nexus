// SLDS tile (blueprint `tiles` base, SLDS 2.264.1):
//   <article class="slds-tile"><h3 class="slds-tile__title slds-truncate" title/><div class="slds-tile__detail"/></article>
import type { ReactNode } from "react";

export function Tile({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <article className="slds-tile">
      <h3 className="slds-tile__title slds-truncate" title={title}>{title}</h3>
      {children != null && <div className="slds-tile__detail">{children}</div>}
    </article>
  );
}

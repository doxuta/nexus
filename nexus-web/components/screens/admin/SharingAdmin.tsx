"use client";

// W7 §11.4 — Sharing / Ownership (admin), read-only surface.
// Shows: the sharing model (how role ACL + team-sets combine), live team-sets usage (who sees
// what and why), and an HONEST-DISABLED note for rule-based sharing — the advanced sharing-rule
// engine is a future slice (E3), so we say so rather than fake a rule builder.
// SLDS (specs/038 T048): page-headers/base · cards · data-tables/base (TeamSetsPanel) ·
// scoped-notifications (the roadmap note).

import { Info, Share2, UserCheck } from "lucide-react";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Card } from "@/components/slds/Card";
import { PageHeader } from "@/components/slds/PageHeader";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { TeamSetsPanel } from "@/components/screens/admin/TeamSetsPanel";

/** Renders a dictionary string with its `{code}` slot as inline code (column/enum names). */
function WithCode({ k, code }: { k: I18nKey; code: string }) {
  const [before, after] = t(k).split("{code}");
  return (
    <>
      {before}
      <code className="slds-text-font_monospace">{code}</code>
      {after}
    </>
  );
}

const ORDER: { k: I18nKey; code?: string }[] = [
  { k: "page.sharing.order.1" },
  { k: "page.sharing.order.2", code: "owner" },
  { k: "page.sharing.order.3", code: "team" },
  { k: "page.sharing.order.4", code: "all" },
  { k: "page.sharing.order.5", code: "field:<trường>" },
];

// The Studio page ("Chia sẻ") uses business words (S13): no tenant/ACL/engine/team set, no raw column codes.
export function SharingAdmin({ inline = false }: { inline?: boolean }) {
  const tx = (k: I18nKey, studioK: I18nKey): string => t(inline ? studioK : k);
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
      <PageHeader
        icon={{ glyph: Share2, name: "datashares" }}
        title={tx("page.sharing.title", "studio.build.access.sharing.title")}
        meta={tx("page.sharing.subtitle", "studio.build.access.sharing.subtitle")}
      />

      {/* mt-0: side by side, `.slds-card+.slds-card{margin-top:1rem}` would drop the second card. */}
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title={tx("page.sharing.model.title", "studio.build.access.sharing.model.title")} icon={{ glyph: Share2, name: "datashares" }} className="mt-0">
          <p>{inline ? t("studio.build.access.sharing.model.body") : <WithCode k="page.sharing.model.body" code="created_by" />}</p>
        </Card>
        <Card title={tx("page.sharing.order.title", "studio.build.access.sharing.order.title")} icon={{ glyph: Info, name: "entitlement-policy" }} className="mt-0">
          <ol className="slds-list_ordered">
            {ORDER.map((o, i) => (
              <li key={o.k}>
                {inline ? t(`studio.build.access.sharing.order.${i + 1}` as I18nKey) : o.code ? <WithCode k={o.k} code={o.code} /> : t(o.k)}
              </li>
            ))}
          </ol>
        </Card>
      </div>

      {/* Live team-sets usage: engine vocabulary, customer face only */}
      {!inline && <TeamSetsPanel />}

      {/* Honest-disabled: rule-based sharing */}
      <ScopedNotification theme="warning">
        <strong>{tx("page.sharing.rules.title", "studio.build.access.sharing.rules.title")}</strong> {tx("page.sharing.rules.body", "studio.build.access.sharing.rules.body")}
      </ScopedNotification>

      {/* Reassign ownership pointer */}
      <Card title={tx("page.sharing.owner.title", "studio.build.access.sharing.owner.title")} icon={{ glyph: UserCheck, name: "user" }}>
        <p>{tx("page.sharing.owner.body", "studio.build.access.sharing.owner.body")}</p>
      </Card>
    </div>
  );
}

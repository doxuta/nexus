"use client";

// HookPage (specs/074 lát f2-automation-hook-calendar): `dung/tu-dong/cong-nhan[/<id>]` — the project's inbound gates as a
// sub page of Tự động. The address follows the open gate (`cong-nhan/<id>`) so a gate can be linked and reloaded; the
// one-time key lives in InboundHookScreen state, so leaving the page loses it by design.
import { useRouter } from "next/navigation";
import { useDirectApplyGate } from "@/components/studio/useDirectApplyGate";
import { t } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";
import { InboundHookScreen } from "@/components/studio/InboundHookScreen";
import { SubPage } from "@/components/studio/AutomationPage";

export function HookPage({ slug, hookId }: { slug: string; hookId?: string }) {
  const router = useRouter();
  const applyGate = useDirectApplyGate(slug, "inbound_hook");
  return (
    <SubPage back={{ label: t("studio.build.automation.back"), href: studioHref(slug, "dung/tu-dong") }}>
      {applyGate.notice && <p className="slds-m-bottom_small">{applyGate.notice}</p>}
      {applyGate.confirm}
      <InboundHookScreen
        gate={applyGate.ask}
        initialHookId={hookId}
        onSelect={(id) => router.replace(id ? studioHref(slug, "dung/tu-dong/cong-nhan/:id", { id }) : studioHref(slug, "dung/tu-dong/cong-nhan"), { scroll: false })}
      />
    </SubPage>
  );
}

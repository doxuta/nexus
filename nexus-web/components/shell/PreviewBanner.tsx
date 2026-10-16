"use client";

// PreviewBanner — the strip above the customer app while THIS tab previews a role
// (specs/064-preview-as-role-sample-data S9-2). SLDS ScopedNotification (warning) + a "Thoát" button.
// It is the only thing that tells the builder they are not looking at their own session, so it is
// mounted by AppShell above the global header and cannot be dismissed — only left.
// Reads the tab's preview state (sessionStorage) through useSyncExternalStore: null on the server and
// on the first client render (no hydration mismatch), the real state right after.
import { useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { Eye } from "lucide-react";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { toast } from "@/components/ui/Toaster";
import { endPreviewSession, leavePreviewEnded, tenantSlug } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { usePermissions } from "@/lib/metadata/permissions";
import { clearDraftPreview, draftPreviewReturnTo, useDraftPreviewTab } from "@/lib/previewMode";
import { getPreview, PREVIEW_STORAGE_KEY } from "@/lib/previewSession";
import { workspaceTenantName } from "@/lib/workspace";

const noopSubscribe = () => () => {};
const readRaw = () => window.sessionStorage.getItem(PREVIEW_STORAGE_KEY);
const serverRaw = () => null;

/** Fired by lib/api.ts when the server answers 403 PREVIEW_READ_ONLY (a write attempted while previewing). */
export const PREVIEW_READ_ONLY_EVENT = "nexus:preview-read-only";

/**
 * The ONE preview strip of the customer app (specs/064 S9-2 role preview + specs/063 S7-3 draft
 * preview): a role preview wins when both flags are somehow set, it is the narrower view.
 */
export function PreviewBanner() {
  const draft = useDraftPreviewTab();
  return draft && !getPreview() ? <DraftPreviewBanner /> : <RolePreviewBanner />;
}

/**
 * "Bạn đang xem BẢN NHÁP — khách chưa thấy · [Thoát]" with the name of the workspace being viewed.
 * This tab previews the builder's unpublished drafts (X-Nexus-Draft on every request); the server
 * refuses every write from it (403 PREVIEW_READ_ONLY), so a refused write only needs one Toast here.
 */
function DraftPreviewBanner() {
  const name = workspaceTenantName() || tenantSlug();
  useEffect(() => {
    const onRefused = () => toast.info(t("draft.preview.readOnly"));
    window.addEventListener(PREVIEW_READ_ONLY_EVENT, onRefused);
    return () => window.removeEventListener(PREVIEW_READ_ONLY_EVENT, onRefused);
  }, []);

  function exit() {
    const to = draftPreviewReturnTo();
    clearDraftPreview();
    window.location.assign(to);
  }

  return (
    <div className="slds-grid slds-grid_vertical-align-center slds-p-around_x-small shrink-0 print:hidden slds-theme_warning">
      <ScopedNotification theme="warning" icon={Eye} className="slds-col slds-grow">
        {name ? tf("draft.preview.bannerOf", { name }) : t("draft.preview.banner")}
      </ScopedNotification>
      <Button variant="neutral" className="slds-m-left_small" onClick={exit}>
        {t("draft.preview.exit")}
      </Button>
    </div>
  );
}

function RolePreviewBanner() {
  const raw = useSyncExternalStore(noopSubscribe, readRaw, serverRaw);
  // `raw` is the dependency: getPreview() re-reads that same string.
  const preview = useMemo(() => (raw ? getPreview() : null), [raw]);
  const [now, setNow] = useState(() => Date.now());
  const [leaving, setLeaving] = useState(false);
  const active = preview !== null;
  // FR-017: the synthetic subject owns nothing, so a role scoped to "của tôi"/"đội" previews as empty lists.
  const { grants, isLoading: grantsLoading } = usePermissions();
  const ownerScoped = !grantsLoading && grants.some((g) => (g.action === "view" || g.action === "*") && (g.accessLevel === "owner" || g.accessLevel === "team"));
  const remainingMs = preview ? new Date(preview.expiresAt).getTime() - now : 1;

  useEffect(() => {
    if (!active) return;
    const id = window.setInterval(() => setNow(Date.now()), 15_000);
    return () => window.clearInterval(id);
  }, [active]);

  // Expired while the tab sat idle: nothing would send a request to notice, so leave by ourselves.
  useEffect(() => {
    if (active && remainingMs <= 0) leavePreviewEnded();
  }, [active, remainingMs]);

  // A write was refused (403 PREVIEW_READ_ONLY): say why, once, wherever in the app it happened.
  useEffect(() => {
    if (!active) return;
    const onRefused = () => toast.info(t("preview.readOnly"));
    window.addEventListener(PREVIEW_READ_ONLY_EVENT, onRefused);
    return () => window.removeEventListener(PREVIEW_READ_ONLY_EVENT, onRefused);
  }, [active]);

  if (!preview) return null;

  const minutes = Math.max(0, Math.floor(remainingMs / 60_000));
  const roleName = preview.role.label || preview.role.name;

  async function exit() {
    setLeaving(true);
    const to = await endPreviewSession("exit");
    window.location.assign(to);
  }

  return (
    <div className="slds-grid slds-grid_vertical-align-center slds-p-around_x-small shrink-0 print:hidden slds-theme_warning">
      <ScopedNotification theme="warning" icon={Eye} className="slds-col slds-grow">
        {preview.draft && <Badge variant="inverse" className="slds-m-right_x-small">{t("preview.banner.draft")}</Badge>}
        {tf("preview.banner.label", { role: roleName })} ·{" "}
        {minutes < 1 ? t("preview.banner.remainingLess") : tf("preview.banner.remaining", { n: minutes })}
        {ownerScoped && <> · {t("preview.banner.ownerHint")}</>}
      </ScopedNotification>
      <Button variant="neutral" className="slds-m-left_small" onClick={exit} disabled={leaving}>
        {t("preview.banner.exit")}
      </Button>
    </div>
  );
}

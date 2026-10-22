"use client";

// IndexFixNotice — "this group-by / sort field has no index yet → Bật index ngay", right under the
// picker (specs/070 #265). The builder used to have to leave the report / widget for Data Model,
// flip "Đánh index", and come back; now the fix is where the need is. It sends ONLY the flag
// (PUT /admin/fields/:id {indexed:true}: the server keeps every other option and any pending field
// draft, #56 — this page's own read of the options may be the live one) and refetches the entity's
// fields: the picker then sees the field as indexed and this notice disappears. Renders nothing for a
// field that is indexed already or cannot take an index (the picker keeps those disabled). Admin-only surfaces (report config
// panel, widget form); the server still gates the write (builderGated).
import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { updateField, type FieldMeta } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { qk } from "@/lib/query/keys";
import { isIndexFixable } from "@/lib/widgetAuthoring";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

export function IndexFixNotice({ entity, field }: { entity: string; field?: FieldMeta }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  if (!field?.id || !isIndexFixable(field)) return null;
  const { id } = field;
  const label = field.label ?? field.name;

  async function enable() {
    setBusy(true); setErr(null);
    try {
      await updateField(id, { indexed: true });
      // options.indexed applies live and never enters the field draft (DraftFieldRepo.SaveEdit); a pending field draft is
      // written back as it was, so the pending count holds: no emitDraftsChanged (#46).
      await qc.invalidateQueries({ queryKey: qk.fields(entity) });
    } catch (e) {
      setErr(e instanceof Error ? e.message : t("page.reports.config.indexError"));
    } finally {
      setBusy(false);
    }
  }

  // ScopedNotification wraps its children in a <p>: phrasing content only (spans + a button).
  return (
    <ScopedNotification theme="warning" className="mt-2">
      {tf("page.reports.config.indexMissing", { label })}
      <span className="block pt-2">
        <Button variant="neutral" onClick={() => void enable()} disabled={busy}>
          {busy ? t("page.reports.config.indexEnabling") : t("page.reports.config.indexEnable")}
        </Button>
      </span>
      {err && <span className="slds-text-color_error block pt-1">{err}</span>}
    </ScopedNotification>
  );
}

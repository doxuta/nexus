"use client";

// ListScreenEditor (specs/075 US5, L2b): edit the LIST of one table on the real screen. ScreenBar on top, the real list
// page (ListCanvas) in the middle, "Cột hiển thị" + "Kanban và Lịch" on the right (below on a narrow work surface).
// Lưu = POST /admin/layouts (viewType list) the first time, PUT /admin/layouts/:id after; "Đưa màn về màn tự sinh" =
// DELETE. A list layout carries no required-field flags, so the save is one write (lib/screenSave.saveListScreen).
import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Card } from "@/components/slds/Card";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { draftStateLabel } from "@/components/studio/StatusText";
import { useDirectApplyGate } from "@/components/studio/useDirectApplyGate";
import { usePublishState } from "@/components/studio/usePublishState";
import { toast } from "@/components/ui/Toaster";
import { deleteLayout, getTenantBrandingDoc, type EntityMeta, type FieldMeta } from "@/lib/api";
import { classOf } from "@/lib/draftClass";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { strictErrorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { useSetBlocked } from "@/lib/navigation-blocker";
import { listDraftEquals, listDraftFromView, listDraftToJson, type ListDraft } from "@/lib/screenEditorModel";
import { saveListScreen } from "@/lib/screenSave";
import { ListColumnsPanel } from "./ListColumnsPanel";
import { ListCanvas } from "./ListCanvas";
import { ScreenBar, screenLabel, type ScreenMan } from "./ScreenBar";
import { editViewOf, type ScreenData } from "./useScreenEditorData";
import { useSurfaceWidth } from "./useSurfaceWidth";
import { ViewFieldsPanel } from "./ViewFieldsPanel";

export interface ListScreenEditorProps {
  slug: string;
  entities: EntityMeta[];
  bang: string;
  /** `?truong=` carries the selected column here. */
  truong: string | null;
  onTruong: (column: string | null) => void;
  onPick: (bang: string, man: ScreenMan) => void;
  data: ScreenData;
  tableLabel: string;
  refetch: () => Promise<unknown>;
}

export function ListScreenEditor({ slug, entities, bang, truong, onTruong, onPick, data, tableLabel, refetch }: ListScreenEditorProps) {
  const { fields, records } = data;
  const { view, isDefault, layoutId } = useMemo(() => editViewOf(data.layout, "list"), [data.layout]);
  const initial = useMemo(() => listDraftFromView(view, fields), [view, fields]);
  const [draft, setDraft] = useState<ListDraft>(initial);
  const [saving, setSaving] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);
  const [gone, setGone] = useState(false);
  const [lockedMsg, setLockedMsg] = useState<string | null>(null);
  const [pending, setPending] = useState<{ bang: string; man: ScreenMan } | null>(null);
  const [resetAsk, setResetAsk] = useState(false);
  const { ref: surfaceRef, stacked } = useSurfaceWidth();
  const brand = useQuery({ queryKey: ["tenant-branding"], queryFn: getTenantBrandingDoc, staleTime: 60_000 });
  const publish = usePublishState(slug).data;
  const gate = useDirectApplyGate(slug, "layout", {
    message: tf("studio.build.screens.draft.applyNow", { what: t("studio.build.screens.draft.thisScreen") }),
    confirm: t("studio.build.screens.draft.applyNowGo"),
  });
  const draftLabel = classOf("layout", publish) === "draft" && view?.draftState ? (draftStateLabel("edited")?.label ?? null) : null;
  const setBlocked = useSetBlocked();

  const dirty = !listDraftEquals(draft, initial);
  useEffect(() => {
    setBlocked(dirty && !saving);
    return () => setBlocked(false);
  }, [dirty, saving, setBlocked]);

  const edit = (next: ListDraft) => { setDraft(next); setSaveErr(null); };
  const noCols = draft.cols.length === 0;
  const byName = useMemo(() => new Map<string, FieldMeta>(fields.map((f) => [f.name, f])), [fields]);
  const selected = truong && draft.cols.includes(truong) && byName.has(truong) ? truong : null;

  async function save(asNew = false) {
    if (noCols) return;
    setSaving(true);
    setSaveErr(null);
    const r = await saveListScreen({ entity: bang, layoutId, asNew, layoutJson: listDraftToJson(draft) });
    if (r.ok) {
      setGone(false);
      emitDraftsChanged("screen-save"); // a pending change on a delivered project: the header recounts
      await refetch(); // new dataUpdatedAt remounts this editor on the saved state
      toast.success(t("studio.build.screens.status.ok"));
      return;
    }
    const err = r.error as { code?: string; details?: { retryAfter?: unknown } } | null;
    if (r.layoutGone) setGone(true);
    else if (err?.code === "TENANT_MAINTENANCE") {
      const sec = err.details?.retryAfter;
      const at = typeof sec === "number" ? new Date(Date.now() + sec * 1000) : null;
      setLockedMsg(at ? tf("studio.build.screens.bar.locked", { time: at.toLocaleTimeString("vi-VN", { hour: "2-digit", minute: "2-digit" }) }) : t("studio.build.screens.bar.lockedNoTime"));
    } else setSaveErr(strictErrorMessage(r.error));
    setSaving(false);
  }

  async function reset() {
    if (!layoutId) return;
    setSaving(true);
    try {
      await deleteLayout(layoutId);
      emitDraftsChanged("screen-reset");
      setResetAsk(false);
      await refetch();
    } catch (e) {
      setSaveErr(strictErrorMessage(e));
      setSaving(false);
    }
  }

  const pick = (b: string, m: ScreenMan) => {
    if (b === bang && m === "danh-sach") return;
    if (dirty) setPending({ bang: b, man: m });
    else onPick(b, m);
  };
  const pendingEntity = pending ? entities.find((e) => e.name === pending.bang) : undefined;

  return (
    <div ref={surfaceRef}>
      <ScreenBar
        entities={entities}
        bang={bang}
        man="danh-sach"
        missing={null}
        dirty={dirty}
        saving={saving}
        locked={lockedMsg != null || noCols}
        canReset={layoutId != null}
        draftLabel={draftLabel}
        onPick={pick}
        onSave={() => gate.ask(() => void save())}
        onDiscard={() => { setDraft(initial); setSaveErr(null); setGone(false); }}
        onReset={() => setResetAsk(true)}
      />
      {pending && (
        <InlineConfirm
          tone="warning"
          message={tf("studio.build.screens.bar.leaveAsk", { screen: screenLabel(pendingEntity ?? { name: pending.bang }, pending.man) })}
          confirmLabel={t("studio.build.screens.bar.leaveGo")}
          cancelLabel={t("studio.build.screens.bar.keep")}
          onConfirm={() => { const p = pending; setPending(null); onPick(p.bang, p.man); }}
          onCancel={() => setPending(null)}
        />
      )}
      {resetAsk && (
        <InlineConfirm
          tone="error"
          message={tf("studio.build.screens.bar.resetAsk", { bang: tableLabel })}
          confirmLabel={t("studio.build.screens.bar.resetGo")}
          onConfirm={() => void reset()}
          onCancel={() => setResetAsk(false)}
          busy={saving}
        />
      )}
      {gate.confirm}
      {lockedMsg && <ScopedNotification theme="warning">{lockedMsg}</ScopedNotification>}
      {saveErr && <ScopedNotification theme="error">{saveErr}</ScopedNotification>}
      {gone && (
        <ScopedNotification theme="warning">
          <span>{t("studio.build.screens.error.gone")} </span>
          <Button variant="neutral" onClick={() => void save(true)}>{t("studio.build.screens.error.saveAsNew")}</Button>
        </ScopedNotification>
      )}
      {isDefault && !dirty && <p className="slds-text-color_weak slds-m-bottom_small">{t("studio.build.screens.bar.autoNote")}</p>}
      <div className={stacked ? "grid grid-cols-1 gap-4" : "grid gap-4"} style={stacked ? undefined : { gridTemplateColumns: "minmax(0,1fr) 300px" }}>
        <div className="slds-theme_shade slds-p-around_medium" data-testid="screen-stage">
          <ListCanvas
            tableLabel={tableLabel}
            fields={fields}
            draft={draft}
            records={records}
            appName={brand.data?.branding.appName || t("studio.build.screens.stage.appFallback")}
            accentColor={brand.data?.branding.accentColor}
            logoUrl={brand.data?.branding.logoUrl}
            selected={selected}
            onSelect={(c) => onTruong(c === selected ? null : c)}
          />
        </div>
        <Card title={t("studio.build.screens.list.panelTitle")} className="slds-m-bottom_none">
          <ListColumnsPanel fields={fields} draft={draft} selected={selected} onSelect={onTruong} onChange={edit} />
          <div className="slds-m-top_large">
            <ViewFieldsPanel fields={fields} draft={draft} onChange={edit} />
          </div>
        </Card>
      </div>
    </div>
  );
}

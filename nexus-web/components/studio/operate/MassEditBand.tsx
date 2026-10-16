"use client";

// MassEditBand — mass update IN the list page (specs/078 FR-054), the Studio form of MassEditModal: pick one
// field, give one value, "Áp dụng cho {n} bản ghi". Same value semantics as the modal: an empty value is NOT
// submitted (never an accident), blanking is its own switch with a second click that states the size.
// Chunking (≤ 500 ids per call) and the scope gate before EVERY chunk live in lib/api bulkUpdateRecords.
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import {
  ApiError,
  MassOpStoppedError,
  bulkUpdateRecords,
  ensureAuth,
  type FieldMeta,
  type MassOpGuard,
  type MassOpResult,
} from "@/lib/api";
import { t } from "@/lib/i18n";
import { formatSaveError } from "@/lib/saveErrors";
import { buildPayload, emptyDefault, READONLY_FIELD_TYPES, SYSTEM_FIELDS } from "@/lib/recordPayload";
import { metaToSpec } from "@/lib/fieldSpec";
import { FieldInput, FieldRow } from "@/components/fields/widgets";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";

export interface MassEditBandProps {
  entity: string;
  fields: FieldMeta[];
  /** Snapshot of the selection taken when the band opened. */
  ids: string[];
  guard?: MassOpGuard;
  /** Fired with whatever landed (also after a stopped loop) so the list can refetch. */
  onApplied?: (result: MassOpResult) => void;
  onClose: () => void;
}

const fill = (key: Parameters<typeof t>[0], values: Record<string, string | number>) =>
  t(key).replace(/\{(\w+)\}/g, (m, k: string) => (k in values ? String(values[k]) : m));

export function MassEditBand({ entity, fields, ids, guard, onApplied, onClose }: MassEditBandProps) {
  // Same filter as the modal: a locked (field-ACL) field offered here would cost the whole batch FIELD_FORBIDDEN.
  const editable = useMemo(
    () =>
      fields.filter(
        (f) => !SYSTEM_FIELDS.has(f.name) && !READONLY_FIELD_TYPES.has(f.fieldType) && !f.readOnly && f.fieldType !== "file" && f.fieldType !== "image",
      ),
    [fields],
  );
  const [name, setName] = useState("");
  const [value, setValue] = useState<unknown>(undefined);
  const [clear, setClear] = useState(false);
  const [armed, setArmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<{ tone: "success" | "warning" | "error"; text: string } | null>(null);
  const first = useRef<HTMLSelectElement>(null);
  useEffect(() => first.current?.focus(), []);

  const meta = editable.find((f) => f.name === name);
  const n = ids.length;
  const payload = useMemo(() => {
    if (!meta) return {};
    if (clear) return { [meta.name]: null };
    return buildPayload([meta], { [meta.name]: value });
  }, [meta, value, clear]);
  const canApply = !busy && !!meta && Object.keys(payload).length > 0;

  const pick = (next: string) => {
    const f = editable.find((x) => x.name === next);
    setName(next);
    setValue(f ? emptyDefault(f) : undefined);
    setClear(false);
    setArmed(false);
    setNote(null);
  };

  const apply = async () => {
    if (clear && !armed) {
      setArmed(true);
      return;
    }
    setBusy(true);
    setNote(null);
    try {
      await ensureAuth();
      const res = await bulkUpdateRecords(entity, ids, payload, guard);
      onApplied?.(res);
      setArmed(false);
      setNote(
        res.failed > 0
          ? { tone: "warning", text: fill("studio.operate.data.mass.partial", { ok: res.succeeded, failed: res.failed, msg: formatSaveError(res.results.find((r) => !r.ok)) }) }
          : { tone: "success", text: fill("studio.operate.data.mass.done", { ok: res.succeeded }) },
      );
    } catch (e) {
      if (e instanceof MassOpStoppedError) {
        onApplied?.(e.partial);
        setNote({ tone: "error", text: fill("studio.operate.data.mass.stopped", { ok: e.partial.succeeded, n: e.total }) });
      } else if (e instanceof ApiError) {
        setNote({ tone: "error", text: e.status === 403 ? t("studio.operate.data.mass.forbidden") : e.message });
      } else {
        setNote({ tone: "error", text: t("studio.operate.data.mass.connError") });
      }
    } finally {
      setBusy(false);
    }
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Escape" && !busy) {
      e.stopPropagation();
      onClose();
    }
  };

  const applyLabel = clear
    ? fill(armed ? "studio.operate.data.mass.confirmClear" : "studio.operate.data.mass.applyClear", { n })
    : fill("studio.operate.data.mass.apply", { n });

  return (
    <div role="group" aria-label={fill("studio.operate.data.mass.edit", { n })} className="slds-box slds-box_x-small slds-m-around_x-small" onKeyDown={onKeyDown}>
      <div className="slds-grid slds-wrap slds-grid_vertical-align-end">
        <div className="slds-m-right_small">
          <Select
            ref={first}
            label={t("studio.operate.data.mass.field")}
            placeholder={t("studio.operate.data.mass.fieldPick")}
            value={name}
            onChange={(e) => pick(e.target.value)}
            options={editable.map((f) => ({ value: f.name, label: f.label ?? f.name }))}
            disabled={busy}
          />
        </div>
        {meta && (
          <div className="slds-m-right_small">
            {/* A blanked field's input is disabled, not hidden: the switch still shows which field it applies to. */}
            <fieldset disabled={clear || busy}>
              <FieldRow field={metaToSpec(meta)}>
                <FieldInput field={metaToSpec(meta)} value={value} onChange={(v) => { setValue(v); setNote(null); }} />
              </FieldRow>
            </fieldset>
            <Checkbox
              label={t("studio.operate.data.mass.clear")}
              checked={clear}
              disabled={busy}
              onChange={(e) => { setClear(e.target.checked); setArmed(false); setNote(null); }}
            />
          </div>
        )}
        <Button variant={clear ? "destructive" : "brand"} onClick={() => void apply()} disabled={!canApply}>
          {applyLabel}
        </Button>
        <Button className="slds-m-left_x-small" onClick={onClose} disabled={busy}>
          {t("studio.operate.data.mass.cancel")}
        </Button>
      </div>
      {meta && <p className="slds-text-color_weak slds-m-top_x-small">{fill(clear ? "studio.operate.data.mass.visibleClear" : "studio.operate.data.mass.visible", { n })}</p>}
      <div aria-live="polite">{note && <ScopedNotification theme={note.tone} className="slds-m-top_x-small">{note.text}</ScopedNotification>}</div>
    </div>
  );
}

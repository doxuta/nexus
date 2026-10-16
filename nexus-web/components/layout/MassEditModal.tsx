"use client";

// MassEditModal — mass update from the list view (ENG-MASSOP, ../specs/022-eng-massop/spec.md) on
// SLDS 2.264.1 (specs/038 T022): modals/base (medium) with the field picker as a menus/dropdown,
// each picked field in an slds-box, scoped notifications for the warning / request error / result.
// Pick 1..n editable fields, enter ONE shared value per field, apply to every selected record via
// POST /:entity/bulk-update. The server runs each row through the full single-record Update path
// (ACL, field-ACL, validation, workflow, audit), so per-row failures (RECORD_DUPLICATE,
// RECORD_NOT_FOUND, …) come back in results[] and are listed here without blocking the rows that
// succeeded.
//
// Value semantics, and they are NOT symmetric — that asymmetry is the whole design:
//   • a field left EMPTY is simply not submitted (buildPayload drops empty optionals), so
//     "I left it blank" means "don't touch it" — the safe reading of an accident;
//   • blanking a field is therefore a SEPARATE, deliberate act: the per-field "Xoá trống" switch
//     sends an explicit `null`, which the single-record Update path (which every bulk row runs
//     through) treats as "clear this value".
// Blanking N records has no undo, so it is gated twice: the switch itself, then a second click on
// a confirm button that states the row×field count out loud.

import { Plus, X } from "lucide-react";
import { useMemo, useState } from "react";
import {
  ensureAuth,
  bulkUpdateRecords,
  ApiError,
  type FieldMeta,
  type MassOpResult,
} from "@/lib/api";
import { t } from "@/lib/i18n";
import { formatSaveError } from "@/lib/saveErrors";
import { buildPayload, emptyDefault, SYSTEM_FIELDS, READONLY_FIELD_TYPES } from "@/lib/recordPayload";
import { metaToSpec } from "@/lib/fieldSpec";
import { FieldRow, FieldInput } from "@/components/fields/widgets";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Checkbox } from "@/components/slds/Checkbox";
import { Menu, MenuItem } from "@/components/slds/Menu";
import { Modal } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

export interface MassEditModalProps {
  entity: string;
  entityLabel: string;
  fields: FieldMeta[];
  ids: string[];
  open: boolean;
  onClose: () => void;
  // Fired once per successful batch call so the list can refetch + clear selection. The modal
  // stays open to show the per-row outcome; onClose dismisses it.
  onApplied?: (result: MassOpResult) => void;
  /** Focus target on close when the flow removed the button that opened the modal (kit Modal). */
  returnFocus?: () => HTMLElement | null | undefined;
}

const fill = (key: Parameters<typeof t>[0], values: Record<string, string | number>) =>
  t(key).replace(/\{(\w+)\}/g, (m, k: string) => (k in values ? String(values[k]) : m));

export function MassEditModal({ entity, entityLabel, fields, ids, open, onClose, onApplied, returnFocus }: MassEditModalProps) {
  // Fields a user may mass-set: declared, writable, and enterable in a form widget.
  // file/image are excluded — their widgets are upload stubs, not shared-value inputs.
  // `readOnly` (queue #12/#13) is the field-ACL lock for THIS caller's role. It must be filtered
  // here and not merely rendered as disabled: CheckWriteFields refuses on the PRESENCE of a locked
  // key, so a locked field the picker offers costs the user the whole batch with FIELD_FORBIDDEN.
  const editableFields = useMemo(
    () =>
      fields.filter(
        (f) =>
          !SYSTEM_FIELDS.has(f.name) &&
          !READONLY_FIELD_TYPES.has(f.fieldType) &&
          !f.readOnly &&
          f.fieldType !== "file" &&
          f.fieldType !== "image",
      ),
    [fields],
  );

  const [picked, setPicked] = useState<string[]>([]);
  const [values, setValues] = useState<Record<string, unknown>>({});
  // Fields the user asked to BLANK (explicit null), kept apart from `values` on purpose: "no
  // value" and "clear the value" are different intents and collapsing them into one empty state
  // is how a blanking becomes something you do by accident.
  const [cleared, setCleared] = useState<string[]>([]);
  // Second gate for the irreversible half — armed only while at least one field is being blanked.
  const [confirmClear, setConfirmClear] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [submitErr, setSubmitErr] = useState("");
  const [result, setResult] = useState<MassOpResult | null>(null);

  const byName = useMemo(() => new Map(editableFields.map((f) => [f.name, f])), [editableFields]);
  const remaining = editableFields.filter((f) => !picked.includes(f.name));
  const pickedMetas = picked.map((n) => byName.get(n)).filter((f): f is FieldMeta => !!f);
  // What would actually be sent — buildPayload drops empty optionals, so an all-empty form
  // disables submit instead of 422 BULK_VALUES_EMPTY.
  const payload = useMemo(() => {
    // Explicit nulls go on AFTER buildPayload, which would otherwise drop them as "empty".
    const base = buildPayload(pickedMetas.filter((f) => !cleared.includes(f.name)), values);
    for (const name of cleared) base[name] = null;
    return base;
  }, [pickedMetas, values, cleared]);
  const clearingCount = pickedMetas.filter((f) => cleared.includes(f.name)).length;
  const canSubmit = !submitting && ids.length > 0 && Object.keys(payload).length > 0;

  if (!open) return null;

  const addField = (name: string) => {
    const f = byName.get(name);
    if (!f) return;
    setPicked((prev) => [...prev, name]);
    setValues((prev) => ({ ...prev, [name]: emptyDefault(f) }));
    setResult(null);
    setSubmitErr("");
  };

  const removeField = (name: string) => {
    setPicked((prev) => prev.filter((n) => n !== name));
    setCleared((prev) => prev.filter((n) => n !== name));
    setConfirmClear(false);
    setValues((prev) => {
      const next = { ...prev };
      delete next[name];
      return next;
    });
    setResult(null);
  };

  const handleApply = async () => {
    setSubmitting(true);
    setSubmitErr("");
    setResult(null);
    try {
      await ensureAuth();
      const res = await bulkUpdateRecords(entity, ids, payload);
      setResult(res);
      setConfirmClear(false);
      onApplied?.(res);
    } catch (e) {
      setSubmitErr(e instanceof ApiError ? e.message : e instanceof Error ? e.message : t("page.list.massEdit.connError"));
    } finally {
      setSubmitting(false);
    }
  };

  const failedRows = result?.results.filter((r) => !r.ok) ?? [];
  const n = ids.length;

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      returnFocus={returnFocus}
      size="medium"
      title={fill("page.list.massEdit.title", { entity: entityLabel })}
      tagline={fill("page.list.massEdit.count", { n })}
      footer={
        <div className="slds-grid slds-grid_vertical-align-center">
          {/* Nút xám mà không nói vì sao là một ngõ cụt: người dùng đã chọn trường, đã thấy nút,
              và không có gì giải thích. Chỉ hiện khi ĐÃ chọn trường nhưng chưa có gì để ghi. */}
          <div className="slds-col_bump-right slds-text-align_left">
            {picked.length > 0 && Object.keys(payload).length === 0 && (
              <p className="slds-text-color_weak">{t("page.list.massEdit.nothing")}</p>
            )}
            {/* The irreversible half gets a second click, and the second click states the SIZE of
                what it does. A blanking cannot be undone from the product, and the trail is what
                would reconstruct it — so this button never appears with a vague label. */}
            {clearingCount > 0 && !result && (
              <p className="slds-text-color_error">{fill("page.list.massEdit.clearScale", { fields: clearingCount, n })}</p>
            )}
          </div>
          <Button onClick={onClose}>{t(result ? "page.list.massEdit.close" : "action.cancel")}</Button>
          <Button
            variant={clearingCount > 0 ? "destructive" : "brand"}
            onClick={() => {
              if (clearingCount > 0 && !confirmClear) {
                setConfirmClear(true);
                return;
              }
              void handleApply();
            }}
            disabled={!canSubmit}
          >
            {clearingCount > 0 && confirmClear
              ? fill("page.list.massEdit.confirmClear", { n })
              : clearingCount > 0
                ? fill("page.list.massEdit.applyAndClear", { n })
                : fill("page.list.massEdit.apply", { n })}
          </Button>
        </div>
      }
    >
      <ScopedNotification theme="warning">
        {t("page.list.massEdit.warnLead")} <strong>{fill("page.list.massEdit.warnAll", { n })}</strong>
        {t("page.list.massEdit.warnTail")}
      </ScopedNotification>

      {pickedMetas.map((f) => {
        const isCleared = cleared.includes(f.name);
        return (
          <div key={f.name} className="slds-box slds-box_x-small slds-m-top_small">
            <div className="slds-grid slds-grid_align-spread slds-grid_vertical-align-start">
              {/* A blanked field's input is disabled (native fieldset), not hidden: the user still
                  sees which field the switch applies to. */}
              <fieldset className="slds-col" disabled={isCleared}>
                <FieldRow field={metaToSpec(f)}>
                  <FieldInput
                    field={metaToSpec(f)}
                    value={values[f.name]}
                    onChange={(v) => { setValues((prev) => ({ ...prev, [f.name]: v })); setResult(null); }}
                  />
                </FieldRow>
              </fieldset>
              <ButtonIcon
                icon={X}
                size="small"
                className="slds-m-left_x-small"
                label={fill("page.list.massEdit.removeField", { field: f.label ?? f.name })}
                onClick={() => removeField(f.name)}
              />
            </div>

            {/* The blank switch. Deliberately NOT a magic empty value: leaving the input blank
                means "don't touch this field", and one gesture must not mean two opposite
                things. Toggling it disarms the confirm below, so arming can never be inherited
                from a previous, different selection. */}
            <div className="slds-m-top_x-small">
              <Checkbox
                label={t("page.list.massEdit.clearSwitch")}
                checked={isCleared}
                onChange={(e) => {
                  setCleared((prev) => (e.target.checked ? [...prev, f.name] : prev.filter((x) => x !== f.name)));
                  setConfirmClear(false);
                  setResult(null);
                }}
              />
            </div>
            {/* Chọn một trường rồi để trống KHÔNG có tác dụng gì — payload bỏ hẳn khoá đó. Trước
                đây chuyện này diễn ra im lặng, và im lặng theo kiểu khó ngờ nhất: ô CHỮ để trống
                thì bị bỏ qua, còn ô Đúng/Sai hay Đánh giá chỉ cần CHỌN là đã ghi `false`/`0` lên
                mọi bản ghi (emptyDefault seed sẵn giá trị đó, và `false` không phải "rỗng"). Cùng
                một cử chỉ, hai kết quả trái ngược, không một lời giải thích. Nay mỗi trường tự
                nói nó sẽ làm gì — đọc TỪ CHÍNH `payload` sắp gửi, không tính lại điều kiện rỗng ở
                chỗ thứ hai (hai lần tính là hai lần trôi khỏi nhau). */}
            {isCleared ? (
              <p className="slds-text-color_error slds-m-top_xx-small">
                {t("page.list.massEdit.willClearLead")} <strong>{n}</strong> {t("page.list.massEdit.willClearTail")}
              </p>
            ) : !(f.name in payload) ? (
              <p className="slds-text-color_weak slds-m-top_xx-small">
                {t("page.list.massEdit.blankLead")} <strong>{t("page.list.massEdit.blankNot")}</strong> {t("page.list.massEdit.blankTail")}
              </p>
            ) : (
              <p className="slds-text-color_weak slds-m-top_xx-small">
                {t("page.list.massEdit.willWriteLead")} <strong>{n}</strong> {t("page.list.massEdit.willWriteTail")}
              </p>
            )}
          </div>
        );
      })}

      {/* Add-field picker (menus/dropdown) */}
      <div className="slds-m-top_small">
        <Menu
          length={7}
          trigger={
            <Button iconLeft={Plus} disabled={remaining.length === 0}>
              {t("page.list.massEdit.addField")}
            </Button>
          }
        >
          {remaining.map((f) => (
            <MenuItem key={f.name} label={f.label ?? f.name} onSelect={() => addField(f.name)} />
          ))}
        </Menu>
      </div>

      {/* Submit error (request-level) */}
      {submitErr && (
        <ScopedNotification theme="error" className="slds-m-top_small">
          {submitErr}
        </ScopedNotification>
      )}

      {/* Result — per-row outcome after a batch ran */}
      {result && (
        <div className="slds-m-top_small">
          <ScopedNotification theme={result.failed > 0 ? "warning" : "success"}>
            {fill("page.list.massEdit.result", { ok: result.succeeded, total: result.succeeded + result.failed })}
            {result.failed > 0 && ` · ${fill("page.list.massEdit.resultFailed", { n: result.failed })}`}
          </ScopedNotification>
          {failedRows.length > 0 && (
            <ul className="slds-has-dividers_bottom-space slds-scrollable_y max-h-44">
              {failedRows.map((r) => {
                // The row's sentence (a before-save rule's own message, a mapped code, or the
                // server's text) — never the error code: that is engine vocabulary (specs/048 FR-028).
                const msg = formatSaveError(r);
                return (
                  <li key={r.id} className="slds-item slds-grid slds-grid_vertical-align-center">
                    <span className="slds-text-color_weak slds-m-right_x-small">{r.id.slice(0, 8)}…</span>
                    <span className="slds-truncate slds-text-color_error" title={msg}>{msg}</span>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      )}
    </Modal>
  );
}

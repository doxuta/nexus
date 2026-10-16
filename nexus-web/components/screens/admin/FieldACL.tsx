"use client";

// W7 §11.1 — Field-level permissions matrix editor (headline).
// Rows = fields of the selected entity (field_order), columns = roles. Each cell is a tri-state
// choice: Sửa (mặc định: hiện + sửa) · Chỉ xem · Ẩn. Loads from GET /admin/field-acl?entity=; saves
// via PUT with EVERY CHANGED cell explicit (upsert-by-key — a cleared restriction sends (true,true)
// so the backend deletes the row; unchanged cells are absent and kept). Pure model in lib/fieldAcl.ts.
//
// SLDS (specs/038 T048): page-headers/base · scoped-notifications · cards · combobox (entity) ·
// data-tables/base with a radio-button-group per cell (RESEARCH §5.2: "ma trận: ghép"). The field
// column is pinned while the role columns scroll; slds-theme_default gives the pinned cell the
// table's white so scrolled cells do not show through it. The table sizes to its content (w-max):
// at 100% width SLDS squeezes a dozen role columns until "Chỉ xem" wraps inside its radio button.
//
// Dead-end locks (queue #146): a Chỉ xem/Ẩn cell on a required field the engine does not fill takes
// the radio-button-group error state, and a warning scoped notification lists every such cell with
// the way out — rows from before the PutGrants guard are visible here, not only at the 422.

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import clsx from "clsx";
import { CheckCircle2, EyeOff, Lock, Pencil, Save, Sigma, TriangleAlert, X } from "lucide-react";
import { listEntities, listFields, listRoles, ensureAuth, ApiError, type EntityMeta, type FieldMeta, type RoleSummary } from "@/lib/api";
import { getFieldAclData, putFieldAcl } from "@/lib/admin";
import {
  buildMatrix, getCellState, setCellState, changedKeys, isDirty,
  buildPutPayload, cellKey, restrictionCountForField,
  studioCanSetDefault, deadEndRoleIds,
  type AclMatrix, type CellState,
} from "@/lib/fieldAcl";
import { FIELD_TYPE_META, type FieldType } from "@/lib/metadata";
import { getHelp } from "@/lib/help-content";
import { t, tf } from "@/lib/i18n";
import { useEntityOptions } from "@/lib/entityOptions";
import type { I18nKey } from "@/lib/i18n/vi";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { Combobox } from "@/components/slds/Combobox";
import { Icon } from "@/components/slds/Icon";
import { PageHeader } from "@/components/slds/PageHeader";
import { RadioButtonGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { useWorkshopScope } from "@/lib/workshopScope";
import { FieldHelp } from "@/components/fields/widgets";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { useDirectApply } from "@/components/shell/useDirectApply";

// Computed field types — hiding these (rollup/lookup/formula/auto_number) is legitimate.
const COMPUTED_TYPES = new Set(["rollup", "lookup", "formula", "auto_number"]);

const STATES: { value: CellState; icon: typeof Lock }[] = [
  { value: "default", icon: Pencil },
  { value: "readonly", icon: Lock },
  { value: "hidden", icon: EyeOff },
];
const stateLabel = (s: CellState) => t(`page.fieldAcl.state.${s}` as I18nKey);
const aclHelp = getHelp("aclFieldLevel");

/**
 * `entity`/`field` (specs/073 kh3, 074 T206): the Studio route hands the pair over; absent = the old one-shot
 * ?entity=&field= read. With `onChange` the URL is the source of truth: picking a table reports (entity, undefined)
 * and a new `entity` prop (Back button) switches the table.
 */
export function FieldACL({ entity: entityProp, field: fieldProp, onChange, inlineApply }: { entity?: string; field?: string; onChange?: (entity: string, field?: string) => void; /** Studio: ask "Áp ngay" in the page instead of a Modal. */ inlineApply?: boolean } = {}) {
  const workshop = useWorkshopScope();
  const { guard, dialog, notice } = useDirectApply({ inline: !!inlineApply });
  // STUDIO-DISCOVERABILITY 1.4a: /admin/field-acl?entity=X&field=Y deep-link (from the Data Model
  // builder's Field Inspector) preselects the entity and highlights+scrolls to the field row.
  // Read once from the URL, SSR-safe lazy initializer (same idiom as LayoutForm's ?pf_ prefill).
  const [urlPreselect] = useState<{ entity: string | null; field: string | null }>(() => {
    if (typeof window === "undefined") return { entity: null, field: null };
    if (entityProp !== undefined || fieldProp !== undefined) return { entity: entityProp ?? null, field: fieldProp ?? null };
    const sp = new URLSearchParams(window.location.search);
    return { entity: sp.get("entity"), field: sp.get("field") };
  });

  const [entities, setEntities] = useState<EntityMeta[]>([]);
  const [roles, setRoles] = useState<RoleSummary[]>([]);
  const [entity, setEntity] = useState<string>("");
  const [fields, setFields] = useState<FieldMeta[]>([]);
  // queue #306: the server's verdict (requiredEngineCannotFill) — not re-derived from field types here.
  const [deadEndFields, setDeadEndFields] = useState<Set<string>>(new Set());

  const [draft, setDraft] = useState<AclMatrix>({});
  const [saved, setSaved] = useState<AclMatrix>({});

  const [bootLoading, setBootLoading] = useState(true);
  const [gridLoading, setGridLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [bootErr, setBootErr] = useState<string | null>(null);
  const [banner, setBanner] = useState<string | null>(null);
  const [savedOk, setSavedOk] = useState(false);
  const [errorCell, setErrorCell] = useState<string | null>(null);

  // Guard unsaved edits when switching entity.
  const [pendingEntity, setPendingEntity] = useState<string | null>(null);

  const dirty = useMemo(() => isDirty(draft, saved), [draft, saved]);
  const changeCount = useMemo(() => changedKeys(draft, saved).length, [draft, saved]);

  // Load fields + grants for the selected entity.
  const loadEntity = useCallback(async (name: string) => {
    setGridLoading(true);
    setBanner(null);
    setErrorCell(null);
    setSavedOk(false);
    try {
      await ensureAuth();
      const [fs, acl] = await Promise.all([listFields(name), getFieldAclData(name)]);
      const m = buildMatrix(acl.grants);
      setDeadEndFields(new Set(acl.deadEndIfLocked ?? []));
      setFields(fs);
      setSaved(m);
      setDraft(m);
    } catch (e) {
      setBanner(e instanceof ApiError ? e.message : t("page.fieldAcl.loadError"));
      setFields([]);
      setDeadEndFields(new Set());
      setSaved({});
      setDraft({});
    } finally {
      setGridLoading(false);
    }
  }, []);

  // Content key, not the Set identity: a nav reload that yields the same tables must not re-run the boot
  // effect (it resets the picked table and drops unsaved edits).
  const namesKey = workshop?.entityNames ? [...workshop.entityNames].sort().join(",") : null;

  // Boot: entities + roles.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        await ensureAuth();
        const [es, rs] = await Promise.all([listEntities(), listRoles()]);
        if (cancelled) return;
        // Inside an app workshop (#302) the table picker offers the app's tables (plus a deep-linked one).
        const names = namesKey === null ? null : new Set(namesKey.split(","));
        const mine = names ? es.filter((e) => names.has(e.name) || e.name === urlPreselect.entity) : es;
        setEntities(mine);
        setRoles(rs);
        // ?entity= wins when it names a real entity; else first entity as before.
        const preset = urlPreselect.entity && mine.some((e) => e.name === urlPreselect.entity)
          ? urlPreselect.entity
          : mine[0]?.name;
        if (preset) {
          setEntity(preset);
          loadEntity(preset);
        }
      } catch (e) {
        if (!cancelled) setBootErr(e instanceof ApiError ? e.message : t("page.fieldAcl.bootError"));
      } finally {
        if (!cancelled) setBootLoading(false);
      }
    })();
    return () => { cancelled = true; };
  }, [urlPreselect.entity, loadEntity, namesKey]);


  // Selecting an entity loads it in the same event (no effect → no cascading render).
  const selectEntity = (name: string, fromUrl = false) => {
    setEntity(name);
    loadEntity(name);
    if (!fromUrl) onChange?.(name, undefined);
  };

  // The URL moved (Back/Forward) to a table other than the one on screen: follow it. With unsaved edits the
  // question is asked in the page (same InlineConfirm as picking a table); cancelling reports the shown table back
  // so the address bar never lies. Handled once per new prop value; waits until the table list is loaded.
  const handledProp = useRef(entityProp);
  /* eslint-disable react-hooks/set-state-in-effect, react-hooks/exhaustive-deps */
  useEffect(() => {
    if (!onChange || entityProp === handledProp.current) return;
    if (entity === "" || entities.length === 0) return;
    handledProp.current = entityProp;
    if (!entityProp || entityProp === entity || !entities.some((e) => e.name === entityProp)) return;
    if (dirty) setPendingEntity(entityProp);
    else selectEntity(entityProp, true);
    // selectEntity only closes over stable setters/loadEntity
  }, [entityProp, entity, entities, dirty, onChange]);
  /* eslint-enable react-hooks/set-state-in-effect, react-hooks/exhaustive-deps */

  // The deep-link highlight only makes sense on the entity it was minted for — switching
  // entity drops it (a same-named field on another entity must not light up).
  const focusField = entity === urlPreselect.entity ? urlPreselect.field : null;

  // Scroll the deep-linked field row into view once it exists in the DOM.
  const focusRowRef = useCallback((el: HTMLTableRowElement | null) => {
    el?.scrollIntoView?.({ block: "center" });
  }, []);

  const handleEntityChange = (next: string | null) => {
    if (!next || next === entity) return;
    if (dirty) {
      setPendingEntity(next);
      return;
    }
    selectEntity(next);
  };

  const setCell = (roleId: string, fieldName: string, state: CellState) => {
    setSavedOk(false);
    setErrorCell(null);
    setDraft((m) => setCellState(m, roleId, fieldName, state));
  };

  const handleReset = () => {
    setDraft(saved);
    setSavedOk(false);
    setBanner(null);
    setErrorCell(null);
  };

  const handleSave = () => {
    if (buildPutPayload(draft, saved, entity).length === 0) return;
    void guard(t("direct.what.fieldAcl"), doSave);
  };

  const doSave = async () => {
    const payload = buildPutPayload(draft, saved, entity);
    if (payload.length === 0) return;
    setSaving(true);
    setBanner(null);
    setErrorCell(null);
    try {
      await putFieldAcl(payload);
      setSaved(draft);
      setSavedOk(true);
      setTimeout(() => setSavedOk(false), 2600);
    } catch (e) {
      if (e instanceof ApiError) {
        const details = (e.details ?? {}) as { index?: number; field?: string };
        // Defensive: a contradictory grant can't arise from a tri-state choice, but if the
        // backend flags a specific cell, highlight it.
        if (typeof details.index === "number" && payload[details.index]) {
          const g = payload[details.index];
          setErrorCell(cellKey(g.roleId, g.fieldName));
        }
        if (e.code === "FIELD_ACL_FIELD_UNKNOWN" || e.code === "FIELD_ACL_ROLE_NOT_FOUND" || e.code === "FIELD_ACL_ENTITY_NOT_FOUND") {
          setBanner(tf("page.fieldAcl.metaChanged", { msg: e.message }));
        } else {
          setBanner(e.message || t("page.fieldAcl.saveError"));
        }
      } else {
        setBanner(t("page.fieldAcl.saveError"));
      }
    } finally {
      setSaving(false);
    }
  };

  const roleLabel = (r: RoleSummary) => r.label ?? r.name;
  const entityChoices = useEntityOptions(entities);
  const entityLabel = (name: string) => entities.find((e) => e.name === name)?.label ?? name;

  // queue #146: every lock that strands a create (saved rows from before the PutGrants guard, or a
  // draft the server will refuse), field by field — the list the admin acts on, with the way out.
  const roleIds = roles.map((r) => r.id);
  const deadEnds = fields.flatMap((f) => {
    if (!deadEndFields.has(f.name)) return [];
    const ids = deadEndRoleIds(draft, f.name, roleIds);
    return ids.length > 0 ? [{ field: f, roleIds: ids }] : [];
  });
  const deadCellCount = deadEnds.reduce((n, d) => n + d.roleIds.length, 0);
  const deadEndRemedy = (f: FieldMeta) =>
    studioCanSetDefault(f.fieldType)
      ? t("page.fieldAcl.deadEnd.remedyDefault")
      : tf("page.fieldAcl.deadEnd.remedyNoDefault", { type: FIELD_TYPE_META[f.fieldType as FieldType]?.label ?? f.fieldType });

  if (bootLoading) {
    return (
      <div className="relative min-h-40 flex-1">
        <Spinner />
      </div>
    );
  }

  if (bootErr) {
    return (
      <div className="p-4">
        <ScopedNotification theme="error">{bootErr}</ScopedNotification>
      </div>
    );
  }

  const actions = [
    ...(aclHelp ? [<FieldHelp key="help" text={aclHelp.short} label={aclHelp.term} />] : []),
    ...(savedOk ? [<Badge key="saved" theme="success" iconLeft={CheckCircle2}>{t("page.fieldAcl.saved")}</Badge>] : []),
    ...(dirty
      ? [
          <span key="count" className="slds-text-color_weak">{tf("page.fieldAcl.changes", { n: changeCount })}</span>,
          <Button key="reset" onClick={handleReset} disabled={saving}>{t("action.cancel")}</Button>,
        ]
      : []),
    <Button key="save" variant="brand" iconLeft={Save} onClick={handleSave} disabled={!dirty || saving}>
      {saving ? t("action.saving") : t("action.save")}
    </Button>,
  ];

  let body: ReactNode;
  if (gridLoading) {
    body = (
      <div className="relative min-h-32">
        <Spinner size="small" label={t("page.fieldAcl.gridLoading")} />
      </div>
    );
  } else if (fields.length === 0 || roles.length === 0) {
    body = (
      <p className="slds-p-around_medium slds-text-color_weak">
        {t(fields.length === 0 ? "page.fieldAcl.noFields" : "page.fieldAcl.noRoles")}
      </p>
    );
  } else {
    body = (
      <>
        <div className="overflow-x-auto">
          <table
            className="slds-table slds-table_cell-buffer slds-table_bordered slds-table_col-bordered w-max min-w-full"
            aria-label={t("page.fieldAcl.grid")}
          >
            <thead>
              <tr className="slds-line-height_reset">
                <th scope="col" className="sticky left-0 z-[1]">
                  <div className="slds-truncate" title={t("page.fieldAcl.col.field")}>{t("page.fieldAcl.col.field")}</div>
                </th>
                {roles.map((r) => (
                  <th key={r.id} scope="col">
                    <div className="slds-truncate" title={r.name}>{roleLabel(r)}</div>
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {fields.map((f) => {
                const label = f.label ?? f.name;
                const computed = COMPUTED_TYPES.has(f.fieldType);
                const deadEnd = deadEndFields.has(f.name);
                const deadRoleCount = deadEnd ? deadEndRoleIds(draft, f.name, roleIds).length : 0;
                const restricted = restrictionCountForField(draft, f.name);
                const focused = focusField === f.name;
                return (
                  <tr
                    key={f.name}
                    ref={focused ? focusRowRef : undefined}
                    className={clsx("slds-hint-parent", focused && "slds-is-selected")}
                  >
                    <th scope="row" data-label={t("page.fieldAcl.col.field")} className="slds-theme_default sticky left-0 z-[1]">
                      <div className="flex items-center gap-1">
                        <span className="slds-truncate" title={label}>{label}</span>
                        {f.isRequired && (
                          <abbr className="slds-required" title={t("page.fieldAcl.required")}>*</abbr>
                        )}
                        {deadRoleCount > 0 && (
                          <Icon
                            icon={TriangleAlert}
                            size="xx-small"
                            tone="warning"
                            label={tf("page.fieldAcl.deadEnd.row", { n: deadRoleCount })}
                          />
                        )}
                        {computed && (
                          <span title={t("page.fieldAcl.computedTitle")}>
                            <Badge iconLeft={Sigma}>{t("page.fieldAcl.computed")}</Badge>
                          </span>
                        )}
                      </div>
                      <div className="slds-text-body_small slds-text-color_weak">
                        {f.name} · {f.fieldType}
                        {restricted > 0 && ` · ${tf("page.fieldAcl.exceptions", { n: restricted })}`}
                      </div>
                    </th>
                    {roles.map((r) => {
                      const state = getCellState(draft, r.id, f.name);
                      const key = cellKey(r.id, f.name);
                      // radio-button-group "states: error": a server-rejected cell, else a lock that
                      // strands every create of this role (queue #146) — saved or still a draft.
                      const cellError = errorCell === key
                        ? t("page.fieldAcl.cellError")
                        : deadEnd && state !== "default" ? t("page.fieldAcl.deadEnd.cell") : undefined;
                      return (
                        <td key={r.id} data-label={roleLabel(r)}>
                          <RadioButtonGroup
                            labelHidden
                            label={tf("page.fieldAcl.cell", { field: label, role: roleLabel(r) })}
                            name={key}
                            options={STATES.map((s) => ({ value: s.value, label: stateLabel(s.value) }))}
                            value={state}
                            onValueChange={(v) => setCell(r.id, f.name, v as CellState)}
                            error={cellError}
                          />
                        </td>
                      );
                    })}
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
        <ul className="slds-list_horizontal slds-has-dividers_left slds-wrap slds-p-around_small slds-text-body_small slds-text-color_weak">
          {STATES.map((s) => (
            <li key={s.value} className="slds-item flex items-center gap-1">
              <Icon icon={s.icon} size="xx-small" />
              {t(`page.fieldAcl.legend.${s.value}` as I18nKey)}
            </li>
          ))}
          <li className="slds-item flex items-center gap-1">
            <Icon icon={TriangleAlert} size="xx-small" tone="warning" />
            {t("page.fieldAcl.legend.deadEnd")}
          </li>
        </ul>
      </>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
      <PageHeader
        icon={{ glyph: Lock, name: "entitlement-policy" }}
        title={t("page.fieldAcl.title")}
        meta={t("page.fieldAcl.subtitle")}
        actions={actions}
      />

      <ScopedNotification>
        <strong>{t("page.fieldAcl.explainLead")}</strong> {t("page.fieldAcl.explainBody")}
      </ScopedNotification>

      {banner && (
        <ScopedNotification theme="error">
          <span className="flex items-center gap-2">
            <span className="flex-1">{banner}</span>
            <ButtonIcon icon={X} variant="inverse" label={t("page.fieldAcl.dismiss")} onClick={() => setBanner(null)} />
          </span>
        </ScopedNotification>
      )}

      {!gridLoading && deadEnds.length > 0 && (
        <ScopedNotification theme="warning">
          <strong>{tf("page.fieldAcl.deadEnd.title", { n: deadCellCount })}</strong> {t("page.fieldAcl.deadEnd.body")}
          {deadEnds.map(({ field, roleIds: ids }) => (
            <span key={field.name} className="slds-show slds-m-top_xx-small">
              {tf("page.fieldAcl.deadEnd.item", {
                field: field.label ?? field.name,
                roles: roles.filter((r) => ids.includes(r.id)).map(roleLabel).join(", "),
                remedy: deadEndRemedy(field),
              })}
            </span>
          ))}
        </ScopedNotification>
      )}

      {notice && <p>{notice}</p>}
      <Card
        title={t("page.fieldAcl.matrix")}
        count={gridLoading ? undefined : fields.length}
        padded={false}
        actions={
          <Combobox
            label={t("page.fieldAcl.entity")}
            labelHidden
            className="w-64"
            value={entity || null}
            onChange={handleEntityChange}
            options={entityChoices}
            placeholder={t("page.fieldAcl.entityPlaceholder")}
          />
        }
      >
        {body}
      </Card>

      {/* Discard-changes guard when switching entity: asked in the page, not in a dialog (FR-065) */}
      {pendingEntity !== null && (
        <InlineConfirm
          tone="error"
          message={`${t("page.fieldAcl.discard.title")} ${tf("page.fieldAcl.discard.desc", { n: changeCount, entity: entityLabel(entity) })}`}
          confirmLabel={t("page.fieldAcl.discard.confirm")}
          onCancel={() => {
            setPendingEntity(null);
            onChange?.(entity, undefined);
          }}
          onConfirm={() => {
            const next = pendingEntity;
            setPendingEntity(null);
            if (next) selectEntity(next);
          }}
        />
      )}
      {dialog}
    </div>
  );
}

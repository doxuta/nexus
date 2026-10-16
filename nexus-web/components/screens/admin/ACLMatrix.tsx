"use client";

// ACLMatrix — the core RBAC widget (C4/C5).
// Renders entity × action grid with per-cell access-level selector.
// Supports: bulk row/column set, wildcard (*) pinned top, dirty-guard.
// SLDS (specs/038 T048, RESEARCH §5 /admin/roles — "ma trận quyền không có blueprint: data-table có
// ô chọn"): a card (cards/base) holding data-tables/base. A cell is not a checkbox — the grant has
// four levels — so each cell is a menus/dropdown whose trigger shows the level as a badge (colour =
// level, so the matrix scans at a glance). Column and row headers open the same menu to set the whole
// column / row. A cell with no grant of its own that inherits from the ✶ row carries a ✶ mark and says
// so in its name (the old italics had no SLDS class and no text for screen readers).
// The cell menu marks the current level like menus/dropdown's selectable items
// (li.slds-is-selected > a[role=menuitemcheckbox][aria-checked], left check); the row / column menus
// set many cells at once and have no current level, so they are plain items.

import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { CheckCircle2, ChevronDown, RotateCcw, Save } from "lucide-react";
import {
  getRole, updateRolePermissions, listEntities, listFields, ensureAuth, ApiError,
} from "@/lib/api";
import {
  grantsToMatrix, matrixToGrants, cellLevel, setCell, setRow, setColumn,
  matrixEqual, ACTION_VERBS, ACCESS_LEVELS, fieldLevel, userFieldOf,
  type MatrixState, type AccessLevel, type BaseAccessLevel, type ActionVerb,
} from "@/lib/admin";
import { getHelp } from "@/lib/help-content";
import { t, tf } from "@/lib/i18n";
import { useSetBlocked } from "@/lib/navigation-blocker";
import type { I18nKey } from "@/lib/i18n/vi";
import { FieldHelp } from "@/components/fields/widgets";
import { Badge, type BadgeProps } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { DataTable, fill } from "@/components/slds/DataTable";
import { Menu, MenuCheckboxItem, MenuGroup, MenuItem } from "@/components/slds/Menu";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { useDirectApply } from "@/components/shell/useDirectApply";

// Access level → label, short badge text and badge look.
const LEVEL_META: Record<BaseAccessLevel, { label: I18nKey; short: I18nKey; badge: Pick<BadgeProps, "variant" | "theme"> }> = {
  none: { label: "page.adminRoles.level.none", short: "page.adminRoles.levelShort.none", badge: {} },
  owner: { label: "page.adminRoles.level.owner", short: "page.adminRoles.levelShort.owner", badge: { variant: "lightest" } },
  team: { label: "page.adminRoles.level.team", short: "page.adminRoles.levelShort.team", badge: { theme: "warning" } },
  all: { label: "page.adminRoles.level.all", short: "page.adminRoles.levelShort.all", badge: { theme: "success" } },
};

const ACTION_LABELS: Record<ActionVerb, I18nKey> = {
  view: "page.adminRoles.action.view",
  create: "page.adminRoles.action.create",
  edit: "page.adminRoles.action.edit",
  delete: "page.adminRoles.action.delete",
};

const aclModuleHelp = getHelp("aclModule");
const aclLevelsHelp = getHelp("aclLevels");

// Matrix rows: the ✶ wildcard pinned on top + EVERY live entity of the tenant with its real
// label (EASE Nước 6, audit F8 — grep-verified): the old hardcoded BASE_ENTITIES showed two
// PHANTOM Vietnamese-labeled rows ("accounts"/"contacts" — entities that don't exist on a
// crm_-prefixed pack app) while real entities appeared as raw engine names, and a NEWLY created
// role had NO way to grant per-entity access at all — the only working control was the
// over-granting wildcard. Same sourcing idiom as the sibling FieldACL screen.
interface EntityRow { entity: string; label: string; wildcard?: boolean }

// Rows = ✶ + live entities (labels from metadata) + any granted name that no longer resolves
// (kept visible so a stale grant on a deleted entity can still be seen and cleared).
function buildEntityRows(live: { name: string; label?: string }[], grants: ReturnType<typeof matrixToGrants>): EntityRow[] {
  const rows: EntityRow[] = [{ entity: "*", label: t("page.adminRoles.matrix.wildcard"), wildcard: true }];
  const seen = new Set<string>(["*"]);
  for (const e of live) {
    seen.add(e.name);
    rows.push({ entity: e.name, label: e.label || e.name });
  }
  for (const g of grants) {
    if (!seen.has(g.entityName)) {
      seen.add(g.entityName);
      rows.push({ entity: g.entityName, label: fill(t("page.adminRoles.matrix.deleted"), { name: g.entityName }) });
    }
  }
  return rows;
}

const rowText = (row: EntityRow) => (row.wildcard ? `✶ ${row.label}` : row.label);

// #283: "field:<name>" is not in LEVEL_META — it carries the name of a Người dùng field. The label of
// that field (what the builder named it, e.g. "Bác sĩ") is looked up in the per-entity map; a field
// that no longer resolves falls back to its raw name so a stale grant stays visible and clearable.
type UserFieldLabels = Record<string, Record<string, string>>; // entity → field name → label
const fieldLabelOf = (labels: UserFieldLabels, entity: string, field: string) => labels[entity]?.[field] ?? field;

function levelText(level: AccessLevel, entity: string, labels: UserFieldLabels): string {
  const field = userFieldOf(level);
  if (field !== null) return fill(t("page.adminRoles.level.field"), { field: fieldLabelOf(labels, entity, field) });
  return t(LEVEL_META[level as BaseAccessLevel].label);
}

function LevelBadge({ level }: { level: AccessLevel }) {
  if (userFieldOf(level) !== null) return <Badge theme="warning">{t("page.adminRoles.levelShort.field")}</Badge>;
  const meta = LEVEL_META[level as BaseAccessLevel];
  return <Badge {...meta.badge}>{t(meta.short)}</Badge>;
}

/**
 * Menu trigger inside a header / cell: plain text-reset button + the blueprint's small down glyph.
 * A plain element, not a component: Radix's Trigger (asChild) must put its ref and handlers on the
 * <button> itself.
 */
function menuTrigger(label: string, content: ReactNode, title = label) {
  return (
    <button type="button" className="slds-button slds-button_reset" aria-label={label} title={title}>
      <span className="flex items-center gap-1">
        {content}
        <ChevronDown aria-hidden="true" className="slds-button__icon slds-button__icon_x-small slds-icon-text-default nx-lucide" />
      </span>
    </button>
  );
}

/** The four levels as menu items. With `current` (a cell) they are selectable items and the current
 *  one is checked; without it (row / column bulk set) plain items. A cell of a table that has Người
 *  dùng fields (`userFields`, name → label) also offers "records whose <field> is me" per field. */
function LevelItems({ current, onPick, userFields }: {
  current?: AccessLevel; onPick: (level: AccessLevel) => void; userFields?: [string, string][];
}) {
  return (
    <>
      {ACCESS_LEVELS.map((lvl) =>
        current ? (
          <MenuCheckboxItem key={lvl} label={t(LEVEL_META[lvl].label)} checked={lvl === current} onCheckedChange={() => onPick(lvl)} />
        ) : (
          <MenuItem key={lvl} label={t(LEVEL_META[lvl].label)} onSelect={() => onPick(lvl)} />
        ),
      )}
      {current && userFields && userFields.length > 0 && (
        <MenuGroup label={t("page.adminRoles.level.fieldGroup")}>
          {userFields.map(([name, label]) => (
            <MenuCheckboxItem
              key={name}
              label={fill(t("page.adminRoles.level.field"), { field: label })}
              checked={current === fieldLevel(name)}
              onCheckedChange={() => onPick(fieldLevel(name))}
            />
          ))}
        </MenuGroup>
      )}
    </>
  );
}

// The matrix and its setters reach the cells through context, NOT through the column defs: flexRender
// renders a `cell` / `header` function as a component type, so columns rebuilt on every edit would
// remount every cell and header menu and drop keyboard focus on <body> after each pick.
interface MatrixApi {
  matrix: MatrixState;
  /** entity → { user field name → label } (#283), for the per-field level and its label. */
  userFields: UserFieldLabels;
  onCell: (entity: string, action: ActionVerb, level: AccessLevel) => void;
  onRow: (entity: string, level: AccessLevel) => void;
  onColumn: (action: ActionVerb, level: AccessLevel) => void;
}
const MatrixContext = createContext<MatrixApi | null>(null);
const useMatrix = () => useContext(MatrixContext)!;

// ── Column header — menu to set the whole column
function ColumnMenu({ action }: { action: ActionVerb }) {
  const { onColumn } = useMatrix();
  return (
    <Menu trigger={menuTrigger(fill(t("page.adminRoles.matrix.setColumn"), { action: t(ACTION_LABELS[action]) }), t(ACTION_LABELS[action]))}>
      <MenuGroup label={t("page.adminRoles.matrix.setAll")}>
        <LevelItems onPick={(lvl) => onColumn(action, lvl)} />
      </MenuGroup>
    </Menu>
  );
}

// ── Row header — menu to set the whole row
function RowMenu({ row }: { row: EntityRow }) {
  const { onRow } = useMatrix();
  return (
    <Menu
      trigger={menuTrigger(
        fill(t("page.adminRoles.matrix.setRow"), { entity: row.label }),
        <span className="slds-truncate">{rowText(row)}</span>,
      )}
    >
      <MenuGroup label={t("page.adminRoles.matrix.setWholeRow")}>
        <LevelItems onPick={(lvl) => onRow(row.entity, lvl)} />
      </MenuGroup>
    </Menu>
  );
}

// ── Cell — dropdown to pick the access level
function LevelCell({ row, action }: { row: EntityRow; action: ActionVerb }) {
  const { matrix, onCell, userFields } = useMatrix();
  const level = cellLevel(matrix, row.entity, action);
  // The ✶ row spans every table, so it cannot name one table's field; only a real entity row can.
  const rowFields = row.wildcard ? undefined : Object.entries(userFields[row.entity] ?? {});
  // A cell without its own value that the ✶ row drives (visual + spoken hint).
  const inherited = !(`${row.entity}:${action}` in matrix) && `*:${action}` in matrix && row.entity !== "*";
  const name =
    fill(t("page.adminRoles.matrix.cell"), { action: t(ACTION_LABELS[action]), entity: row.label, level: levelText(level, row.entity, userFields) }) +
    (inherited ? t("page.adminRoles.matrix.inherited") : "");
  return (
    <Menu
      trigger={menuTrigger(
        name,
        <>
          <LevelBadge level={level} />
          {inherited && <span aria-hidden="true" className="slds-text-color_weak">✶</span>}
        </>,
        inherited ? t("page.adminRoles.matrix.inheritedTitle") : name,
      )}
    >
      <LevelItems current={level} userFields={rowFields} onPick={(lvl) => onCell(row.entity, action, lvl)} />
    </Menu>
  );
}

// ── Main exported component
/**
 * `entityScope` (queue #302, inside an app workshop): only the ✶ row and the app's tables are drawn. The
 * grants on every other table stay in the matrix state untouched — they are saved back as they were — and
 * the column menu stops reaching them (it would otherwise over-grant to other apps through ✶).
 */
export function ACLMatrix({ roleId, onDirtyChange, entityScope = null, renderBar }: {
  roleId: string; onDirtyChange?: (dirty: boolean) => void; entityScope?: ReadonlySet<string> | null;
  /** Studio role page: the page owns the Lưu/Huỷ bar (UnsavedBar) — the card then draws no save buttons of its own. */
  renderBar?: (s: { dirty: boolean; saving: boolean; error: string | null; save: () => void; reset: () => void }) => ReactNode;
}) {
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  // A failed LOAD hides the matrix: an empty grid saved over the real grants would wipe them.
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [savedOk, setSavedOk] = useState(false);
  // Studio (renderBar) asks in the page; the customer face keeps the Modal.
  const { guard, dialog, notice } = useDirectApply({ inline: !!renderBar });

  const [roleName, setRoleName] = useState("");
  const [roleLabel, setRoleLabel] = useState("");
  const [allRows, setEntities] = useState<EntityRow[]>([]);
  const entities = useMemo(
    () => (entityScope ? allRows.filter((r) => r.wildcard || entityScope.has(r.entity)) : allRows),
    [allRows, entityScope],
  );
  const [userFields, setUserFields] = useState<UserFieldLabels>({});
  const [matrix, setMatrix] = useState<MatrixState>({});
  const [savedMatrix, setSavedMatrix] = useState<MatrixState>({});

  const isDirty = useMemo(() => !matrixEqual(matrix, savedMatrix), [matrix, savedMatrix]);
  const prevDirty = useRef(false);
  const setBlocked = useSetBlocked();
  useEffect(() => {
    setBlocked(isDirty && !saving);
    return () => setBlocked(false);
  }, [isDirty, saving, setBlocked]);
  useEffect(() => {
    if (isDirty !== prevDirty.current) {
      prevDirty.current = isDirty;
      onDirtyChange?.(isDirty);
    }
  }, [isDirty, onDirtyChange]);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        setLoading(true);
        setLoadErr(null);
        await ensureAuth();
        const [role, liveEntities] = await Promise.all([getRole(roleId), listEntities()]);
        if (cancelled) return;
        setRoleName(role.name);
        setRoleLabel(role.label ?? role.name);
        const grants = role.actions ?? [];
        setEntities(buildEntityRows(liveEntities, grants));
        // #283: which tables have Người dùng fields (to offer "hồ sơ mà trường … là tôi"). One call per
        // table; a failed one only hides that table's extra option — never the matrix itself.
        const named = await Promise.all(liveEntities.map(async (e) => {
          const fields = await listFields(e.name).catch(() => []);
          const users = fields.filter((f) => f.fieldType === "user");
          return [e.name, Object.fromEntries(users.map((f) => [f.name, f.label || f.name]))] as const;
        }));
        if (cancelled) return;
        setUserFields(Object.fromEntries(named.filter(([, f]) => Object.keys(f).length > 0)));
        const m = grantsToMatrix(grants);
        setMatrix(m);
        setSavedMatrix(m);
      } catch (e) {
        if (!cancelled) setLoadErr(e instanceof ApiError ? e.message : t("page.adminRoles.matrix.loadError"));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => { cancelled = true; };
  }, [roleId]);

  const handleCell = useCallback((entity: string, action: ActionVerb, level: AccessLevel) => {
    setMatrix((m) => setCell(m, entity, action, level));
    setSavedOk(false);
  }, []);

  const handleRow = useCallback((entity: string, level: AccessLevel) => {
    setMatrix((m) => setRow(m, entity, level));
    setSavedOk(false);
  }, []);

  const handleColumn = useCallback((action: ActionVerb, level: AccessLevel) => {
    const entityNames = entities.filter((e) => !(entityScope && e.wildcard)).map((e) => e.entity);
    setMatrix((m) => setColumn(m, entityNames, action, level));
    setSavedOk(false);
  }, [entities, entityScope]);

  const handleReset = useCallback(() => {
    setMatrix(savedMatrix);
    setSavedOk(false);
  }, [savedMatrix]);

  const doSave = useCallback(async () => {
    setSaving(true);
    setErr(null);
    try {
      const grants = matrixToGrants(matrix);
      await updateRolePermissions(roleId, { name: roleName, label: roleLabel, actions: grants });
      setSavedMatrix(matrix);
      setSavedOk(true);
      setTimeout(() => setSavedOk(false), 2500);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : t("page.adminRoles.matrix.saveFailed"));
    } finally {
      setSaving(false);
    }
  }, [matrix, roleId, roleName, roleLabel]);
  const handleSave = useCallback(() => guard(t("direct.what.matrix"), doSave), [guard, doSave]);

  // Built once: the cell / header component types must never change (see MatrixContext).
  const columns = useMemo<ColumnDef<EntityRow>[]>(() => [
    {
      id: "entity",
      accessorFn: rowText,
      header: t("page.adminRoles.matrix.entity"),
      size: 260,
      cell: ({ row }) => <RowMenu row={row.original} />,
    },
    ...ACTION_VERBS.map<ColumnDef<EntityRow>>((a) => ({
      id: a,
      header: () => <ColumnMenu action={a} />,
      cell: ({ row }) => <LevelCell row={row.original} action={a} />,
    })),
  ], []);
  const api = useMemo<MatrixApi>(
    () => ({ matrix, userFields, onCell: handleCell, onRow: handleRow, onColumn: handleColumn }),
    [matrix, userFields, handleCell, handleRow, handleColumn],
  );

  if (loading) {
    return (
      <div className="slds-is-relative p-6">
        <Spinner size="small" label={t("page.adminRoles.matrix.loading")} />
      </div>
    );
  }

  if (loadErr) return <ScopedNotification theme="error">{loadErr}</ScopedNotification>;

  return (
    <>
    {notice && <p className="slds-m-bottom_x-small">{notice}</p>}
    <Card
      title={t("page.adminRoles.matrix.title")}
      padded={false}
      actions={
        <div className="flex flex-wrap items-center justify-end gap-2">
          {aclModuleHelp && <FieldHelp text={aclModuleHelp.short} label={aclModuleHelp.term} />}
          {savedOk && <Badge theme="success" iconLeft={CheckCircle2}>{t("page.adminRoles.matrix.saved")}</Badge>}
          {!renderBar && isDirty && <Badge theme="warning">{t("page.adminRoles.matrix.dirty")}</Badge>}
          {!renderBar && isDirty && (
            <Button iconLeft={RotateCcw} onClick={handleReset}>{t("page.adminRoles.matrix.reset")}</Button>
          )}
          {!renderBar && (
            <Button variant="brand" iconLeft={Save} disabled={!isDirty || saving} onClick={() => void handleSave()}>
              {saving ? t("page.adminRoles.matrix.saving") : t("page.adminRoles.matrix.save")}
            </Button>
          )}
        </div>
      }
      footer={
        <div className="flex flex-wrap items-center gap-3">
          <span>{t("page.adminRoles.matrix.legend")}</span>
          {aclLevelsHelp && <FieldHelp text={aclLevelsHelp.short} label={aclLevelsHelp.term} />}
          {ACCESS_LEVELS.map((lvl) => (
            <span key={lvl} className="flex items-center gap-1">
              <LevelBadge level={lvl} />
              {t(LEVEL_META[lvl].label)}
            </span>
          ))}
          {Object.keys(userFields).length > 0 && (
            <span className="flex items-center gap-1">
              <LevelBadge level={fieldLevel("")} />
              {t("page.adminRoles.matrix.legendField")}
            </span>
          )}
          <span className="slds-text-color_weak">{t("page.adminRoles.matrix.legendInherited")}</span>
        </div>
      }
    >
      <div className="flex flex-col gap-3 px-4 pb-3">
        <ScopedNotification>{t("page.adminRoles.matrix.note")}</ScopedNotification>
        {entityScope && allRows.length > entities.length && (
          <ScopedNotification>
            {tf("workshop.scope.matrixHidden", { n: allRows.length - entities.length })} {t("workshop.scope.columnApp")} {t("workshop.scope.wildcardWarn")}
          </ScopedNotification>
        )}
        {err && !renderBar && <ScopedNotification theme="error">{err}</ScopedNotification>}
      </div>
      {/* Five columns do not fit a phone: below 36rem the matrix scrolls sideways instead of
          squeezing every level badge to nothing. */}
      <div className="overflow-x-auto">
        <div className="min-w-[36rem]">
          <MatrixContext.Provider value={api}>
            <DataTable
              label={t("page.adminRoles.matrix.table")}
              columns={columns}
              data={entities}
              getRowId={(r) => r.entity}
            />
          </MatrixContext.Provider>
        </div>
      </div>
    </Card>
    {renderBar ? dialog && <div className="slds-m-top_small">{dialog}</div> : dialog}
    {renderBar?.({ dirty: isDirty, saving, error: err, save: () => void handleSave(), reset: handleReset })}
    </>
  );
}

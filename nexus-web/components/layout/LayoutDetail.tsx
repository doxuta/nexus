"use client";

// LayoutDetail — the dynamic record page for ANY entity, on SLDS (specs/038 T023): the
// RecordHomePage template (page-headers/record-home + tabs/base in a card).
//
//   • Loads layout + fields + record in parallel. Relation fields are expanded server-side
//     (?expand=<relation names>) so values show a label, not a raw UUID (RM-07); rollups are
//     requested with ?rollup=. Field names come from the metadata, never hard-coded.
//   • Header: object label (entity label) + the record's VIP label (recordLabel) as the title; the
//     detail row carries real values only — last update, up to 2 fields from the layout's first
//     section (headerHighlights), and the owner.
//   • Actions: Sửa / Nhân bản / Xoá follow the caller's grants; In and Đổi chủ are always there
//     (print is read-only; owner change is authorized server-side with a real message);
//     Chia sẻ (specs/051) for an editor; QR thanh toán only when the tenant is configured and the
//     entity has a money field. Whether THIS record takes a write is the server's per-record canEdit
//     (#43), not how the record reached me: canEdit === false hides Sửa / Đổi chủ / Chia sẻ and says
//     why — "shared for viewing" when it came only through a share (then Xoá goes too), "belongs to
//     someone else" otherwise (e.g. edit=own on a colleague's lead; the server answers 403, #18). A
//     shared record my edit scope reaches anyway (view=own, edit=team) stays editable.
//     Chia sẻ opens ./detail/ShareDialog.
//   • Tabs (one mounted at a time, so a tab's requests fire only when it is opened):
//     Tổng quan · Trao đổi · Quan hệ · Tệp · Lịch sử · Duyệt — each lives in ./detail/*.
//   • States: loading · load error · permissions unknown.
//
// Navigation (edit / clone / after delete) is delegated to the route shell via callbacks; "back to
// list" is the shell's own up button + breadcrumb (AppShell CrumbStrip).

import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { FileText } from "lucide-react";
import {
  ensureAuth,
  getLayout,
  listFields,
  getRecord,
  deleteRecord,
  ApiError,
  cellField,
  isSpacerCell,
  listUserDirectory,
  listAllRelationships,
  listEntities,
  listChildRecords,
  type EntityLayout,
  type FieldMeta,
  type ApiRecord,
  type LayoutSection,
} from "@/lib/api";
import { recordLabel } from "@/lib/recordLabel";
import { visibleSections } from "@/lib/visibleWhen";
import { usePermissions } from "@/lib/metadata/permissions";
import { useEntities } from "@/lib/metadata/hooks";
import { isExpandableType } from "@/lib/metadata";
import { useAuth } from "@/lib/auth/AuthProvider";
import { useBranding } from "@/components/brand/BrandingBoot";
import { recordRecordVisit } from "@/lib/recents";
import { metaToSpec } from "@/lib/fieldSpec";
import { qk } from "@/lib/query/keys";
import { t } from "@/lib/i18n";
import { FieldDisplay } from "@/components/fields/display";
import { toast } from "@/components/ui/Toaster";
import { usePaymentQR, PaymentQRModal } from "@/components/layout/PaymentQRModal";
import { Button } from "@/components/slds/Button";
import { Modal } from "@/components/slds/Modal";
import { PageHeader, type PageHeaderDetail } from "@/components/slds/PageHeader";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { RecordHomePage } from "@/components/slds/templates/RecordHomePage";
import type { PageAction } from "@/components/slds/templates/ActionGroup";
import { OverviewTab } from "./detail/DetailSections";
import { NotesTab } from "./detail/NotesTab";
import { RelatedTab } from "./detail/RelatedTab";
import { FilesTab } from "./detail/FilesTab";
import { HistoryTab } from "./detail/HistoryTab";
import { ApprovalsTab } from "./detail/ApprovalsTab";
import { RecordSlaCard } from "./detail/RecordSlaCard";
import { OwnerDialog } from "./detail/OwnerDialog";
import { ShareDialog } from "./detail/ShareDialog";
import { PrintDocument, HIGHLIGHT_SKIP_TYPES, type PrintChildGroup } from "./detail/PrintDocument";

// The tabs are exported from here for their component tests (NotesTab/RelatedTab/OwnerDialog/…).
export { NotesTab } from "./detail/NotesTab";
export { RelatedTab } from "./detail/RelatedTab";
export { HistoryTab, historyDiffRows } from "./detail/HistoryTab";
export { ApprovalsTab } from "./detail/ApprovalsTab";
export { OwnerDialog } from "./detail/OwnerDialog";
export { ShareDialog } from "./detail/ShareDialog";

type LoadState = "loading" | "ok" | "error";
type TabId = "overview" | "notes" | "related" | "files" | "history" | "approvals";

// Cap on child rows fetched per relation for print — listChildRecords is a paginated read, not a
// true "fetch all"; 200 covers any real quote/contract line list without an unbounded query.
const PRINT_CHILD_ROW_LIMIT = 200;

// Generic record tile: Nexus metadata carries no per-entity icon yet.
const RECORD_ICON = { glyph: FileText, name: "record" };

export interface LayoutDetailProps {
  entity: string;
  recordId: string;
  // Navigate to the edit form for this record.
  onEdit?: () => void;
  // Navigate to a NEW form prefilled from this record (§7.1 ?cloneFrom=<id>).
  onClone?: () => void;
  // Called after the record is successfully deleted (route shell sends the user back to the list).
  onDeleted?: () => void;
  // Default true: write "Vừa xem" to the browser's localStorage. The Studio frame passes false (specs/078 FR-132): its
  // browser is shared between customers, so a customer's record label must not outlive the page.
  trackVisit?: boolean;
  // Shown instead of the raw server sentence when the record does not exist (404 RECORD_NOT_FOUND), e.g. a link to the recycle bin.
  notFoundNote?: ReactNode;
}

// Collect every distinct field name a set of sections references, in layout order (skip spacers).
function sectionFieldNames(sections: LayoutSection[] | undefined): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const s of sections ?? []) {
    for (const cell of s.fields) {
      if (isSpacerCell(cell)) continue;
      const fc = cellField(cell);
      if (fc && !seen.has(fc.field)) {
        seen.add(fc.field);
        out.push(fc.field);
      }
    }
  }
  return out;
}

// headerHighlights — MOT-XUONG "không có tóm tắt đầu hồ sơ": a bare title reads as unfinished.
// Picks up to `max` real values from the layout's FIRST detail section (the maker's own field
// order — no name-guessing, no per-entity hardcoding) for the record-home detail row. Skips empty
// values and whichever field's raw value is already the title (recordLabel usually lifts one
// section-1 field verbatim), and skips bulky/non-scalar types that don't read well inline.
export function headerHighlights(
  sections: LayoutSection[],
  fieldsByName: Record<string, FieldMeta>,
  data: Record<string, unknown>,
  title: string,
  max = 2,
): { field: FieldMeta; value: unknown }[] {
  const first = sections[0];
  if (!first) return [];
  const out: { field: FieldMeta; value: unknown }[] = [];
  const titleTrimmed = title.trim();
  for (const cell of first.fields) {
    if (out.length >= max) break;
    if (isSpacerCell(cell)) continue;
    const fc = cellField(cell);
    if (!fc) continue;
    const field = fieldsByName[fc.field];
    if (!field || HIGHLIGHT_SKIP_TYPES.has(field.fieldType)) continue;
    const value = data[fc.field];
    if (value == null || value === "" || (Array.isArray(value) && value.length === 0)) continue;
    if (typeof value === "string" && value.trim() === titleTrimmed) continue; // already the title
    out.push({ field, value });
  }
  return out;
}

export function LayoutDetail({ entity, recordId, onEdit, onClone, onDeleted, trackVisit = true, notFoundNote }: LayoutDetailProps) {
  // Sửa / Nhân bản / Xoá answer 403 for a view-only employee, so they follow the grants.
  const { can: canAct, isError: permsUnknown } = usePermissions();
  const mayEdit = canAct("edit", entity);
  const mayCreate = canAct("create", entity);
  const mayDelete = canAct("delete", entity);
  // A permissions fetch that FAILED leaves every may* false — the right default (never widen access
  // on an error) and the wrong silence: say which it is (notice above the header).

  // DP-1.6: the tech line (entity slug · uuid · version) is builder talk — admins only.
  const { user } = useAuth();
  const isAdminUser = !!user?.isAdmin;
  // MOT-XUONG W3 #6: org name for the printed document header ({} → line omitted, never invented).
  const branding = useBranding();
  // Object label above the title. Empty until entities load — never the machine name.
  const { data: entities } = useEntities();
  const entityLabel = entities?.find((e) => e.name === entity)?.label ?? "";

  const [loadState, setLoadState] = useState<LoadState>("loading");
  const [loadErr, setLoadErr] = useState("");
  const [loadNotFound, setLoadNotFound] = useState(false);
  const [layout, setLayout] = useState<EntityLayout | null>(null);
  const [allFields, setAllFields] = useState<FieldMeta[]>([]);
  const [record, setRecord] = useState<ApiRecord | null>(null);
  const [activeTab, setActiveTab] = useState<TabId>("overview");

  const [deleteOpen, setDeleteOpen] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [delErr, setDelErr] = useState("");

  // "Đổi chủ" (TRỤ 2 mục 2): dialog + a reload key so a successful reassignment re-fetches THIS
  // component's local record (owner columns are server state).
  const [ownerOpen, setOwnerOpen] = useState(false);
  const [shareOpen, setShareOpen] = useState(false);
  const [qrOpen, setQrOpen] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);
  const queryClient = useQueryClient();

  const fieldsByName = useMemo(
    () => Object.fromEntries(allFields.map((f) => [f.name, f])),
    [allFields],
  );

  // Reset to a fresh loading state when the target record changes (entity|id).
  const loadKey = `${entity}/${recordId}`;
  const [prevKey, setPrevKey] = useState(loadKey);
  if (prevKey !== loadKey) {
    setPrevKey(loadKey);
    setLoadState("loading");
    setLoadErr("");
    setLayout(null);
    setAllFields([]);
    setRecord(null);
    setDeleteOpen(false);
    setDelErr("");
    setActiveTab("overview");
    setOwnerOpen(false);
    setShareOpen(false);
    setQrOpen(false);
  }

  // ── Load layout + fields, then the record WITH expand for its relation fields ──
  useEffect(() => {
    let cancelled = false;

    (async () => {
      try {
        await ensureAuth();
        const [lay, flds] = await Promise.all([getLayout(entity), listFields(entity)]);
        if (cancelled) return;
        if (!lay) throw new ApiError(404, "LAYOUT_NOT_FOUND", t("page.record.layoutMissing"));

        // Id-valued fields the detail layout references → expand (isExpandableType is shared with
        // LayoutList, so `user` fields show a name too). Rollups → ?rollup= (read-time opt-in).
        const detailNames = new Set(sectionFieldNames(lay.views.detail?.sections));
        const relationFields = flds
          .filter((f) => isExpandableType(f.fieldType) && detailNames.has(f.name))
          .map((f) => f.name);
        const rollupFields = flds
          .filter((f) => f.fieldType === "rollup" && detailNames.has(f.name))
          .map((f) => f.name);

        const rec = await getRecord(
          entity,
          recordId,
          relationFields.length ? relationFields : undefined,
          rollupFields.length ? rollupFields : undefined,
        );
        if (cancelled) return;

        setLayout(lay);
        setAllFields(flds);
        setRecord(rec);
        setLoadState("ok");
      } catch (e) {
        if (cancelled) return;
        setLoadNotFound(e instanceof ApiError && e.code === "RECORD_NOT_FOUND");
        setLoadErr(e instanceof ApiError ? e.message : e instanceof Error ? e.message : t("page.record.loadErrorFallback"));
        setLoadState("error");
      }
    })();
    return () => { cancelled = true; };
    // reloadKey: bumped after a successful owner change so the page re-reads server state.
  }, [entity, recordId, reloadKey]);

  // Names for owner / audit ids. The tenant directory is small, shared with OwnerDialog's picker
  // and the History tab, and cached — one query per session.
  const { data: directory } = useQuery({
    queryKey: ["user-directory"],
    queryFn: () => listUserDirectory(), // wrapped: react-query passes a context arg, listUserDirectory's first param is `q`
    staleTime: 5 * 60 * 1000,
    retry: false,
    enabled: loadState === "ok",
  });
  const names = useMemo(
    () => Object.fromEntries((directory ?? []).map((u) => [u.id, u.fullName?.trim() || u.email])),
    [directory],
  );
  // Directory not loaded yet, or the owner is no longer an active user: the id rather than a blank
  // that would read as "nobody owns this".
  const ownerLabel = record?.ownerId ? (names[record.ownerId] ?? record.ownerId) : "";

  const paymentReady = usePaymentQR(allFields);

  // After a successful "Đổi chủ": re-fetch the local record + drop the cached record/list queries
  // so every list screen shows the new owner on its next render.
  const handleOwnerChanged = () => {
    setReloadKey((k) => k + 1);
    void queryClient.invalidateQueries({ queryKey: qk.record(entity, recordId) });
    void queryClient.invalidateQueries({ queryKey: qk.records(entity) });
  };

  const handleDelete = async () => {
    setDeleting(true);
    setDelErr("");
    try {
      await deleteRecord(entity, recordId);
      setDeleteOpen(false);
      onDeleted?.();
    } catch (e) {
      // Keep the dialog open with the server's own sentence (403 / 404 / 409 …).
      setDelErr(e instanceof ApiError ? e.message : e instanceof Error ? e.message : t("page.record.delete.error"));
    } finally {
      setDeleting(false);
    }
  };

  // MOT-XUONG W3 #6 — "In" prepares the print-only document (header + child tables) THEN prints:
  // a plain record (no child tables) never pays for the relationship reads, and window.print()
  // never races an unfinished child fetch.
  const [printState, setPrintState] = useState<"idle" | "preparing">("idle");
  const [printData, setPrintData] = useState<{ entityLabel: string; childGroups: PrintChildGroup[]; printedAt: Date } | null>(null);

  const handlePrint = async () => {
    setPrintState("preparing");
    const printedAt = new Date();
    let docLabel = t("page.record.print.fallbackLabel"); // generic noun — NEVER the raw slug on a customer doc
    let childGroups: PrintChildGroup[] = [];
    try {
      await ensureAuth();
      const [rels, ents] = await Promise.all([listAllRelationships(entity), listEntities()]);
      const labelOf = Object.fromEntries(ents.map((e) => [e.name, e.label]));
      docLabel = labelOf[entity] ?? docLabel;

      const parentRels = rels.filter((r) => r.relationshipType === "1-N" && r.parentEntityName === entity);
      const failedLabels: string[] = [];
      const resolved = await Promise.all(parentRels.map(async (rel): Promise<PrintChildGroup | null> => {
        const fkField = (rel.childFkColumn ?? "").replace(/_id$/, "");
        const label = rel.viewConfig?.label || labelOf[rel.childEntityName] || t("page.record.related.parentGroup");
        if (!fkField) return null;
        try {
          const fields = await listFields(rel.childEntityName);
          // DP-2.1: the child's OWN relation fields print as names (the FK back here is excluded).
          const childRels = fields.filter((f) => f.fieldType === "relation" && f.name !== fkField).map((f) => f.name);
          const page = await listChildRecords(rel.childEntityName, fkField, recordId, PRINT_CHILD_ROW_LIMIT, rel.viewConfig?.sort, childRels);
          if (page.records.length === 0) return null;
          return { rel, label, fields, records: page.records };
        } catch {
          failedLabels.push(label);
          return null;
        }
      }));
      childGroups = resolved.filter((g): g is PrintChildGroup => g != null);
      // CẤM nuốt lỗi im lặng: a quote that prints looking complete but missing its lines is the
      // one failure mode this feature must never hide.
      if (failedLabels.length > 0) {
        toast.warning(t("page.record.print.childFailed").replace("{labels}", failedLabels.join(", ")));
      }
    } catch {
      toast.warning(t("page.record.print.relatedFailed"));
    } finally {
      setPrintData({ entityLabel: docLabel, childGroups, printedAt });
      setPrintState("idle");
      // One paint tick so the print-only block is in the DOM before the browser snapshots it.
      requestAnimationFrame(() => window.print());
    }
  };

  // specs/054: a cell whose visibleWhen fails on the stored record is not shown. Tổng quan, the
  // header row and the printed document all read this one list (same evaluator as the form).
  const sections = visibleSections(layout?.views.detail?.sections ?? [], record?.data ?? {});
  const systemFields = layout?.systemFields ?? [];
  const title = record ? recordLabel(record, allFields) : t("page.record.untitled");
  const highlights = record ? headerHighlights(sections, fieldsByName, record.data, title) : [];

  // W3: "Vừa xem" on Home (per-user localStorage) — only for a record that really loaded. Deps
  // include title: the VIP label upgrades from #shortid to the real name once fields arrive.
  useEffect(() => {
    if (!record || !trackVisit) return;
    recordRecordVisit({ entity, id: record.id, label: title });
  }, [record, entity, title, trackVisit]);

  if (loadState !== "ok" || !record) {
    return (
      <div className="slds-p-around_medium">
        <PageHeader
          variant="record-home"
          icon={RECORD_ICON}
          label={entityLabel}
          title={loadState === "error" ? t("page.record.loadError") : title}
        />
        {loadState === "loading" ? (
          <div className="slds-card slds-m-top_medium">
            <div className="relative h-32" aria-busy="true">
              <Spinner size="medium" label={t("page.record.loadingRecord")} />
            </div>
          </div>
        ) : (
          <ScopedNotification theme="error" className="slds-m-top_medium">
            <strong>{t("page.record.loadError")}</strong> <span>{loadNotFound && notFoundNote ? notFoundNote : loadErr}</span>
          </ScopedNotification>
        )}
      </div>
    );
  }

  const details: PageHeaderDetail[] = [
    ...(record.updatedAt
      ? [{
          label: t("page.record.header.updated"),
          value: <FieldDisplay field={metaToSpec({ name: "updated_at", fieldType: "datetime" })} value={record.updatedAt} />,
        }]
      : []),
    ...highlights.map(({ field, value }) => ({
      label: field.label ?? field.name,
      value: <FieldDisplay field={metaToSpec(field)} value={value} />,
    })),
    // owner_id is the column permissions enforce on; shown next to "Đổi chủ" so a reassignment
    // visibly changes something.
    ...(ownerLabel ? [{ label: t("page.record.header.owner"), value: ownerLabel }] : []),
  ];

  // #43: the entity-level grant (mayEdit) says nothing about THIS record; the server's canEdit does
  // (DAILY-QUEUE #334) — false when my edit scope misses it, also for a share-only record (specs/051:
  // a share is read-only). sharedWithMe alone decides nothing: a share takes no edit right away
  // (view=own, edit=team on a shared team record → canEdit absent, the save answers 200).
  const readOnly = record.canEdit === false;
  // Which notice explains it: seen only through a share (403 RECORD_SHARED_READ_ONLY on every write)…
  const sharedReadOnly = readOnly && !!record.sharedWithMe;
  // …or a record that is someone else's (403 FORBIDDEN, #18).
  const outOfEditScope = readOnly && !sharedReadOnly;
  const canWrite = mayEdit && !readOnly;

  const actions: PageAction[] = [
    ...(onEdit && canWrite ? [{ label: t("page.record.action.edit"), onSelect: onEdit }] : []),
    ...(onClone && mayCreate
      ? [{ label: t("page.record.action.clone"), title: t("page.record.action.cloneTitle"), onSelect: onClone }]
      : []),
    // In — always there: an empty record still prints a valid, if sparse, document.
    {
      label: printState === "preparing" ? t("page.record.action.printPreparing") : t("page.record.action.print"),
      title: t("page.record.action.printTitle"),
      disabled: printState === "preparing",
      onSelect: () => void handlePrint(),
    },
    // Đổi chủ — gone when the record is share-only or outside my edit scope (SetOwner needs edit=all or
    // admin, so either case is a certain 403); otherwise authorization stays server-side and a 403 shows
    // its message in the dialog.
    ...(!readOnly
      ? [{ label: t("page.record.action.owner"), title: t("page.record.action.ownerTitle"), onSelect: () => setOwnerOpen(true) }]
      : []),
    // Chia sẻ — whoever may edit the record may share it (Q4a); the server re-checks the edit scope.
    ...(canWrite
      ? [{ label: t("page.record.action.share"), title: t("page.record.action.shareTitle"), onSelect: () => setShareOpen(true) }]
      : []),
    ...(paymentReady
      ? [{ label: t("page.record.action.qr"), title: t("page.record.action.qrTitle"), onSelect: () => setQrOpen(true) }]
      : []),
    ...(mayDelete && !sharedReadOnly ? [{ label: t("page.record.action.delete"), onSelect: () => { setDelErr(""); setDeleteOpen(true); } }] : []),
  ];

  const panels: Record<TabId, () => ReactNode> = {
    overview: () => (
      <OverviewTab
        sections={sections}
        fieldsByName={fieldsByName}
        record={record}
        systemFields={systemFields}
        names={names}
        tech={isAdminUser ? { entity, recordId } : undefined}
      />
    ),
    // A note's floor is the VIEW scope without the share (specs/060 #349), not the edit scope: a share-only
    // reader may not note, a colleague's record (view=all, edit=own) may be noted.
    notes: () => <NotesTab entity={entity} recordId={recordId} readOnly={!!record.sharedWithMe} />,
    related: () => <RelatedTab entity={entity} recordId={recordId} allFields={allFields} />,
    // Upload and "Gửi duyệt" are edit-gated on the server, so they follow canEdit like Sửa.
    files: () => <FilesTab entity={entity} recordId={recordId} record={record} allFields={allFields} readOnly={readOnly} />,
    history: () => <HistoryTab entity={entity} recordId={recordId} allFields={allFields} />,
    approvals: () => <ApprovalsTab entity={entity} recordId={recordId} readOnly={readOnly} />,
  };
  const TAB_LABELS: Record<TabId, string> = {
    overview: t("page.record.tab.overview"),
    notes: t("page.record.tab.notes"),
    related: t("page.record.tab.related"),
    files: t("page.record.tab.files"),
    history: t("page.record.tab.history"),
    approvals: t("page.record.tab.approvals"),
  };
  // Only the active tab mounts: Quan hệ / Lịch sử / … fire their requests when opened, not on load.
  const tabs = (Object.keys(panels) as TabId[]).map((id) => ({
    value: id,
    label: TAB_LABELS[id],
    content: activeTab === id ? panels[id]() : null,
  }));

  return (
    <div className="slds-p-around_medium">
      {/* print:hidden — the whole interactive page (header, actions, tabs) is app tooling, never the
          customer-facing document; PrintDocument below is what prints (MOT-XUONG W3 #6). */}
      <div className="print:hidden">
        {permsUnknown && (
          <ScopedNotification theme="warning" className="slds-m-bottom_small">
            {t("page.record.permsUnknown")}
          </ScopedNotification>
        )}
        {sharedReadOnly && (
          <ScopedNotification className="slds-m-bottom_small">
            {t("page.record.share.readOnly")}
          </ScopedNotification>
        )}
        {mayEdit && outOfEditScope && (
          <ScopedNotification className="slds-m-bottom_small">
            {t("page.record.editScope.readOnly")}
          </ScopedNotification>
        )}
        <RecordSlaCard entity={entity} recordId={recordId} />
        <RecordHomePage
          icon={RECORD_ICON}
          objectLabel={entityLabel}
          title={title}
          details={details}
          actions={actions}
          maxVisibleActions={4}
          tabs={tabs}
          tab={activeTab}
          onTabChange={(v) => setActiveTab(v as TabId)}
          tabsLabel={t("page.record.tabsLabel")}
        />
      </div>

      {printData && (
        <PrintDocument
          entityLabel={printData.entityLabel}
          title={title}
          orgName={branding.appName}
          printedAt={printData.printedAt}
          sections={sections}
          fieldsByName={fieldsByName}
          record={record}
          childGroups={printData.childGroups}
        />
      )}

      <OwnerDialog
        entity={entity}
        recordId={recordId}
        open={ownerOpen}
        onOpenChange={setOwnerOpen}
        onChanged={handleOwnerChanged}
      />
      <ShareDialog
        entity={entity}
        recordId={recordId}
        currentUserId={user?.id}
        open={shareOpen}
        onOpenChange={setShareOpen}
      />
      <PaymentQRModal entity={entity} recordId={recordId} fields={allFields} open={qrOpen} onOpenChange={setQrOpen} />
      <Modal
        open={deleteOpen}
        onOpenChange={deleting ? () => {} : setDeleteOpen}
        title={t("page.record.delete.title")}
        footer={
          <>
            <Button onClick={() => setDeleteOpen(false)} disabled={deleting}>
              {t("page.record.cancel")}
            </Button>
            <Button variant="destructive" onClick={() => void handleDelete()} disabled={deleting}>
              {deleting ? t("page.record.deleting") : t("page.record.action.delete")}
            </Button>
          </>
        }
      >
        <p>{t("page.record.delete.body").replace("{title}", title)}</p>
        {delErr && <ScopedNotification theme="error" className="slds-m-top_small">{delErr}</ScopedNotification>}
      </Modal>
    </div>
  );
}

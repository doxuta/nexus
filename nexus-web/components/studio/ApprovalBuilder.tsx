"use client";

import Link from "next/link";
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { Plus, SquareCheck, Trash2, User, Users } from "lucide-react";
import {
  ensureAuth, listEntities, listAdminUsers, listRoles, listFields,
  listApprovalProcesses, createApprovalProcess, updateApprovalProcess, deleteApprovalProcess, listWorkQueuesAdmin, ApiError,
  getApprovalProcess, getApprovalImpact, saveApprovalDraft, discardApprovalDraft, publishApprovalProcess,
  type ApprovalImpact, type WorkQueue, type ApprovalOptions, type ApprovalGraph, type ApprovalGraphNode,
  type EntityMeta, type AdminUser, type RoleSummary, type ApprovalProcessSummary, type ApprovalStep, type FieldMeta,
} from "@/lib/api";
import { listTeams, listBusinessCalendars, type AdminTeam, type BusinessCalendar } from "@/lib/admin";
import {
  NODE_KINDS, OWNER_FIELD, buildBody, errorStepIndex, holderSig, newNode, newNodeId, readProcess, removeNode, stepCount, stepError, stepsToNodes, type NodeKind, type ProcessModel,
} from "@/lib/processModel";
import { approvalOpsForType, approvalOpsWithEdge, criteriaMatch, criteriaPayload, criteriaToRows, invalidCriteriaRow, noValueOp, type ApprovalMatch, type CondState } from "@/lib/approvalCriteria";
import type { FilterRow } from "@/lib/filterModel";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { entityOptionText, useEntityOptions } from "@/lib/entityOptions";
import { pickScopedEntityIndex, pickScopedRuleId, scopeEntityOptions, splitByScope } from "@/lib/appScope";
import { PANEL_LABELS } from "@/lib/workshopPanels";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { Combobox, type ComboboxOption } from "@/components/slds/Combobox";
import { EmptyState } from "@/components/slds/EmptyState";
import { ExpressionBuilder } from "@/components/slds/ExpressionBuilder";
import { Icon } from "@/components/slds/Icon";
import { Tabs } from "@/components/slds/Tabs";
import { Textarea } from "@/components/slds/Textarea";
import { StatusText } from "@/components/studio/StatusText";
import { InstancesTab, RepinPanel, VersionsTab } from "@/components/studio/ApprovalVersions";
import { Input } from "@/components/slds/Input";
import { RadioGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Spinner } from "@/components/slds/Spinner";
import { Toggle } from "@/components/slds/Toggle";
import { ApprovalStepEditor, type HolderOptions } from "@/components/studio/ApprovalStepEditor";
import { CondEditor, ProcessNodesEditor } from "@/components/studio/ProcessNodesEditor";
import { Menu, MenuItem } from "@/components/slds/Menu";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import type { DirectApplyGate } from "@/components/studio/useDirectApplyGate";

// Approval Builder (EPIC 4) — multi-step approval workflow per entity.
// Wires GET/POST/PUT/DELETE /admin/approval-processes (verified live 2026-07-01).
//
// CONSTRAINT: trigger must be "after_create" | "after_update" (verified live — NOT "record_create").
//
// ENTRY CRITERIA (slice 1.3): the BE gates each process on a stored condition document reusing
// the WORKFLOW vocabulary — criteria: { match, rules: [{field, op, value?}] }, operators
// eq/neq/gt/gte/lt/lte/contains/isnull/notnull. This screen authors AND only; a stored "or" (API,
// specs/043 S2b) is kept and sent back as-is — the VÀ/HOẶC toggle is FE-BPM-PROPS-b on SLDS.
// Rows are edited with the SLDS ExpressionBuilder (same FilterRow contract as the former Nexus Kit
// FilterBuilder); serialization/hydration live in lib/approvalCriteria (unit-tested).
//
// SLDS 2.264.1 (specs/038 T044, RESEARCH §5 `duyet`): the process list is the `split-view/base`
// blueprint (header + listbox of two-line items, the open one aria-selected, roving tabindex); the
// editor is `cards` — general info (input · select · checkbox-toggle · radio-group), entry criteria
// (`expression`), outcome write-back (select · input) and the steps as `progress-indicator/vertical`
// ("varying stage sizes": each stage's content is that step's editor). A step's approver is a lookup
// (`combobox` autocomplete with the object tile — the preloaded users/roles, so the whole list opens
// on click, unlike components/slds/Lookup's type-to-search) beside a `radio-button-group` for the
// approver kind. Delete confirms in `modals/base`.
// Deviation: the vertical progress bar div (role=progressbar) is omitted — these steps are a
// definition in order, not progress, so the ordered list alone carries the meaning.

const APPROVAL_TRIGGERS = [
  // "manual" is what the record screen's "Gửi duyệt" button arms. Without it here the button
  // 422s APPROVAL_NO_PROCESS on every record — the whole WAVE 16 submit flow was unreachable.
  "manual",
  "after_create",
  "after_update",
] as const;

const newStep = (): ApprovalStep => ({ approverType: "user", approverId: "", slaSeconds: 3600 });

export interface ApprovalBuilderProps {
  /** Delivered project: label + in-place question before a save that has no draft (useDirectApplyGate). */
  applyGate?: DirectApplyGate;
  /** Tables in scope (the page's `?app=`); null/absent = every table of the project. */
  entityNames?: Set<string> | null;
  /** The page's `?bang=`: narrows the scope to this one table. */
  entity?: string;
  /** Name of the app the list is narrowed to, for the sentences; null = not filtered by an app. */
  appLabel?: string | null;
  /** The page is still resolving `?app=`: list nothing yet (a pick on the unscoped list would open another app's process). */
  scopeLoading?: boolean;
  /** Where "Xem tất cả" goes (the same page without `?app=`). */
  showAllHref?: string;
  /** Process to open first (`duyet/<id>`); dropped when the scope hides it. */
  initialProcessId?: string | null;
  /** Called after the open process changes (null = none open), so the page keeps the URL in step. */
  onSelect?: (id: string | null) => void;
}

export function ApprovalBuilder({ entityNames, entity, appLabel = null, scopeLoading = false, showAllHref, initialProcessId = null, onSelect, applyGate }: ApprovalBuilderProps = {}) {
  // App-scope (soft filter, INV-2): quy trình ngoài app ẩn khỏi list (đếm trung thực ở header),
  // entity picker chỉ còn entity của app + giá trị đang lưu. The scope is a PROP of the page (specs/074), not the shared lens.
  const scopeApp = appLabel;
  const scopeEntities = useMemo(() => (entity ? new Set([entity]) : entityNames ?? null), [entity, entityNames]);
  const [processes, setProcesses] = useState<ApprovalProcessSummary[]>([]);
  const [entities, setEntities] = useState<EntityMeta[]>([]);
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [roles, setRoles] = useState<RoleSummary[]>([]);
  const [teams, setTeams] = useState<AdminTeam[]>([]);
  const [queues, setQueues] = useState<WorkQueue[]>([]);
  const [calendars, setCalendars] = useState<BusinessCalendar[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const [sel, setSel] = useState<string | null>(initialProcessId);
  const [loaded, setLoaded] = useState<string | null>(null); // the process whose form is filled in
  const [creating, setCreating] = useState(false);

  // Editor state
  const [name, setName] = useState("");
  // "" until a process is opened or started: a hardcoded default read the fields of a table the
  // tenant may not have (GET /metadata/fields/accounts → 404) before anything was even picked.
  const [entityName, setEntityName] = useState("");
  const [trigger, setTrigger] = useState("after_create");
  const [steps, setSteps] = useState<ApprovalStep[]>([newStep()]);
  // Entry criteria rows (FilterRow) + the trigger entity's fields feeding the row editor's
  // field picker / typed value inputs. Empty rows = the process fires on EVERY create/update.
  const [conds, setConds] = useState<FilterRow[]>([]);
  // The stored join mode, carried through a save unchanged (this screen has no toggle for it).
  const [match, setMatch] = useState<ApprovalMatch>("and");
  const [entityFields, setEntityFields] = useState<FieldMeta[]>([]);
  const [isActive, setIsActive] = useState(false);
  // Outcome (WAVE 20): the field a terminal decision writes back on the record. Empty statusField
  // = no write-back (a decision that only notifies). Loaded and SENT — the builder previously did
  // neither, so every edit through this screen silently WIPED an outcome configured via the API
  // (full-replace update with no outcome key).
  const [outcomeField, setOutcomeField] = useState("");
  const [outcomeApproved, setOutcomeApproved] = useState("");
  const [outcomeRejected, setOutcomeRejected] = useState("");
  const [outcomeReturned, setOutcomeReturned] = useState("");
  // How the process is stored (specs/043 FE-BPM-PROPS-a): a graph of plain approval steps is edited as
  // steps and sent back as a graph; a branching graph is shown read-only and sent back untouched.
  const [graphMode, setGraphMode] = useState(false);
  const [editable, setEditable] = useState(true);
  const [rawGraph, setRawGraph] = useState<ApprovalGraph | null>(null);
  const [options, setOptions] = useState<ApprovalOptions>({});
  const [fallback, setFallback] = useState<ProcessModel["fallback"]>(null);
  // Branching graph (PROPS-b): the node list, and the pause-the-clock condition of options.pauseWhen.
  const [general, setGeneral] = useState(false);
  const [nodes, setNodes] = useState<ApprovalGraphNode[]>([]);
  const [pause, setPause] = useState<CondState | null>(null);
  const usedIds = useRef(new Set<string>());
  // A 422 pinned to a step (index) and the 409 "would erase the graph" question.
  const [stepErr, setStepErr] = useState<{ index: number; message: string } | null>(null);
  const [overwrite, setOverwrite] = useState(false);

  const [saving, setSaving] = useState(false);
  const [saveOk, setSaveOk] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);
  const [delConfirm, setDelConfirm] = useState(false);
  const [delLoading, setDelLoading] = useState(false);
  // FE-BPM-VERSIONS (specs/043 T234-T237): draft/publish, the "N hồ sơ still on the old version" hint, the tabs.
  const [tab, setTab] = useState("definition");
  const [impact, setImpact] = useState<ApprovalImpact | null>(null);
  const [repinOpen, setRepinOpen] = useState(false);
  const [publishOpen, setPublishOpen] = useState(false);
  const [publishNote, setPublishNote] = useState("");
  const [publishing, setPublishing] = useState(false);
  const [discardConfirm, setDiscardConfirm] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    (async () => {
      try {
        await ensureAuth();
        // Teams and queues feed the holder pickers; a failure there only empties those lists.
        const [procs, ents, us, rs, tms, qs, cals] = await Promise.all([
          listApprovalProcesses(),
          listEntities(),
          listAdminUsers(),
          listRoles(),
          listTeams().catch(() => [] as AdminTeam[]),
          listWorkQueuesAdmin().catch(() => [] as WorkQueue[]),
          listBusinessCalendars().catch(() => [] as BusinessCalendar[]),
        ]);
        setTeams(tms ?? []);
        setQueues(qs ?? []);
        setCalendars(cals ?? []);
        setProcesses(procs);
        setEntities(ents);
        setUsers(us);
        setRoles(rs);
        // The first process is opened by the scope-aware pick below, not here: procs[0] is the
        // TENANT's first process, not the first one this app's list shows (queue #456).
      } catch (e) {
        setErr(e instanceof Error ? e.message : t("page.approvalBuilder.loadFailed"));
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  // The criteria field picker needs the trigger entity's fields; refreshed when the entity
  // changes, degrades to an empty list on failure (the panel still renders — same pattern as
  // WorkflowBuilder's lazy entity-fields load).
  useEffect(() => {
    if (!entityName) return;
    let alive = true;
    listFields(entityName)
      .then((fs) => { if (alive) setEntityFields(fs); })
      .catch(() => { if (alive) setEntityFields([]); });
    return () => { alive = false; };
  }, [entityName]);

  function loadProcess(p: ApprovalProcessSummary) {
    // A saved draft is what the maker was last editing: the form opens on it, not on the active version.
    const d = p.draft ?? null;
    const src = d ? { ...p, ...d } : p;
    setName(src.name);
    setEntityName(src.entityName);
    setTrigger(src.trigger);
    setIsActive(d?.isActive ?? p.isActive);
    applyModel(readProcess(src as ApprovalProcessSummary), true);
    setConds(criteriaToRows(src.criteria));
    setMatch(criteriaMatch(src.criteria));
    setOutcomeField(src.outcome?.statusField ?? "");
    setOutcomeApproved(src.outcome?.approvedValue ?? "");
    setOutcomeRejected(src.outcome?.rejectedValue ?? "");
    setOutcomeReturned(src.outcome?.returnedValue ?? "");
    setSaveOk(false); setSaveErr(null); setDelConfirm(false); setStepErr(null); setOverwrite(false);
    setNotice(null); setDiscardConfirm(false); setTab("definition");
  }

  // Takes a process read from the server into the editor. `fresh` = opening it; after a save only the
  // steps/mode are adopted (the maker's other edits are what was just saved).
  function applyModel(m: ProcessModel, fresh: boolean) {
    setSteps(m.steps.length ? m.steps : fresh && m.editable && !m.general ? [newStep()] : m.steps);
    setGraphMode(m.graphMode); setEditable(m.editable); setRawGraph(m.rawGraph);
    setOptions(m.options); setFallback(m.fallback);
    setGeneral(m.general); setNodes(m.nodes); setPause(m.pause);
  }

  function openProcess(p: ApprovalProcessSummary) {
    setSel(p.id); setLoaded(p.id); setCreating(false); loadProcess(p);
    setImpact(null); void refreshImpact(p.id);
  }

  function startCreate() {
    setSel(null); setCreating(true);
    setName(t("page.approvalBuilder.defaultName"));
    // The app's first table — never the tenant's first, nor the table of a process left open from
    // "Xem toàn tenant" (entityOptions keeps that one listed). No table at all: create is disabled.
    setEntityName(entities[createIdx]?.name ?? "");
    setTrigger("after_create");
    applyModel({ steps: [newStep()], graphMode: false, editable: true, general: false, nodes: [], rawGraph: null, options: {}, fallback: null, pause: null }, true);
    setConds([]);
    setMatch("and");
    setIsActive(false);
    setOutcomeField(""); setOutcomeApproved(""); setOutcomeRejected(""); setOutcomeReturned("");
    setSaveOk(false); setSaveErr(null); setDelConfirm(false); setStepErr(null); setOverwrite(false);
  }

  function updateStep(idx: number, next: ApprovalStep) {
    setSteps(steps.map((s, i) => {
      if (i !== idx) return s;
      // A step's name (set by a pack or the AI Builder) describes its approver, and the approval
      // screen leads each step with it (#69). This screen has no name input yet
      // (FE-approval-step-name-input), so re-pointing the step drops the name it can't edit —
      // otherwise "Trưởng phòng duyệt" would label a step someone else now owns.
      if (holderSig(next) !== holderSig(s)) { const { name: _n, ...rest } = next; void _n; return rest; }
      return next;
    }));
  }

  function addStep() {
    setSteps([...steps, newStep()]);
  }

  function moveStep(idx: number, dir: -1 | 1) {
    const to = idx + dir;
    if (to < 0 || to >= steps.length) return;
    const next = [...steps];
    [next[idx], next[to]] = [next[to], next[idx]];
    // A reject target must stay EARLIER than its step (the server refuses a forward jump): a swap can break
    // one, so a rejection that no longer points back is reset to "end" rather than saved into a 422.
    const pos = new Map(next.map((s, i) => [s.id, i]));
    setSteps(next.map((s, i) => {
      const target = s.onReject?.to;
      return target && !target.startsWith("@") && !((pos.get(target) ?? i) < i) ? { ...s, onReject: undefined } : s;
    }));
  }

  // Adding anything but a plain approval step turns a linear/chain process into a node graph (ids kept).
  function addNode(kind: NodeKind) {
    nodes.forEach((n) => n.id && usedIds.current.add(n.id));
    const base = general ? nodes : stepsToNodes(steps, usedIds.current);
    setNodes([...base, newNode(kind, newNodeId(usedIds.current))]);
    setGeneral(true); setGraphMode(true);
    setRawGraph((g) => g ?? { nodes: [] });
  }
  function moveNode(idx: number, dir: -1 | 1) {
    const to = idx + dir;
    if (to < 0 || to >= nodes.length) return;
    const next = [...nodes];
    [next[idx], next[to]] = [next[to], next[idx]];
    setNodes(next);
  }

  function removeStep(idx: number) {
    if (steps.length <= 1) return;
    const gone = steps[idx].id;
    // A rejection that went back to the removed step goes to "end" instead of saving into a 422.
    setSteps(steps.filter((_, i) => i !== idx).map((s) => (gone && s.onReject?.to === gone ? { ...s, onReject: undefined } : s)));
  }

  /** The save body, or null (with the error set) when the form is not valid. Shared by "Cập nhật" and "Lưu nháp". */
  function buildPayload() {
    setStepErr(null);
    // Each step must name who holds it (a read-only graph has no steps here to check).
    for (let i = 0; i < steps.length && editable && !general; i++) {
      const e = stepError(steps[i]);
      if (e) {
        const msg = e === "noField" ? "page.approvalBuilder.err.noFieldN" : e === "percent" ? "page.approvalBuilder.err.percentN" : "page.approvalBuilder.err.noApproverN";
        setStepErr({ index: i, message: tf(msg, { n: i + 1 }) }); return null;
      }
    }
    for (let i = 0; i < nodes.length && general; i++) {
      const n = nodes[i];
      const sla = n.sla as { notify?: unknown } | undefined;
      if (n.kind === "wait" && sla && !sla.notify) { setStepErr({ index: i, message: tf("page.approvalBuilder.err.nodeSlaNotify", { n: i + 1 }) }); return null; }
      if (n.kind === "approval" || n.kind === "task") {
        if (stepError({ approverType: "user", approverId: "", ...n } as ApprovalStep)) { setStepErr({ index: i, message: tf("page.approvalBuilder.err.noApproverN", { n: i + 1 }) }); return null; }
      }
    }
    // Validate criteria rows: a half-filled row must not save (BE would either 422 on a blank
    // field or silently gate on ""), and emptiness checks belong to "rỗng"/"không rỗng". A stored
    // `changed` row has no operator label here, so a value typed on it gets its own message.
    const badRow = invalidCriteriaRow(conds);
    if (badRow) {
      setSaveErr(badRow.op === "changed" && badRow.field.trim()
        ? t("page.approvalBuilder.err.changedValue")
        : t("page.approvalBuilder.err.incompleteRow"));
      return null;
    }
    // No rows → OMIT criteria (BE stores NULL = áp dụng cho mọi bản ghi; on update this
    // clears any previously saved criteria — full-replace semantics).
    const criteria = criteriaPayload(conds, match);
    // Outcome: a value with no field, or a field with no value, is a config that silently does
    // nothing — the BE rejects it (APPROVAL_OUTCOME_*), so mirror that guard here for a clear
    // message rather than a round-trip. An entirely empty block = no write-back (omitted).
    const of = outcomeField.trim(), oa = outcomeApproved.trim(), orj = outcomeRejected.trim();
    if (of && !oa && !orj) { setSaveErr(t("page.approvalBuilder.err.outcomeNoValue")); return null; }
    if (!of && (oa || orj)) { setSaveErr(t("page.approvalBuilder.err.outcomeNoField")); return null; }
    const ort = outcomeReturned.trim();
    const outcome = of ? { statusField: of, ...(oa ? { approvedValue: oa } : {}), ...(orj ? { rejectedValue: orj } : {}), ...(ort ? { returnedValue: ort } : {}) } : undefined;
    // outcome is ALWAYS threaded (even as undefined) so a full-replace update that clears it is
    // explicit — this is the accept-then-swallow the builder used to cause by omitting the key.
    return {
      ...buildBody({ steps, graphMode, editable, general, nodes, rawGraph, options, fallback, pause }, { name, entityName, trigger, isActive }),
      ...(criteria ? { criteria } : {}), outcome,
    };
  }

  async function refreshImpact(id: string) {
    try { setImpact(await getApprovalImpact(id)); } catch { setImpact(null); }
  }

  async function handleSave(force = false) {
    setSaving(true); setSaveOk(false); setSaveErr(null); setOverwrite(false); setNotice(null);
    const payload = buildPayload();
    if (!payload) { setSaving(false); return; }
    try {
      // Adopt the SAVED steps: the server gives every id-less step its stable id (specs/043 S0).
      // Keeping the pre-save steps re-sent a just-added step without it on the next save, and the
      // server re-numbered it by its new position — taking over a deleted step's id (FR-002).
      if (sel && !creating) {
        const updated = await updateApprovalProcess(sel, payload, force ? { force: true } : undefined);
        setProcesses(processes.map((p) => p.id === sel ? updated : p));
        adoptSaved(updated);
        void refreshImpact(sel);
        setReloadKey((n) => n + 1);
      } else {
        const created = await createApprovalProcess(payload);
        setProcesses([...processes, created]);
        setSel(created.id); setLoaded(created.id); setCreating(false);
        adoptSaved(created);
      }
      setSaveOk(true);
    } catch (e) {
      if (e instanceof ApiError && e.status === 409 && e.code === "APPROVAL_PROCESS_V2_OVERWRITE") {
        // Not a failure to retry blindly: saving linear would erase the stored graph. Say so and offer it.
        setOverwrite(true);
      } else {
        // A 422 that names a step (`steps[1].…` / `graph.nodes[1].…`) is shown on that step's card.
        const at = e instanceof ApiError ? errorStepIndex(e.details.field, e.message) : null;
        if (at !== null && e instanceof Error) setStepErr({ index: at, message: e.message });
        else setSaveErr(e instanceof Error ? e.message : t("page.approvalBuilder.err.save"));
      }
    } finally {
      setSaving(false);
    }
  }

  // Adopt what the server stored: it gave every id-less step its stable id (specs/043 S0), and a step
  // with several people / a rule is now a graph — its steps come back as nodes.
  function adoptSaved(saved: ApprovalProcessSummary) {
    const m = readProcess(saved);
    if (m.steps.length || m.general || !m.editable) applyModel(m, false);
  }

  // "Lưu nháp": the same body, stored unpublished — hồ sơ mới keep running the active version.
  async function handleSaveDraft() {
    if (!sel) return;
    setSaving(true); setSaveOk(false); setSaveErr(null); setNotice(null);
    const payload = buildPayload();
    if (!payload) { setSaving(false); return; }
    try {
      const updated = await saveApprovalDraft(sel, payload);
      setProcesses(processes.map((p) => p.id === sel ? updated : p));
      setNotice(t("page.approvalVersions.draftSaved"));
    } catch (e) {
      setSaveErr(e instanceof ApiError && e.code === "APPROVAL_PROCESS_V2_OVERWRITE"
        ? t("page.approvalVersions.err.branching")
        : e instanceof Error ? e.message : t("page.approvalVersions.err.draft"));
    } finally {
      setSaving(false);
    }
  }

  async function handleDiscardDraft() {
    if (!sel) return;
    setSaving(true); setSaveErr(null); setNotice(null);
    try {
      await discardApprovalDraft(sel);
      const fresh = await getApprovalProcess(sel);
      setProcesses(processes.map((p) => p.id === sel ? fresh : p));
      loadProcess(fresh);
      setNotice(t("page.approvalVersions.draftDiscarded"));
    } catch (e) {
      setDiscardConfirm(false);
      setSaveErr(e instanceof Error ? e.message : t("page.approvalVersions.err.discard"));
    } finally {
      setSaving(false);
    }
  }

  async function handlePublish() {
    if (!sel) return;
    setPublishing(true); setSaveErr(null);
    try {
      const res = await publishApprovalProcess(sel, publishNote.trim());
      setProcesses(processes.map((p) => p.id === sel ? res.process : p));
      loadProcess(res.process);
      setImpact(res.impact);
      setReloadKey((n) => n + 1);
      setPublishOpen(false); setPublishNote("");
      setNotice(tf("page.approvalVersions.published", { version: res.version }));
    } catch (e) {
      setPublishOpen(false);
      setSaveErr(e instanceof Error ? e.message : t("page.approvalVersions.err.publish"));
    } finally {
      setPublishing(false);
    }
  }

  // Asks the server how many hồ sơ are waiting BEFORE the confirm opens — a deleted process answers not-found.
  async function askDelete() {
    if (sel) await refreshImpact(sel);
    setDelConfirm(true);
  }

  async function handleDelete() {
    if (!sel) return;
    setDelLoading(true);
    try {
      await deleteApprovalProcess(sel);
      setProcesses(processes.filter((p) => p.id !== sel));
      // Nothing open → the scope-aware pick opens the next process OF THIS APP (or none).
      setSel(null); setCreating(false); setDelConfirm(false);
    } catch (e) {
      setDelConfirm(false);
      setSaveErr(e instanceof Error ? e.message : t("page.approvalBuilder.err.delete"));
    } finally {
      setDelLoading(false);
    }
  }

  const showEditor = sel || creating;
  const editingSaved = Boolean(sel && !creating);
  const proc = processes.find((p) => p.id === sel);

  // App-scope projections: visible process rows + scoped entity picker.
  const { visible: shownProcesses, hidden: hiddenProcs } = splitByScope(
    processes, scopeEntities, (p) => p.entityName,
  );
  // The list waits for the scope too: while it loads, the projection above is unscoped and every
  // app's processes would be listed — clickable — for a moment.
  const listLoading = loading || scopeLoading;

  // Queue #456 (the approval twin of #297): the open process always belongs to the list the maker
  // SEES. Opens the first process of the app-filtered list (never the tenant's first — that opened
  // CRM's "Duyệt hợp đồng", Xoá and Cập nhật live, under an app with no process), re-picks when a
  // scope that lands late hides the open one, and opens nothing when the app has none. Waits out
  // a loading scope: a pick made on the stale one would show another app's process first.
  // Adjusted during render ("adjusting state when a prop changes"), not in an effect: the scope is
  // a hook value, so no event handler owns the pick, and an effect paints the wrong state for a
  // frame first (the empty state on every load) — react-hooks/set-state-in-effect. Converges in
  // one pass: the pick returns `sel`, the first visible id, or null.
  if (!listLoading && !creating) {
    // An id that is not in the list at all (`duyet/<id>` of a deleted process) is dropped like one the scope hides.
    const next = pickScopedRuleId(processes, shownProcesses, processes.some((x) => x.id === sel) ? sel : null);
    // `loaded` covers a process named up front (`duyet/<id>`): it is already `sel`, but its form is still empty.
    if (next !== sel || (next && next !== loaded)) {
      const p = processes.find((x) => x.id === next);
      if (p) openProcess(p);
      else setSel(null);
    }
  }

  // Keep the page's URL on the open process. Waits out the load: before the pick settles `sel` is not yet the open one.
  useEffect(() => {
    if (!listLoading) onSelect?.(creating ? null : sel);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- onSelect is the page's callback, not a trigger
  }, [sel, creating, listLoading]);

  const entityOptions = scopeEntityOptions(entities, scopeEntities, entityName);
  const entityChoices = useEntityOptions(entityOptions).map((o) => ({ value: o.value, label: entityOptionText(o) }));
  // The table a NEW process attaches to: the app's first (no stored value carried over); -1 = the
  // app has no table yet, so there is nothing to draft on — said plainly instead.
  const createIdx = pickScopedEntityIndex(entities.map((e) => e.name), scopeEntities);
  const noTable = createIdx < 0;
  const entityLabel = (n: string) => entities.find((e) => e.name === n)?.label || n;

  const userOptions: ComboboxOption[] = users.map((u) => ({
    value: u.id,
    label: u.fullName || u.email,
    meta: u.fullName ? u.email : undefined,
    icon: { glyph: User, name: "user", label: t("page.approvalBuilder.approverUser") },
  }));
  const roleOptions: ComboboxOption[] = roles.map((r) => ({
    value: r.id,
    label: r.label || r.name,
    icon: { glyph: Users, name: "user-role", label: t("page.approvalBuilder.approverRole") },
  }));

  const holderOptions: HolderOptions = {
    users: userOptions,
    roles: roleOptions,
    teams: teams.map((tm) => ({ value: tm.id, label: tm.name, icon: { glyph: Users, name: "group", label: t("page.approvalBuilder.team") } })),
    queues: queues.filter((q) => q.isActive !== false).map((q) => ({ value: q.id, label: q.label || q.name })),
    // The record's owner first, then every user-type field of the table (the server accepts only those).
    fields: [
      { value: OWNER_FIELD, label: t("page.approvalBuilder.fieldOwner") },
      ...entityFields.filter((f) => f.fieldType === "user").map((f) => ({ value: f.name, label: f.label || f.name })),
    ],
  };

  const addMenu = (
    <div className="flex gap-2">
      <Button iconLeft={Plus} onClick={() => (general ? addNode("approval") : addStep())}>{t("page.approvalBuilder.addStep")}</Button>
      <Menu trigger={<Button>{t("page.approvalBuilder.addNode")}</Button>}>
        {NODE_KINDS.filter((k) => k !== "task" && k !== "approval").map((k) => (
          <MenuItem key={k} label={t(`page.approvalBuilder.node.${k}` as I18nKey)} onSelect={() => addNode(k)} />
        ))}
      </Menu>
    </div>
  );

  const definitionBody = (
    <>
            <Card title={t("page.approvalBuilder.general")} icon={{ glyph: SquareCheck, name: "approval" }}>
              <div className="grid gap-3 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)_auto] sm:items-start">
                <Input label={t("page.approvalBuilder.name")} value={name} onChange={(e) => setName(e.target.value)} />
                <Select
                  label={t("page.approvalBuilder.entity")}
                  placeholder={false}
                  value={entityName}
                  onChange={(e) => setEntityName(e.target.value)}
                  options={entityChoices}
                />
                <Toggle
                  label={t("page.approvalBuilder.status")}
                  onText={t("page.approvalBuilder.active")}
                  offText={t("page.approvalBuilder.inactive")}
                  checked={isActive}
                  onChange={(e) => setIsActive(e.target.checked)}
                />
              </div>
              <RadioGroup
                className="slds-m-top_small"
                label={t("page.approvalBuilder.trigger")}
                value={trigger}
                onValueChange={setTrigger}
                options={APPROVAL_TRIGGERS.map((k) => ({ value: k, label: t(`page.approvalBuilder.trigger.${k}` as I18nKey) }))}
              />
            </Card>

            {/* Entry criteria — reuses the workflow condition vocabulary (rows authored here are AND; a stored OR is kept). */}
            <Card title={t("page.approvalBuilder.criteria")}>
              <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_small">
                {match === "or" ? t("page.approvalBuilder.criteriaAny") : t("page.approvalBuilder.criteriaAll")}
              </p>
              {conds.length === 0 && (
                <ScopedNotification className="slds-m-bottom_small">{t("page.approvalBuilder.criteriaNone")}</ScopedNotification>
              )}
              <ExpressionBuilder
                title={null}
                fields={entityFields}
                value={conds}
                onChange={setConds}
                // "vừa đổi (thành)" only where the server takes it: a process fired by an update (S2b).
                opsFor={trigger === "after_update" ? approvalOpsWithEdge : approvalOpsForType}
                valueless={noValueOp}
                relativeDates={false}
                match={match === "or" ? "any" : "all"}
                {...(conds.length > 1 && {
                  onMatchChange: (m: string) => setMatch(m === "any" ? "or" : "and"),
                  matchOptions: ["all", "any"],
                })}
              />
            </Card>

            {/* Outcome write-back (WAVE 20) — what a terminal decision writes on the record. */}
            <Card title={t("page.approvalBuilder.outcome")}>
              <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_small">{t("page.approvalBuilder.outcomeDesc")}</p>
              <div className="grid gap-3 sm:grid-cols-4">
                <Select
                  label={t("page.approvalBuilder.outcomeField")}
                  placeholder={t("page.approvalBuilder.outcomeNone")}
                  value={outcomeField}
                  onChange={(e) => setOutcomeField(e.target.value)}
                  options={entityFields
                    .filter((f) => f.fieldType === "picklist" || f.fieldType === "text")
                    .map((f) => ({ value: f.name, label: f.label || f.name }))}
                  help={outcomeField ? t("page.approvalBuilder.outcomeHelp") : undefined}
                />
                <Input
                  label={t("page.approvalBuilder.outcomeApproved")}
                  value={outcomeApproved}
                  onChange={(e) => setOutcomeApproved(e.target.value)}
                  disabled={!outcomeField}
                  placeholder={t("page.approvalBuilder.outcomeApprovedPh")}
                />
                <Input
                  label={t("page.approvalBuilder.outcomeRejected")}
                  value={outcomeRejected}
                  onChange={(e) => setOutcomeRejected(e.target.value)}
                  disabled={!outcomeField}
                  placeholder={t("page.approvalBuilder.outcomeRejectedPh")}
                />
                <Input
                  label={t("page.approvalBuilder.returnedValue")}
                  value={outcomeReturned}
                  onChange={(e) => setOutcomeReturned(e.target.value)}
                  disabled={!outcomeField}
                  placeholder={t("page.approvalBuilder.returnedValuePh")}
                />
              </div>
            </Card>

            {/* Steps — progress-indicator/vertical, one stage per step in approval order */}
            {general ? (
              <Card title={t("page.approvalBuilder.steps")} count={nodes.length} actions={addMenu}>
                <div className="slds-progress slds-progress_vertical">
                  <ProcessNodesEditor
                    nodes={nodes}
                    onChange={setNodes}
                    onMove={moveNode}
                    onRemove={(i) => setNodes(removeNode(nodes, nodes[i].id ?? ""))}
                    holders={holderOptions}
                    fields={entityFields}
                    errorAt={stepErr}
                  />
                </div>
              </Card>
            ) : !editable ? (
              <Card title={t("page.approvalBuilder.steps")} count={stepCount({ id: "", name: "", entityName: "", trigger: "", isActive: true, graph: rawGraph })}>
                <ScopedNotification>{t("page.approvalBuilder.graphReadonly")}</ScopedNotification>
              </Card>
            ) : (
              <Card
                title={t("page.approvalBuilder.steps")}
                count={steps.length}
                actions={addMenu}
              >
                <div className="slds-progress slds-progress_vertical">
                  <ol className="slds-progress__list" aria-label={t("page.approvalBuilder.steps")}>
                    {steps.map((step, idx) => (
                      <li key={idx} className="slds-progress__item">
                        <div className="slds-progress__marker" />
                        <div className="slds-progress__item_content slds-grid slds-p-bottom_medium">
                          <ApprovalStepEditor
                            step={step}
                            index={idx}
                            steps={steps}
                            options={holderOptions}
                            error={stepErr?.index === idx ? stepErr.message : null}
                            onChange={(next) => updateStep(idx, next)}
                            onMove={(dir) => moveStep(idx, dir)}
                            onRemove={steps.length > 1 ? () => removeStep(idx) : null}
                          />
                        </div>
                      </li>
                    ))}
                  </ol>
                </div>
              </Card>
            )}

            {/* Process-level fallback: who takes a step nobody holds (specs/043 S6a). */}
            {editable && (
              <Card title={t("page.approvalBuilder.fallbackTitle")}>
                <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_small">{t("page.approvalBuilder.fallbackDesc")}</p>
                <div className="grid gap-3 md:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]">
                  <Select
                    label={t("page.approvalBuilder.fallbackIs")}
                    placeholder={false}
                    value={fallback?.type ?? ""}
                    onChange={(e) => setFallback(e.target.value ? { type: e.target.value as "user" | "role", id: "" } : null)}
                    options={[
                      { value: "", label: t("page.approvalBuilder.fallbackNone") },
                      { value: "user", label: t("page.approvalBuilder.holder.user") },
                      { value: "role", label: t("page.approvalBuilder.holder.role") },
                    ]}
                  />
                  {fallback && (
                    <Combobox
                      variant="autocomplete"
                      label={fallback.type === "user" ? t("page.approvalBuilder.approver") : t("page.approvalBuilder.approverRoleField")}
                      placeholder={fallback.type === "user" ? t("page.approvalBuilder.approverPh") : t("page.approvalBuilder.rolePh")}
                      options={fallback.type === "user" ? userOptions : roleOptions}
                      value={fallback.id || null}
                      onChange={(v) => setFallback({ type: fallback.type, id: v ?? "" })}
                    />
                  )}
                </div>
              </Card>
            )}
            {editable && (
              <Card title={t("page.approvalBuilder.optionsTitle")}>
                <div className="grid gap-3 md:grid-cols-2">
                  <Select
                    label={t("page.approvalBuilder.lane")}
                    placeholder={false}
                    value={options.lane ?? "exclusive"}
                    onChange={(e) => setOptions({ ...options, lane: e.target.value === "exclusive" ? undefined : e.target.value })}
                    options={(["exclusive", "independent"] as const).map((k) => ({ value: k, label: t(`page.approvalBuilder.lane.${k}` as I18nKey) }))}
                  />
                  <Select
                    label={t("page.approvalBuilder.calendar")}
                    placeholder={false}
                    value={options.calendarId ?? ""}
                    onChange={(e) => setOptions({ ...options, calendarId: e.target.value || undefined })}
                    options={[{ value: "", label: t("page.approvalBuilder.calendarDefault") }, ...calendars.map((c) => ({ value: c.id, label: c.name }))]}
                  />
                </div>
                <p className="slds-text-title_bold slds-m-top_small">{t("page.approvalBuilder.pauseTitle")}</p>
                <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_small">{t("page.approvalBuilder.pauseDesc")}</p>
                <CondEditor value={pause ?? { match: "and", rows: [] }} fields={entityFields} onChange={setPause} />
              </Card>
            )}
    </>
  );

  return (
    <div className="flex min-h-0 flex-1 overflow-hidden">
      {/* Process list — split-view/base */}
      <div className="slds-split-view_container slds-is-open slds-border_right w-72 shrink-0 grow-0">
        <article className="slds-split-view slds-grid slds-grid_vertical slds-grow min-h-0">
          <header className="slds-split-view__header">
            <div className="slds-grid slds-grid_vertical-align-center slds-m-bottom_xx-small">
              <div className="slds-has-flexi-truncate">
                <div className="slds-media slds-media_center">
                  <div className="slds-media__figure">
                    <Icon icon={SquareCheck} category="standard" name="approval" size="small" />
                  </div>
                  <div className="slds-media__body">
                    <h2 className="slds-text-heading_small slds-text-color_default slds-p-right_x-small slds-truncate" title={t("page.approvalBuilder.listTitle")}>
                      {t("page.approvalBuilder.listTitle")}
                    </h2>
                  </div>
                </div>
              </div>
              <div className="slds-no-flex slds-grid">
                <ButtonIcon icon={Plus} variant="border-filled" label={t("page.approvalBuilder.new")} disabled={noTable} onClick={startCreate} />
              </div>
            </div>
            {!listLoading && !err && (
              <p className="slds-text-body_small slds-text-color_weak">
                {tf("page.approvalBuilder.count", { n: shownProcesses.length })}
                {scopeApp && <ScopeNote app={scopeApp} hidden={hiddenProcs} showAllHref={showAllHref} />}
              </p>
            )}
          </header>
          <ProcessList
            processes={shownProcesses}
            selected={creating ? null : sel}
            loading={listLoading}
            error={err}
            entityLabel={entityLabel}
            onOpen={openProcess}
          />
        </article>
      </div>

      {/* Editor */}
      {showEditor ? (
        <div className="flex min-w-0 flex-1 flex-col">
          <div className="min-h-0 flex-1 overflow-y-auto p-4">
            {editingSaved && (
              <p className="slds-m-bottom_small">
                <StatusText tone={proc?.draft ? "warn" : "success"}>{proc?.draft ? t("page.approvalVersions.badge.draft") : t("page.approvalVersions.badge.active")}</StatusText>
                {" "}<span className="slds-text-color_weak">{tf("page.approvalVersions.versionN", { n: proc?.version ?? 1 })}</span>
              </p>
            )}
            {editingSaved && proc?.draft && (
              <ScopedNotification className="slds-m-bottom_small">{tf("page.approvalVersions.draftNotice", { version: proc.version ?? 1 })}</ScopedNotification>
            )}
            {editingSaved && impact && impact.pendingOnOldDefinition > 0 && (
              <ScopedNotification theme="warning" className="slds-m-bottom_small">
                {tf("page.approvalVersions.impact", { n: impact.pendingOnOldDefinition })}{" "}
                <Button variant="base" onClick={() => setRepinOpen(true)}>{t("page.approvalVersions.impactMove")}</Button>
              </ScopedNotification>
            )}
            {editingSaved && repinOpen && (
              <RepinPanel
                processId={sel!}
                version={impact?.version ?? proc?.version ?? 1}
                onClose={() => setRepinOpen(false)}
                onDone={() => { void refreshImpact(sel!); setReloadKey((n) => n + 1); }}
              />
            )}
            {editingSaved ? (
              <Tabs
                label={t("page.approvalVersions.tabs")}
                value={tab}
                onValueChange={setTab}
                items={[
                  { value: "definition", label: t("page.approvalVersions.tab.definition"), content: definitionBody },
                  { value: "versions", label: t("page.approvalVersions.tab.versions"), content: <VersionsTab processId={sel!} activeVersion={proc?.version ?? 1} reloadKey={reloadKey} /> },
                  { value: "instances", label: t("page.approvalVersions.tab.instances"), content: <InstancesTab processId={sel!} reloadKey={reloadKey} onChanged={() => void refreshImpact(sel!)} /> },
                ]}
              />
            ) : definitionBody}
          </div>

          {saveErr && (
            <ScopedNotification theme="error" className="slds-m-horizontal_medium slds-m-bottom_x-small">{saveErr}</ScopedNotification>
          )}
          {overwrite && (
            <ScopedNotification theme="warning" className="slds-m-horizontal_medium slds-m-bottom_x-small">
              {t("page.approvalBuilder.overwrite")}{" "}
              <Button variant="text-destructive" disabled={saving} onClick={() => void handleSave(true)}>{t("page.approvalBuilder.overwriteBtn")}</Button>
            </ScopedNotification>
          )}
          {/* Footer: delete · status · save */}
          {delConfirm && (
            <div className="slds-border_top px-4 py-3">
              <InlineConfirm
                tone="error"
                message={tf("page.approvalBuilder.deleteBody", { name }) + (impact && impact.pendingTotal > 0 ? ` ${tf("page.approvalVersions.deleteImpact", { n: impact.pendingTotal })}` : "")}
                confirmLabel={delLoading ? t("action.deleting") : t("page.approvalBuilder.deleteConfirm")}
                busy={delLoading}
                onConfirm={() => void handleDelete()}
                onCancel={() => setDelConfirm(false)}
              />
            </div>
          )}
          {publishOpen && (
            <div className="slds-border_top px-4 py-3">
              <p className="slds-text-heading_small slds-m-bottom_x-small">{t("page.approvalVersions.publishTitle")}</p>
              <p className="slds-m-bottom_small">{tf("page.approvalVersions.publishBody", { version: (proc?.version ?? 1) + 1 })}</p>
              <Textarea label={t("page.approvalVersions.publishNote")} placeholder={t("page.approvalVersions.publishNotePh")} value={publishNote} onChange={(e) => setPublishNote(e.target.value)} />
              <div className="flex gap-2 slds-m-top_small">
                <Button variant="brand" disabled={publishing} onClick={() => void handlePublish()}>{t("page.approvalVersions.publish")}</Button>
                <Button onClick={() => setPublishOpen(false)}>{t("page.approvalVersions.cancel")}</Button>
              </div>
            </div>
          )}
          {discardConfirm && (
            <div className="slds-border_top px-4 py-3">
              <InlineConfirm
                tone="error"
                message={tf("page.approvalVersions.discardDraftBody", { name })}
                confirmLabel={t("page.approvalVersions.discardDraftConfirm")}
                busy={saving}
                onConfirm={() => void handleDiscardDraft()}
                onCancel={() => setDiscardConfirm(false)}
              />
            </div>
          )}
          {applyGate?.confirm && <div className="slds-border_top px-4 py-3">{applyGate.confirm}</div>}
          <div className="slds-theme_default slds-border_top flex flex-wrap items-center gap-2 px-4 py-3">
            {editingSaved && !delConfirm && (
              <Button variant="text-destructive" iconLeft={Trash2} onClick={() => void askDelete()}>
                {t("page.approvalBuilder.delete")}
              </Button>
            )}
            {editingSaved && proc?.draft && !discardConfirm && (
              <Button variant="text-destructive" disabled={saving} onClick={() => setDiscardConfirm(true)}>
                {t("page.approvalVersions.discardDraft")}
              </Button>
            )}
            <span className="ml-auto" />
            {applyGate?.notice}
            {notice && <StatusText tone="success">{notice}</StatusText>}
            {saveOk && <Badge theme="success">{t("page.approvalBuilder.saved")}</Badge>}
            {editingSaved && (
              <Button disabled={saving} onClick={() => void handleSaveDraft()}>{t("page.approvalVersions.saveDraft")}</Button>
            )}
            {editingSaved && proc?.draft && (
              <Button disabled={saving} onClick={() => setPublishOpen(true)}>{t("page.approvalVersions.publish")}</Button>
            )}
            <Button variant="brand" disabled={saving} onClick={() => (applyGate ? applyGate.ask(() => void handleSave()) : void handleSave())}>
              {saving ? t("action.saving") : editingSaved ? t("page.approvalBuilder.update") : t("page.approvalBuilder.create")}
            </Button>
          </div>
        </div>
      ) : (
        <div className="flex min-w-0 flex-1 flex-col items-center justify-center p-4">
          {!listLoading && (noTable ? (
            <EmptyState
              size="large"
              icon={SquareCheck}
              title={scopeApp ? tf("page.approvalBuilder.noTableApp", { app: scopeApp }) : t("page.approvalBuilder.noTable")}
              description={tf("page.approvalBuilder.noTableDesc", { panel: PANEL_LABELS.bang })}
            />
          ) : (
            <EmptyState
              size="large"
              icon={SquareCheck}
              title={t("page.approvalBuilder.emptyTitle")}
              description={t("page.approvalBuilder.emptyDesc")}
              action={
                <Button variant="brand" iconLeft={Plus} onClick={startCreate}>
                  {t("page.approvalBuilder.create")}
                </Button>
              }
            />
          ))}
        </div>
      )}

    </div>
  );
}

// "Đang lọc theo app X · n quy trình khác ẩn · Xem toàn tenant" — same wording as the table list
// (DataModelBuilder). Mounted only while scoped: useActiveApp needs the workspace provider.
// "Xem tất cả" is a plain link to the page without `?app=` (specs/074), not a button on the shared lens.
function ScopeNote({ app, hidden, showAllHref }: { app: string; hidden: number; showAllHref?: string }) {
  return (
    <>
      {" · "}{tf("page.approvalBuilder.scopeNote", { app })}
      {hidden > 0 && <>{" · "}{tf("page.approvalBuilder.scopeHidden", { n: hidden })}</>}
      {showAllHref && <>{" · "}<Link href={showAllHref}>{t("studio.build.data.showAll")}</Link></>}
    </>
  );
}

// split-view list: ul[role=listbox] > li.slds-split-view__list-item > a[role=option], one tab stop
// (the focused process while focus is in the list — so Tab leaves it — else the open one, else the
// first), ArrowUp/Down/Home/End move focus, Enter/Space/click open.
function ProcessList({ processes, selected, loading, error, entityLabel, onOpen }: {
  processes: ApprovalProcessSummary[];
  selected: string | null;
  loading: boolean;
  error: string | null;
  entityLabel: (name: string) => string;
  onOpen: (p: ApprovalProcessSummary) => void;
}) {
  const refs = useRef<(HTMLAnchorElement | null)[]>([]);
  const [focused, setFocused] = useState<number | null>(null);
  if (loading) {
    return (
      <div className="relative h-24">
        <Spinner size="small" label={t("page.approvalBuilder.loading")} />
      </div>
    );
  }
  if (error) return <ScopedNotification theme="error" className="slds-m-around_small">{error}</ScopedNotification>;
  if (processes.length === 0) {
    return <p className="slds-p-around_medium slds-text-body_small slds-text-color_weak">{t("page.approvalBuilder.listEmpty")}</p>;
  }
  const stop = focused ?? Math.max(0, processes.findIndex((p) => p.id === selected));
  const onKey = (e: KeyboardEvent<HTMLAnchorElement>, i: number) => {
    const to = e.key === "ArrowDown" ? i + 1 : e.key === "ArrowUp" ? i - 1 : e.key === "Home" ? 0 : e.key === "End" ? processes.length - 1 : null;
    if (to !== null) {
      e.preventDefault();
      refs.current[Math.min(Math.max(to, 0), processes.length - 1)]?.focus();
    } else if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onOpen(processes[i]);
    }
  };
  return (
    <ul
      className="slds-scrollable_y"
      role="listbox"
      aria-label={t("page.approvalBuilder.listTitle")}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget)) setFocused(null);
      }}
    >
      {processes.map((p, i) => (
        <li key={p.id} className="slds-split-view__list-item" role="presentation">
          <a
            ref={(el) => { refs.current[i] = el; }}
            role="option"
            aria-selected={p.id === selected}
            tabIndex={i === stop ? 0 : -1}
            className="slds-split-view__list-item-action slds-grow slds-has-flexi-truncate"
            onFocus={() => setFocused(i)}
            onClick={() => onOpen(p)}
            onKeyDown={(e) => onKey(e, i)}
          >
            <div className="slds-grid slds-wrap">
              <span className="slds-truncate slds-text-body_regular slds-text-color_default" title={p.name}>{p.name}</span>
            </div>
            <div className="slds-grid slds-wrap">
              <span className="slds-truncate">
                {tf("page.approvalBuilder.itemMeta", { entity: entityLabel(p.entityName), n: stepCount(p) })}
              </span>
              <span className="slds-truncate slds-col_bump-left">
                {p.isActive ? t("page.approvalBuilder.active") : t("page.approvalBuilder.inactive")}
              </span>
            </div>
          </a>
        </li>
      ))}
    </ul>
  );
}

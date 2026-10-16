"use client";

import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import {
  Plus, Zap, Mail, Bell, PencilLine, Webhook, Trash2, X, History, FlaskConical, Clock, Users,
  FilePlus2, Timer, Hourglass, ChevronDown, ShieldBan, Power, Eraser, Stamp, type LucideIcon,
} from "lucide-react";
import {
  ensureAuth, getWorkflowSchedulerStatus, createWorkflow, updateWorkflow, deleteWorkflow, setWorkflowActive, disableWorkflowNow, listWorkflows, listRecentWorkflowRuns, listEntities, listFields, listApprovalProcesses, ApiError,
  type ApprovalProcessSummary, type WorkflowSummary, type EntityMeta, type FieldMeta,
} from "@/lib/api";
import { listBusinessCalendars, listTeams, type BusinessCalendar, type AdminTeam } from "@/lib/admin";
import { errorMessage } from "@/lib/errorMessages";
import { emitDraftsChanged, onDraftsChanged } from "@/lib/draftEvents";
import { toast } from "@/components/ui/Toaster";
import { failedWorkflowIds, chunkIds } from "@/lib/ruleLastRun";
import { wf01Field, actionRowPointer, FIELD_TARGET_ACTIONS, triggerLabel } from "@/lib/workflowRuns";
import { OP_MAP, condToRule, isNullOp, ruleValueText } from "@/lib/workflowConds";
import { calendarsHref } from "@/lib/studioRoutes";
import { isFilterableType, nextFilterId, type FilterField, type FilterRow } from "@/lib/filterModel";
import {
  isBeforeTrigger, beforeActionOffered, beforeTriggerLabel, actionAllowed, beforeErrorSlot, edgeOpsOff, saveOnOffered, EDGE_OPS, type SaveOn,
} from "@/lib/workflowBefore";
import {
  TIMER_UNITS, TIMER_DIRECTIONS, TIMER_CREATED_AT, TIMER_UPDATED_AT, REPEAT_UNITS, MAX_REPEAT_COUNT, timerPreview, timerFromCondition, type WorkflowTimer,
  DEADLINE_UNITS, DEADLINE_POLICY_KEYS, deadlinePreview, deadlineParamsFromAction, deadlinePolicyPayload,
} from "@/lib/workflowTimer";
import { entityOptionText, useEntityOptions } from "@/lib/entityOptions";
import { pickScopedEntityIndex, pickScopedRuleId, scopeEntityOptions, splitByScope } from "@/lib/appScope";
import { PANEL_LABELS } from "@/lib/workshopPanels";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { BeforeRuleEditor } from "@/components/studio/before/BeforeRuleEditor";
import { MutateActionEditor } from "@/components/studio/before/MutateActionEditor";
import { NormalizeActionEditor } from "@/components/studio/before/NormalizeActionEditor";
import { normParamsFromAction, normPayload } from "@/lib/workflowNormalize";
import { VetoActionEditor } from "@/components/studio/before/VetoActionEditor";
import { WorkflowRunHistory, RecentRunsStrip } from "./WorkflowRunHistory";
import { WorkflowTestDialog } from "./WorkflowTestDialog";
import { InlineConfirm } from "./InlineConfirm";
import { switchOffNeedsDisableNow } from "@/lib/workflowToggle";
import { draftStateLabel, StatusText } from "./StatusText";
import { UnsavedBar } from "./UnsavedBar";
import { TemplateFieldPicker } from "./TemplateFieldPicker";
import { appendToken } from "@/lib/workflowTemplateVars";
import { FieldInput } from "@/components/fields/widgets";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Checkbox } from "@/components/slds/Checkbox";
import { fill } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { ExpressionBuilder } from "@/components/slds/ExpressionBuilder";
import { Icon } from "@/components/slds/Icon";
import { Input } from "@/components/slds/Input";
import { Menu, MenuItem } from "@/components/slds/Menu";
import { PageHeader } from "@/components/slds/PageHeader";
import { RadioGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Combobox } from "@/components/slds/Combobox";
import { Select } from "@/components/slds/Select";
import { NotifyRecipientPicker } from "@/components/studio/NotifyRecipientPicker";
import { Spinner } from "@/components/slds/Spinner";
import { Toggle } from "@/components/slds/Toggle";
import { metaToSpec } from "@/lib/fieldSpec";
import { emptyDefault, READONLY_FIELD_TYPES, SYSTEM_FIELDS } from "@/lib/recordPayload";
import {
  crValuePairs, crPairsToString, crEmitValues, crHydrateRows,
  CR_SUPPORTED_TYPES, OWNER_ELIGIBLE_TYPES, OWNER_SENTINEL, type CrRow,
} from "@/lib/workflowCreateRecord";
import { fuParamsFromAction, fuPayload, fuTargetEntity, fuTargetFields, fuValueMissing } from "@/lib/workflowFieldUpdate";
import { FieldUpdateEditor } from "./FieldUpdateEditor";
import { TriggerValueSource } from "./TriggerValueSource";
import { isFromTrigger } from "@/lib/workflowValueSource";

// Workflow Builder (EPIC 4) — KHI → NẾU → THÌ. Nexus internal builder tool.
// Wires POST/PUT/DELETE /admin/workflows (verified live 2026-07-01).
// W6: editor tabs (Cấu hình | Lịch sử chạy — §9.3), "Chạy thử" dry-run dialog (§9.4),
// WF-01 422 (details.field) mapped INLINE onto the offending field-target action.
//
// SLDS (specs/038 T043) — no blueprint for a flow builder, so the screen is composed from basic
// ones (RESEARCH-2026-09 §5.2): `split-view` for the rule list (the SLDS list + detail pattern; its
// items are buttons with aria-current=page — the blueprint's listbox/option would forbid the
// per-rule history button beside each item), `page-headers` base with the on/off `checkbox-toggle`
// and "Chạy thử", `tabs`, `cards` for KHI / NẾU / THÌ with one nested boundary card per action,
// `radio-group` for the trigger, `expression` for conditions (the workflow vocabulary:
// 11 engine operators, "changed" takes no value, no relative dates — the engine resolves none),
// `menus` for "Thêm hành động", `prompt` for delete. Tailwind is layout only.
//
// ACTION CONSTRAINTS (verified live):
//   in_app_notification: { type, userId:"$owner", title, message }  (NOT recipient)
//   field_update: { type, via?, field, value }  (via = relation field → write the RELATED record, specs/058)
//   email_alert: { type, to, subject, body }
//   outbound_webhook: { type, url }
//   create_record: { type, entityName, values:{field:value,...} }  (CREATE-RECORD; "$owner" = record owner)
//   set_deadline: { type, field, offset:{amount,unit}, calendarId? }  (field must be `datetime`;
//                 calendarId pins a working calendar, business_hours only — queue #79)
//   set_elapsed: { type, field }  (field must be `number`; working minutes since created_at — specs/046)
//   start_approval: { type, processId }  (manual process of the rule's entity — specs/043 S9b)
//   veto: { type, message, field? }  (before_save / before_delete only — specs/048; message 1–500 chars)

// WF-01 config-time codes (api-conventions §9.1) — all carry details.field.
const WF01_CODES = new Set([
  "RECORD_UNKNOWN_FIELD",
  "RECORD_FORMULA_READONLY",
  "RECORD_LOOKUP_READONLY",
  "RECORD_ROLLUP_READONLY",
  "RECORD_AUTONUMBER_READONLY",
  // Same family, same details.field payload: the target-type guards of the field-targeting
  // actions (services.buildActionsJSON). Without these here the message goes to the footer.
  "ASSIGN_FIELD_TYPE_INVALID",
  "SET_DEADLINE_FIELD_TYPE_INVALID",
  "SET_ELAPSED_FIELD_TYPE_INVALID",
  // specs/058: details.field = the relation a field_update follows — pinned on its record picker.
  "WORKFLOW_RELATED_VIA_INVALID",
  // Queue #282: a field_update value is checked on save with the record validator's own codes
  // (details.field = the target) — pinned on that action's value box.
  "RECORD_INVALID_TYPE",
  "RECORD_INVALID_PICKLIST",
  "RECORD_INVALID_RANGE",
  "RECORD_REQUIRED_MISSING",
  // specs/048 S4/S5: the before_save mutations anchor on details.field — the target they rewrite.
  "NORMALIZE_OPS_INVALID",
  "NORMALIZE_FIELD_TYPE_INVALID",
  "WORKFLOW_BEFORE_VALUE_INVALID",
  "WORKFLOW_BEFORE_TARGET_UNSUPPORTED",
]);

// TIMER-1: date_offset fires at dateField ± offset (condition.timer — backend date_offset trigger).
// specs/048: before_save / before_delete run INSIDE the save or delete and can refuse it; their
// shape (saveOn, the veto action, no edge operators on create/delete) lives in lib/workflowBefore.
export const TRIGGERS = ["after_create", "after_update", "after_delete", "scheduled", "date_offset", "before_save", "before_delete"] as const;
const triggerOption = (key: string) => t(`page.workflow.trigger.${key}` as I18nKey);

// TIMER-1 default timer config for a fresh date_offset rule.
const DEFAULT_TIMER: WorkflowTimer = { dateField: "", offset: { amount: 3, unit: "days" }, direction: "before" };

// `tile` = the SLDS standard icon colour of the action's card — a name SLDS 2.264.1 defines
// (slds-icon-standard-<tile>); an unknown one paints no background and hides the white glyph.
export const ACTION_TYPES: { key: string; icon: LucideIcon; tile: string }[] = [
  { key: "field_update", icon: PencilLine, tile: "record-update" },
  { key: "in_app_notification", icon: Bell, tile: "announcement" },
  { key: "email_alert", icon: Mail, tile: "email" },
  { key: "outbound_webhook", icon: Webhook, tile: "link" },
  // ASSIGN-1/ASSIGN-2: distribute a team into a text field (strategy = round_robin | load_based).
  { key: "assign", icon: Users, tile: "assignment" },
  // CREATE-RECORD: create a NEW record of a target entity (e.g. a follow-up task). "$owner" as a
  // value resolves to the trigger record's owner. Recursion is depth-capped server-side.
  { key: "create_record", icon: FilePlus2, tile: "record" },
  // set_deadline (#9): compute a commitment instant into a datetime field — "phiếu Khẩn xong
  // trong 4 giờ" as software instead of as a verbal convention. field_update cannot express it:
  // it writes a constant, and no formula field can produce a date.
  { key: "set_deadline", icon: Timer, tile: "entitlement" },
  // set_elapsed (#41, specs/046): the other half of set_deadline — MEASURE how many working minutes
  // the record lived from created_at to the moment the rule runs, into a number field.
  { key: "set_elapsed", icon: Hourglass, tile: "timesheet-entry" },
  // start_approval (specs/043 S9b): send the record through a MANUAL approval process of the same entity.
  { key: "start_approval", icon: Stamp, tile: "approval" },
  // normalize (specs/048 S5): clean what a person typed (trim / lower / upper / collapse spaces). before_save only.
  { key: "normalize", icon: Eraser, tile: "record-update" },
  // veto (specs/048): refuse the save/delete with a sentence the user reads. Before rules only.
  { key: "veto", icon: ShieldBan, tile: "restriction-policy" },
];
const actionLabel = (key: string) => t(`page.workflow.action.${key}` as I18nKey);

// Condition operators: the engine's 11 (lib/workflowConds OP_MAP — eq/neq/gt/gte/lt/lte/contains/
// isnull/notnull/changed/changedTo), offered for every field type as before. W0-QW2 still holds:
// no "in" — the backend does not take it.
const WF_OPS: readonly string[] = Object.values(OP_MAP);
// specs/048: "vừa đổi"/"vừa đổi thành" never fire on a create or a delete — not offered there.
const WF_OPS_NO_EDGE: readonly string[] = WF_OPS.filter((op) => !EDGE_OPS.includes(op));
const wfOps = () => WF_OPS;
const wfOpsNoEdge = () => WF_OPS_NO_EDGE;

// CREATE-RECORD values are stored in the action's flat params as a JSON string of [field, value]
// PAIRS (params.values) so the rows editor stays fully controlled (order + blank rows preserved, no
// local state to drift when switching rules). The value is now WIDGET-NATIVE (type-aware, #80) —
// parse/serialize + the widget↔backend transforms live in lib/workflowCreateRecord.

// The keys each action card edits — toActionPayload writes them. Every OTHER key of a stored action
// rides along verbatim (ActionConfig.keep): an action type with no card here (normalize) or a key a
// later backend slice adds before this screen learns it survives "open → Cập nhật" instead of being
// dropped or stringified (queue #282 — the old loader ran every such value through String(v)). A
// webhook's own secret/headers count as edited and are never sent: the read masks them and every write
// refuses them (specs/050 FR-025), so the server answers 409 WEBHOOK_LEGACY_SECRET_PENDING and the
// stored key stays where it is.
const CARD_KEYS: Record<string, readonly string[]> = {
  field_update: ["field", "via", "childEntity", "childVia", "value", "onlyIfEmpty", "valueSource"],
  in_app_notification: ["userId", "title", "message"],
  email_alert: ["to", "subject", "body"],
  outbound_webhook: ["url", "credentialId", "path", "secret", "headers"],
  assign: ["strategy", "team", "field"],
  create_record: ["entityName", "values"],
  set_deadline: ["field", "offset", ...DEADLINE_POLICY_KEYS],
  set_elapsed: ["field"],
  veto: ["message", "field"],
  start_approval: ["processId"],
  normalize: ["field", "ops"],
};

// Produce backend-valid action payloads (verified live against :8090). crFields maps a create_record
// target entity → its field metas (loaded when the entity is picked) so the value editor emits the
// SAME per-type shape a normal record create sends (#80). A box left empty is sent empty — the server
// answers with the real code instead of this layer inventing a value the user never saw.
function toActionPayload(
  type: string,
  params: Record<string, string> = {},
  crFields: Record<string, FieldMeta[]> = {},
  rule: { entity: string; fields: FieldMeta[] } = { entity: "", fields: [] },
): Record<string, unknown> {
  switch (type) {
    case "field_update": {
      // specs/058: the value is typed for the field of the record it lands on — the rule's own
      // entity, or the entity the `via` relation points at. An empty field is sent as-is so the
      // server answers WORKFLOW_ACTION_INVALID instead of this layer inventing "status".
      return fuPayload(params, fuTargetFields(rule.entity, rule.fields, crFields, params.via, params.childEntity));
    }
    case "in_app_notification":
      // userId is REQUIRED (not recipient); the box shows "$owner" (record owner) until typed over.
      // The title is optional: an empty one is not sent, as the server stores none.
      return { type, userId: params.userId ?? "$owner", ...(params.title ? { title: params.title } : {}), message: params.message ?? "" };
    case "email_alert":
      return { type, to: params.to ?? "", subject: params.subject ?? "", body: params.body ?? "" };
    case "outbound_webhook":
      // specs/050 S2b: a rule that uses a vault key keeps {credentialId, path} — never a url. The key
      // picker is S9 (FE-named-credentials-builder); until then such a rule is set by API.
      if (params.credentialId) return { type, credentialId: params.credentialId, ...(params.path ? { path: params.path } : {}) };
      return { type, url: params.url ?? "" };
    case "assign":
      // ASSIGN-1/ASSIGN-2: strategy is round_robin (default) or load_based (fewest active records).
      // team + field come from the dropdowns; empty values fail config validation server-side
      // (ASSIGN_TEAM_INVALID / ASSIGN_FIELD_REQUIRED) rather than persisting a dead rule.
      return { type, strategy: params.strategy || "round_robin", team: params.team || "", field: params.field || "" };
    case "create_record": {
      // CREATE-RECORD: target entity + type-aware field→value map (#80). An empty entity / values
      // fails config validation server-side (WORKFLOW_ACTION_INVALID / CREATE_RECORD_VALUES_REQUIRED).
      const entity = params.entityName || "";
      return { type, entityName: entity, values: crEmitValues(crValuePairs(params.values), crFields[entity] ?? []) };
    }
    case "set_deadline": {
      // set_deadline: the target must be a `datetime` field and the amount a positive integer.
      // An empty/zero value is left as-is so the server answers with the real code
      // (WORKFLOW_ACTION_INVALID / SET_DEADLINE_OFFSET_INVALID / SET_DEADLINE_FIELD_TYPE_INVALID)
      // instead of this layer inventing a plausible default that hides the mistake.
      const amount = Number.parseInt(params.amount ?? "", 10);
      return {
        type,
        field: params.field || "",
        offset: { amount: Number.isFinite(amount) ? amount : 0, unit: params.unit || "hours" },
        ...deadlinePolicyPayload(params),
      };
    }
    // start_approval (specs/043 S9b): the process is picked; an empty/stale one is refused by the server
    // (WF_START_APPROVAL_INVALID) and pinned on the card.
    case "start_approval":
      return { type, processId: params.processId ?? "" };
    // set_elapsed (#41): the target must be a `number` field. Empty is sent as-is so the server
    // answers with the real code (WORKFLOW_ACTION_INVALID) instead of a plausible default.
    case "set_elapsed":
      return { type, field: params.field || "" };
    // veto (specs/048): the message is sent as typed — an empty/over-long one is refused by the
    // server (WORKFLOW_VETO_MESSAGE_INVALID) and pinned on the message box. No anchor = no key.
    case "veto":
      return { type, message: params.message ?? "", ...(params.field ? { field: params.field } : {}) };
    // normalize (specs/048 S5): ops is an ordered array — sent as stored, never "trim,lower".
    case "normalize":
      return normPayload(params);
    default:
      return { type };
  }
}

// keep = the stored keys no card edits (CARD_KEYS), verbatim and typed; sent back under the card's own.
type ActionConfig = { type: string; params: Record<string, string>; keep?: Record<string, unknown> };

// A stored condition rule → an expression row. The backend operator token is kept as-is (condToRule
// passes it through); a null operator's absent value becomes "".
const ruleToRow = (r: { field: string; op: string; value?: unknown }): FilterRow => ({
  id: nextFilterId(),
  field: r.field,
  op: r.op,
  value: ruleValueText(r.value),
});

const DEFAULT_ACTION: ActionConfig = { type: "in_app_notification", params: { userId: "$owner", title: "Thông báo", message: "Workflow đã kích hoạt." } };

const DRAFT_SOURCE = "workflow-builder";

/** A new rule opened from `luat/moi?bang=&khi=&ten=` (specs/074 lát f2-rule-page). */
export interface RuleDraft { entity?: string; trigger?: string; name?: string }

export interface WorkflowBuilderProps {
  /** The app the page is filtered to (`?app=`); entityNames = its tables, null = the whole project. Props, not a hook:
   *  the page decides the scope, so a rule of another app is never opened by the "active app" lens (specs/074). */
  app?: { label: string } | null;
  entityNames?: Set<string> | null;
  scopeLoading?: boolean;
  /** Rule to open once the list has loaded. Not in the (filtered) list ⇒ dropped, nothing else opens in its place. */
  initialRuleId?: string;
  /** A list row was clicked / a new rule was saved: the host writes the id to the URL. Without it the builder opens the rule itself. */
  onSelectRule?: (id: string) => void;
  onDeleted?: () => void;
  newDraft?: RuleDraft;
  /** hideList: only the rule (the rule page); the list is build/RuleListPage. */
  hideList?: boolean;
}

export function WorkflowBuilder({
  app: scopeApp = null, entityNames: scopeEntities = null, scopeLoading = false,
  initialRuleId, onSelectRule, onDeleted, newDraft, hideList = false,
}: WorkflowBuilderProps = {}) {
  const calendarsLink = calendarsHref(usePathname());
  const [workflows, setWorkflows] = useState<WorkflowSummary[]>([]);
  const [entities, setEntities] = useState<EntityMeta[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [toggleErr, setToggleErr] = useState<string | null>(null);
  // #337: rules whose newest run failed (workflowId → that run's id) and the run to open in the history tab.
  const [failedRuns, setFailedRuns] = useState<Map<string, string>>(new Map());
  const [openRunId, setOpenRunId] = useState<string | null>(null);
  const [sel, setSel] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);

  // Editor state
  const [name, setName] = useState("");
  // "" until a rule is opened or started: a hardcoded default read the fields of an entity the
  // tenant may not have (GET /metadata/fields/accounts → 404) before anything was even picked.
  const [entityName, setEntityName] = useState("");
  const [trigger, setTrigger] = useState("after_create");
  // P2 #15: intervalSeconds — only relevant when trigger === "scheduled".
  const [intervalSeconds, setIntervalSeconds] = useState<number>(3600);
  // #276: free typing buffer for the "Chạy" box — floor/fallback applied on blur, not per keystroke.
  const [intervalDraft, setIntervalDraft] = useState<string | null>(null);
  // TIMER-1: date_offset config — only relevant when trigger === "date_offset".
  const [timer, setTimer] = useState<WorkflowTimer>(DEFAULT_TIMER);
  // specs/048: condition.saveOn of a before_save rule ("" = create and update).
  const [saveOn, setSaveOn] = useState<SaveOn>("");
  // The rule entity's fields: condition fields, timer anchor (date/datetime), assign target
  // (user/text), set_deadline target (datetime).
  const [entityFields, setEntityFields] = useState<FieldMeta[]>([]);
  // CREATE-RECORD (#80): field metas of each create_record TARGET entity (may differ from the rule's
  // own entity), keyed by entity name. Drives the type-aware value inputs + the emit/hydrate typing.
  const [crFields, setCrFields] = useState<Record<string, FieldMeta[]>>({});
  const [calendars, setCalendars] = useState<BusinessCalendar[]>([]);
  // ASSIGN-1: teams for the assign-action team dropdown (tenant-global, loaded lazily).
  const [teams, setTeams] = useState<AdminTeam[]>([]);
  const [conds, setConds] = useState<FilterRow[]>([]);
  // Condition join mode: "and" (all rules) | "or" (any rule). Only meaningful with ≥2 rules.
  const [condMatch, setCondMatch] = useState<"and" | "or">("and");
  const [actions, setActions] = useState<ActionConfig[]>([DEFAULT_ACTION]);
  const [isActive, setIsActive] = useState(false);

  const [saving, setSaving] = useState(false);
  const [saveOk, setSaveOk] = useState(false);
  // specs/063: the last save went into the DRAFT of a delivered workspace (the customer does not see it yet).
  const [savedToDraft, setSavedToDraft] = useState(false);
  // "Tắt ngay cho khách" (FR-017): confirm prompt + in-flight flag.
  const [disableOpen, setDisableOpen] = useState(false);
  const [disabling, setDisabling] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);
  const [delConfirm, setDelConfirm] = useState(false);
  const [delLoading, setDelLoading] = useState(false);

  // W6: editor tab (config | runs), dry-run dialog, per-action-row WF-01 errors (idx → message).
  // Unsaved-changes baseline (specs/074 lát 11): bumped whenever the form is (re)filled or saved; the bar shows when the form differs from it.
  const [loadSeq, setLoadSeq] = useState(0);
  const [baseSnap, setBaseSnap] = useState<string | null>(null);
  const runsRef = useRef<HTMLElement>(null);
  const [testOpen, setTestOpen] = useState(false);
  // Keys: "<i>" = the action's target field; "<i>.calendarId" = its pinned calendar (queue #79).
  const [actionErrors, setActionErrors] = useState<Record<string, string>>({});
  // The field name the pinned 422 names — lets a field_update card put the red on its record picker
  // (a `via`) rather than on its field picker (specs/058).
  const [actionErrField, setActionErrField] = useState<string | null>(null);
  // specs/048: server refusals of a veto message (idx → message) and of condition.saveOn.
  const [vetoMsgErrors, setVetoMsgErrors] = useState<Record<number, string>>({});
  const [saveOnErr, setSaveOnErr] = useState<string | undefined>();

  // ASSIGN-1: whether any action is an assign — arms the teams load below.
  const hasAssign = actions.some((a) => a.type === "assign");
  // start_approval: the processes a rule may start (loaded lazily, only when such an action exists).
  const hasStartApproval = actions.some((a) => a.type === "start_approval");
  const [processes, setProcesses] = useState<ApprovalProcessSummary[]>([]);
  useEffect(() => {
    if (!hasStartApproval) return;
    let alive = true;
    listApprovalProcesses().then((rows) => { if (alive) setProcesses(rows); }, () => { if (alive) setProcesses([]); });
    return () => { alive = false; };
  }, [hasStartApproval]);

  // The entity's fields feed the condition rows (every rule has them now that conditions pick a field
  // instead of typing one), the timer anchor, the assign target and the set_deadline target (#64: an
  // after_create deadline rule must see its datetime fields). Refreshed when the entity changes;
  // failure degrades to an empty list (the selects still render created_at).
  useEffect(() => {
    if (!entityName) return;
    let alive = true;
    (async () => {
      try {
        const fields = await listFields(entityName);
        if (alive) setEntityFields(fields);
      } catch {
        if (alive) setEntityFields([]);
      }
    })();
    return () => { alive = false; };
  }, [entityName]);

  // The calendar pickers need the tenant's calendars: the date_offset timer (TIMER-1) and every
  // set_deadline action, which may pin its own (queue #79) — and must SHOW a pin it already has.
  const needsCalendars = trigger === "date_offset" || actions.some((a) => a.type === "set_deadline");
  useEffect(() => {
    if (!needsCalendars) return;
    let alive = true;
    listBusinessCalendars().then((cals) => { if (alive) setCalendars(cals); }, () => { if (alive) setCalendars([]); });
    return () => { alive = false; };
  }, [needsCalendars]);
  // #291: a "scheduled" rule never fires while the server timer is off — say so before saving.
  const [schedulerOff, setSchedulerOff] = useState(false);
  useEffect(() => {
    if (trigger !== "scheduled") return;
    let alive = true;
    getWorkflowSchedulerStatus().then((s) => { if (alive) setSchedulerOff(!s.enabled); }, () => { /* unknown ≠ off: stay silent */ });
    return () => { alive = false; };
  }, [trigger]);
  const calendarNames = useMemo(() => Object.fromEntries(calendars.map((c) => [c.id, c.name])), [calendars]);

  // ASSIGN-1: the assign team dropdown needs the tenant's teams. Loaded lazily (only when an
  // assign action exists) and degrades to an empty list on failure.
  useEffect(() => {
    if (!hasAssign) return;
    let alive = true;
    (async () => {
      try {
        const rows = await listTeams();
        if (alive) setTeams(rows);
      } catch {
        if (alive) setTeams([]);
      }
    })();
    return () => { alive = false; };
  }, [hasAssign]);

  // CREATE-RECORD (#80): fetch the field metas of every create_record target entity referenced by an
  // action so the value editor can render type-aware inputs + emit correctly-typed values. Keyed by
  // entity name and cached (a target's fields are fetched once). Covers user-driven entity selection;
  // loadWorkflow pre-fetches on edit-load so it can reverse stored typed values in one pass.
  // specs/058: a field_update following a relation needs the RELATED entity's fields the same way.
  // specs/084: the "mọi bản ghi con" choices come from the OTHER entities' relation fields, so a
  // field_update card loads them all (once, cached in crFields).
  // ponytail: N listFields calls on first open of a field_update; a server-side "children of X" index if tenants grow past ~50 entities.
  const hasFieldUpdate = actions.some((a) => a.type === "field_update");
  const crEntityKey = [...new Set([
    ...actions.map((a) => a.type === "create_record" ? a.params.entityName || ""
      : a.type === "field_update" && (a.params.via || a.params.childEntity) ? fuTargetEntity(entityName, entityFields, a.params.via, a.params.childEntity) ?? "" : ""),
    ...(hasFieldUpdate ? entities.map((e) => e.name).filter((n) => n !== entityName) : []),
  ])]
    .filter(Boolean)
    .sort()
    .join(",");
  useEffect(() => {
    const wanted = crEntityKey ? crEntityKey.split(",") : [];
    const missing = wanted.filter((e) => !crFields[e]);
    if (missing.length === 0) return;
    let alive = true;
    (async () => {
      const fetched = await Promise.all(
        missing.map((e) => listFields(e).then((fs) => [e, fs] as const).catch(() => [e, [] as FieldMeta[]] as const)),
      );
      if (alive) setCrFields((prev) => ({ ...prev, ...Object.fromEntries(fetched) }));
    })();
    return () => { alive = false; };
  }, [crEntityKey]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    (async () => {
      try {
        await ensureAuth();
        const [wfs, ents] = await Promise.all([listWorkflows(), listEntities()]);
        setWorkflows(wfs);
        setEntities(ents);
        // The first rule is opened by the scope-aware effect below, not here: wfs[0] is the
        // TENANT's first rule, not the first one this app's list shows (queue #297).
      } catch (e) {
        setErr(e instanceof Error ? e.message : t("page.workflow.loadFailed"));
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  // Every inline (per-control) refusal of the last save, cleared together.
  function clearFieldErrors() {
    setActionErrors({}); setActionErrField(null); setVetoMsgErrors({}); setSaveOnErr(undefined);
  }

  // Guards the async loadWorkflow below: rapid switching between two create_record rules could let an
  // earlier listFields resolve last and clobber setActions with the wrong rule's rows. Each load takes
  // a token; a stale load (token superseded) skips its post-fetch setActions.
  const loadTokenRef = useRef(0);

  // Async: a create_record rule's stored values are typed (currency object, number, bool…). To hydrate
  // them back into the widget-native inputs (#80, item #5) we need the TARGET entity's field metas, so
  // we pre-fetch those before reversing (reverseValue is non-idempotent for currency — run it once).
  // specs/063: a publish / discard in the draft bar changes every rule's draftState — reload the list.
  useEffect(() => onDraftsChanged((source) => {
    if (source === DRAFT_SOURCE) return; // our own announcement: this screen has just refreshed itself
    listWorkflows().then(setWorkflows, () => { /* keep the list we have */ });
  }), []);

  async function loadWorkflow(wf: WorkflowSummary) {
    const token = ++loadTokenRef.current;
    setName(wf.name);
    setEntityName(wf.entityName ?? "");
    setTrigger(wf.trigger ?? "after_create");
    // P2 #15: restore intervalSeconds — ALWAYS from the rule (0 = fire once is a real value); a rule
    // without one falls back to the default so the previously-opened rule's cadence never leaks in.
    const rawInterval = (wf as Record<string, unknown>).intervalSeconds;
    setIntervalSeconds(typeof rawInterval === "number" && rawInterval >= 0 ? rawInterval : 3600);
    setIsActive(!!wf.isActive);
    setSaveOk(false); setSavedToDraft(false); setSaveErr(null); setDelConfirm(false); clearFieldErrors();
    setLoadSeq((n) => n + 1);
    // Restore condition rules if present (backend operator tokens kept; null operators carry no value).
    const rawCond = (wf as Record<string, unknown>).condition as { match?: string; saveOn?: string; rules?: Array<{ field: string; op: string; value?: unknown }> } | null;
    setSaveOn(rawCond?.saveOn === "create" || rawCond?.saveOn === "update" ? rawCond.saveOn : "");
    setConds(rawCond?.rules ? rawCond.rules.map(ruleToRow) : []);
    setCondMatch(rawCond?.match?.toLowerCase() === "or" ? "or" : "and");
    // TIMER-1: restore the timer block for a date_offset rule (stored inside condition JSON).
    setTimer(timerFromCondition(rawCond) ?? DEFAULT_TIMER);
    // Restore actions
    const rawActions = (wf as Record<string, unknown>).actions as Array<Record<string, unknown>> | null;
    if (!rawActions?.length) {
      setActions([DEFAULT_ACTION]);
      return;
    }
    // Pre-fetch field metas for every create_record target so crHydrateRows can reverse typed values
    // in one pass. No create_record action → no fetch (instant); the fetch caches into crFields too.
    const crEntities = [...new Set(
      rawActions.filter((a) => a.type === "create_record").map((a) => String(a.entityName ?? "")).filter(Boolean),
    )];
    let fmap: Record<string, FieldMeta[]> = {};
    if (crEntities.length) {
      const fetched = await Promise.all(
        crEntities.map((e) => listFields(e).then((fs) => [e, fs] as const).catch(() => [e, [] as FieldMeta[]] as const)),
      );
      fmap = Object.fromEntries(fetched);
      setCrFields((prev) => ({ ...prev, ...fmap }));
    }
    // A newer load superseded this one while we awaited listFields — don't clobber its rows.
    if (loadTokenRef.current !== token) return;
    // The card's own keys → its params (per type below); every other key → `keep`, verbatim.
    const cardParams = (a: Record<string, unknown>): ActionConfig => {
      // CREATE-RECORD round-trip: `values` is a typed OBJECT → reverse each value into its widget-native
      // form (currency object → decimal string, ISO date → DD/MM…) so the type-aware inputs hydrate.
      if (a.type === "create_record") {
        const ent = String(a.entityName ?? "");
        const vals = a.values && typeof a.values === "object" ? (a.values as Record<string, unknown>) : {};
        const rows = crHydrateRows(vals, fmap[ent] ?? crFields[ent] ?? []);
        return { type: "create_record", params: { entityName: ent, values: crPairsToString(rows) } };
      }
      // set_deadline carries a NESTED offset — the plain-string mapping below would lose it.
      if (a.type === "set_deadline") {
        return { type: "set_deadline", params: deadlineParamsFromAction(a) };
      }
      // field_update keeps its stored value VERBATIM (lib/workflowFieldUpdate `raw`): the old generic
      // mapper stringified it, so re-saving a rule turned `value: 100` into `"100"`.
      if (a.type === "field_update") {
        return { type: "field_update", params: fuParamsFromAction(a) };
      }
      if (a.type === "normalize") return { type: "normalize", params: normParamsFromAction(a) };
      // Every other card edits plain strings.
      const edited = CARD_KEYS[String(a.type)] ?? [];
      return {
        type: String(a.type),
        params: Object.fromEntries(Object.entries(a).filter(([k, v]) => edited.includes(k) && typeof v === "string")) as Record<string, string>,
      };
    };
    setActions(rawActions.map((a) => {
      const edited = new Set(["type", ...(CARD_KEYS[String(a.type)] ?? [])]);
      const keep = Object.fromEntries(Object.entries(a).filter(([k]) => !edited.has(k)));
      return Object.keys(keep).length ? { ...cardParams(a), keep } : cardParams(a);
    }));
  }

  function startCreate() {
    setSel(null); setCreating(true);
    // The app's first table — never the tenant's first, nor the table of a rule left open from
    // "Xem toàn tenant" (entityOptions keeps that one listed). No table at all: create is disabled.
    setName(t("page.workflow.newName")); setEntityName(entities[createIdx]?.name ?? "");
    setTrigger("after_create"); setIntervalSeconds(3600); setTimer(DEFAULT_TIMER); setConds([]); setCondMatch("and"); setIsActive(false);
    setSaveOn(""); setActions([DEFAULT_ACTION]);
    setSaveOk(false); setSavedToDraft(false); setSaveErr(null); setDelConfirm(false); clearFieldErrors();
    setLoadSeq((n) => n + 1);
  }

  function handleSelectWorkflow(wf: WorkflowSummary) {
    setOpenRunId(null);
    setSel(wf.id); setCreating(false); loadWorkflow(wf);
  }

  // W6 entry point: open a rule STRAIGHT into its run history (list row + recent-runs strip).
  // Pause / resume straight from the list: its own write, so a rule that still holds a pre-vault key
  // can be switched off without a PUT (which would answer 409 and loop with the Doctor refusal).
  async function toggleActive(wf: WorkflowSummary) {
    setToggleErr(null);
    // A rule the customer is running stops NOW (063 disable-now), asked in place — a draft switch would leave it running until Publish.
    if (switchOffNeedsDisableNow(wf)) {
      handleSelectWorkflow(wf);
      setDisableOpen(true);
      return;
    }
    try {
      const next = !wf.isActive;
      await setWorkflowActive(wf.id, next);
      setWorkflows((cur) => cur.map((x) => (x.id === wf.id ? { ...x, isActive: next } : x)));
      if (wf.id === sel && !creating) setIsActive(next);
      // On a delivered workspace the switch is a draft edit: re-read so the Nháp badge and the bar's count follow.
      if (wf.draftState) {
        setWorkflows(await listWorkflows());
        emitDraftsChanged(DRAFT_SOURCE);
      }
    } catch (e) {
      setToggleErr(e instanceof Error && e.message ? e.message : t("page.workflow.list.toggleFailed"));
    }
  }

  function openRunHistory(wf: WorkflowSummary, runId: string | null = null) {
    handleSelectWorkflow(wf);
    setOpenRunId(runId);
    setTimeout(() => runsRef.current?.scrollIntoView?.({ block: "start" }), 0);
  }

  function buildPayload() {
    // TIMER-1: a date_offset rule ALWAYS sends a condition (it carries the timer block, exactly
    // where the backend stores it). calendarId only travels when actually picked.
    const timerBlock = trigger === "date_offset"
      ? {
          dateField: timer.dateField,
          offset: { amount: timer.offset.amount, unit: timer.offset.unit },
          direction: timer.direction,
          ...(timer.repeat ? { repeat: timer.repeat } : {}),
          ...(timer.calendarId ? { calendarId: timer.calendarId } : {}),
        }
      : undefined;
    const beforeSaveOn = saveOnOffered(trigger) ? saveOn : "";
    return {
      name,
      entityName,
      trigger,
      // P2 #15: only send intervalSeconds when trigger is "scheduled".
      ...(trigger === "scheduled" ? { intervalSeconds } : {}),
      // condToRule OMITS `value` for isnull/notnull/changed (backend rejects a value on them).
      // match only bites with ≥2 rules; collapse a stale "or" left over from deleting rules down to 1.
      // specs/048: saveOn travels only on a before_save rule, and only when narrowed ("" = both).
      condition: conds.length || timerBlock || beforeSaveOn
        ? {
            match: conds.length > 1 ? condMatch : "and",
            rules: conds.map((c) => condToRule({ field: c.field, op: c.op, val: c.value })),
            ...(timerBlock ? { timer: timerBlock } : {}),
            ...(beforeSaveOn ? { saveOn: beforeSaveOn } : {}),
          }
        : undefined,
      actions: actions.map((a) => ({ ...a.keep, ...toActionPayload(a.type, a.params, crFields, { entity: entityName, fields: entityFields }) })),
      isActive,
    };
  }

  async function handleSave() {
    setSaveOk(false); setSavedToDraft(false); setSaveErr(null); clearFieldErrors();
    // A "Cập nhật trường" with a field but no value would reach the backend as value=null and CLEAR
    // that field on every run (specs/058 §2.9). Refuse here, on the value box, before any request.
    const missing: Record<string, string> = {};
    actions.forEach((a, i) => {
      if (a.type === "field_update" && fuValueMissing(a.params, fuTargetFields(entityName, entityFields, crFields, a.params.via, a.params.childEntity))) {
        missing[i] = t("page.workflow.param.fuValueRequired");
      }
    });
    if (Object.keys(missing).length > 0) {
      setActionErrors(missing);
      setActionErrField(null);
      setSaveErr(t("page.workflow.save.fuValueMissing"));
      return;
    }
    setSaving(true);
    try {
      let savedId = sel;
      if (sel && !creating) {
        await updateWorkflow(sel, buildPayload());
      } else {
        const created = await createWorkflow(buildPayload());
        savedId = created.id;
        setSel(created.id); setCreating(false);
        openedRef.current = created.id; // the URL the host writes next must not reload the rule we are holding
        onSelectRule?.(created.id);
      }
      setSaveOk(true);
      setLoadSeq((n) => n + 1);
      const refreshed = await listWorkflows();
      setWorkflows(refreshed);
      // A delivered workspace answers with draftState: the save went to the draft, not to the customer.
      const st = refreshed.find((w) => w.id === savedId)?.draftState;
      setSavedToDraft(st === "edited" || st === "new");
      emitDraftsChanged(DRAFT_SOURCE); // the draft bar recounts
    } catch (e) {
      // specs/048: a before-rule refusal that belongs to one control goes there (message box of the
      // veto action, "Áp dụng khi"); the footer keeps the concrete sentence too.
      const slot = e instanceof ApiError ? beforeErrorSlot(e.code) : null;
      if (e instanceof ApiError && slot === "vetoMessage") {
        const hits: Record<number, string> = {};
        actions.forEach((a, i) => { if (a.type === "veto") hits[i] = e.message; });
        setVetoMsgErrors(hits);
      } else if (e instanceof ApiError && slot === "saveOn") {
        setSaveOnErr(e.message);
      }
      // WF-01 (§9.1): 422 pointing at a computed/unknown field-target → pin the message onto the
      // offending action(s), not a generic footer error. Field name comes from details.field
      // (contract) or the quoted name in the message (live fallback — wf01Field).
      const badField = e instanceof ApiError && WF01_CODES.has(e.code)
        ? wf01Field(e.details, e.message)
        : null;
      // Queue #79: SET_DEADLINE_CALENDAR_* point at the action ROW by index
      // (details.field = "actions[i].calendarId") → onto that row's calendar dropdown, with the
      // mapped sentence that names the way out (the raw one carries a UUID and engine words).
      const pinRow = e instanceof ApiError ? actionRowPointer(e.details) : null;
      // WF_START_APPROVAL_INVALID names its action as a JSON pointer ("/actions/2").
      const saIdx = e instanceof ApiError && e.code === "WF_START_APPROVAL_INVALID" && typeof e.details?.field === "string"
        ? /^\/actions\/(\d+)$/.exec(e.details.field)?.[1] : undefined;
      // SET_DEADLINE_STAMP_FIELD_INVALID: details.field = the offending stampField (absent when tighten_only
      // has none) -> the stamp box of the set_deadline action(s) that name it.
      if (e instanceof ApiError && e.code === "SET_DEADLINE_STAMP_FIELD_INVALID") {
        const bad = typeof e.details?.field === "string" ? e.details.field : "";
        const hits: Record<string, string> = {};
        actions.forEach((a, i) => {
          if (a.type === "set_deadline" && (!bad || (a.params.stampField ?? "") === bad)) hits[`${i}.stampField`] = e.message;
        });
        setActionErrors(hits);
        setSaveErr(e.message);
      } else if (saIdx !== undefined && e instanceof ApiError) {
        setActionErrors({ [saIdx]: e.message });
        setSaveErr(e.message);
      } else if (pinRow?.key === "calendarId") {
        const msg = errorMessage(e);
        setActionErrors({ [`${pinRow.index}.calendarId`]: msg });
        setSaveErr(msg);
      } else if (e instanceof ApiError && badField) {
        // Raw BE message is builder-specific ("workflow không được ghi đè") — prefer it inline.
        const msg = e.message || errorMessage(e);
        const hits: Record<string, string> = {};
        actions.forEach((a, i) => {
          if (FIELD_TARGET_ACTIONS.has(a.type) && (a.params.field ?? "") === badField) hits[i] = msg;
          if (a.type === "field_update" && (a.params.via === badField || a.params.childVia === badField)) hits[i] = msg;
        });
        if (Object.keys(hits).length > 0) {
          setActionErrors(hits);
          setActionErrField(badField);
          // Keep the CONCRETE message in the footer as well, not just "fix the red row". The row is
          // matched by FIELD NAME, and a field name is not unique across a rule: `checkCreateRecordTargets`
          // reports `details.field` from the create_record TARGET entity, so a rule with
          // set_deadline(due_date) + create_record(values.due_date) pins the red on the innocent
          // action. When the pin lands on the wrong row, the footer is the only place left that still
          // says what is actually wrong.
          setSaveErr(fill(t("page.workflow.save.fieldErrPinned"), { msg, field: badField }));
        } else {
          setSaveErr(fill(t("page.workflow.save.fieldErr"), { msg, field: badField }));
        }
      } else {
        setSaveErr(errorMessage(e));
      }
    } finally {
      setSaving(false);
    }
  }

  // "Tắt ngay cho khách" (specs/063 FR-017): switches the LIVE rule off at once; drafts stay untouched.
  async function handleDisableNow() {
    if (!sel) return;
    const ruleName = name;
    setDisabling(true);
    try {
      const off = await disableWorkflowNow(sel);
      toast.success(tf("workflow.disableNow.done", { name: ruleName }));
      setDisableOpen(false);
      // The editor must show what is live now: a rule with no draft that still read "on" would, on the
      // next Cập nhật, be saved as a draft that Publish silently turns back on (its base is the OFF live rule).
      // A rule that has a draft keeps the draft's own switch — Publish reports the conflict on it by design.
      if (wf?.draftState === "live") setIsActive(!!off.isActive);
      setWorkflows(await listWorkflows());
      emitDraftsChanged(DRAFT_SOURCE);
    } catch (e) {
      setDisableOpen(false);
      setSaveErr(tf("workflow.disableNow.failed", { msg: e instanceof Error ? e.message : String(e) }));
    } finally {
      setDisabling(false);
    }
  }

  async function handleDelete() {
    if (!sel) return;
    setDelLoading(true);
    try {
      await deleteWorkflow(sel);
      const refreshed = await listWorkflows();
      setWorkflows(refreshed);
      emitDraftsChanged(DRAFT_SOURCE);
      // Nothing open → the scope-aware effect opens the next rule OF THIS APP (or none).
      setSel(null); setCreating(false); setDelConfirm(false);
      onDeleted?.();
    } catch (e) {
      // Close the prompt so the footer that carries the message is visible again.
      setDelConfirm(false);
      setSaveErr(e instanceof Error ? e.message : t("page.workflow.delete.failed"));
    } finally {
      setDelLoading(false);
    }
  }

  function updateActionParam(idx: number, key: string, val: string) {
    setActions(actions.map((a, i) => i === idx ? { ...a, params: { ...a.params, [key]: val } } : a));
    // Editing an action clears its inline errors (fresh validation happens on save).
    if (actionErrors[idx] || actionErrors[`${idx}.calendarId`]) {
      setActionErrors((prev) => {
        const next = { ...prev };
        delete next[idx];
        delete next[`${idx}.calendarId`];
        return next;
      });
    }
    if (vetoMsgErrors[idx]) {
      setVetoMsgErrors((prev) => {
        const next = { ...prev };
        delete next[idx];
        return next;
      });
    }
  }

  // One setActions for an editor that changes several params at once (FieldUpdateEditor: picking a
  // different record resets field + value together — updateActionParam twice would race).
  function replaceActionParams(idx: number, params: Record<string, string>) {
    setActions(actions.map((a, i) => i === idx ? { ...a, params } : a));
    if (actionErrors[idx]) {
      setActionErrors((prev) => {
        const next = { ...prev };
        delete next[idx];
        return next;
      });
    }
  }

  function removeAction(idx: number) {
    setActions(actions.filter((_, j) => j !== idx));
    // Indices shift on removal — drop all inline errors instead of remapping stale ones.
    setActionErrors({}); setVetoMsgErrors({});
  }

  // Condition field list: the entity's filterable fields, plus any field a saved rule names that the
  // list lacks (system column, deleted field — shown by name instead of an empty picker), and a saved
  // picklist value outside the field's values kept as an option, so opening a rule never hides what
  // it says.
  const condFields = useMemo<FilterField[]>(() => {
    const list: FilterField[] = entityFields.filter((f) => isFilterableType(f.fieldType)).map((f) => {
      if (f.fieldType !== "picklist" && f.fieldType !== "multi_select") return f;
      const values = Array.isArray(f.options?.values) ? (f.options.values as unknown[]).map(String) : [];
      const extra = [...new Set(conds.filter((c) => c.field === f.name && c.value && !values.includes(c.value)).map((c) => c.value))];
      return extra.length ? { ...f, options: { ...f.options, values: [...values, ...extra] } } : f;
    });
    for (const c of conds) {
      if (c.field && !list.some((f) => f.name === c.field)) list.push({ name: c.field, label: c.field, fieldType: "text" });
    }
    return list;
  }, [entityFields, conds]);

  const wf = workflows.find((w) => w.id === sel) ?? null;
  const showEditor = wf || creating;
  const saved = !!sel && !creating;
  // The form as the user sees it; compared with the snapshot taken when it was filled/saved.
  const formSnap = JSON.stringify([name, entityName, trigger, intervalSeconds, timer, saveOn, conds, condMatch, actions, isActive]);
  useEffect(() => { setBaseSnap(formSnap); }, [loadSeq]); // eslint-disable-line react-hooks/exhaustive-deps
  const dirty = !!showEditor && baseSnap !== null && formSnap !== baseSnap;
  // specs/048: the server cannot rehearse a STORED before rule yet (previewAction has no veto branch) —
  // a dry-run saying "không được hỗ trợ" would read as a broken rule. Opens with FE-before-save-hooks-4
  // (queue #188). Its run history (blocked / skipped runs, S3) reads in the runs tab like any rule.
  // draftState exists ONLY on a delivered workspace read by the builder: a rule that is live (or live with
  // a pending edit) can be switched off for the customer right now; a draft-only rule has nothing live.
  const canDisableNow = saved && !!wf && switchOffNeedsDisableNow(wf);

  // App-scope projections: visible rule rows + scoped entity picker (stored value giữ nguyên).
  const { visible: shownWorkflows } = useMemo(
    () => splitByScope(workflows, scopeEntities, (w) => w.entityName),
    [workflows, scopeEntities],
  );
  // The list (and the recent-runs strip under it) waits for the scope too: while it loads, the
  // projection above is unscoped, and every app's rules were listed — clickable — for a moment.
  const listLoading = loading || scopeLoading;

  // #337: one read per group of <= 100 listed rules (server cap), newest first; the mark rests on the
  // first row of each rule. Non-fatal side signal — on error no mark rather than a false alarm.
  const shownIdsKey = shownWorkflows.map((w) => w.id).join(",");
  useEffect(() => {
    if (listLoading || shownIdsKey === "") return;
    let cancelled = false;
    (async () => {
      try {
        const pages = await Promise.all(chunkIds(shownIdsKey.split(",")).map((ids) => listRecentWorkflowRuns(100, ids)));
        if (cancelled) return;
        const merged = new Map<string, string>();
        for (const runs of pages) for (const [wid, rid] of failedWorkflowIds(runs)) merged.set(wid, rid);
        setFailedRuns(merged);
      } catch {
        if (!cancelled) setFailedRuns(new Map());
      }
    })();
    return () => { cancelled = true; };
  }, [listLoading, shownIdsKey]);

  // Queue #297: the open rule always belongs to the list the maker SEES. Opens the first rule of
  // the app-filtered list (never the tenant's first — that opened Ticketing's SLA rule, Xoá and
  // Cập nhật live, under an app with zero rules), re-picks when a scope that arrives late hides
  // the open rule (the workspace syncs the active app in an effect; the app's nav loads after),
  // and opens nothing when the app has no rule. Waits out a loading scope: a pick made on the
  // stale one would flash another app's rule first (the scope stays loading across an app switch —
  // lib/apps.tsx never lets one app's nav stand in for another's).
  // initialRuleId (specs/074): that rule is opened once, if the filtered list shows it; if not, nothing opens.
  const openedRef = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (!initialRuleId || loading || scopeLoading || creating || openedRef.current === initialRuleId) return;
    openedRef.current = initialRuleId;
    const w = shownWorkflows.find((x) => x.id === initialRuleId);
    if (w) handleSelectWorkflow(w);
    else setSel(null);
  }, [initialRuleId, loading, scopeLoading, creating, shownWorkflows]); // eslint-disable-line react-hooks/exhaustive-deps

  // newDraft (specs/074): `luat/moi` opens a blank rule on the named table, once, after the tables are known.
  const draftedRef = useRef(false);
  useEffect(() => {
    if (!newDraft || loading || scopeLoading || draftedRef.current) return;
    draftedRef.current = true;
    startCreate();
    const ok = newDraft.entity && entities.some((e) => e.name === newDraft.entity) && (!scopeEntities || scopeEntities.has(newDraft.entity));
    if (ok) setEntityName(newDraft.entity as string);
    if (newDraft.trigger && (TRIGGERS as readonly string[]).includes(newDraft.trigger)) setTrigger(newDraft.trigger);
    if (newDraft.name) setName(newDraft.name);
  }, [newDraft, loading, scopeLoading]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (loading || scopeLoading || creating || initialRuleId || newDraft) return;
    const next = pickScopedRuleId(workflows, shownWorkflows, sel);
    if (next === sel) return;
    const w = workflows.find((x) => x.id === next);
    if (w) handleSelectWorkflow(w);
    else setSel(null);
  }, [loading, scopeLoading, creating, workflows, shownWorkflows, sel]); // eslint-disable-line react-hooks/exhaustive-deps
  const entityOptions = scopeEntityOptions(entities, scopeEntities, entityName);
  // The table a NEW rule attaches to (queue #457, the twin of ApprovalBuilder #456): the app's first;
  // -1 = the app has no table yet, so there is nothing to draft on — said plainly instead.
  const createIdx = pickScopedEntityIndex(entities.map((e) => e.name), scopeEntities);
  const noTable = createIdx < 0;
  const entityChoices = useEntityOptions(entityOptions).map((o) => ({ value: o.value, label: entityOptionText(o) }));
  const entityLabel = (n?: string) => (n ? entities.find((e) => e.name === n)?.label ?? n : "—");

  const edgeOff = edgeOpsOff(trigger, saveOn);
  const edgeRows = edgeOff ? conds.filter((c) => EDGE_OPS.includes(c.op)) : [];

  const config = (
    <div className="flex flex-col">
      <Group title={t("studio.build.automation.page.info")}>
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
          <Input label={t("page.workflow.name")} required value={name} onChange={(e) => setName(e.target.value)} />
          {/* Entity picker — scoped theo app (giá trị đang lưu luôn còn trong list) */}
          <Select
            label={t("page.workflow.entity")}
            value={entityName}
            placeholder={false}
            options={entityChoices}
            onChange={(e) => setEntityName(e.target.value)}
          />
        </div>
      </Group>

      {/* KHI */}
      <Group title={t("studio.build.automation.page.when")}>
        {/* Stacked radio-group, not radio-button-group: five descriptive labels need ~790px in a
            row, and the segmented group cannot wrap — below ~1400px the buttons broke mid-label. */}
        <RadioGroup
          label={t("page.workflow.trigger.label")}
          options={TRIGGERS.map((k) => ({ value: k, label: triggerOption(k) }))}
          value={trigger}
          onValueChange={setTrigger}
        />
        {/* specs/048: a before rule runs inside the save — always-on warning + "Áp dụng khi". */}
        {isBeforeTrigger(trigger) && (
          <BeforeRuleEditor trigger={trigger} saveOn={saveOn} onSaveOnChange={setSaveOn} saveOnError={saveOnErr}
            mutating={actions.some((x) => x.type === "field_update")} />
        )}
        {/* TIMER-1: date_offset config — pick the anchor date field, the offset and the
            direction; business_hours optionally pins a calendar. Plain-language preview. */}
        {trigger === "date_offset" && (() => {
          const dateFields = entityFields.filter((f) => f.fieldType === "date" || f.fieldType === "datetime");
          const anchorLabel = timer.dateField === TIMER_CREATED_AT
            ? t("page.workflow.timer.createdAtShort")
            : timer.dateField === TIMER_UPDATED_AT
            ? t("page.workflow.timer.updatedAtShort")
            : dateFields.find((f) => f.name === timer.dateField)?.label || timer.dateField;
          return (
            <fieldset className="slds-form-element slds-m-top_medium">
              <legend className="slds-form-element__legend slds-form-element__label">{t("page.workflow.timer.legend")}</legend>
              <div className="slds-form-element__control">
                <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
                  <Input
                    label={t("page.workflow.timer.amount")}
                    type="number"
                    min={1}
                    value={timer.offset.amount}
                    onChange={(e) => setTimer({ ...timer, offset: { ...timer.offset, amount: Math.max(1, Number(e.target.value) || 1) } })}
                  />
                  <Select
                    label={t("page.workflow.timer.unit")}
                    placeholder={false}
                    value={timer.offset.unit}
                    options={TIMER_UNITS.map((u) => ({ value: u.key, label: u.label }))}
                    onChange={(e) => {
                      const unit = e.target.value;
                      // business_hours is after-only (backend v1) — flip direction along.
                      setTimer({
                        ...timer,
                        offset: { ...timer.offset, unit },
                        direction: unit === "business_hours" ? "after" : timer.direction,
                        calendarId: unit === "business_hours" ? timer.calendarId : undefined,
                      });
                    }}
                  />
                  <Select
                    label={t("page.workflow.timer.direction")}
                    placeholder={false}
                    value={timer.direction}
                    options={TIMER_DIRECTIONS.map((d) => ({
                      value: d.key,
                      label: d.label,
                      disabled: (timer.offset.unit === "business_hours" || timer.dateField === TIMER_UPDATED_AT) && d.key === "before",
                    }))}
                    onChange={(e) => setTimer({ ...timer, direction: e.target.value })}
                  />
                  <Select
                    label={t("page.workflow.timer.anchor")}
                    placeholder={t("page.workflow.timer.anchorPh")}
                    value={timer.dateField}
                    options={[
                      { value: TIMER_CREATED_AT, label: t("page.workflow.timer.createdAt") },
                      { value: TIMER_UPDATED_AT, label: t("page.workflow.timer.updatedAt") },
                      ...dateFields.map((f) => ({ value: f.name, label: f.label || f.name })),
                    ]}
                    onChange={(e) => setTimer({ ...timer, dateField: e.target.value, ...(e.target.value === TIMER_UPDATED_AT ? { direction: "after" } : {}) })}
                  />
                </div>
                {timer.offset.unit === "business_hours" && (
                  <div className="slds-m-top_small flex flex-wrap items-end gap-3">
                    <div className="min-w-64 flex-1">
                      <Select
                        label={t("page.workflow.timer.calendar")}
                        placeholder={t("page.workflow.timer.calendarDefault")}
                        value={timer.calendarId ?? ""}
                        options={calendars.map((c) => ({ value: c.id, label: c.isDefault ? fill(t("page.workflow.timer.calendarIsDefault"), { name: c.name }) : c.name }))}
                        onChange={(e) => setTimer({ ...timer, calendarId: e.target.value || undefined })}
                      />
                    </div>
                    {/* STUDIO-DISCOVERABILITY 1.4b: calendar CRUD lives at /admin/business-calendars
                        but nothing here pointed to it — makers saw an empty dropdown dead-end. */}
                    <Link href={calendarsLink} className="slds-p-bottom_x-small" title={t("page.workflow.timer.calendarManageTitle")}>
                      {t("page.workflow.timer.calendarManage")}
                    </Link>
                  </div>
                )}
                {/* RECURRENCE (#3b). Off by default: a rule that repeats when the maker only
                    asked for one reminder is worse than one that does not repeat. The count is
                    the TOTAL number of fires, and it is bounded — the server refuses anything
                    past MAX_REPEAT_COUNT, so the input must not offer more than that. */}
                <div className="slds-m-top_small">
                  <Checkbox
                    label={t("page.workflow.timer.repeat")}
                    checked={!!timer.repeat}
                    onChange={(e) =>
                      setTimer({
                        ...timer,
                        repeat: e.target.checked ? { every: 1, unit: "days", count: 3 } : undefined,
                      })
                    }
                  />
                </div>
                {timer.repeat && (
                  <div className="slds-m-top_x-small grid grid-cols-3 gap-3">
                    <Input
                      label={t("page.workflow.timer.every")}
                      type="number"
                      min={1}
                      max={365}
                      value={timer.repeat.every}
                      onChange={(e) =>
                        setTimer({ ...timer, repeat: { ...timer.repeat!, every: Math.max(1, Number(e.target.value) || 1) } })
                      }
                    />
                    <Select
                      label={t("page.workflow.timer.repeatUnit")}
                      placeholder={false}
                      value={timer.repeat.unit}
                      options={REPEAT_UNITS.map((u) => ({ value: u.key, label: u.label }))}
                      onChange={(e) => setTimer({ ...timer, repeat: { ...timer.repeat!, unit: e.target.value } })}
                    />
                    <Input
                      label={fill(t("page.workflow.timer.count"), { max: MAX_REPEAT_COUNT })}
                      type="number"
                      min={2}
                      max={MAX_REPEAT_COUNT}
                      value={timer.repeat.count}
                      onChange={(e) =>
                        setTimer({
                          ...timer,
                          repeat: {
                            ...timer.repeat!,
                            count: Math.min(MAX_REPEAT_COUNT, Math.max(2, Number(e.target.value) || 2)),
                          },
                        })
                      }
                    />
                  </div>
                )}
                <p className="slds-m-top_small flex items-center gap-1">
                  <Icon icon={Clock} size="x-small" />
                  {t("page.workflow.timer.preview")} <strong>{timerPreview(timer, timer.dateField ? anchorLabel : undefined)}</strong>
                </p>
              </div>
            </fieldset>
          );
        })()}
        {/* P2 #15: intervalSeconds input — only shown for scheduled trigger */}
        {trigger === "scheduled" && schedulerOff && (
          <ScopedNotification theme="warning" className="slds-m-top_medium">{t("page.workflow.schedulerOff")}</ScopedNotification>
        )}
        {trigger === "scheduled" && (
          <div className="slds-m-top_medium max-w-80">
            <Input
              label={t("page.workflow.interval")}
              type="number"
              min={60}
              step={60}
              value={intervalDraft ?? intervalSeconds}
              fixedTextRight={t("page.workflow.intervalUnit")}
              help={`≈ ${intervalSeconds >= 86400
                ? fill(t("page.workflow.intervalDays"), { n: (intervalSeconds / 86400).toFixed(1) })
                : intervalSeconds >= 3600
                ? fill(t("page.workflow.intervalHours"), { n: (intervalSeconds / 3600).toFixed(1) })
                : fill(t("page.workflow.intervalMinutes"), { n: Math.round(intervalSeconds / 60) })}`}
              onChange={(e) => {
                setIntervalDraft(e.target.value);
                const n = Number(e.target.value);
                if (n >= 60) setIntervalSeconds(n); // last valid value is what Save sends
              }}
              onBlur={() => {
                setIntervalSeconds(Math.max(60, Number(intervalDraft ?? intervalSeconds) || 3600));
                setIntervalDraft(null);
              }}
            />
          </div>
        )}
      </Group>

      {/* NẾU */}
      <Group title={t("studio.build.automation.page.if")}>
        {conds.length === 0 && <p className="slds-text-color_weak slds-m-bottom_small">{t("page.workflow.cond.none")}</p>}
        {/* specs/048: "vừa đổi"/"vừa đổi thành" are offered only where an update runs the rule; a row
            still holding one elsewhere (saved earlier, or the trigger changed after) is named here —
            the server refuses it as WORKFLOW_CONDITION_NEVER_TRUE. */}
        {edgeRows.length > 0 && (
          <ScopedNotification theme="error" className="slds-m-bottom_small">
            {fill(t("workflow.before.edgeOpNever"), {
              fields: edgeRows.map((c) => condFields.find((f) => f.name === c.field)?.label ?? c.field).join(", "),
            })}
          </ScopedNotification>
        )}
        <ExpressionBuilder
          title={null}
          fields={condFields}
          value={conds}
          onChange={setConds}
          opsFor={edgeOff ? wfOpsNoEdge : wfOps}
          valueless={isNullOp}
          relativeDates={false}
          compareFields
          // AND/OR select only once there are two rows to join (as before).
          {...(conds.length > 1 && {
            match: condMatch === "or" ? "any" : "all",
            onMatchChange: (m) => setCondMatch(m === "any" ? "or" : "and"),
            matchOptions: ["all", "any"],
          })}
        />
      </Group>

      {/* THÌ */}
      <Group
        title={t("studio.build.automation.page.then")}
        actions={
          <Menu align="right" trigger={<Button iconLeft={Plus} iconRight={ChevronDown}>{t("page.workflow.action.add")}</Button>}>
            {ACTION_TYPES.filter((at) => actionAllowed(trigger, at.key)
              && (!isBeforeTrigger(trigger) || beforeActionOffered(actions.map((x) => x.type), at.key))).map((at) => (
              <MenuItem
                key={at.key}
                label={actionLabel(at.key)}
                icon={at.icon}
                // A new notification starts with the same visible text as a new rule's first card.
                onSelect={() => setActions((prev) => [...prev, at.key === DEFAULT_ACTION.type ? DEFAULT_ACTION : { type: at.key, params: {} }])}
              />
            ))}
          </Menu>
        }
      >
        {actions.length === 0 && <p className="slds-text-color_weak">{t("page.workflow.action.none")}</p>}
        <div className="flex flex-col gap-3">
          {actions.map((a, i) => (
            <ActionCard
              key={i}
              index={i}
              action={a}
              trigger={trigger}
              error={actionErrors[i]}
              messageError={vetoMsgErrors[i]}
              calendarError={actionErrors[`${i}.calendarId`]}
              stampError={actionErrors[`${i}.stampField`]}
              errorField={actionErrField}
              entityFields={entityFields}
              teams={teams}
              calendars={calendars}
              processes={processes}
              crFields={crFields}
              entityChoices={entityChoices}
              entityLabel={entityLabel}
              ruleEntity={entityName}
              onParam={(key, val) => updateActionParam(i, key, val)}
              onParams={(params) => replaceActionParams(i, params)}
              onEntity={(target) =>
                // Single setActions so BOTH entityName + cleared values land (updateActionParam twice
                // would race on the stale `actions` closure). Rows clear — old fields don't apply.
                setActions(actions.map((act, j) => j === i
                  ? { ...act, params: { ...act.params, entityName: target, values: crPairsToString([]) } } : act))
              }
              onRemove={() => removeAction(i)}
            />
          ))}
        </div>
      </Group>

    </div>
  );

  // The rule asked for by the URL is not in the list at all (never existed / deleted) vs. only hidden by the app filter.
  const ruleMissing = !!initialRuleId && !listLoading && !err && !workflows.some((w) => w.id === initialRuleId);

  return (
    // Page mode (hideList): the page scrolls, so neither this box nor the editor may clip or scroll on its own — the
    // sticky Huỷ / Lưu bar sticks to the viewport only when no ancestor is a scroll container.
    <div className={hideList ? "flex min-h-0 flex-1" : "flex min-h-0 flex-1 overflow-hidden"}>
      {/* Rule list — blueprint split-view */}
      {!hideList && (
      <div className="slds-split-view_container slds-is-open w-72 flex-none">
        <article className="slds-split-view slds-grid slds-grid_vertical slds-grow min-h-0" aria-labelledby="nx-wf-list-title">
          <header className="slds-split-view__header">
            <div className="slds-grid slds-grid_vertical-align-center slds-m-bottom_xx-small">
              <div className="slds-has-flexi-truncate">
                <div className="slds-media slds-media_center">
                  <div className="slds-media__figure">
                    <Icon icon={Zap} category="standard" name="flow" size="small" />
                  </div>
                  <div className="slds-media__body">
                    <h1 id="nx-wf-list-title" className="slds-text-heading_small slds-text-color_default slds-p-right_x-small slds-truncate">
                      {t("page.workflow.list.title")}
                    </h1>
                  </div>
                </div>
              </div>
              <div className="slds-no-flex slds-grid">
                <ButtonIcon icon={Plus} label={t("page.workflow.list.new")} variant="border-filled" disabled={noTable} onClick={startCreate} />
              </div>
            </div>
            {!listLoading && !err && (
              <p className="slds-text-body_small slds-text-color_weak">{fill(t("page.workflow.list.count"), { n: shownWorkflows.length })}</p>
            )}
          </header>
          <ul className="slds-scrollable_y min-h-0 flex-1" aria-label={t("page.workflow.list.title")}>
            {listLoading && (
              <li className="slds-p-around_medium flex items-center gap-2">
                <Spinner size="x-small" inline label={t("page.workflow.list.loading")} />
                <span className="slds-text-color_weak">{t("page.workflow.list.loading")}</span>
              </li>
            )}
            {!listLoading && err && (
              <li className="slds-p-around_small">
                <ScopedNotification theme="error">{err}</ScopedNotification>
              </li>
            )}
            {toggleErr && (
              <li className="slds-p-around_small">
                <ScopedNotification theme="error">{toggleErr}</ScopedNotification>
              </li>
            )}
            {!listLoading && !err && shownWorkflows.map((w) => (
              <li key={w.id} className="slds-split-view__list-item">
                <button
                  type="button"
                  onClick={() => (onSelectRule ? onSelectRule(w.id) : handleSelectWorkflow(w))}
                  aria-current={w.id === sel ? "page" : undefined}
                  className="slds-split-view__list-item-action slds-button_reset slds-grow slds-has-flexi-truncate slds-text-align_left"
                >
                  <span className="slds-grid slds-wrap">
                    <span className="slds-truncate slds-text-body_regular slds-text-color_default" title={w.name}>{w.name}</span>
                  </span>
                  <span className="slds-grid slds-wrap">
                    {/* specs/048 US5-4: a rule that runs inside the save wears a badge, not just words. */}
                    <span className="slds-truncate">
                      {entityLabel(w.entityName)} ·{" "}
                      {isBeforeTrigger(w.trigger) ? <Badge>{beforeTriggerLabel(w.trigger)}</Badge> : triggerLabel(w.trigger ?? "—")}
                    </span>
                    <span className="slds-truncate slds-col_bump-left">
                      {(() => {
                        const d = draftStateLabel(w.draftState);
                        return d ? <StatusText tone={d.tone} className="slds-m-right_xx-small">{d.label}</StatusText> : null;
                      })()}
                      {w.isActive ? t("page.workflow.on") : t("page.workflow.off")}
                    </span>
                  </span>
                </button>
                {/* W6: jump straight to this rule's run history */}
                <div className="slds-no-flex slds-p-right_x-small flex items-center">
                  {failedRuns.has(w.id) && (
                    <Button
                      variant="base"
                      className="slds-text-color_error slds-no-flex"
                      onClick={() => openRunHistory(w, failedRuns.get(w.id) ?? null)}
                    >
                      {t("page.workflow.list.lastRunFailed")}
                    </Button>
                  )}
                  <ButtonIcon
                    icon={Power}
                    label={fill(t(w.isActive ? "page.workflow.list.pause" : "page.workflow.list.resume"), { name: w.name })}
                    onClick={() => void toggleActive(w)}
                  />
                  <ButtonIcon
                    icon={History}
                    label={fill(t("page.workflow.list.history"), { name: w.name })}
                    onClick={() => openRunHistory(w)}
                  />
                </div>
              </li>
            ))}
          </ul>
          {/* W6: recent runs — GET /admin/workflows/runs; under an app scope only the runs of the
              rules listed above (queue #297), no strip at all when the app has no rule, and no
              read while the scope loads (it would list every app's runs first). */}
          {!listLoading && !err && shownWorkflows.length > 0 && (
            <RecentRunsStrip
              workflows={workflows}
              workflowIds={scopeEntities ? shownWorkflows.map((w) => w.id) : undefined}
              onOpen={(wid) => {
                const w = workflows.find((x) => x.id === wid);
                if (w) openRunHistory(w);
              }}
            />
          )}
        </article>
      </div>
      )}

      {/* Editor */}
      {showEditor ? (
        <div className={hideList ? "flex min-w-0 flex-1 flex-col" : "flex min-w-0 flex-1 flex-col overflow-y-auto"}>
          <PageHeader
            icon={{ glyph: Zap, name: "flow" }}
            label={t("page.workflow.objectLabel")}
            title={name || t("page.workflow.untitled")}
            meta={(() => {
              const d = draftStateLabel(wf?.draftState);
              return (
                <>
                  {`${entityLabel(entityName)} · ${triggerOption(trigger)}`}
                  {d && <StatusText tone={d.tone} className="slds-m-left_small">{d.label}</StatusText>}
                </>
              );
            })()}
            actions={[
              <Toggle
                key="active"
                label={t("page.workflow.active")}
                onText={t("page.workflow.onLong")}
                offText={t("page.workflow.offLong")}
                checked={isActive}
                onChange={(e) => setIsActive(e.target.checked)}
              />,
              // specs/063 FR-017: stop a misbehaving LIVE rule now, without publishing the other drafts.
              canDisableNow ? (
                <Button key="disable-now" variant="outline-brand" iconLeft={ShieldBan} onClick={() => setDisableOpen(true)}>
                  {t("workflow.disableNow")}
                </Button>
              ) : null,
              // W6: dry-run — needs a SAVED rule (backend evaluates by id)
              <Button
                key="test"
                iconLeft={FlaskConical}
                onClick={() => setTestOpen((o) => !o)}
                disabled={!saved}
                title={saved ? t("page.workflow.test.hint") : t("page.workflow.test.saveFirst")}
              >
                {t("page.workflow.test.run")}
              </Button>,
            ]}
          />

          <div className="slds-p-horizontal_medium">
            {config}

            {/* Chạy thử trên dữ liệu mẫu (POST /admin/workflows/:id/test, zero side effect): opens in place. */}
            {saved && testOpen && (
              <Group title={t("studio.build.automation.page.test")}>
                <WorkflowTestDialog
                  workflowId={sel!}
                  workflowName={name}
                  entityName={entityName}
                  calendarNames={calendarNames}
                  trigger={wf?.trigger}
                  fields={entityFields}
                  saveOn={saveOn}
                  onClose={() => setTestOpen(false)}
                />
              </Group>
            )}

            {/* Lần chạy gần đây — the same component the run-history page uses, one fetch per mount. */}
            <section ref={runsRef} aria-labelledby="nx-rule-runs" className="slds-m-bottom_large">
              <h2 id="nx-rule-runs" className="slds-text-heading_small slds-border_bottom slds-p-bottom_x-small slds-m-bottom_small">
                {t("studio.build.automation.page.runs")}
              </h2>
              {saved ? (
                <WorkflowRunHistory key={`${sel}:${openRunId ?? ""}`} workflowId={sel!} entityName={entityName} openRunId={openRunId} />
              ) : (
                <p className="slds-text-color_weak"><strong>{t("page.workflow.runs.saveFirstTitle")}</strong> {t("page.workflow.runs.saveFirstDesc")}</p>
              )}
            </section>

            {/* "Tắt ngay cho khách" (specs/063 FR-017) and delete both ask in place (FR-065). */}
            {disableOpen && (
              <div className="slds-m-bottom_medium" role="group" aria-label={t("workflow.disableNow.title")}>
                <InlineConfirm
                  tone="warning"
                  message={`${t("workflow.disableNow.title")} ${fill(t("workflow.disableNow.body"), { name })}${wf?.draftState === "edited" ? ` ${t("workflow.disableNow.hasDraft")}` : ""}`}
                  confirmLabel={disabling ? t("workflow.disableNow.working") : t("workflow.disableNow.confirm")}
                  busy={disabling}
                  onConfirm={handleDisableNow}
                  onCancel={() => setDisableOpen(false)}
                />
              </div>
            )}
            {saved && (
              <section aria-label={t("page.workflow.delete.open")} className="slds-m-bottom_large">
                {delConfirm ? (
                  <div role="group" aria-label={t("page.workflow.delete.title")}>
                  <InlineConfirm
                    tone="error"
                    message={fill(t("page.workflow.delete.body"), { name })}
                    confirmLabel={delLoading ? t("page.workflow.delete.deleting") : t("page.workflow.delete.confirm")}
                    busy={delLoading}
                    onConfirm={handleDelete}
                    onCancel={() => setDelConfirm(false)}
                  />
                  </div>
                ) : (
                  <Button variant="text-destructive" iconLeft={Trash2} onClick={() => setDelConfirm(true)}>
                    {t("page.workflow.delete.open")}
                  </Button>
                )}
              </section>
            )}
          </div>

          <UnsavedBar
            dirty={dirty}
            alwaysShow={creating}
            saving={saving}
            onSave={handleSave}
            onCancel={() => { if (wf && !creating) loadWorkflow(wf); else startCreate(); }}
            saveLabel={saved ? t("page.workflow.save.update") : t("page.workflow.save.create")}
            error={saveErr}
          />
          {!dirty && saveOk && (
            <p role="status" className="slds-p-horizontal_medium slds-p-bottom_small">
              <StatusText tone="success">{savedToDraft ? t("workflow.draft.saved") : t("page.workflow.save.ok")}</StatusText>
            </p>
          )}
        </div>
      ) : (
        <div className="flex min-w-0 flex-1 flex-col items-center justify-center">
          {ruleMissing ? (
            <EmptyState size="large" icon={Zap} title={t("studio.build.automation.ruleNotFound")} />
          ) : !listLoading && (noTable ? (
            <EmptyState
              size="large"
              icon={Zap}
              title={scopeApp ? fill(t("page.workflow.noTableApp"), { app: scopeApp.label }) : t("page.workflow.noTable")}
              description={fill(t("page.workflow.noTableDesc"), { panel: PANEL_LABELS.bang })}
            />
          ) : (
            <EmptyState
              size="large"
              icon={Zap}
              title={scopeApp && shownWorkflows.length === 0 ? tf("studio.build.automation.noRulesApp", { app: scopeApp.label }) : t("page.workflow.empty.title")}
              description={t("page.workflow.empty.desc")}
              action={<Button variant="brand" iconLeft={Plus} onClick={startCreate}>{t("page.workflow.empty.create")}</Button>}
            />
          ))}
        </div>
      )}
    </div>
  );
}

// A flat settings group (specs/074 lát 11, UI-ARCH 4.11 "Form cài đặt"): 16px/600 heading + a rule under it, no card.
function Group({ title, actions, children }: { title: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="slds-m-bottom_large">
      <div className="slds-grid slds-grid_vertical-align-center slds-border_bottom slds-p-bottom_x-small slds-m-bottom_small">
        <h2 className="slds-text-heading_small slds-col">{title}</h2>
        {actions}
      </div>
      {children}
    </section>
  );
}

// One action of THÌ: blueprint `cards` nested-with-boundary, type-specific parameter fields inside.
function ActionCard({
  index,
  action: a,
  trigger,
  error: rowErr,
  messageError,
  calendarError,
  stampError,
  errorField,
  entityFields,
  teams,
  calendars,
  processes,
  crFields,
  entityChoices,
  entityLabel,
  ruleEntity,
  onParam,
  onParams,
  onEntity,
  onRemove,
}: {
  index: number;
  action: ActionConfig;
  /** The rule's trigger — an action it may not carry is flagged (specs/048). */
  trigger: string;
  /** WF-01 inline: the 422 details.field pinned onto THIS action. */
  error?: string;
  /** veto: the server's refusal of the message (WORKFLOW_VETO_MESSAGE_INVALID). */
  messageError?: string;
  /** Queue #79: a SET_DEADLINE_CALENDAR_* 422 pinned onto THIS action's calendar dropdown. */
  calendarError?: string;
  /** set_deadline: the server's refusal of stampField (SET_DEADLINE_STAMP_FIELD_INVALID). */
  stampError?: string;
  /** The field that 422 names (a field_update puts it on its record picker when it is the via). */
  errorField?: string | null;
  entityFields: FieldMeta[];
  teams: AdminTeam[];
  calendars: BusinessCalendar[];
  processes: ApprovalProcessSummary[];
  crFields: Record<string, FieldMeta[]>;
  entityChoices: { value: string; label: string }[];
  entityLabel: (name?: string) => string;
  /** The entity the rule runs on (a value taken from "the record that fired the rule"). */
  ruleEntity: string;
  onParam: (key: string, val: string) => void;
  onParams: (params: Record<string, string>) => void;
  onEntity: (entity: string) => void;
  onRemove: () => void;
}) {
  const pathname = usePathname();
  const at = ACTION_TYPES.find((x) => x.key === a.type);
  const p = a.params;
  return (
    <article className="slds-box slds-box_x-small">
      <div className="slds-grid slds-grid_vertical-align-center slds-m-bottom_x-small">
        <h3 className="slds-text-title_bold slds-col" title={at ? actionLabel(at.key) : a.type}>{at ? actionLabel(at.key) : a.type}</h3>
        <ButtonIcon icon={Trash2} label={fill(t("page.workflow.action.remove"), { n: index + 1 })} variant="border-filled" onClick={onRemove} />
      </div>
      {/* specs/048: switching the trigger does not silently drop actions — one the new trigger may
          not carry says so here (the server refuses it: WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER). */}
      {!actionAllowed(trigger, a.type) && (
        <ScopedNotification theme="error" className="slds-m-bottom_small">
          {fill(t("workflow.before.actionNotAllowed"), { trigger: triggerOption(trigger) })}
        </ScopedNotification>
      )}
      {a.type === "veto" && (
        <VetoActionEditor
          trigger={trigger}
          message={p.message ?? ""}
          field={p.field ?? ""}
          fields={entityFields}
          messageError={messageError}
          fieldError={rowErr}
          onChange={onParam}
        />
      )}
      {a.type === "start_approval" && (() => {
        const chosen = p.processId ?? "";
        // Manual + switched on + same entity: exactly what checkStartApprovalTargets accepts. A saved
        // choice that no longer qualifies stays listed (by name when known) so opening and saving the rule
        // never drops it unseen — the server then says why on this card.
        const offered = processes.filter((x) => x.trigger === "manual" && x.isActive && x.entityName === ruleEntity);
        const saved = processes.find((x) => x.id === chosen);
        const options = [
          ...offered.map((x) => ({ value: x.id, label: x.name })),
          ...(chosen && !offered.some((x) => x.id === chosen) ? [{ value: chosen, label: saved?.name ?? fill(t("page.workflow.param.staleField"), { name: chosen }) }] : []),
        ];
        return (
          <div className="flex flex-col gap-3">
            <Combobox
              label={t("page.workflow.param.approvalProcess")}
              placeholder={t("page.workflow.param.approvalProcessPh")}
              options={options}
              value={chosen || null}
              error={rowErr}
              onChange={(v) => onParam("processId", v ?? "")}
            />
            {!offered.length && <p className="slds-text-color_weak">{t("page.workflow.param.noManualProcess")}</p>}
            <p className="slds-text-color_weak">{t("page.workflow.param.approvalProcessNote")}</p>
          </div>
        );
      })()}
      {a.type === "in_app_notification" && (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
          <NotifyRecipientPicker value={p.userId ?? "$owner"} entityFields={entityFields} onChange={(v) => onParam("userId", v)} />
          {/* DAILY-QUEUE #335: title/message take the same {{field}} / {{relation.field}} placeholders as the email. */}
          <div className="flex items-end gap-2">
            <div className="min-w-0 flex-1">
              <Input label={t("page.workflow.param.title")} value={p.title ?? ""} onChange={(e) => onParam("title", e.target.value)} />
            </div>
            <TemplateFieldPicker fields={entityFields} label={t("page.workflow.tpl.insertInTitle")} onPick={(tok) => onParam("title", appendToken(p.title ?? "", tok))} />
          </div>
          <div className="flex items-end gap-2 md:col-span-2">
            <div className="min-w-0 flex-1">
              <Input label={t("page.workflow.param.message")} value={p.message ?? ""} onChange={(e) => onParam("message", e.target.value)} />
            </div>
            <TemplateFieldPicker fields={entityFields} label={t("page.workflow.tpl.insertInBody")} onPick={(tok) => onParam("message", appendToken(p.message ?? "", tok))} />
          </div>
        </div>
      )}
      {/* specs/058: which record (this one or a related one) → a field of it → a typed value. */}
      {a.type === "field_update" && isBeforeTrigger(trigger) && (
        <MutateActionEditor params={p} fields={entityFields} error={rowErr} errorField={errorField} onChange={onParams} />
      )}
      {a.type === "normalize" && <NormalizeActionEditor params={p} fields={entityFields} error={rowErr} onChange={onParams} />}
      {a.type === "field_update" && !isBeforeTrigger(trigger) && (
        <FieldUpdateEditor
          params={p}
          entityFields={entityFields}
          fieldsByEntity={crFields}
          entityLabel={entityLabel}
          ruleEntity={ruleEntity}
          error={rowErr}
          errorField={errorField}
          onChange={onParams}
        />
      )}
      {/* specs/059: the recipient is PICKED — the record's email or the linked record's (the patient
          of an appointment) — and subject/body can take any field of either; the engine resolves
          {{relation.field}} and lookups at send time. Free text still works for a fixed address. */}
      {a.type === "email_alert" && (
        <div className="grid grid-cols-1 gap-3">
          <div className="flex items-end gap-2">
            <div className="min-w-0 flex-1">
              <Input label={t("page.workflow.param.to")} value={p.to ?? ""} placeholder={t("page.workflow.param.toPh")} help={t("page.workflow.tpl.toHelp")} onChange={(e) => onParam("to", e.target.value)} />
            </div>
            <TemplateFieldPicker fields={entityFields} emailOnly label={t("page.workflow.tpl.pickRecipient")} onPick={(tok) => onParam("to", tok)} />
          </div>
          <div className="flex items-end gap-2">
            <div className="min-w-0 flex-1">
              <Input label={t("page.workflow.param.subject")} value={p.subject ?? ""} onChange={(e) => onParam("subject", e.target.value)} />
            </div>
            <TemplateFieldPicker fields={entityFields} label={t("page.workflow.tpl.insertInSubject")} onPick={(tok) => onParam("subject", appendToken(p.subject ?? "", tok))} />
          </div>
          <div className="flex items-end gap-2">
            <div className="min-w-0 flex-1">
              <Input label={t("page.workflow.param.body")} value={p.body ?? ""} onChange={(e) => onParam("body", e.target.value)} />
            </div>
            <TemplateFieldPicker fields={entityFields} label={t("page.workflow.tpl.insertInBody")} onPick={(tok) => onParam("body", appendToken(p.body ?? "", tok))} />
          </div>
        </div>
      )}
      {a.type === "outbound_webhook" && (p.credentialId ? (
        // specs/050 S2b: read-only until the key picker (S9) — editing a URL here would drop the key.
        <p className="slds-text-body_small">{tf("page.workflow.param.usesCredential", { path: p.path || "/" })}</p>
      ) : (
        <Input label={t("page.workflow.param.url")} value={p.url ?? ""} placeholder="https://example.com/hook" onChange={(e) => onParam("url", e.target.value)} />
      ))}
      {/* ASSIGN-1/ASSIGN-2: distribute a team → a text field. Strategy picks the rule:
          round_robin (rotate) or load_based (fewest active records). */}
      {a.type === "assign" && (() => {
        // Trường đích của `assign` nhận kiểu `user` HOẶC `text` (services.assignTargetTypeOK).
        // Danh sách này từng lọc cứng `text` — có từ trước khi kiểu `user` tồn tại — nên sau
        // khi pack đổi `assigned_to` sang `user`, admin mở rule ra là thấy ô rỗng và KHÔNG
        // chọn lại được đúng trường mà rule đang dùng. Không phải lỗi ghi: select là
        // controlled, không chạm thì state giữ nguyên giá trị cũ và lưu lại vẫn đúng — lỗi
        // là admin không QUẢN được nữa.
        const eligible = entityFields.filter((f) => f.fieldType === "user" || f.fieldType === "text");
        const target = p.field ?? "";
        return (
          <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
            <Select
              label={t("page.workflow.param.strategy")}
              placeholder={false}
              value={p.strategy ?? "round_robin"}
              options={[
                { value: "round_robin", label: t("page.workflow.param.roundRobin") },
                { value: "load_based", label: t("page.workflow.param.loadBased") },
              ]}
              onChange={(e) => onParam("strategy", e.target.value)}
            />
            <Select
              label={t("page.workflow.param.team")}
              placeholder={t("page.workflow.param.teamPh")}
              value={p.team ?? ""}
              options={teams.map((tm) => ({ value: tm.id, label: tm.name }))}
              onChange={(e) => onParam("team", e.target.value)}
            />
            <Select
              label={t("page.workflow.param.assignTarget")}
              placeholder={t("page.workflow.param.fieldPh")}
              value={target}
              error={rowErr}
              options={[
                ...eligible.map((f) => ({
                  value: f.name,
                  label: f.fieldType === "user" ? fill(t("page.workflow.param.userField"), { label: f.label || f.name }) : f.label || f.name,
                })),
                // Rule đã lưu trỏ vào một trường nay không còn hợp kiểu (bị xoá, bị đổi kiểu):
                // vẫn hiện nó ra thay vì âm thầm về rỗng — nếu không, chỉ cần MỞ rule rồi bấm
                // Lưu là xoá mất trường đích mà không ai thấy.
                ...(target && !eligible.some((f) => f.name === target)
                  ? [{ value: target, label: fill(t("page.workflow.param.staleField"), { name: target }) }]
                  : []),
              ]}
              onChange={(e) => onParam("field", e.target.value)}
            />
          </div>
        );
      })()}
      {/* set_deadline (#9): "+N đơn-vị kể từ lúc luật chạy" ghi vào một trường
          datetime. Danh sách trường CHỈ lọc `datetime` vì action ghi RFC3339 —
          một trường `date` sẽ nhận 422 ở lúc chạy chứ không phải lúc lưu, tức là
          luật trông như đã lưu xong mà mỗi lần chạy là một run FAILED không ai
          đọc. Đơn vị CÓ "giờ làm việc" từ hàng đợi #10: action tra lịch làm việc
          của tổ chức qua đúng đường mà trigger `date_offset` đang dùng, nên hạn và
          lời nhắc không bao giờ hiểu "+4 giờ làm việc" theo hai nghĩa khác nhau.
          Tổ chức chưa khai lịch thì rơi về giờ thường — nói thẳng ngay dưới ô chọn
          thay vì để admin tưởng giờ hành chính đã được áp. */}
      {a.type === "set_deadline" && (() => {
        const dtFields = entityFields.filter((f) => f.fieldType === "datetime");
        const target = p.field ?? "";
        const targetMeta = dtFields.find((f) => f.name === target);
        const amount = Number.parseInt(p.amount ?? "", 10);
        const pin = p.calendarId ?? "";
        return (
          <div className="flex flex-col gap-3">
            <div className="grid grid-cols-[8rem_1fr] gap-3">
              <Input label={t("page.workflow.param.after")} type="number" min={1} value={p.amount ?? ""} placeholder="4" onChange={(e) => onParam("amount", e.target.value)} />
              <Select
                label={t("page.workflow.timer.unit")}
                placeholder={false}
                value={p.unit ?? "hours"}
                options={DEADLINE_UNITS.map((u) => ({ value: u.key, label: u.label }))}
                onChange={(e) => onParam("unit", e.target.value)}
              />
            </div>
            {/* Queue #79: "giờ làm việc" walks the org's default calendar or one pinned here. The
                first entry clears the pin (payload omits calendarId); a pin whose calendar is gone
                stays listed by id instead of silently reading as "default" — that is the rule the
                run history warns about, and this is where the admin fixes it. Switching the unit to
                wall-clock hides the dropdown and deadlinePolicyPayload drops the pin. */}
            {p.unit === "business_hours" && (
              <>
                <div className="flex flex-wrap items-end gap-3">
                  <div className="min-w-64 flex-1">
                    <Select
                      label={t("page.workflow.timer.calendar")}
                      placeholder={t("page.workflow.param.calendarDefault")}
                      value={pin}
                      error={calendarError}
                      options={[
                        ...calendars.map((c) => ({ value: c.id, label: c.isDefault ? fill(t("page.workflow.timer.calendarIsDefault"), { name: c.name }) : c.name })),
                        ...(pin && !calendars.some((c) => c.id === pin) ? [{ value: pin, label: fill(t("page.workflow.param.calendarStale"), { id: pin }) }] : []),
                      ]}
                      onChange={(e) => onParam("calendarId", e.target.value)}
                    />
                  </div>
                  <Link href={calendarsHref(pathname)} className="slds-p-bottom_x-small" title={t("page.workflow.timer.calendarManageTitle")}>
                    {t("page.workflow.timer.calendarManage")}
                  </Link>
                </div>
                <p className="slds-text-color_weak">{t(pin ? "page.workflow.param.calendarPinnedNote" : "page.workflow.param.businessHoursNote")}</p>
              </>
            )}
            <Select
              label={t("page.workflow.param.deadlineTarget")}
              placeholder={t("page.workflow.param.fieldPh")}
              value={target}
              error={rowErr}
              help={!dtFields.length ? t("page.workflow.param.noDatetime") : undefined}
              options={[
                ...dtFields.map((f) => ({ value: f.name, label: f.label || f.name })),
                // Trường đã lưu nay không còn hợp kiểu (bị xoá / đổi kiểu): vẫn hiện
                // ra thay vì âm thầm về rỗng — cùng lý do như `assign` ở trên: mở
                // rule rồi bấm Lưu không được phép xoá mất đích mà không ai thấy.
                ...(target && !targetMeta ? [{ value: target, label: fill(t("page.workflow.param.staleField"), { name: target }) }] : []),
              ]}
              onChange={(e) => onParam("field", e.target.value)}
            />
            {/* Queue #81 (specs/043 S14a): what a RE-RUN does to a deadline that already exists. "Chỉ rút
                ngắn" never lengthens a promise and never touches a hand-typed deadline; it can only tell the
                two apart through the machine stamp, so the stamp field is required for it (server:
                SET_DEADLINE_STAMP_FIELD_INVALID). "Ghi đè" / "Lúc luật chạy" are the defaults and travel as an
                absent key. */}
            <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
              <Select
                label={t("page.workflow.param.deadlineMode")}
                placeholder={false}
                value={p.mode === "tighten_only" ? "tighten_only" : "overwrite"}
                options={[
                  { value: "overwrite", label: t("page.workflow.param.deadlineMode.overwrite") },
                  { value: "tighten_only", label: t("page.workflow.param.deadlineMode.tighten") },
                ]}
                onChange={(e) => onParam("mode", e.target.value === "tighten_only" ? "tighten_only" : "")}
              />
              <Select
                label={t("page.workflow.param.deadlineAnchor")}
                placeholder={false}
                value={p.anchor === "created_at" ? "created_at" : "now"}
                options={[
                  { value: "now", label: t("page.workflow.param.deadlineAnchor.now") },
                  { value: "created_at", label: t("page.workflow.param.deadlineAnchor.created") },
                ]}
                onChange={(e) => onParam("anchor", e.target.value === "created_at" ? "created_at" : "")}
              />
            </div>
            <Select
              label={t("page.workflow.param.deadlineStamp")}
              placeholder={t("page.workflow.param.deadlineStampNone")}
              value={p.stampField ?? ""}
              error={stampError}
              help={p.mode === "tighten_only" ? t("page.workflow.param.deadlineStampNeeded") : t("page.workflow.param.deadlineStampHelp")}
              options={[
                ...dtFields.filter((f) => f.name !== target).map((f) => ({ value: f.name, label: f.label || f.name })),
                ...(p.stampField && !dtFields.some((f) => f.name === p.stampField)
                  ? [{ value: p.stampField, label: fill(t("page.workflow.param.staleField"), { name: p.stampField }) }] : []),
              ]}
              onChange={(e) => onParam("stampField", e.target.value)}
            />
            <p className="slds-text-color_weak">
              {deadlinePreview({ field: target, offset: { amount: Number.isFinite(amount) ? amount : 0, unit: p.unit ?? "hours" } }, targetMeta?.label)}
            </p>
          </div>
        );
      })()}
      {/* set_elapsed (#41, specs/046): số phút giờ làm việc từ lúc tạo bản ghi tới lúc luật
          chạy, ghi vào một trường. Danh sách CHỈ lọc `number` — kiểu duy nhất action được ghi
          (SET_ELAPSED_FIELD_TYPE_INVALID lúc lưu). Trường đã lưu nay không còn hợp kiểu vẫn hiện
          ra như `set_deadline`. Dòng mô tả nói lịch nào được dùng, chưa khai lịch thì sao, và vì
          sao luật đóng phiếu nên dùng "vừa đổi thành" (spec §2.5: "=" ghi đè mỗi lần lưu). */}
      {a.type === "set_elapsed" && (() => {
        const numFields = entityFields.filter((f) => f.fieldType === "number");
        const target = p.field ?? "";
        return (
          <div className="flex flex-col gap-3">
            <Select
              label={t("page.workflow.param.elapsedTarget")}
              placeholder={t("page.workflow.param.fieldPh")}
              value={target}
              error={rowErr}
              help={!numFields.length ? t("page.workflow.param.noNumber") : undefined}
              options={[
                ...numFields.map((f) => ({ value: f.name, label: f.label || f.name })),
                ...(target && !numFields.some((f) => f.name === target)
                  ? [{ value: target, label: fill(t("page.workflow.param.staleField"), { name: target }) }]
                  : []),
              ]}
              onChange={(e) => onParam("field", e.target.value)}
            />
            <p className="slds-text-color_weak">{t("page.workflow.param.elapsedNote")}</p>
          </div>
        );
      })()}
      {/* CREATE-RECORD (#80): target entity + TYPE-AWARE field→value rows. Each value uses
          the same FieldInput widget the App record form renders for that field type, and
          emits the same wire shape a normal record create sends (number/currency/boolean/
          rating no longer 422 RECORD_INVALID_TYPE). Tick "$owner" to gán người phụ trách. */}
      {a.type === "create_record" && (() => {
        const targetEntity = p.entityName ?? "";
        const targetFields = crFields[targetEntity];              // undefined = still fetching
        const all = targetFields ?? [];
        // Writable = not a system/audit column, not server-computed/assigned, and a type the
        // value editor can emit (polymorphic/file/image/composite omitted — see note below).
        const writable = all.filter((f) =>
          !SYSTEM_FIELDS.has(f.name) && !READONLY_FIELD_TYPES.has(f.fieldType) && CR_SUPPORTED_TYPES.has(f.fieldType));
        const writableByName = new Map(writable.map((f) => [f.name, f]));
        const unsupported = all.filter((f) =>
          !SYSTEM_FIELDS.has(f.name) && !READONLY_FIELD_TYPES.has(f.fieldType) && !CR_SUPPORTED_TYPES.has(f.fieldType));
        const pairs = crValuePairs(p.values);
        const setPairs = (next: CrRow[]) => onParam("values", crPairsToString(next));
        return (
          <div className="flex flex-col gap-3">
            <Select
              label={t("page.workflow.param.targetEntity")}
              placeholder={t("page.workflow.param.targetEntityPh")}
              value={targetEntity}
              options={entityChoices}
              onChange={(e) => onEntity(e.target.value)}
            />
            {!targetEntity ? (
              <p className="slds-text-color_weak">{t("page.workflow.param.pickEntityFirst")}</p>
            ) : targetFields === undefined ? (
              // A <div>, not a <p>: the SLDS spinner is a <div>, which a <p> cannot contain (hydration error).
              <div className="flex items-center gap-2">
                <Spinner size="x-small" inline label={t("page.workflow.param.loadingFields")} />
                <span className="slds-text-color_weak">{t("page.workflow.param.loadingFields")}</span>
              </div>
            ) : (
              <fieldset className="slds-form-element">
                <legend className="slds-form-element__legend slds-form-element__label">{t("page.workflow.param.values")}</legend>
                <div className="slds-form-element__control">
                  <div className="flex flex-col gap-2">
                    {pairs.map(([fname, val], pi) => {
                      const meta = writableByName.get(fname);
                      const isOwner = val === OWNER_SENTINEL;
                      const fromTrigger = isFromTrigger(val);
                      const ownerEligible = !!meta && OWNER_ELIGIBLE_TYPES.has(meta.fieldType);
                      const setVal = (nv: unknown) => setPairs(pairs.map((x, j) => j === pi ? [x[0], nv] : x));
                      const setField = (fieldName: string) => {
                        // Reset the value to the new type's empty default (avoid a stale cross-type value).
                        const nm = writableByName.get(fieldName);
                        setPairs(pairs.map((x, j) => j === pi ? [fieldName, nm ? emptyDefault(nm) : ""] : x));
                      };
                      return (
                        <div key={pi} className="slds-box slds-box_x-small flex flex-col gap-2">
                          <div className="flex items-end gap-2">
                            <div className="min-w-0 flex-1">
                              <Select
                                label={fill(t("page.workflow.param.valueField"), { n: pi + 1 })}
                                placeholder={t("page.workflow.param.fieldPh")}
                                value={fname}
                                options={[
                                  ...writable.map((wf) => ({ value: wf.name, label: `${wf.label || wf.name}${wf.isRequired ? " *" : ""}` })),
                                  // Keep an unknown/stale selected field visible so re-saving can't silently drop it.
                                  ...(fname && !writableByName.has(fname) ? [{ value: fname, label: fname }] : []),
                                ]}
                                onChange={(e) => setField(e.target.value)}
                              />
                            </div>
                            {ownerEligible && (
                              <div className="slds-p-bottom_x-small" title={t("page.workflow.param.ownerTitle")}>
                                <Checkbox
                                  label="$owner"
                                  checked={isOwner}
                                  onChange={(e) => setVal(e.target.checked ? OWNER_SENTINEL : emptyDefault(meta!))}
                                />
                              </div>
                            )}
                            <ButtonIcon
                              icon={X}
                              label={fill(t("page.workflow.param.removeValue"), { n: pi + 1 })}
                              variant="border-filled"
                              onClick={() => setPairs(pairs.filter((_, j) => j !== pi))}
                            />
                          </div>
                          {isOwner ? (
                            <p className="slds-text-color_weak">
                              <span className="slds-text-font_monospace">$owner</span> — {t("page.workflow.param.ownerNote")}
                            </p>
                          ) : !fname ? null : meta ? (
                            <>
                              {/* queue #305: copy the value from the record that fired the rule. */}
                              <TriggerValueSource target={meta} ruleFields={entityFields} ruleEntity={ruleEntity} value={val} onChange={setVal} />
                              {fromTrigger
                                ? <p className="slds-text-color_weak">{t("page.workflow.param.srcNote")}</p>
                                : <FieldInput field={metaToSpec(meta)} value={val} onChange={setVal} />}
                              {/* #335: a text value can carry {{field}} / {{relation.field}} of the record that fired the rule. */}
                              {!fromTrigger && meta.fieldType === "text" && typeof val === "string" && (
                                <div>
                                  <TemplateFieldPicker fields={entityFields} label={t("page.workflow.tpl.insertInValue")} onPick={(tok) => setVal(appendToken(val, tok))} />
                                </div>
                              )}
                            </>
                          ) : (
                            // Unknown/stale field (metadata not loaded) → plain input so the value isn't lost.
                            <Input
                              label={t("page.workflow.param.value")}
                              value={typeof val === "string" ? val : String(val ?? "")}
                              onChange={(e) => setVal(e.target.value)}
                            />
                          )}
                        </div>
                      );
                    })}
                    <div>
                      <Button variant="base" iconLeft={Plus} onClick={() => setPairs([...pairs, ["", ""]])}>
                        {t("page.workflow.param.addValue")}
                      </Button>
                    </div>
                    {unsupported.length > 0 && (
                      <p className="slds-text-body_small slds-text-color_weak">
                        {fill(t("page.workflow.param.unsupported"), { n: unsupported.length })}
                      </p>
                    )}
                  </div>
                </div>
              </fieldset>
            )}
          </div>
        );
      })()}
    </article>
  );
}

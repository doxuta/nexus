"use client";

// ApprovalDetail (W6 §3.12) — /approvals/[id]: full context for ONE approval instance.
// GET /approvals/:id (api-conventions §9.5 — camelCase, protected NOT admin-gated; 404 no-leak).
// Answers the "duyệt mù" gap: record link, process name, step progress, SLA + escalated badge,
// history timeline, approve/reject with comment, recall, delegate (existing endpoints).
// Actor/approver ids resolve to names via /users/directory — readable by EVERY authenticated user,
// which is the point: the people who live in this screen are non-admins. Roles still need
// /admin/roles, so a non-admin sees "vai trò #<id8>" there. An id that resolves to nothing stays a
// short id; nothing is ever invented.
//
// SLDS (specs/038 T031, RESEARCH §5 /approvals/[id]), record home composed from the blueprints:
//   page-headers/record-home (status + escalated badges, entity LABEL, step x/y, current approver as
//   highlights; the way back to the inbox is the shell's breadcrumb) · slds-grid 2/3 + 1/3 ·
//   left: cards "Quyết định của bạn" (textarea + button-groups row) and "Dòng thời gian"
//   (activity-timeline) · right: "Bối cảnh" (form-element/record-detail, view mode), "Các bước duyệt"
//   (progress-indicator/vertical, the pinned-steps note as scoped-notifications in the same card).
// Only the people the server lets act get a verb (canDecide / canRecall mirror bpm.CanDecide and
// Requester.Recall): a colleague who can merely see the record gets no buttons (spec "Nợ còn mở").
// FE-BPM-DETAIL (#130) extends this screen (branches, parallel seats, per-step durations).

import { useCallback, useEffect, useState, type ReactNode } from "react";
import { useRouter } from "next/navigation";
import Link from "next/link";
import { CheckSquare, Clock, Send, ThumbsDown, ThumbsUp, Undo2 } from "lucide-react";
import {
  ensureAuth, getApprovalDetail, approveRecord, rejectRecord, overrideApproval, getRecord,
  recallApproval, delegateApproval,
  listUserDirectory, listRoles, ApiError,
} from "@/lib/api";
import {
  approvalStatusMeta, historyActionMeta, fmtSLA, slaBasisLabel, elapsedLabel, canDecide, canRecall,
  type ApprovalDetailData,
} from "@/lib/approvalDetail";
import { approvalPath } from "@/lib/approvalPath";
import { useAuth } from "@/lib/auth/AuthProvider";
import { useEntities } from "@/lib/metadata/hooks";
import { errorMessage } from "@/lib/errorMessages";
import { recordLabel } from "@/lib/recordLabel";
import { formatDateTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import { ActivityTimeline, type ActivityTimelineItem } from "@/components/slds/ActivityTimeline";
import { Badge } from "@/components/slds/Badge";
import { Button, buttonClassName } from "@/components/slds/Button";
import { Avatar } from "@/components/slds/Avatar";
import { Card } from "@/components/slds/Card";
import { ProgressBar } from "@/components/slds/ProgressBar";
import { EmptyState } from "@/components/slds/EmptyState";
import { FormElement } from "@/components/slds/FormElement";
import { PageHeader } from "@/components/slds/PageHeader";
import { ProgressIndicator } from "@/components/slds/ProgressIndicator";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { Textarea } from "@/components/slds/Textarea";
import { DelegateButton } from "@/components/ui/DelegateButton";
import { toast } from "@/components/ui/Toaster";
import { BADGE, TILE, historyTimeline, withComments } from "./approvalShared";

const REASON_REQUIRED = "APPROVAL_REJECT_REASON_REQUIRED";
const OVERRIDE_NOTE_REQUIRED = "APPROVAL_OVERRIDE_NOTE_REQUIRED"; // specs/043 S4: «Quyết thay» without its reason
const CONFLICT = "APPROVAL_CONFLICT"; // specs/043 S3b: another write got in between your read and your click

export function ApprovalDetail({ id }: { id: string }) {
  const router = useRouter();
  const { user: me } = useAuth();
  const { data: entities = [] } = useEntities();
  const [data, setData] = useState<ApprovalDetailData | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [notFound, setNotFound] = useState(false);

  // Name resolution (tenant directory for people, admin-only for roles; short id when unresolved)
  const [userMap, setUserMap] = useState<Record<string, string>>({});
  const [roleMap, setRoleMap] = useState<Record<string, string>>({});
  const [recLabel, setRecLabel] = useState<string | null>(null);

  // Decision form. `deciding` doubles as the in-flight lock for EVERY verb (approve/reject/recall/
  // delegate) so the four buttons share one disabled gate and can't race each other.
  const [note, setNote] = useState("");
  const [deciding, setDeciding] = useState<"approve" | "reject" | "recall" | "delegate" | "override" | null>(null);
  const [decideErr, setDecideErr] = useState<string | null>(null);
  const [noteErr, setNoteErr] = useState<string | null>(null); // the reason is missing — said at the box

  // Throws on failure: the first load turns that into the not-found / error shell, a refresh after
  // a decision into a trip back to the inbox (refreshOrLeave). Nothing is set before the first
  // await: the initial render already shows the spinner, and a refresh keeps the page up instead of
  // flashing it (react-hooks/set-state-in-effect).
  const load = useCallback(async () => {
    await ensureAuth();
    const d = await getApprovalDetail(id);
    setData(d);
    // Best-effort record label (record may be deleted / unreadable — keep the id link anyway)
    if (d.recordId && d.entityName) {
      getRecord(d.entityName, d.recordId)
        .then((r) => setRecLabel(recordLabel(r, [])))
        .catch(() => setRecLabel(null));
    }
    // People resolve for EVERYONE. /users/directory is readable by any authenticated user — it
    // exists precisely so a non-admin can name a colleague — while /admin/users is not. Gating
    // the lookup on isAdmin meant the people who actually live in the approval inbox saw raw
    // UUID fragments ("#<id8>") for the submitter, the approver and every delegation target,
    // while the admin who never opens it saw names. The same directory the delegate picker in
    // this very screen already uses.
    listUserDirectory()
      .then((us) => setUserMap(Object.fromEntries(us.map((u) => [u.id, u.fullName?.trim() || u.email]))))
      .catch(() => { /* refused → personName falls back to the short id, never a fabricated name */ });
  }, [id]);

  useEffect(() => {
    (async () => {
      try {
        await load();
      } catch (e) {
        // 404 only is "không tìm thấy"; a 500, a dropped connection or a 403 says what it is.
        if (e instanceof ApiError && e.status === 404) setNotFound(true);
        else setErr(errorMessage(e));
      } finally {
        setLoading(false);
      }
    })();
  }, [load]);

  // Roles stay admin-gated: there is no non-admin role directory, so a non-admin sees
  // "vai trò #<id8>" — honest, and better than inventing one.
  const isAdmin = !!me?.isAdmin;
  useEffect(() => {
    if (!isAdmin) return;
    listRoles()
      .then((rs) => setRoleMap(Object.fromEntries(rs.map((r) => [r.id, r.label || r.name]))))
      .catch(() => {});
  }, [isAdmin]);

  const personName = (type?: string, pid?: string): string => {
    if (!pid) return "—";
    if (type === "role") return roleMap[pid] ?? tf("page.approvals.roleShort", { id: pid.slice(0, 8) });
    return userMap[pid] ?? `#${pid.slice(0, 8)}`;
  };

  // After any verb, refresh the detail. A now-terminal (or, for the submitter, recalled) instance
  // may 404 for a non-admin viewer — bounce back to the inbox rather than showing an error. After a
  // 409 (`conflictMsg`) the reason the user typed stays, and if the page is going away the sentence
  // that explains why rides along as a toast: otherwise the loser is sent to the inbox unexplained.
  async function refreshOrLeave(conflictMsg?: string) {
    if (!conflictMsg) setNote("");
    try {
      await load();
    } catch {
      if (conflictMsg) toast.error(conflictMsg);
      router.push("/approvals");
    }
  }

  // Runs one verb under the shared lock; a missing reason goes to the box, anything else above it.
  async function act(verb: NonNullable<typeof deciding>, fn: () => Promise<void>, done: string) {
    setDeciding(verb); setDecideErr(null); setNoteErr(null);
    try {
      await fn();
      toast.success(done);
      await refreshOrLeave();
    } catch (e) {
      if (e instanceof ApiError && (e.code === REASON_REQUIRED || e.code === OVERRIDE_NOTE_REQUIRED)) setNoteErr(errorMessage(e));
      else {
        const msg = errorMessage(e);
        setDecideErr(msg);
        // Someone else decided first (specs/043 S3b, 409): show the state that won, with the reason above it.
        if (e instanceof ApiError && e.code === CONFLICT) await refreshOrLeave(msg);
      }
    } finally {
      setDeciding(null);
    }
  }

  function decide(mode: "approve" | "reject") {
    if (!data) return;
    if (mode === "reject" && !note.trim()) {
      setDecideErr(null);
      setNoteErr(t("page.approvals.detail.noteRequired"));
      return;
    }
    if (mode === "approve") act("approve", () => approveRecord(data.id, note.trim() || undefined, data.currentStep), t("page.approvals.decide.approved"));
    else act("reject", () => rejectRecord(data.id, note.trim(), data.currentStep), t("page.approvals.decide.rejected"));
  }

  // «Quyết thay» (specs/043 S4, FR-023): an admin with no seat on a step held by several people decides
  // it in their place. The reason is mandatory for BOTH directions — said at the box before any call.
  function override(decision: "approve" | "reject") {
    if (!data) return;
    if (!note.trim()) {
      setDecideErr(null);
      setNoteErr(t("page.approvals.detail.overrideNoteRequired"));
      return;
    }
    act("override", () => overrideApproval(data.id, decision, note.trim(), data.currentStep),
      t(decision === "approve" ? "page.approvals.decide.approved" : "page.approvals.decide.rejected"));
  }

  // Recall: the SENDER withdraws a still-pending request (server: APPROVAL_NOT_SUBMITTER otherwise).
  const recall = () => data && act("recall", () => recallApproval(data.id, note.trim() || undefined), t("page.approvals.decide.recalled"));
  // Delegate: the current approver hands THIS step to a named person. The server authorizes
  // exactly as deciding does (if you may approve it, you may pass it on).
  const delegate = (toUserId: string) =>
    data && act("delegate", () => delegateApproval(data.id, toUserId, note.trim() || undefined), t("page.approvals.decide.delegated"));

  // ── Loading / error shells ──────────────────────────────────────────────────
  if (loading) {
    return (
      <div className="relative min-h-0 flex-1">
        <Spinner size="medium" label={t("page.approvals.detail.loading")} />
      </div>
    );
  }

  // The error first: a failed first load leaves `data` null too, and must not read as "not found".
  if (err) {
    return (
      <div className="flex min-h-0 flex-1 flex-col gap-4 p-4">
        <ScopedNotification theme="error">{err}</ScopedNotification>
        <div>
          <Link href="/approvals" className={buttonClassName("neutral")}>{t("page.approvals.detail.backToInbox")}</Link>
        </div>
      </div>
    );
  }

  if (notFound || !data) {
    return (
      <div className="flex min-h-0 flex-1 flex-col p-4">
        <article className="slds-card">
          <EmptyState
            className="slds-p-around_large"
            icon={CheckSquare}
            title={t("page.approvals.detail.notFound")}
            description={t("page.approvals.detail.notFoundDesc")}
            action={
              <div className="slds-m-top_medium">
                <Link href="/approvals" className={buttonClassName("neutral")}>{t("page.approvals.detail.backToInbox")}</Link>
              </div>
            }
          />
        </article>
      </div>
    );
  }

  const statusMeta = approvalStatusMeta(data.status);
  const sla = fmtSLA(data.slaDueAt);
  const slaBasis = slaBasisLabel(data.slaBasis); // specs/043 S1 — what the deadline is counted in
  const elapsed = elapsedLabel(data.durations, data.status); // specs/043 S2 — "Mất 3 giờ làm việc"
  const isPending = data.status === "pending";
  const processName = data.process?.name || t("page.approvals.detail.fallbackTitle"); // deleted process → name ""
  const steps = data.process?.steps ?? [];
  const path = approvalPath(data); // the steps and branches this approval went through (specs/043 US6)
  const entityLabel = entities.find((e) => e.name === data.entityName)?.label ?? data.entityName;
  // An admin who holds no seat of a council step does not "approve" it — that would settle the other
  // people's vote — they use «Quyết thay», with a reason (the server answers 409 otherwise).
  const needsOverride = !!me?.isAdmin && isPending && !data.awaitingRework && !!data.completion && data.awaitsMe === false;
  const mayDecide = canDecide(data, me) && !needsOverride;
  const mayRecall = canRecall(data, me);
  // " · <step name>" for step index i, or "" when the step is unnamed (#69). A history entry's own
  // recorded name wins (S0 — it stays right with no process context at all); else process.steps, the
  // chain the engine walks (the pinned copy when there is one), so history indexes line up.
  const stepName = (i: number, recorded?: string) => {
    const n = recorded || steps[i]?.name;
    return n ? ` · ${n}` : "";
  };

  // "2/3 đã duyệt — cần 2" for a step held by several people (specs/043 S4); "" otherwise.
  const councilTally = data.completion
    ? tf("page.approvals.detail.councilTally", { approved: data.completion.approved, total: data.completion.total, required: data.completion.required })
    : "";
  const seatText = (s: NonNullable<ApprovalDetailData["seats"]>[number]): string => {
    if (s.status === "open") return t("page.approvals.detail.seatOpen");
    if (s.outcome === "approved") return t("page.approvals.detail.seatApproved");
    if (s.outcome === "rejected") return t("page.approvals.detail.seatRejected");
    if (s.outcome === "reassigned") return t("page.approvals.detail.seatHandedOn");
    return t("page.approvals.detail.seatClosed");
  };

  const timeline: ActivityTimelineItem[] = [];
  // The engine writes its own "submitted" entry since S0; the synthetic one only stands in for
  // instances armed before that, so the timeline never says "Gửi duyệt" twice.
  const hasSubmitted = data.history.some((h) => historyActionMeta(h.action).tone === "ac");
  if (data.createdAt && !hasSubmitted) {
    timeline.push({
      id: "submitted",
      icon: TILE.ac.icon,
      iconName: TILE.ac.name,
      title: t("page.approvals.detail.submitted"),
      date: formatDateTime(data.createdAt),
      summary: tf("page.approvals.by", { name: personName("user", data.createdBy) }),
    });
  }
  timeline.push(...historyTimeline(data.history, (pid) => personName("user", pid), stepName, "h"));
  if (isPending) {
    timeline.push({
      id: "pending",
      icon: Clock,
      iconName: TILE.wn.name,
      // currentStep is 0-indexed (verified live) — show 1-based
      title: tf("page.approvals.detail.pendingTitle", { n: data.currentStep + 1, name: stepName(data.currentStep) }),
      date: sla.label !== "—" ? sla.label : "",
      summary: (
        <>
          {data.completion ? councilTally : tf("page.approvals.detail.pendingApprover", { name: personName(data.currentApproverType, data.currentApproverId) })}
          {sla.overdue && <span className="slds-text-color_error slds-m-left_x-small">{t("page.approvals.detail.overdue")}</span>}
        </>
      ),
    });
  } else {
    const end = TILE[statusMeta.tone] ?? TILE.neu;
    timeline.push({
      id: "ended",
      icon: end.icon,
      iconName: end.name,
      title: tf("page.approvals.detail.ended", { status: statusMeta.label.toLowerCase() }),
      date: data.updatedAt ? formatDateTime(data.updatedAt) : "",
    });
  }

  const context: { label: string; value: ReactNode }[] = [
    {
      label: t("page.approvals.detail.record"),
      value: data.recordId ? (
        <Link href={`/${data.entityName}/${data.recordId}`}>{recLabel ?? `#${data.recordId.slice(0, 8)}`}</Link>
      ) : "—",
    },
    {
      label: t("page.approvals.detail.due"),
      value: (
        <span className={sla.overdue ? "slds-text-color_error" : undefined}>
          {data.slaDueAt ? `${formatDateTime(data.slaDueAt)} · ${sla.label}` : sla.label}
        </span>
      ),
    },
    ...(slaBasis ? [{ label: t("page.approvals.detail.basis"), value: slaBasis }] : []),
    { label: t("page.approvals.detail.sender"), value: personName("user", data.createdBy) },
    ...(data.createdAt ? [{ label: t("page.approvals.detail.createdAt"), value: formatDateTime(data.createdAt) }] : []),
    ...(elapsed ? [{ label: t("page.approvals.detail.elapsed"), value: elapsed }] : []),
  ];

  const pinnedNote = !data.process
    ? t("page.approvals.detail.noProcess")
    : data.process.deleted
      ? t("page.approvals.detail.deletedPinned")
      : data.process.pinned
        ? t("page.approvals.detail.pinned")
        : null;

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto p-4">
      <PageHeader
        variant="record-home"
        icon={{ glyph: CheckSquare, name: "approval" }}
        label={t("page.approvals.detail.object")}
        title={processName}
        details={[
          {
            label: t("page.approvals.detail.status"),
            value: (
              <>
                <Badge theme={BADGE[statusMeta.tone]}>{statusMeta.label}</Badge>
                {data.escalated && <Badge theme="error" className="slds-m-left_xx-small">{t("page.approvals.escalated")}</Badge>}
              </>
            ),
          },
          { label: t("page.approvals.detail.entity"), value: entityLabel },
          {
            label: t("page.approvals.detail.progress"),
            value: steps.length > 0
              ? tf("page.approvals.detail.progressOf", { n: data.currentStep + 1, total: steps.length })
              : tf("page.approvals.step", { n: data.currentStep + 1 }),
          },
          // Which published version of the process this approval runs — it keeps that one even after
          // the process is republished (specs/043 S10). Absent for one started before versions.
          ...(data.process?.version
            ? [{ label: t("page.approvals.detail.version"), value: tf("page.approvals.detail.versionN", { n: data.process.version }) }]
            : []),
          ...(isPending
            ? [{ label: t("page.approvals.detail.approver"), value: data.completion ? councilTally : personName(data.currentApproverType, data.currentApproverId) }]
            : []),
        ]}
      />

      <div className="slds-grid slds-wrap slds-gutters_x-small">
        {/* Main: decision + timeline */}
        <div className="slds-col slds-size_1-of-1 slds-large-size_2-of-3 slds-m-top_medium">
          {data.awaitingRework && isPending && (
            <ScopedNotification theme="warning" className="slds-m-bottom_small">
              {t("page.approvals.detail.reworkWaiting")}
              {data.returnReason ? ` ${tf("page.approvals.detail.reworkReason", { reason: data.returnReason })}` : ""}
            </ScopedNotification>
          )}
          {(mayDecide || mayRecall || needsOverride) && (
            <Card title={t("page.approvals.detail.decision")} icon={{ glyph: CheckSquare, name: "approval" }}>
              {decideErr && <ScopedNotification theme="error" className="slds-m-bottom_small">{decideErr}</ScopedNotification>}
              <Textarea
                label={t("page.approvals.detail.note")}
                rows={2}
                value={note}
                onChange={(e) => { setNote(e.target.value); setNoteErr(null); }}
                placeholder={t("page.approvals.detail.notePlaceholder")}
                error={noteErr ?? undefined}
              />
              <ul className="slds-button-group-row slds-m-top_small">
                {mayDecide && (
                  <>
                    <li className="slds-button-group-item">
                      <Button variant="brand" iconLeft={ThumbsUp} onClick={() => decide("approve")} disabled={!!deciding}>
                        {t("page.approvals.detail.approve")}
                      </Button>
                    </li>
                    <li className="slds-button-group-item">
                      <Button iconLeft={ThumbsDown} onClick={() => decide("reject")} disabled={!!deciding}>
                        {t("page.approvals.detail.reject")}
                      </Button>
                    </li>
                  </>
                )}
                {needsOverride && (
                  <>
                    <li className="slds-button-group-item">
                      <Button variant="brand" iconLeft={ThumbsUp} onClick={() => override("approve")} disabled={!!deciding}>
                        {t("page.approvals.detail.overrideApprove")}
                      </Button>
                    </li>
                    <li className="slds-button-group-item">
                      <Button iconLeft={ThumbsDown} onClick={() => override("reject")} disabled={!!deciding}>
                        {t("page.approvals.detail.overrideReject")}
                      </Button>
                    </li>
                  </>
                )}
                {mayRecall && (
                  <li className="slds-button-group-item">
                    <Button iconLeft={Undo2} onClick={recall} disabled={!!deciding} title={t("page.approvals.detail.recallTitle")}>
                      {t("page.approvals.detail.recall")}
                    </Button>
                  </li>
                )}
                {mayDecide && (
                  <li className="slds-button-group-item">
                    <DelegateButton disabled={!!deciding} onPick={delegate} />
                  </li>
                )}
                {deciding && (
                  <li className="slds-button-group-item relative w-8">
                    <Spinner size="x-small" label={t("page.approvals.decide.working")} />
                  </li>
                )}
              </ul>
            </Card>
          )}

          {/* A step held by several people (specs/043 S4): where the votes stand and who decided what. */}
          {data.completion && data.seats && data.seats.length > 0 && (
            <Card title={t("page.approvals.detail.council")} icon={{ glyph: CheckSquare, name: "approval" }} className="slds-m-bottom_medium">
              {/* total 0 = every seat was closed without a vote (an admin's «Quyết thay»): no "0/0" to show. */}
              {data.completion.total > 0 && (
                <>
                  <p className="slds-text-heading_small slds-m-bottom_x-small">{councilTally}</p>
                  <ProgressBar
                    value={(data.completion.approved / Math.max(1, data.completion.required)) * 100}
                    label={councilTally}
                    size="medium"
                    success={data.completion.approved >= data.completion.required}
                    className="slds-m-bottom_small"
                  />
                </>
              )}
              <p className="slds-text-color_weak slds-m-bottom_small">
                {data.completion.mode === "quorum"
                  ? tf("page.approvals.detail.councilModeQuorum", { percent: data.completion.percent })
                  : t(data.completion.mode === "any" ? "page.approvals.detail.councilModeAny" : "page.approvals.detail.councilModeAll")}
              </p>
              <ul className="slds-has-dividers_bottom-space">
                {data.seats.map((s) => (
                  <li key={s.id} className="slds-item slds-grid slds-grid_vertical-align-center">
                    <Avatar shape="circle" size="small" name={personName(s.assigneeType, s.assigneeId)} className="slds-m-right_small" />
                    <span className="slds-col">
                      <span className="slds-text-body_regular">{personName(s.assigneeType, s.assigneeId)}</span>
                      {" — "}
                      <Badge theme={s.outcome === "approved" ? "success" : s.outcome === "rejected" ? "error" : undefined}>{seatText(s)}</Badge>
                      {s.comment && <span className="slds-text-color_weak slds-m-left_x-small">“{s.comment}”</span>}
                    </span>
                  </li>
                ))}
              </ul>
            </Card>
          )}

          <Card title={t("page.approvals.detail.timeline")} icon={{ glyph: Send, name: "submit-for-approval" }}>
            <ActivityTimeline items={timeline} defaultExpanded={withComments(timeline)} />
          </Card>
        </div>

        {/* Aside: context + step progress */}
        <div className="slds-col slds-size_1-of-1 slds-large-size_1-of-3 slds-m-top_medium">
          <Card title={t("page.approvals.detail.context")}>
            <div className="slds-form" role="list">
              {context.map((c) => (
                <div key={c.label} className="slds-form__row">
                  <div className="slds-form__item" role="listitem">
                    <FormElement label={c.label} layout="stacked" readOnly>
                      {c.value}
                    </FormElement>
                  </div>
                </div>
              ))}
            </div>
          </Card>

          {/* The pinned-steps note is about these steps, so it lives in their card — and gets the
              card on its own when no process is left (loose on the page background, the light
              scoped notification's grey fill disappears). */}
          {(path.length > 0 || pinnedNote) && (
            <Card title={t("page.approvals.detail.steps")}>
              {path.length > 0 && (
                <ProgressIndicator
                  vertical
                  success={data.status === "approved"}
                  error={data.status === "rejected"}
                  current={-1}
                  label={t("page.approvals.detail.steps")}
                  steps={path.map((p) => {
                    const src = steps[p.index];
                    if (p.kind === "gateway") {
                      return {
                        id: p.id,
                        label: p.name || t("page.approvals.detail.gateway"),
                        state: p.state === "done" ? "completed" : "upcoming",
                        description: p.branch
                          ? tf("page.approvals.detail.branchTaken", { name: p.branch })
                          : t(p.state === "skipped" ? "page.approvals.detail.pathSkipped" : "page.approvals.detail.branchPending"),
                      };
                    }
                    // The step's own name leads (#69): for a role step a non-admin has no role
                    // directory, so the approver alone reads "vai trò #<uuid8>".
                    const due = typeof src.slaSeconds === "number" && src.slaSeconds > 0
                      ? ` · ${tf("page.approvals.detail.stepSla", { h: Math.round(src.slaSeconds / 3600) })}`
                      : "";
                    let detail: string;
                    // A step held by several people (S4): the head-count and the policy, not a blank approver.
                    if (src.approvers && src.approvers.length > 0) {
                      const policy = src.completion === "quorum"
                        ? tf("page.approvals.detail.policyQuorum", { percent: src.completionPercent ?? 0 })
                        : t(src.completion === "any" ? "page.approvals.detail.policyAny" : "page.approvals.detail.policyAll");
                      detail = tf("page.approvals.detail.stepMany", { n: src.approvers.length, policy });
                    } else {
                      const who = personName(src.approverType, src.approverId);
                      const kind = t(src.approverType === "role" ? "page.approvals.detail.stepRole" : "page.approvals.detail.stepUser");
                      detail = `${src.name ? `${who} · ` : ""}${kind}`;
                    }
                    const label = src.name || (src.approvers?.length ? t("page.approvals.detail.council") : personName(src.approverType, src.approverId));
                    return {
                      id: p.id,
                      label,
                      state: p.state === "done" ? "completed" : p.state === "current" ? "active" : p.state === "rejected" ? "error" : "upcoming",
                      description: p.state === "skipped" ? t("page.approvals.detail.pathSkipped") : `${detail}${due}`,
                    };
                  })}
                />
              )}
              {pinnedNote && (
                <ScopedNotification className={path.length > 0 ? "slds-m-top_medium" : undefined}>{pinnedNote}</ScopedNotification>
              )}
            </Card>
          )}
        </div>
      </div>
    </div>
  );
}

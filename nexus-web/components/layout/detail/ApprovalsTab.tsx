"use client";

// "Duyệt" tab — GET /:entity/:id/approvals → one card per approval instance (cards blueprint,
// boundary inside the tab): step + status badges + "Chi tiết" link in the header, the history on
// the activity timeline (same component as "Lịch sử"), decision buttons in the footer.
// ⚠️ lib/api.listRecordApprovals normalizes the PascalCase bpm.Instance and camelCase dialects.
// Empty = no approval process armed for this record yet → "Gửi duyệt".
// Every button shows for everyone and the SERVER decides: the browser cannot know which roles you
// hold, so a hidden button is a guess. A refusal arrives as the server's own sentence
// (APPROVAL_NOT_SUBMITTER / APPROVAL_NOT_AUTHORIZED).
// The one exception is "Gửi duyệt" on a record seen only through a manual share (readOnly,
// specs/051 FR-009): that is a write on the record, and the browser DOES know it is refused.
import { useEffect, useState } from "react";
import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import { ArrowUpRight, CheckCircle2, Clock, Send, ThumbsDown, ThumbsUp, Undo2 } from "lucide-react";
import {
  ensureAuth, listRecordApprovals, listStartableProcesses, approveRecord, submitForApproval, recallApproval, delegateApproval,
  listUserDirectory, ApiError,
} from "@/lib/api";
import { approvalStatusMeta, historyActionMeta, fmtSLA, type ApprovalDetailData } from "@/lib/approvalDetail";
import { formatDateTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { ActivityTimeline, type ActivityTimelineItem } from "@/components/slds/ActivityTimeline";
import { Badge } from "@/components/slds/Badge";
import { Button, buttonClassName } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { Combobox } from "@/components/slds/Combobox";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { DelegateButton } from "@/components/ui/DelegateButton";
// approvalDetail tones → SLDS badge themes / timeline tiles: one map for the three approval surfaces.
import { BADGE, TILE } from "@/components/screens/approvalShared";

export function ApprovalsTab({ entity, recordId, readOnly = false }: { entity: string; recordId: string; readOnly?: boolean }) {
  const [items, setItems] = useState<ApprovalDetailData[]>([]);
  const [loading, setLoading] = useState(true);
  const [errMsg, setErrMsg] = useState("");
  const [busy, setBusy] = useState<string | null>(null); // instance id (or "submit") being acted on

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        await ensureAuth();
        const instances = await listRecordApprovals(entity, recordId);
        if (!cancelled) {
          setItems(instances);
          setLoading(false);
        }
      } catch (e) {
        if (!cancelled) {
          setErrMsg(e instanceof ApiError ? e.message : t("page.record.approvals.loadError"));
          setLoading(false);
        }
      }
    })();
    return () => { cancelled = true; };
  }, [entity, recordId]);

  // Approve only. Rejecting needs a reason (specs/043 FR-027) and this tab has no box for one, so
  // "Từ chối" below links to /approvals/:id, which asks for it and shows the server's 422 sentence.
  const approve = async (item: ApprovalDetailData) => {
    setBusy(item.id);
    try {
      await ensureAuth();
      await approveRecord(item.id, undefined, item.currentStep);
      setItems((prev) => prev.filter((a) => a.id !== item.id));
    } catch (e) {
      setErrMsg(e instanceof ApiError ? e.message : t("page.record.approvals.actionError"));
    } finally {
      setBusy(null);
    }
  };

  const run = async (id: string, fn: () => Promise<void>) => {
    setBusy(id);
    setErrMsg("");
    try {
      await ensureAuth();
      await fn();
      setItems(await listRecordApprovals(entity, recordId));
    } catch (e) {
      setErrMsg(e instanceof ApiError ? e.message : t("page.record.approvals.actionError"));
    } finally {
      setBusy(null);
    }
  };

  // Same directory the record header and the history tab use — reachable by a non-admin. An id
  // that resolves to nothing stays a short id: never a fabricated name.
  const { data: dir } = useQuery({
    queryKey: ["user-directory"],
    queryFn: () => listUserDirectory(),
    staleTime: 5 * 60 * 1000,
    retry: false,
  });
  const personName = (id?: string) => {
    if (!id) return t("page.record.approvals.system");
    const u = (dir ?? []).find((x) => x.id === id);
    return u ? (u.fullName?.trim() || u.email) : id.slice(0, 8);
  };

  // The manual processes this record may go through (specs/043 S9b). Several → the sender picks;
  // one → it is the one; none → send without a processId and let the server answer in its own words.
  const { data: startable = [] } = useQuery({
    queryKey: ["startable-processes", entity, recordId],
    queryFn: () => listStartableProcesses(entity, recordId),
    enabled: !readOnly,
    retry: false,
  });
  const [picked, setPicked] = useState<string | null>(null);
  const processId = startable.some((p) => p.processId === picked) ? (picked ?? undefined) : startable[0]?.processId;
  const submit = () => run("submit", () => submitForApproval(entity, recordId, processId));
  const recall = (item: ApprovalDetailData) => run(item.id, () => recallApproval(item.id));
  const delegate = (item: ApprovalDetailData, toUserId: string) => run(item.id, () => delegateApproval(item.id, toUserId));

  if (loading) {
    return (
      <div className="relative h-24">
        <Spinner size="small" label={t("page.record.approvals.loading")} />
      </div>
    );
  }

  return (
    <>
      {errMsg && <ScopedNotification theme="warning" className="slds-m-bottom_small">{errMsg}</ScopedNotification>}
      {items.length === 0 && !errMsg ? (
        <EmptyState
          icon={CheckCircle2}
          title={t("page.record.approvals.emptyTitle")}
          description={readOnly ? t("page.record.approvals.emptyTextReadOnly") : t("page.record.approvals.emptyText")}
          action={readOnly ? undefined : (
            <div className="slds-grid slds-grid_vertical slds-grid_vertical-align-center">
              {startable.length > 1 && (
                <Combobox
                  label={t("page.record.approvals.pickProcess")}
                  className="slds-m-bottom_small"
                  options={startable.map((p) => ({ value: p.processId, label: p.name }))}
                  value={processId ?? null}
                  onChange={setPicked}
                />
              )}
              <Button variant="brand" iconLeft={Send} onClick={submit} disabled={!!busy}>
                {busy === "submit" ? t("page.record.approvals.submitting") : t("page.record.approvals.submit")}
              </Button>
            </div>
          )}
        />
      ) : (
        items.map((item) => {
          const status = approvalStatusMeta(item.status);
          const sla = fmtSLA(item.slaDueAt);
          const isPending = item.status === "pending";
          const step = String(item.currentStep + 1); // currentStep is 0-indexed
          const timeline: ActivityTimelineItem[] = item.history.map((h, i) => {
            const m = historyActionMeta(h.action);
            const tile = TILE[m.tone] ?? TILE.neu;
            return {
              id: `${item.id}-${i}`,
              icon: tile.icon,
              iconName: tile.name,
              title: t("page.record.approvals.historyTitle").replace("{action}", m.label).replace("{step}", String(h.step + 1)),
              date: h.at ? formatDateTime(h.at) : "",
              summary: (
                <>
                  {t("page.record.approvals.by")} <strong>{personName(h.actor)}</strong>
                  {h.target && <> · {t("page.record.approvals.to")} <strong>{personName(h.target)}</strong></>}
                </>
              ),
              body: h.comment ? { label: t("page.record.approvals.comment"), text: `“${h.comment}”` } : undefined,
            };
          });
          if (isPending) {
            timeline.push({
              id: `${item.id}-pending`,
              icon: Clock,
              iconName: "change-request",
              title: t("page.record.approvals.pendingTitle").replace("{step}", step),
              date: sla.label !== "—" ? sla.label : "",
              summary: sla.overdue ? <span className="slds-text-color_error">{t("page.record.approvals.overdue")}</span> : undefined,
            });
          }
          return (
            <Card
              key={item.id}
              boundary
              className="slds-m-bottom_small"
              title={t("page.record.approvals.step").replace("{n}", step)}
              icon={{ glyph: CheckCircle2, name: "approval" }}
              actions={
                <div className="slds-grid slds-grid_vertical-align-center">
                  <Badge theme={BADGE[status.tone]}>{status.label}</Badge>
                  {item.escalated && <Badge theme="error" className="slds-m-left_xx-small">{t("page.record.approvals.escalated")}</Badge>}
                  <Link href={`/approvals/${item.id}`} className="slds-m-left_small">
                    {t("page.record.approvals.detail")}
                    <ArrowUpRight aria-hidden="true" className="slds-icon slds-icon_xx-small slds-icon-text-default slds-m-left_xx-small nx-lucide" />
                  </Link>
                </div>
              }
              footer={isPending ? (
                <div className="slds-button-group-row slds-grid slds-grid_align-end">
                  <Button variant="brand" iconLeft={ThumbsUp} onClick={() => approve(item)} disabled={!!busy}>
                    {t("page.record.approvals.approve")}
                  </Button>
                  <Link href={`/approvals/${item.id}`} title={t("page.record.approvals.rejectTitle")} className={buttonClassName("neutral")}>
                    <ThumbsDown aria-hidden="true" className="slds-button__icon slds-button__icon_left nx-lucide" />
                    {t("page.record.approvals.reject")}
                  </Link>
                  <Button iconLeft={Undo2} onClick={() => recall(item)} disabled={!!busy} title={t("page.record.approvals.recallTitle")}>
                    {t("page.record.approvals.recall")}
                  </Button>
                  <DelegateButton disabled={!!busy} onPick={(uid) => delegate(item, uid)} />
                </div>
              ) : undefined}
            >
              {timeline.length > 0 ? (
                <ActivityTimeline items={timeline} />
              ) : (
                <p className="slds-text-color_weak">{t("page.record.approvals.noHistory")}</p>
              )}
            </Card>
          );
        })
      )}
    </>
  );
}

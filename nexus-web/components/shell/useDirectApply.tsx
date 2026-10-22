"use client";

// useDirectApply — specs/063-draft-publish S7-5 (US6). On a DELIVERED project, roles, grants, field
// permissions, app create/delete and a table that seeds default grants have no draft: they reach real
// users the moment they are saved. `guard` asks for confirmation first; `delivered` lets a screen that
// already has its own confirm Modal just add the notice. A 403 on /admin/publish/state is the legacy
// customer admin (builder-only route): no Modal for them, their write still leaves a trace server-side.
import { createContext, useCallback, useContext, useEffect, useState } from "react";
import { ApiError, getPublishState } from "@/lib/api";
import { onDraftsChanged } from "@/lib/draftEvents";
import { t } from "@/lib/i18n";
import { Button } from "@/components/slds/Button";
import { Modal } from "@/components/slds/Modal";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { StatusText } from "@/components/studio/StatusText";

// true/false = answer; null = the read failed (not a 403), so the caller must not cache "false".
const answerOfError = (e: unknown) => (e instanceof ApiError && e.status === 403 ? false : null);
async function readDelivered(): Promise<boolean | null> {
  try {
    return (await getPublishState()).delivered;
  } catch (e) {
    return answerOfError(e);
  }
}
async function isDelivered(): Promise<boolean> {
  return (await readDelivered()) === true;
}

/**
 * The project frame's publish-state read (ProjectFrame → usePublishState, one per project). Inside a Studio project the hook
 * answers from it instead of reading on mount and on every announcement itself (#53). null on the customer face: own read.
 */
export const FramePublishState = createContext<{ data?: { delivered: boolean }; error: unknown } | null>(null);

/**
 * `inline` (Studio, specs/074 lát f2-direct-apply-access, FR-065): the same question is asked IN the page as an
 * InlineConfirm — never a dialog — and `notice` is the warn label "Áp ngay cho người dùng thật" for the screen to put
 * next to the control. The customer face keeps the Modal.
 */
export function useDirectApply({ inline = false }: { inline?: boolean } = {}) {
  const frame = useContext(FramePublishState);
  const framed = frame !== null;
  const [ownDelivered, setDelivered] = useState(false);
  const [ownKnown, setKnown] = useState(false);
  const [pending, setPending] = useState<{ what: string; run: () => void | Promise<void> } | null>(null);
  // In a frame its last read answers, mapped as readDelivered maps ours (a failed one keeps the last known answer).
  const answer = frame && (frame.error ? answerOfError(frame.error) : (frame.data?.delivered ?? null));
  const delivered = frame ? (answer ?? frame.data?.delivered ?? false) : ownDelivered;
  const known = frame ? answer !== null : ownKnown;

  // Read on mount and again whenever drafts change (the first publish happens in this very page via the DraftBar).
  // In a frame its read already does both: none of our own.
  useEffect(() => {
    if (framed) return;
    let live = true;
    const load = () =>
      void readDelivered().then((d) => {
        if (!live) return;
        if (d === null) {
          setKnown(false); // failed read: guard falls back to a fresh read each time
          return;
        }
        setDelivered(d);
        setKnown(true);
      });
    load();
    const off = onDraftsChanged(load);
    return () => {
      live = false;
      off();
    };
  }, [framed]);

  /** Runs `run` at once on a project that is not delivered; otherwise after the "ÁP NGAY" confirmation. */
  const guard = useCallback(async (what: string, run: () => void | Promise<void>) => {
    // ponytail: inline (Studio matrix edits, one call per cell) trusts the state read on mount — no request per cell, and
    // `run` stays synchronous so a change made just before leaving the page is not lost. Refreshed on the drafts bus (the DraftBar
    // publishes in this page); stale only if the first publish happens in another tab. A failed read leaves it unknown.
    if (inline && known ? delivered : await isDelivered()) setPending({ what, run });
    else await run();
  }, [inline, known, delivered]);

  const done = (go: boolean) => {
    const p = pending;
    setPending(null);
    if (go && p) void p.run();
  };
  const notice = inline && delivered ? <StatusText tone="warn">{t("studio.build.access.applyNow.label")}</StatusText> : null;
  const dialog = inline ? (
    pending && (
      <InlineConfirm
        tone="warning"
        message={`${t("studio.build.access.applyNow.label")}: ${pending.what}`}
        confirmLabel={t("direct.confirm")}
        cancelLabel={t("direct.cancel")}
        onConfirm={() => done(true)}
        onCancel={() => done(false)}
      >
        {t("studio.build.access.applyNow.body")}
      </InlineConfirm>
    )
  ) : (
    <Modal
      open={pending !== null}
      onOpenChange={(o) => {
        if (!o) setPending(null);
      }}
      title={t("direct.title")}
      footer={
        <>
          <Button onClick={() => setPending(null)}>{t("direct.cancel")}</Button>
          <Button
            variant="brand"
            onClick={() => {
              const p = pending;
              setPending(null);
              if (p) void p.run();
            }}
          >
            {t("direct.confirm")}
          </Button>
        </>
      }
    >
      <p>{pending?.what}</p>
      <p className="slds-m-top_small">{t("direct.body")}</p>
    </Modal>
  );

  return { delivered, guard, dialog, notice };
}

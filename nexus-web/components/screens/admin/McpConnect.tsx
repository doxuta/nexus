"use client";

// Vận hành › Việc tự động › Khoá truy cập › "Kết nối agent (MCP)" (issue #3): a builder lets an outside agent (Claude Code,
// Claude Desktop) PROPOSE changes to this project. BE contract (lib/admin.ts, /admin/mcp-tokens, builder-gated):
// issue → the key once, with the exact `claude mcp add` line; list → the live keys; revoke → asked in the row.
// Every key writes its proposals to one assistant conversation; "Mở cuộc trò chuyện" loads it into the assistant panel,
// where the cards are applied like the assistant's own. Same secret rules as ApiKeys.tsx: the key comes from a plain
// `await` and lives only in state, never in a query/mutation cache, URL or storage.
import { useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { useMemo, useRef, useState } from "react";
import { Button } from "@/components/slds/Button";
import { DataTable, fill } from "@/components/slds/DataTable";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { toast } from "@/components/ui/Toaster";
import { useOpenThread } from "@/components/studio/assistant/AssistantProvider";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { QueryState } from "@/components/studio/QueryState";
import { SecretOnceBlock } from "@/components/studio/SecretOnceBlock";
import { OperateSection } from "@/components/studio/operate/OperateSection";
import { useScopeGate } from "@/components/studio/operate/useScopeGate";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { ApiError, claudeMcpAddCommand, createMcpToken, listMcpTokens, revokeMcpToken, type McpToken } from "@/lib/admin";
import { formatDateTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { useStudioIdentity } from "@/lib/studioContext";

const prefixText = (k: Pick<McpToken, "tokenPrefix">) => fill(t("studio.operate.mcp.prefix"), { prefix: k.tokenPrefix });
const LIST_KEY = (slug: string) => ["project", slug, "operate", "mcp"] as const;

/** Mount gate, as ApiKeys: nothing is fetched or drawn while the token in hand is not this project's. */
export function McpConnect({ slug }: { slug: string }) {
  const identity = useStudioIdentity();
  if (!identity.matches(slug)) {
    return (
      <div className="slds-is-relative slds-p-around_large">
        <Spinner size="small" />
      </div>
    );
  }
  return <McpConnectSection key={slug} slug={slug} />;
}

function McpConnectSection({ slug }: { slug: string }) {
  const qc = useQueryClient();
  const guard = useScopeGate(true);
  const openThread = useOpenThread();
  const tokens = useProjectQuery(slug, ["operate", "mcp"], listMcpTokens);
  const [made, setMade] = useState<{ token: McpToken; secret: string } | null>(null);
  const [issuing, setIssuing] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [revokeFor, setRevokeFor] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [rowErr, setRowErr] = useState<Record<string, string>>({});
  const newBtn = useRef<HTMLButtonElement>(null);
  const refresh = () => qc.invalidateQueries({ queryKey: LIST_KEY(slug) });

  async function issue() {
    setIssuing(true);
    setErr(null);
    try {
      if (!guard.assertScope?.()) {
        setErr(t("studio.operate.mcp.err.scope"));
        return;
      }
      setMade(await createMcpToken()); // plain await: the key must not enter a mutation cache
      void refresh();
    } catch (ex) {
      setErr(t(ex instanceof ApiError && ex.code === "MCP_TOKEN_WORKSPACE_UNSUPPORTED" ? "studio.operate.mcp.err.workspace" : "studio.operate.mcp.err.issue"));
    } finally {
      setIssuing(false);
    }
  }

  async function act(k: McpToken, run: () => Promise<void>, failKey: Parameters<typeof t>[0]) {
    setBusy(k.id);
    setRowErr((m) => ({ ...m, [k.id]: "" }));
    try {
      if (!guard.assertScope?.()) {
        setRowErr((m) => ({ ...m, [k.id]: t("studio.operate.mcp.err.scope") }));
        return;
      }
      await run();
    } catch (ex) {
      // The key's conversation was deleted: its new proposals have nowhere to land, so say what fixes it.
      setRowErr((m) => ({ ...m, [k.id]: t(ex instanceof ApiError && ex.code === "SESSION_NOT_FOUND" ? "studio.operate.mcp.open.gone" : failKey) }));
    } finally {
      setBusy(null);
    }
  }

  const revoke = (k: McpToken) =>
    act(k, async () => {
      await revokeMcpToken(k.id);
      setRevokeFor(null);
      toast.success(fill(t("studio.operate.mcp.revoke.done"), { prefix: prefixText(k) }));
      await refresh();
    }, "studio.operate.mcp.revoke.error");

  const columns = useMemo<ColumnDef<McpToken>[]>(
    () => [
      { id: "prefix", header: t("studio.operate.mcp.col.prefix"), accessorFn: (k) => prefixText(k), cell: ({ row: { original: k } }) => <span className="slds-text-font_monospace">{prefixText(k)}</span> },
      { id: "expires", header: t("studio.operate.mcp.col.expires"), accessorFn: (k) => k.expiresAt, cell: ({ row: { original: k } }) => formatDateTime(k.expiresAt) },
      { id: "used", header: t("studio.operate.mcp.col.used"), accessorFn: (k) => k.lastUsedAt ?? "", cell: ({ row: { original: k } }) => (k.lastUsedAt ? formatDateTime(k.lastUsedAt) : t("studio.operate.mcp.unused")) },
      {
        id: "actions",
        header: () => <span className="slds-assistive-text">{t("studio.operate.mcp.col.actions")}</span>,
        enableSorting: false,
        cell: ({ row: { original: k } }) => (
          <span className="slds-grid slds-grid_align-end gap-2">
            {openThread && (
              <Button variant="neutral" className="whitespace-nowrap" disabled={busy === k.id} aria-label={fill(t("studio.operate.mcp.open.rowOf"), { prefix: prefixText(k) })}
                onClick={() => void act(k, () => openThread(k.sessionId), "studio.operate.mcp.open.error")}>
                {t("studio.operate.mcp.open.row")}
              </Button>
            )}
            <Button variant="base" className="whitespace-nowrap" disabled={busy === k.id} aria-label={fill(t("studio.operate.mcp.revoke.rowOf"), { prefix: prefixText(k) })} onClick={() => setRevokeFor(k.id)}>
              {t("studio.operate.mcp.revoke.row")}
            </Button>
          </span>
        ),
      },
    ],
    // act/openThread close over the row state; busy gates the row buttons.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [busy, openThread],
  );

  const rowDetail = (k: McpToken) => {
    const parts = [];
    if (rowErr[k.id]) parts.push(<ScopedNotification key="e" theme="error">{rowErr[k.id]}</ScopedNotification>);
    if (revokeFor === k.id) {
      parts.push(
        <InlineConfirm
          key="revoke"
          tone="error"
          message={fill(t("studio.operate.mcp.revoke.ask"), { prefix: prefixText(k) })}
          confirmLabel={t("studio.operate.mcp.revoke.confirm")}
          busy={busy === k.id}
          onConfirm={() => void revoke(k)}
          onCancel={() => setRevokeFor(null)}
        />,
      );
    }
    return parts.length ? <div className="flex flex-col gap-2 py-2">{parts}</div> : null;
  };

  return (
    <div className="slds-p-horizontal_medium slds-p-bottom_medium">
      <OperateSection
        title={t("studio.operate.mcp.title")}
        actions={
          <Button ref={newBtn} variant="brand" disabled={issuing || made !== null} onClick={() => void issue()}>
            {issuing ? t("action.saving") : t("studio.operate.mcp.new")}
          </Button>
        }
      >
        <p className="nx-lede max-w-3xl">{t("studio.operate.mcp.lede")}</p>
        {err && <div className="slds-m-top_small"><ScopedNotification theme="error">{err}</ScopedNotification></div>}

        {made && (
          <div className="slds-m-top_medium flex flex-col gap-2" data-testid="mcp-secret">
            <p>{fill(t("studio.operate.mcp.created"), { prefix: prefixText(made.token), time: formatDateTime(made.token.expiresAt) })}</p>
            <SecretOnceBlock
              rows={[
                { label: t("studio.operate.mcp.secret.command"), value: claudeMcpAddCommand(made.secret) },
                { label: t("studio.operate.mcp.secret.label"), value: made.secret },
              ]}
              hint={t("studio.operate.mcp.secret.hint")}
              onDone={() => { setMade(null); queueMicrotask(() => newBtn.current?.focus()); }}
            >
              {t("studio.operate.mcp.secret.warning")}
            </SecretOnceBlock>
          </div>
        )}

        <div className="slds-m-top_medium">
          <QueryState query={tokens} empty={<p className="slds-p-around_medium slds-text-color_weak">{t("studio.operate.mcp.empty")}</p>}>
            <DataTable
              label={t("studio.operate.mcp.table")}
              columns={columns}
              data={tokens.data ?? []}
              getRowId={(k) => k.id}
              stacked
              rowClassName={(k) => (revokeFor === k.id ? "slds-is-selected" : undefined)}
              rowDetail={rowDetail}
            />
          </QueryState>
        </div>
      </OperateSection>
    </div>
  );
}

// lib/appScope.ts — app scope as an EXPLICIT value (specs/074 FR-003). The Studio build pages read `?app=` from the URL
// and ask this module which tables the app holds; nothing here reads or writes localStorage (`nexus.activeApp`), so the
// App face's lens can never leak into the builder.

import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { appNav, appsCatalog, type AppNavEntry, type AppSummary } from "@/lib/api";
import { qk } from "@/lib/query/keys";

// ── Pure derivations (unit-tested in appScope.pure.test.ts) ──────────────────────

/** Entity slugs of an app nav. Entity items carry href "/<entityName>" (lib/api AppNavEntry). */
export function entityNamesFromNav(nav: AppNavEntry[] | null): Set<string> | null {
  if (!nav) return null;
  return new Set(
    nav.filter((i) => i.type === "entity").map((i) => i.href.replace(/^\//, "")),
  );
}

/**
 * Soft-filter a row list by the entity each row belongs to. Unscoped (null) passes everything
 * through; rows with NO entity attribution stay visible (never hide what we can't attribute).
 */
export function splitByScope<T>(
  items: T[],
  entityNames: Set<string> | null,
  nameOf: (item: T) => string | null | undefined,
): { visible: T[]; hidden: number } {
  if (!entityNames) return { visible: items, hidden: 0 };
  const visible = items.filter((item) => {
    const n = nameOf(item);
    return !n || entityNames.has(n);
  });
  return { visible, hidden: items.length - visible.length };
}

/**
 * Entity options for a <select> under scope: scoped entities PLUS the currently-stored value
 * (an out-of-scope selection must keep rendering — a native select silently blanks otherwise,
 * same trap as DataModelBuilder's withStoredFallback).
 */
export function scopeEntityOptions<T extends { name: string }>(
  entities: T[],
  entityNames: Set<string> | null,
  current?: string | null,
): T[] {
  if (!entityNames) return entities;
  return entities.filter((e) => entityNames.has(e.name) || e.name === current);
}

/**
 * A dashboard is in scope when ANY of its widgets reads an in-scope entity. No widgets /
 * no entity info → visible (honest: can't attribute → don't hide).
 */
export function dashboardInScope(
  widgetEntities: Array<string | null | undefined>,
  entityNames: Set<string> | null,
): boolean {
  if (!entityNames) return true;
  const named = widgetEntities.filter((e): e is string => !!e);
  if (named.length === 0) return true;
  return named.some((e) => entityNames.has(e));
}

// ── Hook ───────────────────────────────────────────────────────────────────────

export interface AppEntityNames {
  /** The app summary, null = no app asked for (or not found). */
  app: AppSummary | null;
  /** Table codes of the app's menu; null = unscoped (no app), still loading, or unknown app. */
  entityNames: Set<string> | null;
  isLoading: boolean;
  /** The app's menu failed to load: scope unknown, nothing in scope (an empty set, never "everything"). */
  error: boolean;
  /** `app` is not in GET /apps — the page says "Không có app {mã}" instead of an empty list nobody can explain. */
  unknown: boolean;
}

/**
 * Tables of one app by its code. `app = null` = the whole project (no filter). Switching `app` reads as loading with no
 * names at once: the client-wide keepPreviousData is switched off so one app's menu never stands in for another's
 * (queue #297/#454).
 */
export function useAppEntityNames(app: string | null): AppEntityNames {
  const appsQ = useQuery({ queryKey: qk.apps(), queryFn: appsCatalog, staleTime: 60_000, enabled: app !== null });
  const meta = useMemo(() => (app ? appsQ.data?.find((a) => a.name === app) ?? null : null), [app, appsQ.data]);
  const unknown = app !== null && appsQ.isSuccess && !meta;
  const navQ = useQuery({
    queryKey: qk.appNav(app ?? ""),
    queryFn: () => appNav(app as string),
    staleTime: 60_000,
    enabled: app !== null && !!meta,
    placeholderData: undefined,
  });
  const entityNames = useMemo(
    () => (app === null || unknown ? null : navQ.isError ? new Set<string>() : entityNamesFromNav(navQ.data?.items ?? null)),
    [app, unknown, navQ.isError, navQ.data],
  );
  const isLoading = app !== null && !unknown && !appsQ.isError && (appsQ.isPending || (!!meta && navQ.isPending));
  return { app: meta, entityNames, isLoading, error: app !== null && (appsQ.isError || navQ.isError), unknown };
}

/**
 * Chọn bảng nào khi builder phải tự chọn một bảng theo app đang scope: xưa là khung sửa-trường lúc
 * mở lần đầu (khung Bảng nay là danh sách, không tự mở bảng nào), nay là bảng của bản nháp quy
 * trình duyệt mới (ApprovalBuilder.startCreate, queue #456).
 *
 * Trả về -1 = KHÔNG chọn gì (caller phải hiện empty-state trung thực).
 *
 * Vì sao là hàm riêng có tên: quy tắc này đã sai HAI lần theo hai cách khác nhau.
 *  1) `before-02` — không có cold-start theo scope, nên "Bố cục" mở ra bảng đầu tiên của TENANT.
 *  2) app RỖNG — có cold-start nhưng khi không tìm thấy thì KHÔNG làm gì, nên index giữ nguyên 0 =
 *     vẫn là bảng đầu tiên của tenant. Người dùng thấy bảng của app KHÁC nằm dưới một danh sách
 *     rỗng, và nút "Thêm trường" khi đó ghi trường vào bảng của người khác.
 * Đặt tên cho quy tắc rồi ghim bằng test thì nó không sai lần thứ ba.
 */
export function pickScopedEntityIndex(
  entityNames: readonly string[],
  scope: ReadonlySet<string> | null,
): number {
  if (entityNames.length === 0) return -1;
  if (!scope) return 0; // không scope (xem toàn tenant) — bảng đầu tiên là lựa chọn hợp lý
  return entityNames.findIndex((n) => scope.has(n)); // -1 khi app chưa có bảng nào
}

/**
 * Mục nào (luật / quy trình) khung Tự động hoá (WorkflowBuilder) và khung Duyệt (ApprovalBuilder)
 * giữ mở dưới scope của app.
 *
 * Trả về id cần mở, hoặc null = KHÔNG mở mục nào (caller hiện empty-state).
 *
 * Cùng họ bug với pickScopedEntityIndex, lần thứ ba (queue #297): khung chọn luật ĐẦU TIÊN CỦA
 * TENANT trước khi lọc theo app, nên app 0 luật mở "Đặt hạn SLA" của Ticketing kèm nút Xoá/Cập nhật.
 * Lần thứ tư (queue #456): khung Duyệt y hệt — app 0 quy trình mở "Duyệt hợp đồng" của CRM.
 *  - `current` còn trong danh sách đã lọc → giữ.
 *  - `current` chưa có trong danh sách (luật vừa tạo, danh sách chưa tải lại) → giữ.
 *  - còn lại (chưa mở gì, hoặc luật đang mở bị scope ẩn) → luật đầu tiên ĐÃ LỌC, hoặc null.
 */
export function pickScopedRuleId(
  all: readonly { id: string }[],
  visible: readonly { id: string }[],
  current: string | null,
): string | null {
  if (current && (visible.some((w) => w.id === current) || !all.some((w) => w.id === current))) return current;
  return visible[0]?.id ?? null;
}

/**
 * The app a new table is put in when the form does not ask: `?app=` when it names a real app of the project, else the
 * project's only app, else null (two or more apps and no valid choice: the form asks, never guesses).
 */
export function resolveCreateApp(apps: AppSummary[], appParam: string | null | undefined): AppSummary | null {
  const named = appParam ? apps.find((a) => a.name === appParam) : undefined;
  return named ?? (apps.length === 1 ? apps[0] : null);
}

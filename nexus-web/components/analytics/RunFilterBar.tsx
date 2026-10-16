"use client";

// RunFilterBar — the viewer filter bar of dashboards/[id] + reports/[id], on SLDS (specs/038 T029).
// Wired to the REAL run overlay (api-conventions §4.4): the date range (DateRangePicker popover) and
// the owner scope flow into ?dateFrom/?dateTo/?ownerId of /run and /export.csv; each active filter
// is a removable pill (blueprint `pills`, listbox of pill options as a borderless selection group —
// arrow keys move between pills, Delete/Backspace removes).
//
// Owner scope — HONEST about permissions: the user list comes from GET /admin/users (admin-only).
// Admin viewer → combobox over every user (blueprint `combobox` autocomplete, type to filter by
// name or email); anyone else → a "Chỉ của tôi" checkbox (created_by = their own id) — no
// pretending to have a directory the backend will not serve.

import { useQuery } from "@tanstack/react-query";
import { UserRound } from "lucide-react";
import { getCachedMe, getMe } from "@/lib/api";
import { listUsers } from "@/lib/admin";
import { t, tf } from "@/lib/i18n";
import { qk } from "@/lib/query/keys";
import { countActiveFilters, filterChips, removeChip, type RunFilters } from "@/lib/runFilters";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { Combobox } from "@/components/slds/Combobox";
import { Pill, PillContainer } from "@/components/slds/Pill";
import { DateRangePicker } from "./DateRangePicker";

/** The directory allows a user without a full name (invited, never set one): fall back to the email. */
const nameOf = (u: { fullName?: string | null; email: string }) => u.fullName || u.email;

export function RunFilterBar({
  filters,
  onChange,
}: {
  filters: RunFilters;
  onChange: (next: RunFilters) => void;
}) {
  // Current user: cached /auth/me from login, refreshed lazily.
  const { data: me } = useQuery({
    queryKey: qk.me(),
    queryFn: getMe,
    staleTime: 5 * 60_000,
    initialData: getCachedMe() ?? undefined,
  });

  // Admin-only user directory for the owner picker. retry:false — a 403 is an answer, not a
  // flake; on failure we fall back to the self-scope checkbox below.
  const isAdmin = Boolean(me?.isAdmin);
  const { data: users, isError: usersDenied } = useQuery({
    queryKey: ["admin-users", "picker"],
    queryFn: listUsers,
    staleTime: 5 * 60_000,
    retry: false,
    enabled: isAdmin,
  });

  const ownerLabel = (() => {
    if (!filters.ownerId) return undefined;
    if (me && filters.ownerId === me.id) return t("page.runFilter.mine");
    const u = users?.find((x) => x.id === filters.ownerId);
    return u ? tf("page.runFilter.ownerValue", { name: nameOf(u) }) : undefined;
  })();

  const chips = filterChips(filters, ownerLabel);
  const active = countActiveFilters(filters);
  const picker = isAdmin && users && users.length > 0 && !usersDenied;

  return (
    <div className="flex flex-wrap items-center gap-2">
      <DateRangePicker
        value={filters}
        onChange={(d) => onChange({ ...filters, dateFrom: d.dateFrom, dateTo: d.dateTo })}
      />

      {picker ? (
        <Combobox
          variant="autocomplete"
          label={t("page.runFilter.owner")}
          labelHidden
          placeholder={t("page.runFilter.ownerAll")}
          noResultsText={t("page.runFilter.noUsers")}
          className="w-64"
          options={users.map((u) => ({
            value: u.id,
            label: u.id === me?.id ? tf("page.runFilter.me", { name: nameOf(u) }) : nameOf(u),
            meta: u.fullName ? u.email : undefined, // a nameless user's label already is the email
            // Person option (blueprint combobox entity option): the email shows on the meta line.
            icon: { glyph: UserRound, name: "user", label: t("page.runFilter.userKind") },
          }))}
          value={filters.ownerId ?? null}
          onChange={(ownerId) => onChange({ ...filters, ownerId: ownerId ?? undefined })}
        />
      ) : (
        // Non-admin (or the directory was refused): the only honest scope is oneself.
        <Checkbox
          label={t("page.runFilter.mine")}
          title={t("page.runFilter.mineHint")}
          disabled={!me?.id}
          checked={Boolean(filters.ownerId && filters.ownerId === me?.id)}
          onChange={() =>
            onChange({ ...filters, ownerId: filters.ownerId === me?.id ? undefined : me?.id })
          }
        />
      )}

      {/* Active filter pills — mirror the server's appliedFilters; removing one clears it. */}
      {chips.length > 0 && (
        <PillContainer variant="selection" className="slds-is-expanded" label={t("page.runFilter.applied")}>
          {chips.map((c) => (
            <Pill
              key={c.kind}
              label={c.label}
              removeLabel={t("page.runFilter.removeChip")}
              onRemove={() => onChange(removeChip(filters, c.kind))}
            />
          ))}
        </PillContainer>
      )}

      {active > 1 && (
        <Button variant="base" onClick={() => onChange({})}>
          {t("page.runFilter.clearAll")}
        </Button>
      )}
    </div>
  );
}

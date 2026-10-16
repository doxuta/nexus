"use client";

import { useEffect, useState } from "react";
import { Select } from "@/components/slds/Select";
import { listRoles, listAdminUsers, type RoleSummary, type AdminUser } from "@/lib/api";
import { listTeams, type AdminTeam } from "@/lib/admin";
import type { FieldMeta } from "@/lib/api";
import { t } from "@/lib/i18n";

// Recipient of an in-app notification (queue #281). The rule stores ONE string (the engine's
// userId): "$owner", "$field:<user field>", "$role:<id>", "$team:<id>" or a user id.
type Kind = "owner" | "field" | "person" | "role" | "team";

export function parseRecipient(spec: string): { kind: Kind; value: string } {
  if (!spec || spec === "$owner") return { kind: "owner", value: "" };
  if (spec.startsWith("$field:")) return { kind: "field", value: spec.slice(7) };
  if (spec.startsWith("$role:")) return { kind: "role", value: spec.slice(6) };
  if (spec.startsWith("$team:")) return { kind: "team", value: spec.slice(6) };
  return { kind: "person", value: spec };
}

const PICK_LABEL = {
  field: "page.workflow.recipient.pick.field",
  person: "page.workflow.recipient.pick.person",
  role: "page.workflow.recipient.pick.role",
  team: "page.workflow.recipient.pick.team",
  owner: "page.workflow.recipient.pick.field",
} as const;

const PREFIX: Record<Kind, string> = { owner: "$owner", field: "$field:", person: "", role: "$role:", team: "$team:" };

export function NotifyRecipientPicker({
  value,
  entityFields,
  onChange,
}: {
  value: string;
  entityFields: FieldMeta[];
  onChange: (spec: string) => void;
}) {
  // "Một người cụ thể" with nobody picked yet is the empty string, which reads as "$owner" — so the
  // chosen kind is remembered here until a value exists.
  const [kindSel, setKindSel] = useState<Kind>(parseRecipient(value).kind);
  const parsed = parseRecipient(value);
  const kind = value ? parsed.kind : kindSel;
  const picked = value ? parsed.value : "";
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [roles, setRoles] = useState<RoleSummary[]>([]);
  const [teams, setTeams] = useState<AdminTeam[]>([]);
  // Per kind: "ok" once its directory loaded, "fail" if the load failed. Unset = still loading.
  const [loaded, setLoaded] = useState<Partial<Record<Kind, "ok" | "fail">>>({});

  // Each directory loads only when its kind is chosen; a failed load leaves an empty list.
  useEffect(() => {
    let alive = true;
    const load = kind === "person" ? listAdminUsers().then((r) => alive && setUsers(r))
      : kind === "role" ? listRoles().then((r) => alive && setRoles(r))
      : kind === "team" ? listTeams().then((r) => alive && setTeams(r))
      : null;
    load?.then(() => alive && setLoaded((m) => ({ ...m, [kind]: "ok" })))
      .catch(() => alive && setLoaded((m) => ({ ...m, [kind]: "fail" })));
    return () => { alive = false; };
  }, [kind]);

  const userFields = entityFields.filter((f) => f.fieldType === "user");
  const options: { value: string; label: string }[] =
    kind === "field" ? userFields.map((f) => ({ value: f.name, label: f.label || f.name }))
    : kind === "person" ? users.map((u) => ({ value: u.id, label: u.fullName || u.email }))
    : kind === "role" ? roles.map((r) => ({ value: r.id, label: r.label || r.name }))
    : kind === "team" ? teams.map((tm) => ({ value: tm.id, label: tm.name }))
    : [];
  // A saved value no longer in the list stays visible instead of silently blanking on Save.
  if (picked && !options.some((o) => o.value === picked)) {
    // Only a LOADED directory can say "gone"; while loading (or after a failed load) the saved
    // recipient must not look deleted.
    const state = loaded[kind];
    options.push({
      value: picked,
      label: kind === "field" || state === "fail" ? picked
        : state === "ok" ? t("page.workflow.recipient.stale")
        : t("shell.nav.loading"),
    });
  }

  return (
    <>
      <Select
        label={t("page.workflow.recipient.kind")}
        placeholder={false}
        value={kind}
        options={[
          { value: "owner", label: t("page.workflow.recipient.owner") },
          { value: "field", label: t("page.workflow.recipient.field") },
          { value: "person", label: t("page.workflow.recipient.person") },
          { value: "role", label: t("page.workflow.recipient.role") },
          { value: "team", label: t("page.workflow.recipient.team") },
        ]}
        onChange={(e) => {
          const k = e.target.value as Kind;
          setKindSel(k);
          onChange(PREFIX[k]);
        }}
      />
      {kind !== "owner" && (
        <Select
          label={t(PICK_LABEL[kind])}
          placeholder={t("page.workflow.recipient.pickPh")}
          value={picked}
          options={options}
          onChange={(e) => onChange(PREFIX[kind] + e.target.value)}
        />
      )}
    </>
  );
}

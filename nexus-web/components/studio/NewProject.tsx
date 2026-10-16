"use client";

// Dự án mới (specs/073 tao1 T521; demo A shots 1440-12/13): a page, not a modal. Left: the form. Right (from 1024px): what the
// project will contain — printed only from GET /admin/verticals/builtin, never invented. Behind the `nha` gate (a build
// session cannot create projects). Secrets (FR-103): createProject is a plain async call, so the one-time password lives in
// this component's state only — not in the query/mutation cache, storage, URL or toast — and leaves state on "Xong".
import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, createProject, enterWorkspace, listBuiltinVerticals, type CreatedProject } from "@/lib/api";
import { slugifyAppName } from "@/lib/apps";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { assistantReadsBrief, savePendingBrief } from "@/lib/studioAssistantBrief";
import { homeSlug } from "@/lib/studioContext";
import { Button } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { ComboboxField, matches } from "@/components/slds/Combobox";
import { Checkbox } from "@/components/slds/Checkbox";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Textarea } from "@/components/slds/Textarea";
import { VisualPicker } from "@/components/slds/VisualPicker";
import { ContextGate } from "./ContextGate";
import { TENANT_SLUG_RE, TenantCredentialsReveal } from "./workspaceShared";

type Mode = "pack" | "blank" | "ai";

export function NewProject() {
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [slugTouched, setSlugTouched] = useState(false);
  const [industry, setIndustry] = useState("");
  const picked = useRef(false); // the combobox clears its text right after a pick; keep the chosen label

  const [email, setEmail] = useState("");
  const [fullName, setFullName] = useState("");
  const [generate, setGenerate] = useState(true);
  const [password, setPassword] = useState("");
  const [mode, setMode] = useState<Mode>("blank");
  const [pack, setPack] = useState("");
  const [brief, setBrief] = useState("");
  const [saving, setSaving] = useState(false);
  const [fieldErr, setFieldErr] = useState<{ slug?: string; name?: string; adminEmail?: string; adminPassword?: string }>({});
  const [formErr, setFormErr] = useState<string | null>(null);
  const [done, setDone] = useState<CreatedProject | null>(null);
  const [opening, setOpening] = useState(false);

  const packs = useQuery({ queryKey: ["verticals", "builtin"], queryFn: listBuiltinVerticals });
  const builtins = packs.data ?? [];
  const chosen = builtins.find((b) => b.slug === (pack || builtins[0]?.slug));
  const secretHeld = done?.password != null;

  // Leaving with the password unsaved is blocked in the page (FR-103): the browser's own prompt covers close/reload.
  useEffect(() => {
    if (!secretHeld) return;
    const stop = (e: BeforeUnloadEvent) => { e.preventDefault(); };
    window.addEventListener("beforeunload", stop);
    return () => window.removeEventListener("beforeunload", stop);
  }, [secretHeld]);

  const modes = [
    { value: "pack", label: t("studio.newProject.mode.pack"), description: t("studio.newProject.mode.packDesc") },
    { value: "blank", label: t("studio.newProject.mode.blank"), description: t("studio.newProject.mode.blankDesc") },
    ...(assistantReadsBrief ? [{ value: "ai", label: t("studio.newProject.mode.ai"), description: t("studio.newProject.mode.aiDesc") }] : []),
  ];
  const canSubmit = name.trim() !== "" && TENANT_SLUG_RE.test(slug) && email.trim() !== "" && (generate || password !== "") && !saving;

  function mapError(e: unknown) {
    if (!(e instanceof ApiError)) return setFormErr(errorMessage(e));
    switch (e.code) {
      case "TENANT_ALREADY_EXISTS": return setFieldErr((f) => ({ ...f, slug: t("page.packInstall.tenant.slugTaken") }));
      case "TENANT_SLUG_INVALID": return setFieldErr((f) => ({ ...f, slug: e.message }));
      case "TENANT_NAME_TOO_LONG": return setFieldErr((f) => ({ ...f, name: e.message }));
      case "PASSWORD_TOO_WEAK": return setFieldErr((f) => ({ ...f, adminPassword: e.message }));
      case "TENANT_FIELD_REQUIRED":
        if (e.details.field === "adminEmail") return setFieldErr((f) => ({ ...f, adminEmail: t("page.packInstall.tenant.required") }));
        return setFormErr(e.message);
      case "VERTICAL_BUILTIN_NOT_FOUND": return setFormErr(t("studio.newProject.packMissing"));
      case "TENANT_PASSWORD_CONFLICT": return setFormErr(t("page.packInstall.tenant.passwordConflict"));
      default: return setFormErr(errorMessage(e));
    }
  }

  async function submit(ev: FormEvent) {
    ev.preventDefault();
    if (!canSubmit) return;
    setSaving(true);
    setFormErr(null);
    setFieldErr({});
    try {
      const res = await createProject({
        name: name.trim(), slug, ...(industry.trim() ? { industry: industry.trim() } : {}),
        adminEmail: email.trim(), ...(fullName.trim() ? { adminFullName: fullName.trim() } : {}),
        ...(generate ? { generatePassword: true } : { adminPassword: password }),
        start: mode === "pack" ? { mode, pack: chosen?.slug } : { mode },
      });
      if (mode === "ai" && brief.trim()) savePendingBrief({ slug: res.slug, brief: brief.trim() });
      setPassword("");
      setDone(res);
    } catch (e) {
      mapError(e);
    } finally {
      setSaving(false);
    }
  }

  async function open(s: string) {
    setOpening(true);
    try {
      await enterWorkspace(s);
    } catch (e) {
      setOpening(false);
      setFormErr(errorMessage(e));
    }
  }

  let body;
  if (done) {
    const failed = done.start.status === "failed";
    body = (
      <div className="flex max-w-[640px] flex-col gap-4">
        <ScopedNotification theme="success">{tf("studio.newProject.created", { name: done.name })}</ScopedNotification>
        {failed && <ScopedNotification theme="error">{tf("studio.newProject.startFailed", { message: done.start.error?.message ?? done.start.mode })}</ScopedNotification>}
        {formErr && <ScopedNotification theme="error">{formErr}</ScopedNotification>}
        {done.password != null ? (
          <TenantCredentialsReveal
            result={{
              tenant: { id: "", slug: done.slug, name: done.name, status: "active", createdAt: "" },
              admin: { id: "", email: done.adminEmail, isAdmin: true },
              oneTimeCredentials: { password: done.password },
            }}
            onDone={() => setDone({ ...done, password: null })}
          />
        ) : (
          <div><Button variant="brand" disabled={opening} onClick={() => open(done.slug)}>{t("studio.newProject.open")}</Button></div>
        )}
      </div>
    );
  } else {
    body = (
      <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,640px)_minmax(0,1fr)]">
        <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
          {formErr && <ScopedNotification theme="error">{formErr}</ScopedNotification>}
          <Input label={t("studio.newProject.name")} required value={name} disabled={saving} maxLength={150} placeholder={t("studio.newProject.namePh")}
            error={fieldErr.name}
            onChange={(e) => { setName(e.target.value); if (!slugTouched) setSlug(slugifyAppName(e.target.value).replace(/_/g, "-")); }} />
          <Input label={t("studio.newProject.slug")} required value={slug} disabled={saving} error={fieldErr.slug} help={t("studio.newProject.slugHelp")}
            onChange={(e) => { setSlug(e.target.value); setSlugTouched(true); }} />
          <ComboboxField mode="autocomplete" label={t("studio.newProject.industry")} placeholder={t("studio.newProject.industryPh")} disabled={saving}
            inputValue={industry} onInputChange={(v) => { if (v === "" && picked.current) { picked.current = false; return; } setIndustry(v.slice(0, 120)); }} selected={[]} noResultsText={false}
            options={builtins.map((b) => ({ value: b.slug, label: b.manifest.name })).filter((o) => industry.trim() !== "" && matches(o.label, industry))}
            onSelect={(o) => { picked.current = true; setIndustry(o.label); }} onDeselect={() => {}} />
          <Input label={t("studio.newProject.email")} required type="email" value={email} disabled={saving} error={fieldErr.adminEmail}
            onChange={(e) => setEmail(e.target.value)} />
          <Input label={t("studio.newProject.fullName")} value={fullName} disabled={saving} maxLength={150} onChange={(e) => setFullName(e.target.value)} />
          <Checkbox label={t("studio.newProject.generate")} help={t("studio.newProject.generateHelp")} checked={generate} disabled={saving}
            onChange={(e) => setGenerate(e.target.checked)} />
          {!generate && (
            <Input label={t("studio.newProject.password")} required type="password" autoComplete="new-password" value={password} disabled={saving}
              error={fieldErr.adminPassword} onChange={(e) => setPassword(e.target.value)} />
          )}
          <VisualPicker label={t("studio.newProject.start")} variant="vertical" value={mode} options={modes} disabled={saving}
            onChange={(v) => setMode(v as Mode)} />
          {mode === "pack" && (
            builtins.length === 0
              ? <p className="slds-text-color_weak">{t("studio.newProject.packNone")}</p>
              : <Select label={t("studio.newProject.pack")} value={chosen?.slug ?? ""} disabled={saving}
                  options={builtins.map((b) => ({ value: b.slug, label: b.manifest.name || b.slug }))} onChange={(e) => setPack(e.target.value)} />
          )}
          {mode === "ai" && <Textarea label={t("studio.newProject.brief")} value={brief} disabled={saving} rows={4} onChange={(e) => setBrief(e.target.value)} />}
          <div className="flex gap-2">
            <Button variant="brand" type="submit" disabled={!canSubmit}>{saving ? t("studio.newProject.creating") : t("studio.newProject.create")}</Button>
          </div>
        </form>
        <Card title={t("studio.newProject.willHave")}>
          <p>
            {mode === "pack" && chosen
              ? tf("studio.newProject.willHavePack", { name: chosen.manifest.name || chosen.slug, count: chosen.entityCount })
              : t("studio.newProject.willHaveBlank")}
          </p>
        </Card>
      </div>
    );
  }

  return (
    <ContextGate slug={homeSlug()} ctx="nha">
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-[1560px] flex-col gap-4 p-6 max-md:p-4">
          <header>
            <Link href="/studio" className="slds-text-body_small">{t("studio.newProject.back")}</Link>
            <h1 className="slds-text-heading_large">{t("studio.newProject.title")}</h1>
            <p className="nx-lede slds-m-top_xx-small">{t("studio.newProject.lede")}</p>
          </header>
          {body}
        </div>
      </div>
    </ContextGate>
  );
}

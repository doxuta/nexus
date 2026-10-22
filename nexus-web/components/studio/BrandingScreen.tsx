"use client";

// BrandingScreen — Thương hiệu (specs/074 lát f2-branding-preview; data contract: ../specs/013-tenant-branding/spec.md §5).
// Two columns: [form 340px | the customer's screen]. The frame is CustomerPreviewFrame under the UNSAVED accent, so a
// colour change shows at once on exactly the pixels the customer app paints with the brand ramp; Studio chrome stays Nexus.
// Lưu/Huỷ are the page's UnsavedBar (only while something is unsaved). Validation is server-side (BRANDING_INVALID) and
// shown verbatim; the only client gates are the logo size/type before base64 and the contrast hint (never blocks saving).
import { useEffect, useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Landmark, RotateCcw } from "lucide-react";
import {
  ApiError, ensureAuth, getTenantBrandingDoc, updateTenantBrandingDoc, type Branding, type BrandingDoc,
  getTenantPayment, updateTenantPayment, type PaymentConfig,
} from "@/lib/api";
import { applyBranding } from "@/lib/branding";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { contrastOnWhite, CONTRAST_MIN } from "@/lib/contrast";
import { t, tf } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { StatusText } from "./StatusText";
import { useProposedChanges } from "@/components/studio/assistant/ProposalOverlay";
import { Button } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { ColorPicker } from "@/components/slds/ColorPicker";
import { fill } from "@/components/slds/DataTable";
import { FileSelector } from "@/components/slds/FileSelector";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { Tabs } from "@/components/slds/Tabs";
import { CustomerPreviewFrame, DEFAULT_CUSTOMER_ACCENT } from "./CustomerPreviewFrame";
import { UnsavedBar } from "./UnsavedBar";

// ~72KB raw → ~96k base64 chars, safely under the server's 100k logoUrl cap.
const LOGO_MAX_BYTES = 72 * 1024;

/** `tabs` = labels of the customer app's first menu entries (BrandingPage reads them from the real app nav). */
export function BrandingScreen({ tabs = [] }: { tabs?: string[] }) {
  const [authed, setAuthed] = useState(false);
  useEffect(() => { ensureAuth().then(() => setAuthed(true)).catch(() => {}); }, []);

  const saved = useQuery({ queryKey: ["tenant-branding"], queryFn: getTenantBrandingDoc, enabled: authed });
  // specs/063 S7-3: on a delivered workspace the builder edits a DRAFT of the branding; the label says so.
  const isDraft = saved.data?.draftState === "edited";

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      {isDraft && (
        <p className="slds-p-horizontal_medium slds-m-top_small">
          <StatusText tone="warn">{t("studio.build.branding.draft.label")}</StatusText>{" "}
          <span className="slds-text-color_weak">{t("studio.build.branding.draft.hint")}</span>
        </p>
      )}
      {saved.isLoading ? (
        <div className="slds-p-around_medium flex items-center gap-2">
          <Spinner size="x-small" inline />
          <span className="slds-text-color_weak">{t("state.loading")}</span>
        </div>
      ) : (
        // Keyed by the query's data version: the form starts from the saved document and starts
        // over whenever the server document changes (first load, after a save) — no state-sync effect.
        <BrandingForm key={saved.dataUpdatedAt} initial={saved.data?.branding ?? {}} tabs={tabs} after={<PaymentSection />} />
      )}
    </div>
  );
}

function BrandingForm({ initial, tabs, after }: { initial: Branding; tabs: string[]; after: ReactNode }) {
  const qc = useQueryClient();
  const [form, setForm] = useState<Branding>(initial);
  const [err, setErr] = useState("");
  const [logoErr, setLogoErr] = useState("");

  const save = useMutation({
    mutationFn: (b: Branding) => updateTenantBrandingDoc(b),
    onSuccess: ({ branding: b, draftState }: BrandingDoc) => {
      const draft = draftState === "edited";
      toast.success(draft ? t("draft.saved") : t(b.appName || b.logoUrl || b.accentColor ? "page.branding.saved" : "page.branding.resetDone"));
      setErr("");
      // A draft is shown in this tab only: never into the shared localStorage boot cache, or every
      // other tab (and the next login page) would paint a colour the customer has not been given.
      applyBranding(b, draft ? { persist: false } : undefined); // brand this session immediately — no reload needed
      qc.setQueryData(["tenant-branding"], { branding: b, draftState } satisfies BrandingDoc);
      emitDraftsChanged("branding"); // a branding draft on a delivered project: the header recounts
    },
    onError: (e) => setErr(e instanceof ApiError ? e.message : t("page.branding.saveFailed")),
  });

  function onLogoFile(file: File | undefined) {
    if (!file) return;
    if (file.size > LOGO_MAX_BYTES) {
      setLogoErr(fill(t("page.branding.logoTooBig"), { kb: Math.round(LOGO_MAX_BYTES / 1024) }));
      return;
    }
    if (!/^image\/(png|jpeg|svg\+xml|webp)$/.test(file.type)) {
      setLogoErr(t("page.branding.logoType"));
      return;
    }
    const reader = new FileReader();
    reader.onload = () => {
      setLogoErr("");
      setForm((f) => ({ ...f, logoUrl: String(reader.result) }));
    };
    reader.readAsDataURL(file);
  }

  const dirty =
    (form.appName ?? "") !== (initial.appName ?? "") ||
    (form.logoUrl ?? "") !== (initial.logoUrl ?? "") ||
    (form.accentColor ?? "") !== (initial.accentColor ?? "");
  // Assistant preview (docType branding): the frame wears the proposed values; the form keeps what is saved.
  const proposal = useProposedChanges("branding")[0];
  const next = (proposal?.after ?? {}) as Branding;
  const prev = (proposal?.before ?? {}) as Branding;
  const shown: Branding = proposal ? { ...form, ...next } : form;
  const was: string[] = [];
  if (proposal) {
    if (next.appName !== undefined && next.appName !== (prev.appName ?? "")) was.push(prev.appName || t("studio.build.ai.branding.defaultName"));
    if (next.accentColor !== undefined && next.accentColor !== (prev.accentColor ?? "")) was.push(prev.accentColor ? tf("studio.build.ai.branding.colorWas", { color: prev.accentColor }) : t("studio.build.ai.branding.defaultColor"));
    if (next.logoUrl !== undefined && next.logoUrl !== (prev.logoUrl ?? "")) was.push(prev.logoUrl ? t("studio.build.ai.branding.logoWas") : t("studio.build.ai.branding.noLogo"));
  }
  const ratio = contrastOnWhite(form.accentColor);
  const lowContrast = ratio !== null && ratio < CONTRAST_MIN;

  return (
    <>
      <div className="flex flex-col items-stretch gap-6 slds-p-around_medium lg:flex-row lg:items-start">
        <div className="flex w-full shrink-0 flex-col gap-4 lg:w-[340px]">
          <Input
            label={t("page.branding.appName")}
            value={form.appName ?? ""}
            onChange={(e) => setForm((f) => ({ ...f, appName: e.target.value }))}
            placeholder={t("page.branding.appNamePh")}
            maxLength={60}
            help={t("page.branding.appNameHelp")}
          />

          <div className="flex flex-col gap-2">
            <div className="flex flex-wrap items-end gap-3">
              <ColorPicker
                label={t("page.branding.accent")}
                value={form.accentColor ?? ""}
                placeholderColor={DEFAULT_CUSTOMER_ACCENT}
                onChange={(hex) => setForm((f) => ({ ...f, accentColor: hex }))}
              />
              {form.accentColor && (
                <Button variant="base" onClick={() => setForm((f) => ({ ...f, accentColor: "" }))}>
                  {t("page.branding.accentDefault")}
                </Button>
              )}
            </div>
            <p className="slds-text-body_small slds-text-color_weak">{t("page.branding.accentHelp")}</p>
            {lowContrast && (
              <p role="status" className="slds-text-body_small slds-text-color_error">
                {fill(t("studio.build.branding.contrast"), { ratio: ratio.toFixed(1).replace(".", ",") })}
              </p>
            )}
          </div>

          <div className="flex flex-col gap-2">
            <FileSelector
              label={t("page.branding.logo")}
              variant="images"
              accept="image/png,image/jpeg,image/svg+xml,image/webp"
              error={logoErr || undefined}
              onFiles={(files) => onLogoFile(files[0])}
            />
            <p className="slds-text-body_small slds-text-color_weak">{t("page.branding.logoHelp")}</p>
            {form.logoUrl && (
              <div className="flex items-center gap-3">
                {/* eslint-disable-next-line @next/next/no-img-element -- data-URI preview */}
                <img src={form.logoUrl} alt={t("page.branding.logoCurrent")} className="size-10 object-contain" />
                <Button variant="base" onClick={() => setForm((f) => ({ ...f, logoUrl: "" }))}>
                  {t("page.branding.logoRemove")}
                </Button>
              </div>
            )}
          </div>

          <div>
            <Button iconLeft={RotateCcw} disabled={save.isPending} onClick={() => { setForm({}); save.mutate({}); }}>
              {t("page.branding.reset")}
            </Button>
          </div>
        </div>

        <div className="min-w-0 flex-1">
          {/* A picture of the customer screen: `inert` (no tab stops), one image for assistive tech. */}
          <div role="img" aria-label={t("studio.build.branding.previewLabel")} inert data-testid="branding-preview">
            <CustomerPreviewFrame accentColor={shown.accentColor} appName={shown.appName || "Nexus"} logoUrl={shown.logoUrl}>
              {proposal && (
                <p className="nx-proposed slds-p-around_x-small slds-m-bottom_small slds-text-body_small" data-proposal-key={proposal.proposalKey}>
                  <StatusText tone="proposed">{t("studio.status.proposed")}</StatusText>
                  {was.length > 0 && <> {tf("studio.build.ai.beforeNamed", { before: was.join(", ") })}</>}
                </p>
              )}
              {tabs.length > 0 && (
                <Tabs label={t("studio.build.branding.previewLabel")} items={tabs.map((label, i) => ({ value: String(i), label, content: null }))} />
              )}
              <div className="flex flex-wrap items-end gap-3 slds-m-top_small">
                <Input label={t("studio.build.branding.sampleField")} defaultValue="" />
                <Button variant="brand">{t("studio.build.branding.sampleButton")}</Button>
              </div>
              <a href="#" className="slds-text-body_small slds-m-top_small slds-show">{t("studio.build.branding.sampleLink")}</a>
            </CustomerPreviewFrame>
          </div>
          <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("studio.build.branding.previewNote")}</p>
        </div>
      </div>
      <div className="slds-p-horizontal_medium slds-p-bottom_medium">{after}</div>
      <UnsavedBar
        dirty={dirty}
        saving={save.isPending}
        error={err || undefined}
        saveLabel={t("page.branding.save")}
        onSave={() => save.mutate(form)}
        onCancel={() => { setForm(initial); setLogoErr(""); setErr(""); }}
      />
    </>
  );
}

// ── DONG-TIEN-KHEP-KIN p1: per-tenant bank config for VietQR (../specs/016-payment-flow/spec.md §6) ───────

function PaymentSection() {
  const saved = useQuery({ queryKey: ["tenant-payment"], queryFn: getTenantPayment });
  return (
    <Card title={t("page.branding.payment.title")} icon={{ glyph: Landmark, name: "currency" }}>
      <p className="slds-text-color_weak slds-m-bottom_small">{t("page.branding.payment.intro")}</p>
      {saved.isLoading ? (
        <div className="flex items-center gap-2">
          <Spinner size="x-small" inline />
          <span className="slds-text-color_weak">{t("state.loading")}</span>
        </div>
      ) : (
        <PaymentForm key={saved.dataUpdatedAt} initial={saved.data ?? {}} />
      )}
    </Card>
  );
}

function PaymentForm({ initial }: { initial: PaymentConfig }) {
  const qc = useQueryClient();
  const [form, setForm] = useState<PaymentConfig>(initial);
  const [err, setErr] = useState("");

  const save = useMutation({
    mutationFn: (p: PaymentConfig) => updateTenantPayment(p),
    onSuccess: (p) => {
      toast.success(t(p.bankBin ? "page.branding.payment.saved" : "page.branding.payment.cleared"));
      setErr("");
      qc.setQueryData(["tenant-payment"], p);
    },
    onError: (e) => setErr(e instanceof ApiError ? e.message : t("page.branding.payment.saveFailed")),
  });

  return (
    <div className="flex flex-col gap-3">
      <div className="slds-grid slds-wrap slds-gutters_x-small">
        <div className="slds-col slds-size_1-of-1 slds-medium-size_1-of-3">
          <Input
            label={t("page.branding.payment.bin")}
            value={form.bankBin ?? ""}
            onChange={(e) => setForm((f) => ({ ...f, bankBin: e.target.value }))}
            placeholder={t("page.branding.payment.binPh")}
            inputMode="numeric"
            maxLength={6}
            help={t("page.branding.payment.binHelp")}
          />
        </div>
        <div className="slds-col slds-size_1-of-1 slds-medium-size_1-of-3">
          <Input
            label={t("page.branding.payment.account")}
            value={form.accountNumber ?? ""}
            onChange={(e) => setForm((f) => ({ ...f, accountNumber: e.target.value }))}
            placeholder={t("page.branding.payment.accountPh")}
          />
        </div>
        <div className="slds-col slds-size_1-of-1 slds-medium-size_1-of-3">
          <Input
            label={t("page.branding.payment.holder")}
            value={form.accountName ?? ""}
            onChange={(e) => setForm((f) => ({ ...f, accountName: e.target.value }))}
            placeholder={t("page.branding.payment.holderPh")}
          />
        </div>
      </div>
      {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
      <div className="flex flex-wrap items-center gap-2">
        <Button variant="brand" disabled={save.isPending} onClick={() => save.mutate(form)}>
          {save.isPending ? t("action.saving") : t("page.branding.payment.save")}
        </Button>
        {(form.bankBin || form.accountNumber) && (
          <Button disabled={save.isPending} onClick={() => { setForm({}); save.mutate({}); }}>
            {t("page.branding.payment.clear")}
          </Button>
        )}
      </div>
    </div>
  );
}

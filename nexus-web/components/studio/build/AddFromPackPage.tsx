"use client";

// AddFromPackPage (specs/074 FR-0xx, lát f2-add-from-pack): `dung/du-lieu/them-tu-goi` — three scrolling steps on one page
// (choose → see the plan → install), one main button per step (architecture "Danh sách kiểm"). Seeing the plan is a dry
// run and writes nothing; only "Cài vào dự án" writes. A delivered project answers 409 PROJECT_DELIVERED_INSTALL_BLOCKED:
// the plan stays visible and the page points at Bảo trì › Nâng cấp. No dialog anywhere: the "type the code" guard for
// plans with warnings is an InlineConfirm. PlanView / InstallResultView are the shared renderers (workspaceShared).

import { useState } from "react";
import Link from "next/link";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { dryRunInstall, installPack, listBuiltinVerticals, ApiError, type PackSource } from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { parsePackFile, requiresTypeName, type InstallPlan, type InstallResult } from "@/lib/verticalPlan";
import { studioHref } from "@/lib/studioRoutes";
import { Button } from "@/components/slds/Button";
import { FileSelector } from "@/components/slds/FileSelector";
import { Input } from "@/components/slds/Input";
import { RadioGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { InstallResultView, PlanView } from "@/components/studio/workspaceShared";

/** What the maker picked: a shipped pack (by code) or a parsed file. `key` is what must be typed back when the plan warns. */
interface Choice { id: string; source: PackSource; name: string; key: string }

const DELIVERED = "PROJECT_DELIVERED_INSTALL_BLOCKED";

export function AddFromPackPage({ slug }: { slug: string }) {
  const qc = useQueryClient();
  const router = useGuardedRouter();
  const packsQ = useQuery({ queryKey: ["studio", "builtinPacks"], queryFn: listBuiltinVerticals, staleTime: 60_000 });
  const packs = packsQ.data ?? [];

  const [choice, setChoice] = useState<Choice | null>(null);
  const [fileNote, setFileNote] = useState<string | null>(null);
  const [fileErr, setFileErr] = useState<string | null>(null);
  const [plan, setPlan] = useState<InstallPlan | null>(null);
  const [planning, setPlanning] = useState(false);
  const [planErr, setPlanErr] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [typed, setTyped] = useState("");
  const [installing, setInstalling] = useState(false);
  const [installErr, setInstallErr] = useState<{ code: string; message: string } | null>(null);
  const [result, setResult] = useState<InstallResult | null>(null);

  // A new choice makes the old plan and any old failure meaningless.
  function pick(next: Choice | null) {
    setChoice(next); setPlan(null); setPlanErr(null); setConfirming(false); setTyped(""); setInstallErr(null);
  }

  function onFiles(files: File[]) {
    const file = files[0];
    if (!file) return;
    setFileErr(null);
    const reader = new FileReader();
    reader.onerror = () => setFileErr(t("studio.build.pack.fileUnreadable"));
    reader.onload = () => {
      const parsed = parsePackFile(String(reader.result ?? ""));
      if (!parsed.ok) { setFileErr(parsed.error); return; }
      setFileNote(tf("studio.build.pack.fileChosen", { name: file.name, n: parsed.entityCount }));
      pick({ id: "file", source: { pack: parsed.pack }, name: parsed.manifest.name, key: parsed.manifest.slug || parsed.manifest.name });
    };
    reader.readAsText(file);
  }

  async function viewPlan() {
    if (!choice || planning) return;
    setPlanning(true); setPlan(null); setPlanErr(null); setInstallErr(null); setConfirming(false);
    try {
      setPlan(await dryRunInstall(choice.source));
    } catch (e) {
      setPlanErr(errorMessage(e));
    } finally {
      setPlanning(false);
    }
  }

  async function install() {
    if (!choice || installing) return;
    setInstalling(true); setInstallErr(null);
    try {
      setResult(await installPack(choice.source));
      setConfirming(false);
    } catch (e) {
      setConfirming(false);
      setInstallErr({ code: e instanceof ApiError ? e.code : "", message: errorMessage(e) });
    } finally {
      setInstalling(false);
      // A failed answer (timeout) may still have committed: refresh in `finally`. Tables and the apps a pack mints.
      void qc.invalidateQueries({ queryKey: qk.entities() });
      void qc.invalidateQueries({ queryKey: ["studio", "count", "entities"] });
      void qc.invalidateQueries({ queryKey: qk.apps() });
      void qc.invalidateQueries({ queryKey: qk.adminApps() });
    }
  }

  const askFirst = !!plan && requiresTypeName(plan);
  const listHref = studioHref(slug, "dung/du-lieu");

  let chooser: React.ReactNode;
  if (packsQ.isPending) {
    chooser = <div className="slds-is-relative slds-p-around_large"><Spinner size="small" /></div>;
  } else if (packsQ.isError) {
    chooser = (
      <ScopedNotification theme="error">
        {t("studio.build.pack.loadFailed")} {errorMessage(packsQ.error)}
        <Button className="slds-m-left_small" onClick={() => void packsQ.refetch()}>{t("studio.query.retry")}</Button>
      </ScopedNotification>
    );
  } else if (packs.length === 0) {
    chooser = (
      <div>
        <p className="slds-text-heading_small">{t("studio.build.pack.empty")}</p>
        <p className="slds-text-color_weak slds-m-top_xx-small">{t("studio.build.pack.emptyHint")}</p>
      </div>
    );
  } else {
    chooser = (
      <RadioGroup
        label={t("studio.build.pack.choose")}
        value={choice?.id === "file" ? "" : choice?.id ?? ""}
        onValueChange={(id) => {
          const p = packs.find((x) => x.slug === id);
          setFileNote(null);
          if (p) pick({ id: p.slug, source: { builtin: p.slug }, name: p.manifest.name || p.slug, key: p.slug });
        }}
        options={packs.map((p) => ({ value: p.slug, label: tf("studio.build.pack.option", { name: p.manifest.name || p.slug, n: p.entityCount }) }))}
      />
    );
  }

  return (
    <div className="slds-p-horizontal_medium slds-p-bottom_large" data-testid="add-from-pack">
      <p className="slds-text-color_weak slds-m-bottom_medium">{t("studio.build.pack.intro")}</p>

      <section className="slds-m-bottom_large" aria-labelledby="pack-step1">
        <h2 id="pack-step1" className="slds-text-heading_small slds-m-bottom_small">{t("studio.build.pack.step1")}</h2>
        {chooser}
        <div className="slds-m-top_medium max-w-xl">
          <FileSelector
            label={t("studio.build.pack.fileLabel")}
            accept="application/json,.json"
            buttonLabel={t("studio.build.pack.fileButton")}
            dropLabel={t("studio.build.pack.fileDrop")}
            error={fileErr ?? undefined}
            onFiles={onFiles}
          />
          {fileNote && !fileErr && <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{fileNote}</p>}
        </div>
      </section>

      <section className="slds-m-bottom_large" aria-labelledby="pack-step2">
        <h2 id="pack-step2" className="slds-text-heading_small slds-m-bottom_small">{t("studio.build.pack.step2")}</h2>
        {!choice && <p className="slds-text-color_weak">{t("studio.build.pack.planNeeds")}</p>}
        {choice && (
          <Button variant={plan ? "neutral" : "brand"} onClick={() => void viewPlan()} disabled={planning}>
            {planning ? t("studio.build.pack.planning") : t("studio.build.pack.planBtn")}
          </Button>
        )}
        {choice && !plan && !planErr && !planning && <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("studio.build.pack.planNoWrite")}</p>}
        {planErr && (
          <div className="slds-m-top_small">
            <ScopedNotification theme="error">{t("studio.build.pack.planFailed")} {planErr}</ScopedNotification>
          </div>
        )}
        {plan && <div className="slds-m-top_medium"><PlanView plan={plan} /></div>}
      </section>

      <section aria-labelledby="pack-step3">
        <h2 id="pack-step3" className="slds-text-heading_small slds-m-bottom_small">{t("studio.build.pack.step3")}</h2>
        {result ? (
          <InstallResultView result={result} onClose={() => router.push(listHref)} />
        ) : (
          <>
            {!plan && <p className="slds-text-color_weak">{t("studio.build.pack.installNeeds")}</p>}
            {plan && !confirming && (
              <Button variant="brand" disabled={installing} onClick={() => (askFirst ? setConfirming(true) : void install())}>
                {installing ? t("studio.build.pack.installing") : t("studio.build.pack.installBtn")}
              </Button>
            )}
            {plan && confirming && choice && (
              <InlineConfirm
                tone="warning"
                message={t("studio.build.pack.confirmTitle")}
                confirmLabel={t("studio.build.pack.confirmGo")}
                busy={installing}
                confirmDisabled={typed.trim() !== choice.key}
                onConfirm={() => void install()}
                onCancel={() => { setConfirming(false); setTyped(""); }}
              >
                <Input label={tf("studio.build.pack.confirmType", { key: choice.key })} value={typed} onChange={(e) => setTyped(e.target.value)} layout="stacked" />
              </InlineConfirm>
            )}
            {installErr && (
              <div className="slds-m-top_small">
                {installErr.code === DELIVERED ? (
                  <ScopedNotification theme="warning">
                    <span className="flex flex-wrap items-center gap-2">
                      <span>{t("studio.build.pack.delivered")}</span>
                      <Link href={studioHref(slug, "bao-tri/nang-cap")}>{t("studio.build.pack.deliveredLink")}</Link>
                    </span>
                  </ScopedNotification>
                ) : (
                  <ScopedNotification theme="error">
                    <strong>{t("studio.build.pack.installFailed")}</strong> {installErr.message}
                    {installErr.code && <span className="slds-text-body_small slds-m-left_x-small">{tf("studio.build.pack.errCode", { code: installErr.code })}</span>}
                  </ScopedNotification>
                )}
              </div>
            )}
          </>
        )}
      </section>
    </div>
  );
}

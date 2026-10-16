"use client";

// PreferencesScreen — /settings (Giao diện / Ngôn ngữ / Giới thiệu).
// All preferences persisted to localStorage "nexus.prefs.v1" — no backend needed.
// Language choice feeds the i18n provider (locale key).
// SLDS (specs/038 T032): page-headers/base + vertical-tabs/base (with icons); the language is a
// radio-button-group, the about facts are read-only form-element/record-detail fields.
// No light/dark switch: SLDS has no dark theme (FR-012). No notifications tab: nothing reads
// notifyInApp/notifyEmail (removed on purpose in c60449f2 — a toggle that changes nothing lies).
import { useRef, useState, type ReactNode } from "react";
import { Globe, Info, Monitor, Settings } from "lucide-react";
import { loadPrefs, savePrefs, type NexusPrefs } from "@/lib/prefs";
import { t } from "@/lib/i18n";
import { Badge } from "@/components/slds/Badge";
import { FormElement } from "@/components/slds/FormElement";
import { PageHeader } from "@/components/slds/PageHeader";
import { RadioButtonGroup } from "@/components/slds/RadioGroup";
import { VerticalTabs } from "@/components/slds/VerticalTabs";

type Tab = "appearance" | "locale" | "about";

export function PreferencesScreen() {
  const [tab, setTab] = useState<Tab>("appearance");
  const [prefs, setPrefs] = useState<NexusPrefs>(() => loadPrefs());
  const [saved, setSaved] = useState(false);
  const savedTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  // Persist on change, then flash "Đã lưu". Done in the change handler, not an effect on
  // `prefs`: the effect version set state inside the effect (react-hooks/set-state-in-effect).
  // ponytail: the flash timer is not cleared on unmount — a late setSaved(false) is a no-op.
  const set: Setter = (key, value) => {
    const next = { ...prefs, [key]: value };
    setPrefs(next);
    savePrefs(next);
    setSaved(true);
    clearTimeout(savedTimer.current);
    savedTimer.current = setTimeout(() => setSaved(false), 1400);
  };

  return (
    <div className="flex min-h-0 flex-1 overflow-y-auto">
      <div className="slds-p-around_medium slds-size_1-of-1">
        <PageHeader
          icon={{ glyph: Settings, name: "settings" }}
          title={t("settings.title")}
          meta={t("page.settings.meta")}
          actions={saved ? [<Badge key="saved" theme="success">{t("page.settings.saved")}</Badge>] : undefined}
        />
        <VerticalTabs
          className="slds-m-top_medium"
          label={t("page.settings.tabsLabel")}
          value={tab}
          onValueChange={(v) => setTab(v as Tab)}
          items={[
            {
              value: "appearance",
              label: t("settings.appearance"),
              icon: { glyph: Monitor, name: "display-text" },
              content: <AppearancePanel />,
            },
            {
              value: "locale",
              label: t("settings.locale"),
              icon: { glyph: Globe, name: "person-language" },
              content: <LocalePanel prefs={prefs} set={set} />,
            },
            {
              value: "about",
              label: t("settings.about"),
              icon: { glyph: Info, name: "account-info" },
              content: <AboutPanel />,
            },
          ]}
        />
      </div>
    </div>
  );
}

type Setter = <K extends keyof NexusPrefs>(key: K, v: NexusPrefs[K]) => void;

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="slds-text-longform">
      <h3 className="slds-text-heading_medium">{title}</h3>
      {children}
    </div>
  );
}

// ── Appearance ─────────────────────────────────────────────────────────────
// Light only: SLDS has no dark theme (specs/038 FR-012), so the old light/dark switch is gone.
function AppearancePanel() {
  return (
    <Section title={t("page.settings.density.title")}>
      <p>{t("page.settings.density.body")}</p>
    </Section>
  );
}

// ── Locale ──────────────────────────────────────────────────────────────────
function LocalePanel({ prefs, set }: { prefs: NexusPrefs; set: Setter }) {
  return (
    <Section title={t("page.settings.locale.title")}>
      <RadioButtonGroup
        label={t("settings.locale")}
        options={[
          { value: "vi", label: t("page.settings.locale.vi") },
          { value: "en", label: t("page.settings.locale.en") },
        ]}
        value={prefs.locale}
        onValueChange={(v) => set("locale", v as NexusPrefs["locale"])}
        help={t("page.settings.locale.note")}
      />
    </Section>
  );
}

// ── About ─────────────────────────────────────────────────────────────────
function AboutPanel() {
  const facts: { label: string; value: ReactNode }[] = [
    { label: t("page.settings.about.version"), value: <span className="slds-text-font_monospace">{t("page.settings.about.versionValue")}</span> },
    { label: t("page.settings.about.engine"), value: t("page.settings.about.engineValue") },
    { label: t("page.settings.about.by"), value: t("page.settings.about.byValue") },
    { label: t("page.settings.about.arch"), value: t("page.settings.about.archValue") },
  ];
  const support: { label: string; value: ReactNode }[] = [
    { label: t("page.settings.about.supportEmail"), value: <a href="mailto:support@example.com">support@example.com</a> },
    { label: t("page.settings.about.docs"), value: <span className="slds-text-font_monospace">nexus-platform/docs/</span> },
  ];
  return (
    <>
      <Section title={t("page.settings.about.product")}>
        <ReadOnlyFields rows={facts} />
      </Section>
      <div className="slds-m-top_large">
        <Section title={t("page.settings.about.support")}>
          <ReadOnlyFields rows={support} />
        </Section>
      </div>
    </>
  );
}

// form-element/record-detail, horizontal read-only rows.
function ReadOnlyFields({ rows }: { rows: { label: string; value: ReactNode }[] }) {
  return (
    <div className="slds-form" role="list">
      {rows.map((r) => (
        <div key={r.label} className="slds-form__row">
          <div className="slds-form__item" role="listitem">
            <FormElement label={r.label} layout="horizontal" readOnly>
              {r.value}
            </FormElement>
          </div>
        </div>
      ))}
    </div>
  );
}

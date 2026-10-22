"use client";

// Landing: the public "/" page (E2-IA-DESIGN-SYSTEM.md §10.3). Static, no API calls, no invented numbers: every
// figure on it is in the README. The copy (lib/i18n, landing.*) states the README's positioning (issue #8): agents
// edit a typed, versioned, permissioned model; people review, publish and roll back.
//
// Language: the server cannot know the reader's language, so it renders Vietnamese, and the first client render
// must produce the same text or React throws #418. The locale is therefore read through useSyncExternalStore with
// a Vietnamese server snapshot: hydration matches the server, then the page re-renders in the reader's language.
import { useSyncExternalStore } from "react";
import { clsx } from "clsx";
import Link from "next/link";
import Image from "next/image";
import {
  Bot, CheckCheck, Users, FileDiff, History, ShieldCheck, Plug, Gauge, Terminal, ArrowRight, ArrowUpRight,
  type LucideIcon,
} from "lucide-react";
import { NexusLogo } from "@/components/brand/NexusLogo";
import { Reveal } from "@/components/marketing/Reveal";
import { MarketingAuthGate } from "@/components/marketing/MarketingAuthGate";
import { BrowserFrame } from "@/components/marketing/BrowserFrame";
import { getLocale, t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import heroApp from "@/public/landing/hero-app.png";

const ICON_STROKE = 1.75; // §10.4: Lucide line-art strokeWidth 1.75 across the landing

const REPO = "https://github.com/doxuta/nexus";
const README = `${REPO}#readme`;
const WRITEUP = `${REPO}/blob/main/docs/writeup/turning-the-lights-back-on.md`;

type Item = { icon: LucideIcon; title: I18nKey; body: I18nKey };

const LOOP: (Item & { n: string })[] = [
  { n: "01", icon: Bot, title: "landing.loop.propose.title", body: "landing.loop.propose.body" },
  { n: "02", icon: CheckCheck, title: "landing.loop.publish.title", body: "landing.loop.publish.body" },
  { n: "03", icon: Users, title: "landing.loop.use.title", body: "landing.loop.use.body" },
];

const DIFFERENCES: Item[] = [
  { icon: FileDiff, title: "landing.diff.review.title", body: "landing.diff.review.body" },
  { icon: History, title: "landing.diff.versions.title", body: "landing.diff.versions.body" },
  { icon: ShieldCheck, title: "landing.diff.safe.title", body: "landing.diff.safe.body" },
  { icon: Plug, title: "landing.diff.mcp.title", body: "landing.diff.mcp.body" },
  { icon: Gauge, title: "landing.diff.measured.title", body: "landing.diff.measured.body" },
  { icon: Terminal, title: "landing.diff.run.title", body: "landing.diff.run.body" },
];

const READ_ON: (Omit<Item, "icon"> & { href: string })[] = [
  { href: README, title: "landing.try.source.title", body: "landing.try.source.body" },
  { href: WRITEUP, title: "landing.try.writeup.title", body: "landing.try.writeup.body" },
];

const noSubscription = () => () => {};
const serverLocale = () => "vi" as const;

export function Landing() {
  const locale = useSyncExternalStore(noSubscription, getLocale, serverLocale);
  const tr = (key: I18nKey) => t(key, locale);

  return (
    <div className="min-h-screen bg-bg">
      <MarketingAuthGate />

      {/* Minimal sticky nav: brand + links, light backdrop blur (token bg-bg/85, no new colour). */}
      <header className="sticky top-0 z-40 border-b border-bd bg-bg/85 backdrop-blur-sm">
        <div className="mx-auto flex h-16 max-w-[1120px] items-center justify-between px-6">
          <NexusLogo size={26} />
          <nav className="flex items-center gap-5">
            <a
              href={REPO}
              className="hidden text-body font-medium text-mut transition-colors hover:text-fg sm:block"
            >
              {tr("landing.nav.github")}
            </a>
            <Link
              href="/login"
              className="flex h-9 items-center rounded-input border border-bd px-3.5 text-body font-medium text-fg transition-colors hover:border-ac hover:text-ac"
            >
              {tr("landing.nav.login")}
            </Link>
          </nav>
        </div>
      </header>

      {/* Hero, not wrapped in Reveal: above-the-fold content shows on the first paint. Headline, one lead,
          CTAs, then a REAL screenshot of the app overlapping the next section (negative margin + z-10). */}
      <section className="mx-auto max-w-[1120px] px-6 pt-16 sm:pt-24">
        <div className="mx-auto max-w-[780px] text-center">
          <h1 className="text-balance font-display text-34px font-semibold leading-[1.15] tracking-tight text-fg sm:text-48px">
            {tr("landing.hero.title")}
          </h1>
          <p className="mx-auto mt-5 max-w-[600px] text-pretty text-title leading-relaxed text-mut sm:text-16px">
            {tr("landing.hero.lead")}
          </p>
          <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
            <Link
              href="/login"
              className="flex h-10 items-center gap-1.5 rounded-input bg-ac px-4 text-13-5px font-medium text-acf transition-colors hover:bg-ac2"
            >
              {tr("landing.hero.demo")}
              <ArrowRight className="size-4" strokeWidth={ICON_STROKE} />
            </Link>
            <a
              href={REPO}
              className="flex h-10 items-center rounded-input border border-bd px-4 text-13-5px font-medium text-fg transition-colors hover:border-ac hover:text-ac"
            >
              {tr("landing.hero.source")}
            </a>
          </div>
        </div>

        <div className="relative z-10 mx-auto -mb-16 mt-12 max-w-[1000px] sm:-mb-24 sm:mt-16">
          <BrowserFrame>
            <Image
              src={heroApp}
              alt={tr("landing.hero.imageAlt")}
              priority
              placeholder="blur"
              sizes="(min-width: 1064px) 1000px, 92vw"
              className="w-full"
            />
          </BrowserFrame>
        </div>
      </section>

      {/* The loop: large top padding leaves room for the hero screenshot. */}
      <section className="border-t border-bd bg-panel">
        <div className="mx-auto max-w-[1120px] px-6 pb-16 pt-32 sm:pb-20 sm:pt-44">
          <Reveal>
            <p className="font-mono text-caption font-medium tracking-wider text-fnt">{tr("landing.loop.eyebrow")}</p>
            <h2 className="mt-2 font-display text-24px font-semibold text-fg sm:text-28px">{tr("landing.loop.title")}</h2>
          </Reveal>

          <div className="mt-10 grid grid-cols-1 gap-8 sm:grid-cols-3 sm:gap-0">
            {LOOP.map((step, i) => {
              const Icon = step.icon;
              return (
                <Reveal
                  key={step.n}
                  delayMs={Math.min(i, 5) * 40}
                  className={clsx(i > 0 && "sm:border-l sm:border-bd sm:pl-8", i < LOOP.length - 1 && "sm:pr-8")}
                >
                  <div className="flex items-center gap-2.5">
                    <Icon className="size-5 text-ac" strokeWidth={ICON_STROKE} />
                    <span className="font-mono text-sub text-fnt">{step.n}</span>
                  </div>
                  <h3 className="mt-3 text-title font-semibold text-fg">{tr(step.title)}</h3>
                  <p className="mt-1.5 text-13-5px leading-relaxed text-mut">{tr(step.body)}</p>
                </Reveal>
              );
            })}
          </div>
        </div>
      </section>

      {/* What is different: the README's differentiators. */}
      <section className="border-t border-bd">
        <div className="mx-auto max-w-[1120px] px-6 py-16 sm:py-20">
          <Reveal>
            <p className="font-mono text-caption font-medium tracking-wider text-fnt">{tr("landing.diff.eyebrow")}</p>
            <h2 className="mt-2 font-display text-24px font-semibold text-fg sm:text-28px">{tr("landing.diff.title")}</h2>
          </Reveal>

          <div className="mt-10 grid grid-cols-1 gap-x-8 gap-y-9 sm:grid-cols-2 lg:grid-cols-3">
            {DIFFERENCES.map((item, i) => {
              const Icon = item.icon;
              return (
                <Reveal key={item.title} delayMs={Math.min(i, 5) * 40} className="border-t border-bd pt-5">
                  <Icon className="size-5 text-ac" strokeWidth={ICON_STROKE} />
                  <h3 className="mt-3 text-14-5px font-semibold text-fg">{tr(item.title)}</h3>
                  <p className="mt-1.5 text-13-5px leading-relaxed text-mut">{tr(item.body)}</p>
                </Reveal>
              );
            })}
          </div>
        </div>
      </section>

      {/* Try it: the demo login, then the README and the write-up. */}
      <section className="border-t border-bd bg-panel">
        <div className="mx-auto max-w-[1120px] px-6 py-16 sm:py-20">
          <Reveal>
            <p className="font-mono text-caption font-medium tracking-wider text-fnt">{tr("landing.try.eyebrow")}</p>
            <h2 className="mt-2 font-display text-24px font-semibold text-fg sm:text-28px">{tr("landing.try.title")}</h2>
          </Reveal>

          <div className="mt-8 space-y-4">
            <Reveal className="flex flex-col justify-between gap-4 rounded-card border border-bd bg-bg p-6 sm:flex-row sm:items-center">
              <div>
                <h3 className="text-title font-semibold text-fg">{tr("landing.try.demo.title")}</h3>
                <p className="mt-1 max-w-[720px] text-13-5px text-mut">{tr("landing.try.demo.body")}</p>
              </div>
              <Link
                href="/login"
                className="flex h-10 shrink-0 items-center gap-1.5 self-start rounded-input bg-ac px-4 text-13-5px font-medium text-acf transition-colors hover:bg-ac2 sm:self-center"
              >
                {tr("landing.nav.login")}
                <ArrowRight className="size-4" strokeWidth={ICON_STROKE} />
              </Link>
            </Reveal>

            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              {READ_ON.map((card, i) => (
                <Reveal key={card.href} delayMs={(i + 1) * 40}>
                  <a
                    href={card.href}
                    className="flex h-full items-center justify-between gap-4 rounded-card border border-bd bg-bg p-6 transition-colors hover:border-ac"
                  >
                    <div>
                      <h3 className="text-14px font-semibold text-fg">{tr(card.title)}</h3>
                      <p className="mt-1 text-body text-mut">{tr(card.body)}</p>
                    </div>
                    <ArrowUpRight className="size-5 shrink-0 text-ac" strokeWidth={ICON_STROKE} />
                  </a>
                </Reveal>
              ))}
            </div>
          </div>
        </div>
      </section>

      <footer className="border-t border-bd">
        <div className="mx-auto flex max-w-[1120px] flex-col items-start justify-between gap-2 px-6 py-8 text-12-5px text-fnt sm:flex-row sm:items-center">
          <p>{tr("landing.footer.note")}</p>
          <div className="flex items-center gap-5">
            <a href={REPO} className="text-fnt transition-colors hover:text-ac">
              {tr("landing.nav.github")}
            </a>
            <Link href="/login" className="text-fnt transition-colors hover:text-ac">
              {tr("landing.nav.login")}
            </Link>
          </div>
        </div>
      </footer>
    </div>
  );
}

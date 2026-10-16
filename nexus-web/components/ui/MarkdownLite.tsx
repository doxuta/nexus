"use client";

// MarkdownLite — XSS-safe markdown-lite renderer (tokenized to React nodes, NEVER
// dangerouslySetInnerHTML). Handles the subset the AI personas emit: **bold**, `code`,
// - bullets, ### headings, --- dividers, GFM tables, paragraphs, and EXACTLY ONE link shape:
// [label](/internal/path) — app-internal, whitelisted by isSafeInternalHref, rendered via
// next/link (SPA). Every other link shape (external http(s), protocol-relative //, javascript:,
// data:, mailto:, …) renders as PLAIN TEXT exactly as before — the anti-exfil property is a
// structural whitelist, never a blacklist, so ⌘K/assistant replies can deep-link modules to
// each other without ever being able to name an external host.
// Extracted verbatim from the old AI Builder page so the global ⌘K assistant can
// reuse it without bundling the whole studio composer.

import { Fragment, type ReactNode } from "react";
import Link from "next/link";

// isSafeInternalHref — the STRUCTURAL whitelist for the one renderable link target: a
// path-absolute, same-origin app route. Rules (all must hold):
//   · non-empty string, length ≤ 200 (a longer "path" is nobody's honest route);
//   · starts with "/" — which also guarantees no scheme/colon can precede the first "/",
//     so javascript:/data:/mailto:/http(s): are structurally impossible;
//   · NOT "//…" — protocol-relative is an external URL;
//   · NOT "/\…" — WHATWG URL parsing folds "\" into "/", so "/\evil.com" would become
//     protocol-relative "//evil.com" (the classic open-redirect bypass).
// Anything failing falls back to plain text — never a link.
export function isSafeInternalHref(href: string): boolean {
  if (typeof href !== "string" || href.length === 0 || href.length > 200) return false;
  if (!href.startsWith("/")) return false;
  if (href.startsWith("//") || href.startsWith("/\\")) return false;
  return true;
}

// One inline markdown link token, already isolated by the tokenizer: "[label](target)".
const LINK_TOKEN = /^\[([^\]]+)\]\(([^)]+)\)$/;

interface Look {
  strong: string; code: string; link: string; p: string; ul: string; h: string; hr: string;
  tableWrap: string; table: string; tr: string; thr: string; th: string; td: string; root: string;
  /** Maps a model-written internal path to the page it should open; null ⇒ plain text. */
  resolveHref?: (href: string) => string | null;
}
const DEFAULT_LOOK: Look = {
  strong: "font-semibold text-fg", code: "rounded bg-bg px-1 py-0.5 font-mono text-11-5px text-ac-hi",
  link: "font-medium text-ac underline decoration-ac/40 underline-offset-2 hover:text-ac-hi",
  p: "my-1 whitespace-pre-wrap text-body leading-relaxed text-fnt first:mt-0 last:mb-0",
  ul: "my-1 list-disc space-y-0.5 pl-4 text-body leading-relaxed text-fnt",
  h: "mb-1 mt-2 text-body font-semibold text-fg first:mt-0", hr: "my-2.5 border-bd",
  tableWrap: "my-1.5 overflow-x-auto rounded-input border border-bd", table: "w-full border-collapse text-left text-sub",
  tr: "border-t border-bd/60", thr: "bg-panel", th: "border-b border-bd px-2.5 py-1.5 font-medium text-fg", td: "px-2.5 py-1.5 align-top text-fnt", root: "text-fnt",
};
// SLDS look (docked assistant): SLDS hooks only, no Nexus colour tokens.
const SLDS_LOOK: Look = {
  strong: "slds-text-title_bold", code: "slds-text-font_monospace", link: "",
  p: "slds-m-vertical_xx-small slds-text-body_regular", ul: "slds-list_dotted slds-m-vertical_xx-small slds-text-body_regular",
  h: "slds-text-title_bold slds-m-top_x-small", hr: "slds-m-vertical_small",
  tableWrap: "slds-scrollable_x", table: "slds-table slds-table_bordered slds-table_col-bordered", tr: "", thr: "slds-line-height_reset",
  th: "slds-text-title_caps", td: "", root: "",
};

function renderInline(text: string, keyBase: string, look: Look = DEFAULT_LOOK): ReactNode[] {
  const parts = text.split(/(\*\*[^*]+\*\*|`[^`]+`|\[[^\]]+\]\([^)]+\))/g);
  return parts.filter((p) => p !== "").map((p, i) => {
    if (p.startsWith("**") && p.endsWith("**")) {
      return <strong key={`${keyBase}-${i}`} className={look.strong}>{p.slice(2, -2)}</strong>;
    }
    if (p.startsWith("`") && p.endsWith("`")) {
      return <code key={`${keyBase}-${i}`} className={look.code}>{p.slice(1, -1)}</code>;
    }
    if (p.startsWith("[")) {
      const m = p.match(LINK_TOKEN);
      const target = m && isSafeInternalHref(m[2]) ? (look.resolveHref ? look.resolveHref(m[2]) : m[2]) : null;
      if (m && target && isSafeInternalHref(target)) {
        return (
          <Link
            key={`${keyBase}-${i}`}
            href={target}
            className={look.link || undefined}
          >
            {m[1]}
          </Link>
        );
      }
      // Unsafe or non-link "[" text: fall through — the original characters render verbatim.
    }
    return <Fragment key={`${keyBase}-${i}`}>{p}</Fragment>;
  });
}

// splitTableRow: "| a | b |" → ["a","b"] (drop the empty edges from leading/trailing pipes).
function splitTableRow(line: string): string[] {
  return line.trim().replace(/^\||\|$/g, "").split("|").map((c) => c.trim());
}
const isTableSep = (line: string) => /^\s*\|?[\s:|-]*-[\s:|-]*\|?\s*$/.test(line) && line.includes("-");
const isTableRow = (line: string) => /\|/.test(line) && line.trim().startsWith("|");

export function MarkdownLite({ text, variant, resolveHref }: { text: string; variant?: "slds"; resolveHref?: (href: string) => string | null }) {
  const look: Look = variant === "slds" ? { ...SLDS_LOOK, resolveHref } : { ...DEFAULT_LOOK, resolveHref };
  const lines = text.replace(/\r\n/g, "\n").split("\n");
  const blocks: ReactNode[] = [];
  let list: string[] = [];
  let para: string[] = [];

  const flushList = () => {
    if (list.length === 0) return;
    const items = list;
    blocks.push(
      <ul key={`ul-${blocks.length}`} className={look.ul}>
        {items.map((li, i) => <li key={i}>{renderInline(li, `ul${blocks.length}-${i}`, look)}</li>)}
      </ul>,
    );
    list = [];
  };
  const flushPara = () => {
    if (para.length === 0) return;
    const joined = para.join("\n");
    blocks.push(
      <p key={`p-${blocks.length}`} className={look.p}>
        {renderInline(joined, `p${blocks.length}`, look)}
      </p>,
    );
    para = [];
  };

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trimEnd();

    // GFM table: a header row followed by a |---|---| separator, then body rows.
    if (isTableRow(line) && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      flushPara(); flushList();
      const header = splitTableRow(line);
      const rows: string[][] = [];
      let j = i + 2;
      while (j < lines.length && isTableRow(lines[j])) { rows.push(splitTableRow(lines[j])); j++; }
      const bi = blocks.length;
      blocks.push(
        <div key={`tbl-${bi}`} className={look.tableWrap}>
          <table className={look.table}>
            <thead>
              <tr className={look.thr}>
                {header.map((h, k) => (
                  <th key={k} className={look.th}>{renderInline(h, `th${bi}-${k}`, look)}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((r, ri) => (
                <tr key={ri} className={look.tr || undefined}>
                  {header.map((_, k) => (
                    <td key={k} className={look.td}>{renderInline(r[k] ?? "", `td${bi}-${ri}-${k}`, look)}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>,
      );
      i = j - 1;
      continue;
    }

    if (/^\s*(-{3,}|\*{3,}|_{3,})\s*$/.test(line)) {
      flushPara(); flushList();
      blocks.push(<hr key={`hr-${blocks.length}`} className={look.hr} />);
      continue;
    }
    const heading = line.match(/^(#{1,6})\s+(.*)$/);
    if (heading) {
      flushPara(); flushList();
      blocks.push(
        <p key={`h-${blocks.length}`} className={look.h}>
          {renderInline(heading[2], `h${blocks.length}`, look)}
        </p>,
      );
      continue;
    }
    const bullet = line.match(/^\s*[-*]\s+(.*)$/);
    if (bullet) {
      flushPara();
      list.push(bullet[1]);
      continue;
    }
    if (line.trim() === "") {
      flushPara(); flushList();
      continue;
    }
    flushList();
    para.push(line);
  }
  flushPara(); flushList();

  return <div className={look.root || undefined}>{blocks}</div>;
}

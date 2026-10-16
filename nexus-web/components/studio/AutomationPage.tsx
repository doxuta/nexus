"use client";

// SubPage: the "← Tự động" back link every sub page of Tự động wears (the rule list itself is build/RuleListPage).
import Link from "next/link";
import type { ReactNode } from "react";

export function SubPage({ back, children }: { back: { label: string; href: string }; children: ReactNode }) {
  return (
    <div>
      <p className="slds-m-vertical_small slds-p-left_medium">
        <Link href={back.href}>{`← ${back.label}`}</Link>
      </p>
      {children}
    </div>
  );
}

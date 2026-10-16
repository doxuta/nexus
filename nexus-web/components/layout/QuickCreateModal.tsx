"use client";

// QuickCreateModal — compact create form in an SLDS modal (contracts/shell.md: quick-create =
// RecordFormPage in Modal form, modals/base). Opened from the list header, the calendar's per-day
// "+", a related list, or ⌘K > "Tạo mới". The dialog is LayoutForm's `modal` mode (the template's
// modal variant): Huỷ / Tạo in slds-modal__footer, Esc / X / focus trap / focus return from the kit
// Modal (Radix Dialog), whose layer stack lets Esc close an open picklist or calendar first.
//
// Usage:
//   <QuickCreateModal entity="accounts" open={open} onClose={() => setOpen(false)} onSaved={(id) => router.push(`/accounts/${id}`)} />

import { LayoutForm } from "@/components/layout/LayoutForm";

export interface QuickCreateModalProps {
  entity: string;
  open: boolean;
  onClose: () => void;
  onSaved?: (id: string) => void;
  // Field values seeded into the fresh form (LayoutForm `prefill`) — e.g. a related-list create
  // pre-links the parent FK so the child is born attached (STUDIO-DISCOVERABILITY 1.4c).
  prefill?: Record<string, unknown>;
}

export function QuickCreateModal({ entity, open, onClose, onSaved, prefill }: QuickCreateModalProps) {
  // Unmounted while closed: each open is a fresh form that reads the current `prefill`.
  if (!open) return null;

  const handleSaved = (id: string) => {
    onSaved?.(id);
    onClose();
  };

  return <LayoutForm entity={entity} modal onSaved={handleSaved} onCancel={onClose} prefill={prefill} />;
}

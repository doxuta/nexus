"use client";

// "Đổi chủ" (PUT /:entity/:id/owner, TRỤ 2 mục 2) — SLDS modal (modals/base) around a people lookup
// (lookups blueprint: "a lookup is a combobox" — the kit's ComboboxField, autocomplete): focus stays
// in the search field, ArrowDown/ArrowUp walk the options by aria-activedescendant, Enter picks, and
// the options are not tab stops, so Tab goes on to the footer (Huỷ / Đổi chủ). Options are entity
// rows (user tile · name · email); typing filters by name OR email, diacritics-insensitive.
// The action NEVER hides on a permission guess — the rule (manager with edit-all, or admin) lives in
// RecordService.SetOwner; we call and show the real 403/422 message inside the dialog.
// People come from `listUserDirectory` (GET /users/directory), NOT `listAdminUsers`: changing owner
// needs `edit@all` (a manager), but /admin/users is admin-only, so a non-admin manager with the right
// could not load anyone to pick (bug-class §0b). The directory already holds active users only.
import { useState } from "react";
import { UserRound } from "lucide-react";
import { ensureAuth, setRecordOwner, ApiError, type DirectoryUser } from "@/lib/api";
import { t } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { ComboboxField, type ComboboxOption } from "@/components/slds/Combobox";
import { Modal } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { useUserPicker } from "./useUserPicker";

function toOption(u: DirectoryUser): ComboboxOption {
  return {
    value: u.id,
    label: u.fullName || u.email,
    meta: u.email,
    icon: { glyph: UserRound, name: "user", label: t("page.record.owner.userKind") },
  };
}

export function OwnerDialog({ entity, recordId, open, onOpenChange, onChanged }: {
  entity: string;
  recordId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onChanged?: () => void;
}) {
  const [filter, setFilter] = useState("");
  const [selectedId, setSelectedId] = useState("");
  const [busy, setBusy] = useState(false);
  const [errMsg, setErrMsg] = useState("");
  const picker = useUserPicker(open, filter, t("page.record.owner.loadError"));

  // Every opening starts clean (adjusting state during render, not in an effect).
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    setWasOpen(open);
    if (open) {
      setErrMsg("");
      setSelectedId("");
      setFilter("");
    }
  }

  const chosen = picker.find(selectedId);

  const submit = async () => {
    if (!selectedId || busy) return;
    setBusy(true);
    setErrMsg("");
    try {
      await ensureAuth();
      await setRecordOwner(entity, recordId, selectedId);
      const name = chosen?.fullName ?? "";
      toast.success(name ? t("page.record.owner.doneNamed").replace("{name}", name) : t("page.record.owner.done"));
      onChanged?.();
      onOpenChange(false);
    } catch (e) {
      // 403 (manager/admin only) · 422 RECORD_OWNER_INVALID · 404 — the server's real message.
      setErrMsg(e instanceof ApiError ? e.message : t("page.record.owner.error"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      open={open}
      onOpenChange={busy ? () => {} : onOpenChange}
      title={t("page.record.owner.title")}
      tagline={t("page.record.owner.tagline")}
      footer={
        <>
          <Button onClick={() => onOpenChange(false)} disabled={busy}>
            {t("page.record.cancel")}
          </Button>
          <Button variant="brand" onClick={() => void submit()} disabled={busy || !selectedId}>
            {t("page.record.owner.submit")}
          </Button>
        </>
      }
    >
      {picker.loadErr ? (
        <ScopedNotification theme="warning">{picker.loadErr}</ScopedNotification>
      ) : !picker.ready ? (
        <div className="relative h-16">
          <Spinner size="small" label={t("page.record.owner.loading")} />
        </div>
      ) : (
        <>
          <ComboboxField
            mode="autocomplete"
            label={t("page.record.owner.search")}
            placeholder={t("page.record.owner.searchPlaceholder")}
            options={picker.visible.map(toOption)}
            selected={chosen ? [toOption(chosen)] : []}
            onSelect={(o) => setSelectedId(o.value)}
            onDeselect={() => setSelectedId("")}
            inputValue={filter}
            onInputChange={setFilter}
            noResultsText={t("page.record.owner.noMatch")}
          />
          {picker.visible.length === 0 && !filter.trim() && <p className="slds-text-color_weak slds-m-top_small">{t("page.record.owner.noMatch")}</p>}
        </>
      )}
      {errMsg && <ScopedNotification theme="error" className="slds-m-top_small">{errMsg}</ScopedNotification>}
    </Modal>
  );
}

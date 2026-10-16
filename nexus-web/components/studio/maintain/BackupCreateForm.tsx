"use client";

// BackupCreateForm — "Sao lưu ngay" in place (specs/079 f7-sao-luu-bang): an optional note (≤ 200 characters, the
// server's own limit BACKUP_NOTE_INVALID is mirrored so a long one is stopped before the call), no modal.
import { useState } from "react";
import { Button } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { t } from "@/lib/i18n";

export const NOTE_MAX = 200;

export function BackupCreateForm({ busy, onSubmit, onCancel }: { busy: boolean; onSubmit: (note: string) => void; onCancel: () => void }) {
  const [note, setNote] = useState("");
  const tooLong = note.trim().length > NOTE_MAX;
  return (
    <form
      className="flex flex-wrap items-end gap-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (!tooLong && !busy) onSubmit(note.trim());
      }}
    >
      <div className="grow">
        <Input
          label={t("backup.noteLabel")}
          placeholder={t("backup.notePh")}
          value={note}
          autoFocus
          error={tooLong ? t("studio.maintain.backup.noteTooLong") : undefined}
          onChange={(e) => setNote(e.target.value)}
        />
      </div>
      <Button type="submit" variant="brand" disabled={busy || tooLong}>
        {busy ? t("backup.creating") : t("backup.create")}
      </Button>
      <Button variant="neutral" disabled={busy} onClick={onCancel}>
        {t("studio.maintain.backup.formClose")}
      </Button>
    </form>
  );
}

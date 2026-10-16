"use client";

// "Tệp" tab — one card per file/image field of the record. SLDS: file cards from the files
// blueprint (files/base "File with no image" + "File with actions": slds-file_card, doctype glyph
// in the crop, download/delete in slds-file__actions-menu) and the upload control from
// file-selector/base (files variant: neutral "upload" button + drop zone).
// Deviation: the blueprint's crop is a "Preview:" link and SLDS draws an eye on its hover. Nexus has
// no preview (opening a user-uploaded file as a same-origin blob would run its HTML in the app), so
// the crop is a plain glyph with pointer events off — no eye promising a preview that is really a
// download. Download/delete are the actions-menu buttons, and each names its file ("Xoá tệp: a.pdf")
// so a screen-reader user tabbing through several files knows which one a click deletes.
// Files are stored as [{id, name}] arrays in the record's file/image field values — there is NO
// list endpoint, the list is derived from the record data.
//   Upload:   POST /:entity/:id/files multipart with `field=<fieldname>` (REQUIRED).
//   Download: GET /files/:id via fetch+auth → blob → save (auth header required; no ?token=).
//   Delete:   DELETE /files/:id → 204.
// readOnly (specs/051 FR-009 — the record is seen only through a manual share): no upload zone and no
// delete button, both are writes the server refuses; download stays.
import { useId, useState, type DragEvent } from "react";
import clsx from "clsx";
import { Download, FileText, Paperclip, Upload, X } from "lucide-react";
import { ensureAuth, uploadFile, downloadFile, deleteFile, ApiError, type ApiFile, type ApiRecord, type FieldMeta } from "@/lib/api";
import { t } from "@/lib/i18n";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { formatBytes } from "@/lib/format";

export function FilesTab({
  entity,
  recordId,
  record,
  allFields,
  readOnly = false,
}: {
  entity: string;
  recordId: string;
  record: ApiRecord;
  allFields: FieldMeta[];
  readOnly?: boolean;
}) {
  const fileFields = allFields.filter((f) => f.fieldType === "file" || f.fieldType === "image");

  // Files per field (derived from record.data, mutated on upload/delete).
  const [fieldFiles, setFieldFiles] = useState<Record<string, ApiFile[]>>(() => {
    const init: Record<string, ApiFile[]> = {};
    for (const f of fileFields) {
      const raw = record.data?.[f.name];
      // May be an array of {id,name}, a single object, or null.
      const arr = Array.isArray(raw) ? raw : raw != null ? [raw] : [];
      init[f.name] = arr.map((x) => ({ id: String(x.id ?? ""), name: String(x.name ?? "?") }));
    }
    return init;
  });
  const [uploading, setUploading] = useState<string | null>(null); // field being uploaded to
  const [dragOver, setDragOver] = useState<string | null>(null);
  const [errMsg, setErrMsg] = useState("");
  const [downloading, setDownloading] = useState<string | null>(null); // file id
  const idBase = useId();

  const handleUpload = async (fieldName: string, file: File) => {
    setUploading(fieldName);
    setErrMsg("");
    try {
      await ensureAuth();
      const uploaded = await uploadFile(entity, recordId, file, fieldName);
      setFieldFiles((prev) => ({ ...prev, [fieldName]: [...(prev[fieldName] ?? []), uploaded] }));
    } catch (e) {
      setErrMsg(e instanceof ApiError ? e.message : t("page.record.files.uploadError"));
    } finally {
      setUploading(null);
    }
  };

  const handleDelete = async (fieldName: string, fileId: string) => {
    setErrMsg("");
    try {
      await ensureAuth();
      await deleteFile(fileId);
      setFieldFiles((prev) => ({ ...prev, [fieldName]: (prev[fieldName] ?? []).filter((f) => f.id !== fileId) }));
    } catch (e) {
      setErrMsg(e instanceof ApiError ? e.message : t("page.record.files.deleteError"));
    }
  };

  const handleDownload = async (file: ApiFile) => {
    setDownloading(file.id);
    try {
      await downloadFile(file.id, file.name);
    } catch (e) {
      setErrMsg(e instanceof ApiError ? e.message : t("page.record.files.downloadError"));
    } finally {
      setDownloading(null);
    }
  };

  const onDrop = (fieldName: string) => (e: DragEvent) => {
    e.preventDefault();
    setDragOver(null);
    const file = e.dataTransfer.files?.[0];
    if (file && uploading !== fieldName) void handleUpload(fieldName, file);
  };

  if (fileFields.length === 0) {
    return (
      <EmptyState icon={FileText} title={t("page.record.files.unsupportedTitle")} description={t("page.record.files.unsupportedText")} />
    );
  }

  return (
    <>
      {errMsg && <ScopedNotification theme="warning" className="slds-m-bottom_small">{errMsg}</ScopedNotification>}

      {fileFields.map((f, fi) => {
        const files = fieldFiles[f.name] ?? [];
        const isUploading = uploading === f.name;
        const inputId = `${idBase}-${fi}`;
        return (
          <Card
            key={f.name}
            boundary
            className="slds-m-bottom_small"
            title={f.label ?? f.name}
            icon={{ glyph: Paperclip, name: "file" }}
            count={files.length}
          >
            {files.length > 0 && (
              <ul className="slds-grid slds-wrap slds-gutters_x-small">
                {files.map((file) => {
                  const caption = file.size != null ? `${file.name} · ${formatBytes(file.size)}` : file.name;
                  return (
                    <li key={file.id} className="slds-col slds-size_1-of-2 slds-medium-size_1-of-4 slds-m-bottom_small">
                      <div className="slds-file slds-file_card slds-has-title">
                        <figure>
                          <div className="slds-file__crop pointer-events-none" aria-hidden="true">
                            <span className="slds-file__icon slds-icon_container">
                              <FileText className="slds-icon slds-icon-text-default nx-lucide" />
                            </span>
                          </div>
                          <figcaption className="slds-file__title slds-file__title_card slds-file-has-actions">
                            <div className="slds-media slds-media_small slds-media_center">
                              <div className="slds-media__figure slds-line-height_reset">
                                <FileText aria-hidden="true" className="slds-icon slds-icon_x-small slds-icon-text-default nx-lucide" />
                              </div>
                              <div className="slds-media__body">
                                <span className="slds-file__text slds-truncate" title={caption}>{file.name}</span>
                              </div>
                            </div>
                          </figcaption>
                        </figure>
                        <div className="slds-file__actions-menu">
                          <div className="slds-button-group" role="group">
                            <ButtonIcon
                              icon={Download}
                              label={`${t("page.record.files.download")}: ${file.name}`}
                              size="x-small"
                              disabled={downloading === file.id}
                              onClick={() => void handleDownload(file)}
                            />
                            {!readOnly && (
                              <ButtonIcon
                                icon={X}
                                label={`${t("page.record.files.delete")}: ${file.name}`}
                                size="x-small"
                                onClick={() => void handleDelete(f.name, file.id)}
                              />
                            )}
                          </div>
                        </div>
                      </div>
                    </li>
                  );
                })}
              </ul>
            )}

            {/* file-selector/base, files variant. */}
            {!readOnly && <div className="slds-form-element" role="group" aria-label={`${t("page.record.files.upload")}: ${f.label ?? f.name}`}>
              <div className="slds-form-element__control">
                <div className="slds-file-selector slds-file-selector_files">
                  <div
                    className={clsx("slds-file-selector__dropzone", dragOver === f.name && "slds-has-drag-over")}
                    onDragOver={(e) => { e.preventDefault(); setDragOver(f.name); }}
                    onDragLeave={() => setDragOver(null)}
                    onDrop={onDrop(f.name)}
                  >
                    <input
                      id={inputId}
                      type="file"
                      className="slds-file-selector__input slds-assistive-text"
                      disabled={isUploading}
                      onChange={(e) => { const fl = e.target.files?.[0]; if (fl) void handleUpload(f.name, fl); e.target.value = ""; }}
                    />
                    <label className="slds-file-selector__body" htmlFor={inputId}>
                      <span className="slds-file-selector__button slds-button slds-button_neutral">
                        <Upload aria-hidden="true" className="slds-button__icon slds-button__icon_left nx-lucide" />
                        {isUploading ? t("page.record.files.uploading") : t("page.record.files.upload")}
                      </span>
                      <span className="slds-file-selector__text slds-medium-show">{t("page.record.files.drop")}</span>
                    </label>
                  </div>
                </div>
              </div>
            </div>}
          </Card>
        );
      })}
    </>
  );
}

"use client";

// SLDS file selector (blueprint `file-selector` base / image / integrated, SLDS 2.264.1):
//   div.slds-form-element[role=group] > span.slds-form-element__label
//     + div.slds-form-element__control > div.slds-file-selector.slds-file-selector_<files|images>
//       > div.slds-file-selector__dropzone[.slds-has-drag-over]
//         > input.slds-file-selector__input.slds-assistive-text[type=file]
//         + label.slds-file-selector__body > span.slds-file-selector__button.slds-button.slds-button_neutral
//                                           + span.slds-file-selector__text.slds-medium-show
//   Integrated: div.slds-file-selector_integrated wraps the page content; its dropzone is an overlay
//   (aria-hidden, input tabindex=-1) that shows while files are dragged over (`slds-has-drag`).
// Behaviour is the native file input: the label opens the OS picker (Enter/Space on the focused
// input too), drag & drop is the HTML drag events. Files are handed to the owner unfiltered — the
// owner checks type/size/count and shows `error` (the browser only applies `accept` in the picker).
// A disabled selector ignores drops and never lights up (blueprint "Dragover with error": the input
// is disabled and the zone keeps its rest look).
import clsx from "clsx";
import { Upload } from "lucide-react";
import { useRef, useState, type DragEvent, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import { describedBy, useFieldIds } from "./FormElement";

export interface FileSelectorProps {
  /** Group label (files/images); the integrated variant has none. */
  label?: string;
  /** "files" (default) = one-line zone; "images" = square image zone; "integrated" = drop over `children`. */
  variant?: "files" | "images" | "integrated";
  accept?: string;
  multiple?: boolean;
  disabled?: boolean;
  /** Error text (unsupported type, too many files…) — the owner validates what `onFiles` received. */
  error?: string;
  /** Overrides the button / drop text (default "Tải tệp lên" / "hoặc thả tệp vào đây"). */
  buttonLabel?: string;
  dropLabel?: string;
  onFiles: (files: File[]) => void;
  id?: string;
  className?: string;
  /** Integrated variant: the content the drop overlay covers. */
  children?: ReactNode;
}

const hasFiles = (e: DragEvent) => Array.from(e.dataTransfer?.types ?? []).includes("Files");

export function FileSelector({
  label,
  variant = "files",
  accept,
  multiple,
  disabled,
  error,
  buttonLabel,
  dropLabel,
  onFiles,
  id,
  className,
  children,
}: FileSelectorProps) {
  const { controlId, labelId, helpId } = useFieldIds(id);
  // dragenter/dragleave fire for every child crossed; count them so hovering the label text does
  // not flicker the drag-over state off.
  const depth = useRef(0);
  const [over, setOver] = useState(false);
  const integrated = variant === "integrated";
  const images = variant === "images";

  const reset = () => {
    depth.current = 0;
    setOver(false);
  };
  const dragProps = {
    onDragEnter: (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      depth.current += 1;
      setOver(true);
    },
    onDragOver: (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault(); // allow the drop
      e.dataTransfer.dropEffect = disabled ? "none" : "copy";
    },
    onDragLeave: () => {
      depth.current = Math.max(0, depth.current - 1);
      if (depth.current === 0) setOver(false);
    },
    onDrop: (e: DragEvent) => {
      e.preventDefault();
      reset();
      if (disabled) return;
      const files = Array.from(e.dataTransfer.files ?? []);
      if (files.length) onFiles(files);
    },
  };

  const input = (
    <input
      className="slds-file-selector__input slds-assistive-text"
      accept={accept}
      type="file"
      id={controlId}
      multiple={multiple}
      disabled={disabled}
      tabIndex={integrated ? -1 : undefined}
      aria-describedby={describedBy(!integrated && !!error && helpId)}
      onChange={(e) => {
        const files = Array.from(e.target.files ?? []);
        e.target.value = ""; // picking the same file again must fire change again
        if (files.length) onFiles(files);
      }}
    />
  );

  if (integrated) {
    return (
      <div className={clsx("slds-file-selector slds-file-selector_integrated", className)} {...dragProps}>
        <div
          className={clsx(
            "slds-file-selector__dropzone slds-file-selector__dropzone_integrated",
            over && "slds-has-drag",
            over && !disabled && "slds-has-drag-over",
          )}
          aria-hidden="true"
        >
          {input}
          <label className="slds-file-selector__body slds-file-selector__body_integrated" htmlFor={controlId}>
            <Upload aria-hidden="true" className="slds-file-selector__body-icon slds-icon slds-icon-text-default nx-lucide" />
            <span className="slds-file-selector__text slds-file-selector__text_integrated slds-text-heading_medium slds-text-align_center">
              {error ?? dropLabel ?? t("slds.fileSelector.dropIntegrated")}
            </span>
          </label>
        </div>
        {children}
      </div>
    );
  }

  return (
    <div className={clsx("slds-form-element", error && "slds-has-error", className)} role="group" aria-labelledby={label ? labelId : undefined}>
      {label && (
        <span className="slds-form-element__label" id={labelId}>
          {label}
        </span>
      )}
      <div className="slds-form-element__control">
        <div className={clsx("slds-file-selector", images ? "slds-file-selector_images" : "slds-file-selector_files")}>
          <div className={clsx("slds-file-selector__dropzone", over && !disabled && "slds-has-drag-over")} {...dragProps}>
            {input}
            <label className="slds-file-selector__body" htmlFor={controlId}>
              <span className="slds-file-selector__button slds-button slds-button_neutral">
                <Upload aria-hidden="true" className="slds-button__icon slds-button__icon_left nx-lucide" />
                {buttonLabel ?? t(images ? "slds.fileSelector.uploadImage" : "slds.fileSelector.upload")}
              </span>
              <span className="slds-file-selector__text slds-medium-show">
                {dropLabel ?? t(images ? "slds.fileSelector.dropImage" : "slds.fileSelector.drop")}
              </span>
            </label>
          </div>
        </div>
      </div>
      {error && (
        <div className="slds-form-element__help" id={helpId}>
          {error}
        </div>
      )}
    </div>
  );
}

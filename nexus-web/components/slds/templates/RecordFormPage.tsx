"use client";

// Record form template (specs/038 contracts/shell.md "Template trang", RESEARCH §5.5 #3).
// Presentation only: no fetching, no metadata — the L2 form renderer (LayoutForm) supplies the
// section bodies (field widgets, 422 errors at the field), the submit handler (react-hook-form's
// handleSubmit) and the saving state during the Phase 2 migration. SLDS 2.264.1 markup:
//   page:  PageHeader base (page-headers/base) + div.slds-card > form > scoped notification (live region)
//          + div.slds-section × n (expandable-section/base) … div.slds-docked-form-footer
//   modal: Modal (modals/base, medium: the sections keep their two columns) > form in
//          slds-modal__content, Huỷ/Lưu in slds-modal__footer — quick-create (contracts/shell.md)
// `status` stands in for the form while the caller has nothing to edit yet (loading, load error, no
// fields): the modal is the SAME dialog from the spinner to the form (one focus trap, one focus
// return to the opener), and the footer offers Huỷ only.
// The footer buttons live outside the <form> in both variants (the docked footer is
// position:fixed, the modal footer is a sibling of the content), so Save is tied to the form with
// the native `form` attribute: Enter in a field and the Save button take the same submit path.
// `noValidate`: validation belongs to the caller (react-hook-form), not the browser's bubbles.
// The docked footer is fixed to the viewport bottom and is 3rem tall, so the page root reserves
// 3rem of bottom padding (a margin on the last card would collapse out of the root and the footer
// would cover the last section).
import clsx from "clsx";
import { ChevronDown, type LucideIcon } from "lucide-react";
import { useId, useState, type FormEvent, type ReactElement, type ReactNode, type Ref } from "react";
import { t } from "@/lib/i18n";
import { Modal } from "../Modal";
import { PageHeader } from "../PageHeader";
import { GLYPH, VARIANT_LABEL } from "../Toast";

export interface RecordFormSection {
  id: string;
  title: string;
  body: ReactNode;
  /** Start collapsed. */
  collapsed?: boolean;
  /** A field inside has an error: the section is held open so the error stays visible. */
  hasError?: boolean;
}

export type RecordFormNotificationTone = "info" | "success" | "warning" | "error";

interface RecordFormBaseProps {
  /** Page title / modal heading, e.g. "Khách hàng mới". */
  title: string;
  /** Page variant: object icon beside the title; `objectLabel` is its assistive text. */
  icon?: { glyph: LucideIcon; name: string };
  objectLabel?: string;
  /** Line under the title (page) or modal tagline. */
  meta?: ReactNode;
  sections: RecordFormSection[];
  /** Shown instead of the form (and Lưu) while there is nothing to edit yet: spinner, load error. */
  status?: ReactNode;
  /** The <form>, e.g. to focus the first invalid control after a failed submit. */
  formRef?: Ref<HTMLFormElement>;
  /** Form-level message above the sections: 409 conflict (warning), non-field 422 / network (error). */
  notification?: { tone: RecordFormNotificationTone; body: ReactNode };
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  /** Huỷ; in the modal variant also Esc and the X (after `onOpenChange(false)`). */
  onCancel: () => void;
  saving?: boolean;
  /** Save button label; default "Lưu". */
  submitLabel?: string;
  /** Extra footer buttons between Huỷ and Lưu, e.g. "Lưu & tạo mới". */
  extraActions?: ReactNode;
  className?: string;
  /** Page variant only. false = the button row sits inline under the card (no fixed footer): the Studio screen editor
   *  draws the form inside a preview frame. Default true keeps the docked footer byte for byte. */
  docked?: boolean;
  /** With docked={false}: the button row is a picture only (not focusable, not clickable). */
  inertFooter?: boolean;
}

// The modal is controlled only. Left to Radix, its own state would close the dialog on Esc while
// saving (unmounting the form and the typed data) and the caller could not close it after a
// successful save.
export type RecordFormPageProps = RecordFormBaseProps &
  (
    | { variant?: "page"; open?: never; onOpenChange?: never; trigger?: never }
    | { variant: "modal"; open: boolean; onOpenChange: (open: boolean) => void; trigger?: ReactElement }
  );

// TODO(T014): swap for components/slds/ExpandableSection once merged.
function FormSection({ title, body, collapsed, hasError }: RecordFormSection) {
  const [open, setOpen] = useState(!collapsed);
  // An error latches the section open (render-phase state update): when the user fixes the field
  // and the error clears, a collapse would hide the input that still has focus. It runs on every
  // render, so the section also cannot be collapsed while the error lasts.
  if (hasError && !open) setOpen(true);
  const expanded = open || Boolean(hasError);
  const id = useId();
  return (
    <div className={clsx("slds-section", expanded && "slds-is-open")}>
      <h3 className="slds-section__title">
        <button
          type="button"
          aria-controls={id}
          aria-expanded={expanded}
          className="slds-button slds-section__title-action"
          onClick={() => setOpen(!open)}
        >
          <ChevronDown aria-hidden="true" className="slds-section__title-action-icon slds-button__icon slds-button__icon_left nx-lucide" />
          <span className="slds-truncate" title={title}>
            {title}
          </span>
        </button>
      </h3>
      <div className="slds-section__content" id={id} hidden={!expanded}>
        {body}
      </div>
    </div>
  );
}

// TODO(T014): swap for components/slds/ScopedNotification once merged.
// Blueprint scoped-notifications/base: "info" is the default light theme (text-default icon);
// the other tones are the themed states, whose icon takes the theme's foreground.
// The role="status" element is always mounted, bare and empty while there is no message, and React
// keeps the same node when the message arrives: a live region inserted together with its text is
// often not announced (VoiceOver, NVDA), and after a failed save focus is still on Lưu, so this is
// the only thing that tells a screen-reader user why nothing happened.
function Notification({ notification }: { notification?: RecordFormBaseProps["notification"] }) {
  if (!notification) return <div role="status" />;
  const { tone, body } = notification;
  const Glyph = GLYPH[tone];
  const label = t(VARIANT_LABEL[tone]);
  return (
    <div
      className={clsx(
        "slds-scoped-notification slds-media slds-media_center slds-m-bottom_small",
        tone === "info" ? "slds-scoped-notification_light" : `slds-theme_${tone}`,
      )}
      role="status"
    >
      <div className="slds-media__figure">
        <span className={`slds-icon_container slds-icon-utility-${tone}`} title={label}>
          <Glyph aria-hidden="true" className={clsx("slds-icon slds-icon_small nx-lucide", tone === "info" && "slds-icon-text-default")} />
          <span className="slds-assistive-text">{label}</span>
        </span>
      </div>
      <div className="slds-media__body">
        <p>{body}</p>
      </div>
    </div>
  );
}

export function RecordFormPage(props: RecordFormPageProps) {
  const { title, icon, objectLabel, meta, sections, status, formRef, notification, onSubmit, onCancel, saving = false, submitLabel, extraActions, className, docked = true, inertFooter = false } =
    props;
  const formId = useId();
  // Modal: Huỷ, Esc and the X all close here. Huỷ is disabled while saving; Esc and the X stay live,
  // so they wait too: closing mid-save would unmount the form and lose the typed data if the save
  // comes back with a 422/409.
  const changeOpen = (next: boolean) => {
    if (props.variant !== "modal" || (!next && saving)) return;
    props.onOpenChange(next);
    if (!next) onCancel();
  };
  const form = status ?? (
    <form ref={formRef} id={formId} noValidate aria-busy={saving} onSubmit={onSubmit}>
      <Notification notification={notification} />
      {sections.map((s) => (
        <FormSection key={s.id} {...s} />
      ))}
    </form>
  );
  const buttons = (
    <>
      <button
        type="button"
        className="slds-button slds-button_neutral"
        disabled={saving}
        onClick={props.variant === "modal" ? () => changeOpen(false) : onCancel}
      >
        {t("action.cancel")}
      </button>
      {status == null && (
        <>
          {extraActions}
          <button type="submit" form={formId} className="slds-button slds-button_brand" disabled={saving}>
            {saving ? t("action.saving") : (submitLabel ?? t("action.save"))}
          </button>
        </>
      )}
    </>
  );

  if (props.variant === "modal") {
    return (
      <Modal
        open={props.open}
        onOpenChange={changeOpen}
        trigger={props.trigger}
        size="medium"
        title={title}
        tagline={meta}
        footer={buttons}
        className={className}
      >
        {form}
      </Modal>
    );
  }

  return (
    <div className={clsx(docked && "slds-p-bottom_xx-large", className)}>
      <PageHeader icon={icon && { ...icon, label: objectLabel }} title={title} meta={meta} />
      <div className="slds-card slds-m-top_medium">
        <div className="slds-card__body slds-card__body_inner">{form}</div>
      </div>
      {docked ? (
        <div className="slds-docked-form-footer">{buttons}</div>
      ) : (
        <div className="slds-grid slds-grid_align-end slds-m-top_medium" inert={inertFooter || undefined}>{buttons}</div>
      )}
    </div>
  );
}

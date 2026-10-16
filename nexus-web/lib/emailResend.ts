// The server's refusal codes of "Gửi lại" (POST /admin/email-log/:id/resend), in the words the admin reads.
// Shared by the customer-admin screen (Modal) and the Studio (InlineConfirm).
import { ApiError } from "@/lib/admin";
import { t } from "@/lib/i18n";

export function resendRefusal(e: unknown): { text: string; reload: boolean; closes: boolean } {
  const code = e instanceof ApiError ? e.code : "";
  switch (code) {
    case "EMAIL_NOT_RESENDABLE":
    case "EMAIL_LOG_NOT_FOUND":
      return { text: t("page.emailLog.resend.alreadyDone"), reload: true, closes: true };
    case "EMAIL_RESEND_RULE_GONE":
      return { text: t("page.emailLog.resend.ruleGone"), reload: true, closes: false };
    case "EMAIL_RESEND_RULE_DISABLED":
      return { text: t("page.emailLog.resend.ruleDisabled"), reload: false, closes: false };
    case "EMAIL_RESEND_RECORD_GONE":
      return { text: t("page.emailLog.resend.recordGone"), reload: true, closes: false };
    case "EMAIL_RESEND_NO_RECIPIENT":
      return { text: t("page.emailLog.resend.noRecipient"), reload: false, closes: false };
    case "MAIL_NOT_CONFIGURED":
      return { text: t("page.emailLog.resend.mailOff"), reload: false, closes: false };
    case "EMAIL_RESEND_FAILED":
      return { text: t("page.emailLog.resend.sendFailed"), reload: true, closes: true };
    default:
      return { text: e instanceof ApiError ? e.message : t("page.emailLog.resend.error"), reload: false, closes: false };
  }
}

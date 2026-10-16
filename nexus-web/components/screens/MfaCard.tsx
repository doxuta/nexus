"use client";

// MfaCard — thẻ "Xác thực hai lớp (MFA)" trong /profile.
// Backend: POST /auth/mfa/{enroll,activate,disable} (api-conventions §3b, MFA-STEPUP).
// State machine thuần nằm ở lib/mfaPanel.ts (unit-tested); component này chỉ wire API + render.
//
// SLDS (specs/038 T032): the card (blueprint `cards`) shows the resting state — not enrolled /
// enabled / unavailable. Every step-up flow (enroll → activate → backup codes, and disable) runs
// in a `modals/base` dialog: form in `slds-modal__content`, actions in `slds-modal__footer`
// (footer submit buttons point at the form through the `form` attribute).
//
// Trung thực có chủ đích:
//  • FE-CATCHUP-1: GET /auth/me LUÔN trả `mfaEnabled: true|false` → thẻ khởi tạo trạng thái thật
//    từ prop `mfaEnabled`. Phản hồi hành động (409 = đã bật, 422 = chưa đăng ký) vẫn giữ làm
//    fallback (BE cũ / flag stale giữa chừng).
//  • Sau activate/disable thành công → gọi `onMfaChanged` (Profile truyền refetchMe) để làm mới
//    /auth/me. Không có prop → tự gọi getMe() (best-effort; getMe ghi lại cache).
//  • Chưa render mã QR (không thêm dependency nặng) → hiển thị secret + otpauth URL copy được.
//  • 503 MFA_UNAVAILABLE → thông báo "server thiếu khóa MFA", không giả vờ hoạt động.
//  • Đóng hộp thoại ở bước mã dự phòng = MFA ĐÃ bật phía server ⇒ về trạng thái "đang bật",
//    không bao giờ rơi về "chưa bật" (CANCEL ở phase đó sẽ là một trạng thái giả).
//  • Focus on close: back to the button that opened the dialog; when the flow replaced it
//    (enabled, disabled, 503) → the card's new action button, else the card body itself.
import { useEffect, useId, useReducer, useRef, useState, type FormEvent } from "react";
import { Check, Copy, ShieldCheck, ShieldOff } from "lucide-react";
import { mfaEnroll, mfaActivate, mfaDisable, getMe } from "@/lib/api";
import { mfaReduce, initialMfaState, groupSecret, mfaFailureOutcome, type MfaPhase } from "@/lib/mfaPanel";
import { toast } from "@/components/ui/Toaster";
import { t } from "@/lib/i18n";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { Input } from "@/components/slds/Input";
import { Modal } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

type ModalPhase = Extract<MfaPhase, { phase: "enroll-password" | "activate" | "backup-codes" | "disable" }>;

function isModalPhase(step: MfaPhase): step is ModalPhase {
  return step.phase === "enroll-password" || step.phase === "activate" || step.phase === "backup-codes" || step.phase === "disable";
}

export function MfaCard({
  mfaEnabled, onMfaChanged,
}: {
  /** Cờ MFA thật từ /auth/me (AuthProvider). undefined = chưa được báo (BE cũ / đang tải). */
  mfaEnabled?: boolean;
  /** Gọi sau khi bật/tắt MFA thành công (server đã xác nhận) để caller refresh /auth/me. */
  onMfaChanged?: () => void | Promise<void>;
} = {}) {
  const [state, dispatch] = useReducer(mfaReduce, initialMfaState);
  const formId = useId();
  const bodyRef = useRef<HTMLDivElement>(null);

  // Đồng bộ trạng thái thật từ /auth/me: reducer khởi động ở idle/unknown (SSR-safe), rồi
  // SYNC_STATUS đưa panel về đúng phase khi flag tới (cached → live). Reducer tự chặn sync
  // giữa chừng flow — chỉ áp ở phase nghỉ (idle/enabled).
  useEffect(() => {
    if (typeof mfaEnabled === "boolean") dispatch({ type: "SYNC_STATUS", enabled: mfaEnabled });
  }, [mfaEnabled]);

  // Làm mới /auth/me sau một thay đổi MFA server-đã-xác-nhận, để cả AuthProvider.user lẫn cache
  // localStorage (nexus_user) mang flag mới. Best-effort: lỗi mạng ở đây không đụng tới UI panel.
  function refreshMfaFlag() {
    try {
      const p = onMfaChanged ? onMfaChanged() : getMe();
      void Promise.resolve(p).catch(() => { /* best-effort cache refresh */ });
    } catch { /* best-effort cache refresh */ }
  }
  // Mật khẩu step-up: nhập ở bước enroll, GIỮ trong bộ nhớ cho bước activate (BE yêu cầu
  // password ở cả 2 call), xoá ngay khi flow kết thúc/huỷ. Không bao giờ ghi ra storage.
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  // Disable flow có bộ input riêng (mật khẩu + mã TOTP/backup).
  const [disablePwd, setDisablePwd] = useState("");
  const [disableCode, setDisableCode] = useState("");

  function resetInputs() {
    setPassword(""); setCode(""); setDisablePwd(""); setDisableCode("");
  }

  // LOW-1: một fail-jump (mfaFailureOutcome trả jumpTo — 401 activate→enroll-password re-key,
  // 409 đã-bật, 422 chưa-đăng-ký, 503 unavailable) nghĩa là phase sắp đổi RA KHỎI form đang
  // giữ mật khẩu/mã này — xoá ngay để giá trị sai/đã-cũ không bị hiện lại cho lần submit kế
  // tiếp. Lỗi KHÔNG nhảy phase thì GIỮ nguyên giá trị để người dùng chỉ cần sửa tại chỗ.
  function failStepUp(err: unknown, currentPhase: MfaPhase) {
    if (mfaFailureOutcome(err, currentPhase).jumpTo) {
      setPassword("");
      setCode("");
    }
    dispatch({ type: "FAIL", error: err });
  }

  function failDisable(err: unknown, currentPhase: MfaPhase) {
    if (mfaFailureOutcome(err, currentPhase).jumpTo) {
      setDisablePwd("");
      setDisableCode("");
    }
    dispatch({ type: "FAIL", error: err });
  }

  async function submitEnroll(e: FormEvent) {
    e.preventDefault();
    if (state.busy || !password) return; // nút submit đã disabled khi thiếu mật khẩu
    dispatch({ type: "SUBMIT" });
    try {
      const { secret, otpauthUrl } = await mfaEnroll(password);
      dispatch({ type: "ENROLL_OK", secret, otpauthUrl });
    } catch (err) {
      failStepUp(err, state.step);
    }
  }

  async function submitActivate(e: FormEvent) {
    e.preventDefault();
    if (state.busy) return;
    dispatch({ type: "SUBMIT" });
    try {
      const codes = await mfaActivate(password, code.trim());
      setCode("");
      setPassword("");
      dispatch({ type: "ACTIVATE_OK", codes });
      // MFA vừa BẬT (server xác nhận) → refresh /auth/me để mfaEnabled trong cache không stale.
      refreshMfaFlag();
    } catch (err) {
      // 401 INVALID_CREDENTIALS ở đây nhảy về enroll-password (jumpTo) → failStepUp xoá mật
      // khẩu sai, không hiện lại cho lần submit kế (LOW-1).
      failStepUp(err, state.step);
    }
  }

  async function submitDisable(e: FormEvent) {
    e.preventDefault();
    if (state.busy) return;
    dispatch({ type: "SUBMIT" });
    try {
      await mfaDisable(disablePwd, disableCode.trim());
      resetInputs();
      dispatch({ type: "DISABLE_OK" });
      // MFA vừa TẮT (server xác nhận) → refresh /auth/me để mfaEnabled trong cache không stale.
      refreshMfaFlag();
      toast.success(t("page.profile.mfa.toastDisabled"));
    } catch (err) {
      failDisable(err, state.step);
    }
  }

  function cancel() {
    resetInputs();
    dispatch({ type: "CANCEL" });
    // CANCEL falls back to idle/unknown; when /auth/me already told us on/off, say that instead
    // of the "chưa đọc được từ máy chủ" note (the prop did not change, so the effect won't re-run).
    if (typeof mfaEnabled === "boolean") dispatch({ type: "SYNC_STATUS", enabled: mfaEnabled });
  }

  function codesSaved() {
    dispatch({ type: "CODES_SAVED" });
    toast.success(t("page.profile.mfa.toastEnabled"));
  }

  const { step, error, busy } = state;
  const inModal = isModalPhase(step);
  // What the card shows: the resting state, including the one a dialog was opened from.
  const cardEnabled = step.phase === "enabled" || (step.phase === "disable" && step.from === "enabled");

  function onDialogOpenChange(open: boolean) {
    if (open) return;
    // Backup-codes step: MFA is already on server-side — closing acknowledges, never cancels.
    if (step.phase === "backup-codes") codesSaved();
    else cancel();
  }

  return (
    <Card
      title={t("page.profile.mfa.title")}
      icon={{ glyph: ShieldCheck, name: "digital-verification-config" }}
      actions={step.phase === "enabled" ? <Badge theme="success">{t("page.profile.mfa.badgeOn")}</Badge> : undefined}
    >
      {/* Focus target when the dialog closes and its opener is gone (see returnFocus below). */}
      <div ref={bodyRef} tabIndex={-1}>
        {step.phase === "unavailable" ? (
          <ScopedNotification>
            {t("page.profile.mfa.unavailableBefore")} (<span className="slds-text-font_monospace">MFA_SECRET_ENC_KEY</span>){" "}
            {t("page.profile.mfa.unavailableAfter")}
          </ScopedNotification>
        ) : cardEnabled ? (
          <>
            <p>{t("page.profile.mfa.enabledDesc")}</p>
            {!inModal && <ErrorNote error={error} />}
            <div className="slds-m-top_small">
              <Button
                variant="text-destructive"
                iconLeft={ShieldOff}
                onClick={() => { resetInputs(); dispatch({ type: "START_DISABLE" }); }}
              >
                {t("page.profile.mfa.disable")}
              </Button>
            </div>
          </>
        ) : (
          <>
            <p>{t("page.profile.mfa.intro")}</p>
            {step.phase === "idle" && step.known === "unknown" && (
              <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("page.profile.mfa.unknownNote")}</p>
            )}
            {!inModal && <ErrorNote error={error} />}
            <div className="slds-m-top_small">
              <Button
                variant="brand"
                iconLeft={ShieldCheck}
                onClick={() => { resetInputs(); dispatch({ type: "START_ENROLL" }); }}
              >
                {t("page.profile.mfa.enable")}
              </Button>
            </div>
            {/* Lối thoát "đã bật từ trước" chỉ còn nghĩa khi trạng thái CHƯA rõ; server đã
                xác nhận tắt (mfaEnabled=false từ /auth/me) thì không mời tắt cái không bật. */}
            {step.phase === "idle" && step.known === "unknown" && (
              <div className="slds-m-top_x-small">
                <Button variant="base" onClick={() => { resetInputs(); dispatch({ type: "START_DISABLE" }); }}>
                  {t("page.profile.mfa.alreadyOn")}
                </Button>
              </div>
            )}
          </>
        )}
      </div>

      <Modal
        open={inModal}
        onOpenChange={onDialogOpenChange}
        returnFocus={() => bodyRef.current?.querySelector("button") ?? bodyRef.current}
        title={t(step.phase === "disable" ? "page.profile.mfa.disableTitle" : "page.profile.mfa.enrollTitle")}
        footer={inModal ? dialogFooter() : undefined}
      >
        {step.phase === "enroll-password" && (
          <form id={formId} onSubmit={submitEnroll} noValidate>
            <p>{t("page.profile.mfa.enrollPrompt")}</p>
            <div className="slds-m-top_small">
              <Input
                label={t("page.profile.mfa.password")}
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="current-password"
              />
            </div>
            <ErrorNote error={error} />
          </form>
        )}

        {step.phase === "activate" && (
          <form id={formId} onSubmit={submitActivate} noValidate>
            <p>
              {t("page.profile.mfa.activateBefore")} <strong>{t("page.profile.mfa.activateStrong")}</strong>{" "}
              {t("page.profile.mfa.activateAfter")}
            </p>
            <CopyRow label={t("page.profile.mfa.secretLabel")} display={groupSecret(step.secret)} raw={step.secret} />
            <CopyRow label={t("page.profile.mfa.otpauthLabel")} display={step.otpauthUrl} raw={step.otpauthUrl} />
            <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("page.profile.mfa.noQr")}</p>
            <div className="slds-m-top_small">
              <Input
                label={t("page.profile.mfa.codeLabel")}
                value={code}
                onChange={(e) => setCode(e.target.value)}
                placeholder="123456"
                inputMode="numeric"
                autoComplete="one-time-code"
                maxLength={32}
                className="slds-text-font_monospace"
              />
            </div>
            <ErrorNote error={error} />
          </form>
        )}

        {step.phase === "backup-codes" && (
          <>
            <ScopedNotification theme="warning">
              {t("page.profile.mfa.backupBefore")} <strong>{t("page.profile.mfa.backupStrong")}</strong>
              {t("page.profile.mfa.backupAfter")}
            </ScopedNotification>
            <ul
              className="slds-grid slds-wrap slds-box slds-box_x-small slds-theme_shade slds-m-top_small"
              aria-label={t("page.profile.mfa.backupListLabel")}
            >
              {step.codes.map((c) => (
                <li key={c} className="slds-col slds-size_1-of-2 slds-p-vertical_xx-small slds-text-font_monospace">{c}</li>
              ))}
            </ul>
          </>
        )}

        {step.phase === "disable" && (
          <form id={formId} onSubmit={submitDisable} noValidate>
            <p>{t("page.profile.mfa.disablePrompt")}</p>
            <div className="slds-m-top_small">
              <Input
                label={t("page.profile.mfa.password")}
                type="password"
                value={disablePwd}
                onChange={(e) => setDisablePwd(e.target.value)}
                autoComplete="current-password"
              />
            </div>
            <div className="slds-m-top_small">
              <Input
                label={t("page.profile.mfa.disableCode")}
                value={disableCode}
                onChange={(e) => setDisableCode(e.target.value)}
                placeholder="123456"
                inputMode="numeric"
                autoComplete="one-time-code"
                maxLength={32}
                className="slds-text-font_monospace"
              />
            </div>
            <ErrorNote error={error} />
          </form>
        )}
      </Modal>
    </Card>
  );

  // Footer actions of the dialog, per step. Submit buttons sit outside the <form> (blueprint
  // footer), so they reach it through the `form` attribute; Enter inside a field submits too.
  function dialogFooter() {
    const cancelButton = <Button onClick={cancel}>{t("action.cancel")}</Button>;
    switch (step.phase) {
      case "enroll-password":
        return (
          <>
            {cancelButton}
            <Button variant="brand" type="submit" form={formId} disabled={busy || !password}>
              {busy ? t("page.profile.mfa.creating") : t("page.profile.mfa.continue")}
            </Button>
          </>
        );
      case "activate":
        return (
          <>
            {cancelButton}
            <Button variant="brand" type="submit" form={formId} disabled={busy || !code.trim()}>
              {busy ? t("page.profile.mfa.activating") : t("page.profile.mfa.activate")}
            </Button>
          </>
        );
      case "backup-codes":
        return (
          <>
            <CopyAllButton codes={step.codes} />
            <Button variant="brand" iconLeft={Check} onClick={codesSaved}>
              {t("page.profile.mfa.codesSaved")}
            </Button>
          </>
        );
      case "disable":
        return (
          <>
            {cancelButton}
            <Button
              variant="destructive"
              type="submit"
              form={formId}
              disabled={busy || !disablePwd || !disableCode.trim()}
            >
              {busy ? t("page.profile.mfa.disabling") : t("page.profile.mfa.disable")}
            </Button>
          </>
        );
      default:
        return null;
    }
  }
}

// ── Nhỏ lẻ tái dùng trong thẻ ─────────────────────────────────────────────────────────

function ErrorNote({ error }: { error: string | null }) {
  if (!error) return null;
  return (
    <ScopedNotification theme="error" className="slds-m-top_small">
      {error}
    </ScopedNotification>
  );
}

// Ô hiển thị giá trị copy-được: input readonly (chọn tay được kể cả khi Clipboard API bị chặn)
// + nút copy có phản hồi "đã chép".
function CopyRow({ label, display, raw }: { label: string; display: string; raw: string }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(raw);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      toast.error(t("page.profile.mfa.copyFail"));
    }
  }
  return (
    <div className="slds-grid slds-grid_vertical-align-end slds-m-top_small">
      <div className="slds-col slds-grow">
        <Input
          label={label}
          readOnly
          value={display}
          onFocus={(e) => e.currentTarget.select()}
          className="slds-text-font_monospace"
        />
      </div>
      <div className="slds-col slds-no-flex slds-p-left_x-small">
        <ButtonIcon
          variant="border-filled"
          icon={copied ? Check : Copy}
          label={`${t(copied ? "page.profile.mfa.copied" : "page.profile.mfa.copy")} ${label}`}
          onClick={copy}
        />
      </div>
    </div>
  );
}

function CopyAllButton({ codes }: { codes: string[] }) {
  const [copied, setCopied] = useState(false);
  async function copyAll() {
    try {
      await navigator.clipboard.writeText(codes.join("\n"));
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      toast.error(t("page.profile.mfa.copyAllFail"));
    }
  }
  return (
    <Button iconLeft={copied ? Check : Copy} onClick={copyAll}>
      {t(copied ? "page.profile.mfa.copiedAll" : "page.profile.mfa.copyAll")}
    </Button>
  );
}

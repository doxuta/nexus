"use client";

// QR thanh toán — DONG-TIEN-KHEP-KIN p1 (../specs/016-payment-flow/spec.md §6).
// usePaymentQR probes GET /tenant/payment-status once (long-lived query, user-level, boolean only):
// the record page offers the action ONLY when the tenant is configured AND the entity has a money
// field. PaymentQRModal is an SLDS modal (modals/base, small): amount field (select) + transfer memo
// (input) on one row, then the QR drawn client-side from the server's EMVCo payload (`qrcode` lib)
// with amount + memo + account for eye-checking.
import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import QRCode from "qrcode";
import { ApiError, getPaymentQR, getPaymentConfigured, type FieldMeta, type PaymentQR } from "@/lib/api";
import { t } from "@/lib/i18n";
import { Input } from "@/components/slds/Input";
import { Modal } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Spinner } from "@/components/slds/Spinner";

const MONEY_TYPES = new Set(["currency", "number"]);

/** true when the tenant has a bank config AND this entity has a money field. */
export function usePaymentQR(fields: FieldMeta[]): boolean {
  // The USER-level status endpoint, not the admin config read: GET /admin/tenant/payment 403s for
  // everyone who is not an admin, which hid the action from exactly the people collecting money.
  const { data: paymentConfigured } = useQuery({
    queryKey: ["tenant-payment-status"],
    queryFn: getPaymentConfigured,
    staleTime: 5 * 60 * 1000,
    retry: false,
  });
  return !!paymentConfigured && fields.some((f) => MONEY_TYPES.has(f.fieldType));
}

export function PaymentQRModal({ entity, recordId, fields, open, onOpenChange }: {
  entity: string;
  recordId: string;
  fields: FieldMeta[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const moneyFields = useMemo(() => fields.filter((f) => MONEY_TYPES.has(f.fieldType)), [fields]);
  if (moneyFields.length === 0) return null;
  return (
    <Modal open={open} onOpenChange={onOpenChange} size="small" title={t("page.record.qr.title")}>
      {/* Mounted only while open, so every opening starts from the default memo. */}
      {open && <QRBody entity={entity} recordId={recordId} moneyFields={moneyFields} uniqueField={fields.find((f) => f.isUnique)?.name} />}
    </Modal>
  );
}

function QRBody({ entity, recordId, moneyFields, uniqueField }: {
  entity: string;
  recordId: string;
  moneyFields: FieldMeta[];
  uniqueField?: string;
}) {
  const currencyFirst = moneyFields.find((f) => f.fieldType === "currency") ?? moneyFields[0];
  const [amountField, setAmountField] = useState(currencyFirst.name);
  const defaultMemo = uniqueField ? `TT {{${uniqueField}}}` : "TT {{id}}";
  const [memo, setMemo] = useState(defaultMemo);
  // The query keys off the COMMITTED memo (blur/Enter), not every keystroke — otherwise each typed
  // character is a fresh query key and an immediate refetch (review W1).
  const [committedMemo, setCommittedMemo] = useState(defaultMemo);

  const { data, error, isFetching } = useQuery<PaymentQR, unknown>({
    queryKey: ["payment-qr", entity, recordId, amountField, committedMemo],
    queryFn: () => getPaymentQR(entity, recordId, amountField, committedMemo),
    retry: false,
  });

  // The image is keyed by the payload it was drawn from, so a stale one is never shown.
  const [drawn, setDrawn] = useState<{ payload: string; url: string } | null>(null);
  const payload = data?.payload;
  useEffect(() => {
    if (!payload) return;
    let alive = true;
    QRCode.toDataURL(payload, { width: 280, margin: 1 })
      .then((url) => { if (alive) setDrawn({ payload, url }); })
      .catch(() => {});
    return () => { alive = false; };
  }, [payload]);
  const png = drawn && drawn.payload === payload ? drawn.url : "";

  return (
    <>
      <div className="slds-grid slds-gutters_x-small slds-m-bottom_medium">
        <div className="slds-col slds-size_1-of-2">
          <Select
            label={t("page.record.qr.amountField")}
            value={amountField}
            onChange={(e) => setAmountField(e.target.value)}
            placeholder={false}
            options={moneyFields.map((f) => ({ value: f.name, label: f.label || f.name }))}
          />
        </div>
        <div className="slds-col slds-size_1-of-2">
          <Input
            label={t("page.record.qr.memo")}
            value={memo}
            onChange={(e) => setMemo(e.target.value)}
            onBlur={() => setCommittedMemo(memo)}
            onKeyDown={(e) => { if (e.key === "Enter") setCommittedMemo(memo); }}
          />
        </div>
      </div>

      {error ? (
        <ScopedNotification theme="error">
          {error instanceof ApiError ? error.message : t("page.record.qr.error")}
        </ScopedNotification>
      ) : isFetching || !png ? (
        <div className="relative h-72">
          <Spinner size="medium" label={t("page.record.qr.loading")} />
        </div>
      ) : (
        <div className="slds-text-align_center">
          <div className="slds-align_absolute-center">
            {/* eslint-disable-next-line @next/next/no-img-element -- locally-generated data URL */}
            <img src={png} alt="VietQR" className="slds-box slds-box_x-small slds-theme_default" />
          </div>
          <p className="slds-text-heading_medium slds-m-top_small tabular-nums">
            {data!.amount.toLocaleString("vi-VN")} ₫
          </p>
          <p className="slds-text-color_weak">{data!.memo || t("page.record.qr.noMemo")}</p>
          {data!.bank.accountName && (
            <p className="slds-text-body_small slds-m-top_xx-small">
              {data!.bank.accountName} · {data!.bank.accountNumber}
            </p>
          )}
          <p className="slds-text-body_small slds-text-color_weak slds-m-top_small">{t("page.record.qr.hint")}</p>
        </div>
      )}
    </>
  );
}

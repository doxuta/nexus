-- DONG-TIEN-KHEP-KIN p1 (docs/payment-flow.md): per-tenant bank config for VietQR.
-- NULL = not configured. Shape: {bankBin, accountNumber, accountName?} — validated app-side.
ALTER TABLE tenants ADD COLUMN payment JSON NULL;

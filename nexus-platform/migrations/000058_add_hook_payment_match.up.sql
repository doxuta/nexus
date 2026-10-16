-- DONG-TIEN-KHEP-KIN p2 (docs/payment-flow.md §8): bank-credit matching on inbound hooks.
-- payment_match: {targetEntity, refField, refPattern, memoSource, statusField, statusValue,
-- amountSource?, amountField?} — NULL = hook has no matching step.
ALTER TABLE sys_inbound_hooks ADD COLUMN payment_match JSON NULL;
-- Delivery log gains the match outcome: matched | unmatched (+ the matched target record).
ALTER TABLE sys_hook_deliveries
    ADD COLUMN match_status VARCHAR(16) NULL,
    ADD COLUMN match_record_id CHAR(36) NULL;

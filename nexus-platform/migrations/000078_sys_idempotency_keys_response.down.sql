-- Reverse of 000078: drop the stored bulk/import response. Claims of POST /:entity are untouched; a
-- completed bulk/import claim loses its replay body (a resend within its window then answers 409 until
-- the claim expires — never a second write).
ALTER TABLE sys_idempotency_keys DROP COLUMN response;

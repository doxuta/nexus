-- Reverts 000081: the email log goes with its table (nothing references it).
DROP TABLE IF EXISTS sys_email_log;

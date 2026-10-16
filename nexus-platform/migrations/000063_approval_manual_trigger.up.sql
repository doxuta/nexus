-- APPROVAL-REQUEST: allow a process to be armed by a PERSON, not only by a save.
--
-- Before this, sys_approval_processes.trigger_event was ENUM('after_create','after_update'):
-- an approval could only fire the moment a record was written. That covers a rule ("any discount
-- over 30% needs sign-off") and misses a request ("I am done with this quote, please review it"),
-- which is most real back-office work. Worse, the submitter had no verb at all — unable to ask,
-- therefore unable to withdraw.
--
-- The Go trigger validator accepting 'manual' while this column did not is the project's
-- accept-then-swallow class: the write is accepted upstream and dies at INSERT as MySQL 1265
-- (Data truncated) → a 500 with no usable message. Widening the column is what makes the
-- validator's answer true.
--
-- Widening an ENUM adds a value at the END, so existing rows keep their stored values and their
-- ordinal positions are untouched: no data is rewritten and no index is invalidated.
ALTER TABLE sys_approval_processes
    MODIFY COLUMN trigger_event ENUM('after_create','after_update','manual') NOT NULL;

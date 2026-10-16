-- APPROVAL-OUTCOME: let a decision change the record it decided about.
--
-- Until now an approval produced a terminal instance state and nothing else. StatusApproved had
-- exactly one producer (bpm.Decider.transition) and ZERO consumers outside package bpm: the quote
-- everyone signed off stayed in whatever status it was in, and no automation could react, because
-- the decision emitted no event and wrote no field. Approving was a note in an inbox.
--
-- `outcome` holds the write-back the process performs when it reaches a terminal state:
--   {"statusField":"status","approvedValue":"Chấp nhận","rejectedValue":"Từ chối"}
-- NULL (the default, and every existing row) = no write-back, exactly today's behaviour. So this
-- migration changes nothing on its own; it makes the capability expressible.
--
-- One JSON column rather than three scalar ones, matching how `criteria` and `steps` already store
-- their shapes here: the write-back is one concept, and splitting it invites half-configured rows
-- (a field with no values, values with no field) that the loader would then have to reconcile.
ALTER TABLE sys_approval_processes
    ADD COLUMN outcome JSON DEFAULT NULL AFTER steps;

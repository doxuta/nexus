-- #283 rowscope-by-user-field: a grant may now carry access_level "field:<user field name>"
-- ("records whose <user field> is me"). The longest value is "field:" + a 26-char identifier = 32
-- chars, which no longer fits VARCHAR(20). MODIFY keeps NOT NULL DEFAULT 'all' exactly as 000005.
ALTER TABLE acl_role_actions
    MODIFY COLUMN access_level VARCHAR(40) NOT NULL DEFAULT 'all';

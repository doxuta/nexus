-- KHUON-SONG-P1 rollback: drop the pack provenance table. Provenance content is derivable only
-- at install time (packs are not retained server-side), so this down IS lossy by nature — the
-- table exists precisely because this data cannot be backfilled. Standard down for dev resets.
DROP TABLE IF EXISTS sys_pack_items;

-- Reverts 000083: nothing to restore. The purged rows belonged to entities/fields that no longer
-- exist, so they carried no effect; re-creating them would only re-introduce the orphans.
SELECT 1;

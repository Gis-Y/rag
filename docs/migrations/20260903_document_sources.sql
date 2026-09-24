-- Keep source numbers bound to document identities when conversations reload.
-- New databases already have this column through docs/ddl.sql.
ALTER TABLE conversations ADD COLUMN sources JSON NULL;

-- Deleting a bridge (docs/adr/0014-bridges.md) is done by the node that
-- takes its lease: it logs the account out on the network, then removes
-- the bridge, its links and its ghosts' mappings. Until then deleted_at
-- says it is going; 0 is not.
ALTER TABLE bridges ADD COLUMN deleted_at BIGINT NOT NULL DEFAULT 0;

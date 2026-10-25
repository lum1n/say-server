-- Per-room limits on automated senders (docs/adr/0005-agent-safety.md).
-- A zero limit means the server default.
ALTER TABLE rooms ADD COLUMN agents_paused INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rooms ADD COLUMN max_hop_count INTEGER NOT NULL DEFAULT 0;
-- At most agent_turn_limit automated messages, from all senders together,
-- in any agent_turn_window milliseconds.
ALTER TABLE rooms ADD COLUMN agent_turn_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rooms ADD COLUMN agent_turn_window BIGINT NOT NULL DEFAULT 0;
-- At most this many messages per minute from one automated sender.
ALTER TABLE rooms ADD COLUMN agent_rate_limit INTEGER NOT NULL DEFAULT 0;

-- 1 for events from automated senders (agents and the system), which are
-- the ones the budgets count.
ALTER TABLE events ADD COLUMN automated INTEGER NOT NULL DEFAULT 0;
UPDATE events SET automated = 1
WHERE sender_id IN (SELECT id FROM principals WHERE kind IN ('agent', 'system'));

CREATE INDEX events_automated ON events (room_id, seq) WHERE automated = 1;
CREATE INDEX events_automated_sender ON events (room_id, sender_id, seq) WHERE automated = 1;

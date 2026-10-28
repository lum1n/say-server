-- Reactions: who reacted to which message with what. A row is one
-- sender's one key on one message; adding it again or removing what is not
-- there changes nothing and logs nothing. event_seq is the message's seq,
-- so a page of events finds its messages' reactions by range.
CREATE TABLE reactions (
    room_id    TEXT NOT NULL REFERENCES rooms (id),
    event_id   TEXT NOT NULL,
    event_seq  BIGINT NOT NULL,
    sender_id  TEXT NOT NULL REFERENCES principals (id),
    key        TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    PRIMARY KEY (event_id, sender_id, key)
);

CREATE INDEX reactions_room_seq ON reactions (room_id, event_seq);

-- Each change is also a reaction event in the log: `reacts` is the message,
-- `reaction_key` the key, and `removed` 1 when it was taken back.
ALTER TABLE events ADD COLUMN reacts TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN reaction_key TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN removed INTEGER NOT NULL DEFAULT 0;

-- Streaming messages and edits (docs/adr/0010-streaming-and-edits.md). An
-- edit event names the message it changes in `edits`, and the message's
-- row is updated to the result, so readers never fold edits themselves.
ALTER TABLE events ADD COLUMN edits TEXT NOT NULL DEFAULT '';
-- When a final message was last edited, in unix milliseconds; 0 if never.
-- Ending a stream is not an edit.
ALTER TABLE events ADD COLUMN edited_at BIGINT NOT NULL DEFAULT 0;

CREATE INDEX events_streaming ON events (sender_id) WHERE stream = 'streaming';

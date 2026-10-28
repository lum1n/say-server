-- Replies: the message a message answers, shown as a quote. It is kept
-- apart from caused_by, which tracks automated chains: an agent's answer
-- has a cause but is not a reply unless its sender made it one.
ALTER TABLE events ADD COLUMN reply_to TEXT NOT NULL DEFAULT '';

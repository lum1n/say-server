-- Media: the files people send (docs/adr/0019-media.md). A row says what
-- a file is and which room it was sent to; the bytes live in the media
-- store under the same id. A message's attachment parts name their media,
-- which must be the sender's own upload to the same room.
CREATE TABLE media (
    id          TEXT PRIMARY KEY,
    room_id     TEXT NOT NULL REFERENCES rooms (id),
    uploader_id TEXT NOT NULL REFERENCES principals (id),
    mime        TEXT NOT NULL,
    name        TEXT NOT NULL,
    size        BIGINT NOT NULL,
    width       INTEGER NOT NULL DEFAULT 0,
    height      INTEGER NOT NULL DEFAULT 0,
    sha256      TEXT NOT NULL,
    created_at  BIGINT NOT NULL
);

CREATE INDEX media_room ON media (room_id);

-- An anonymous visitor who signs in as a subject that already has a
-- visitor keeps its own thread, and signed_in_as says whose it was: the
-- verified visitor's principal id, so the team can follow the person to
-- that thread (docs/adr/0012-widget.md). '' is not linked.
ALTER TABLE widget_visitors ADD COLUMN signed_in_as TEXT NOT NULL DEFAULT '';

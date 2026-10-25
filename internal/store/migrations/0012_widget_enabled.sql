-- A widget can be switched off without deleting it (docs/adr/0012-widget.md).
ALTER TABLE widgets ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1;

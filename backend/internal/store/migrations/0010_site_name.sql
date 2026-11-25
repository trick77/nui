-- The sites file's name: what the dashboard and the messages call the site. '': its domain.
ALTER TABLE site ADD COLUMN name TEXT NOT NULL DEFAULT '';

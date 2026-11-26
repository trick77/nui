-- The bot list's name of a known bot's or probe's client: its agent's, else its service's, as the list spells it. ''
-- on every other class. The events before it are named by a background job (classify.NameRaw), not here.
ALTER TABLE event ADD COLUMN svc TEXT NOT NULL DEFAULT '';

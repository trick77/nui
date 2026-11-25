-- One alert: a key a rule matched (a fact entering its set, a condition holding) on a site or the instance, its
-- history and its delivery. The table is the outbox, the dedup state and the history in one: unsent rows outlive a
-- restart. Times are unix ms.
CREATE TABLE alert (
    id          INTEGER PRIMARY KEY,
    -- 0: the instance (a job, the size cap, unknown domains).
    site_id     INTEGER NOT NULL,
    -- The rule's name in the notify file, its identity (a catalogue id for the rules recorded without one).
    rule        TEXT    NOT NULL,
    -- The catalogue's trigger id (error.new, source.silent, ...).
    kind        TEXT    NOT NULL,
    -- The subject within the kind (an error's message, an address, a job), '' for the kind as a whole. Raw, clipped.
    key         TEXT    NOT NULL DEFAULT '',
    -- baseline: there at the rule's first pass (or a new signature), recorded, never sent nor shown. pending: a
    -- condition waiting out the rule's for. open, cleared: a condition. once: a fact, or a quiet note.
    state       TEXT    NOT NULL CHECK (state IN ('baseline', 'pending', 'open', 'cleared', 'once')),
    severity    TEXT    NOT NULL CHECK (severity IN ('low', 'normal', 'high')),
    -- The number it is about and what it counts; NULL and '' for none.
    figure      REAL,
    unit        TEXT    NOT NULL DEFAULT '',
    -- One line each, defused already: the outside world's text never reaches a message as it came.
    text        TEXT    NOT NULL,
    detail      TEXT    NOT NULL DEFAULT '',
    -- Where its row jumps to in the dashboard: a panel and its filter (JSON object).
    panel       TEXT    NOT NULL DEFAULT '',
    filter      TEXT    NOT NULL DEFAULT '{}',
    first_at    INTEGER NOT NULL,
    last_at     INTEGER NOT NULL,
    -- When the key left the rule's set: a fact gone from the window, a condition cleared. NULL while in it; the
    -- cooldown against re-entry runs from here.
    left_at     INTEGER,
    -- What the recipients last heard: '' nothing, open, clear. A clear is sent only after an open was.
    told        TEXT    NOT NULL DEFAULT '' CHECK (told IN ('', 'open', 'clear')),
    -- The message waiting to be sent ('' none) and since when: nothing older than 6 hours is sent.
    due         TEXT    NOT NULL DEFAULT '' CHECK (due IN ('', 'open', 'clear', 'remind')),
    due_at      INTEGER,
    -- When a channel last took a message about it.
    notified_at INTEGER,
    -- Sends of the due message so far, the channels it goes to, those that took it and those that refused it
    -- (JSON arrays of channel names).
    attempts    INTEGER NOT NULL DEFAULT 0,
    channels    TEXT    NOT NULL DEFAULT '[]',
    delivered   TEXT    NOT NULL DEFAULT '[]',
    failed      TEXT    NOT NULL DEFAULT '[]'
);
-- A rule's rows in a site's set, and a key's latest row (the cooldown).
CREATE INDEX alert_rule_key ON alert (rule, site_id, key, id);
-- The history of one site, newest first.
CREATE INDEX alert_site ON alert (site_id, id);
-- The outbox.
CREATE INDEX alert_due ON alert (due_at) WHERE due != '';
-- The history's pruning by age.
CREATE INDEX alert_last ON alert (last_at);

-- A rule's last pass per site (0: the instance): the signature of its trigger and parameters then. A rule without a
-- row, with another signature or not run for 6 hours starts with a baseline: what is there is recorded, not sent.
CREATE TABLE alert_rule (
    rule       TEXT    NOT NULL,
    site_id    INTEGER NOT NULL,
    sig        TEXT    NOT NULL,
    checked_at INTEGER NOT NULL,
    PRIMARY KEY (rule, site_id)
);

-- A channel's last delivery and last failure, kept across restarts for the dashboard's footer. error is scrubbed:
-- never a URL.
CREATE TABLE alert_channel (
    name     TEXT    PRIMARY KEY,
    last_ok  INTEGER,
    error_at INTEGER,
    error    TEXT    NOT NULL DEFAULT ''
);

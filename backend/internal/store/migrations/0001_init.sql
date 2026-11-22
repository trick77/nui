-- Sites are declared in the sites file (BACKEND_SITES_FILE) and synced here at start, so events keep a stable
-- numeric site id. A site dropped from the file stays (active = 0): its events are still on disk until retention.
CREATE TABLE site (
    id            INTEGER PRIMARY KEY,
    domain        TEXT    NOT NULL UNIQUE,
    kind          TEXT    NOT NULL DEFAULT 'site' CHECK (kind IN ('site', 'api')),
    product       TEXT    NOT NULL DEFAULT '',
    -- SHA-256 hex of the server feed's bearer key; '' means the site has no feed.
    feed_key_hash TEXT    NOT NULL DEFAULT '',
    -- The ordered path rules (JSON array of {pattern, template}) as last synced.
    path_rules    TEXT    NOT NULL DEFAULT '[]',
    active        INTEGER NOT NULL DEFAULT 1,
    created_at    INTEGER NOT NULL
);

-- One row per browser event or server request. ts is the collector's receive time (unix ms). Everything without a
-- column of its own sits in props (JSON).
CREATE TABLE event (
    id       INTEGER PRIMARY KEY,
    site_id  INTEGER NOT NULL REFERENCES site (id),
    ts       INTEGER NOT NULL,
    source   TEXT    NOT NULL CHECK (source IN ('browser', 'server')),
    kind     TEXT    NOT NULL,
    name     TEXT    NOT NULL DEFAULT '',
    -- The browser's random id, else 'h:' + a hash of address key and user agent.
    visitor  TEXT    NOT NULL DEFAULT '',
    sid      TEXT    NOT NULL DEFAULT '',
    ip       TEXT    NOT NULL,
    -- The address as is for IPv4, its /64 for IPv6: who an actor is.
    ip_key   TEXT    NOT NULL,
    ua       TEXT    NOT NULL DEFAULT '',
    path     TEXT    NOT NULL DEFAULT '',
    template TEXT    NOT NULL DEFAULT '',
    url      TEXT    NOT NULL DEFAULT '',
    ref      TEXT    NOT NULL DEFAULT '',
    ref_host TEXT    NOT NULL DEFAULT '',
    method   TEXT    NOT NULL DEFAULT '',
    status   INTEGER NOT NULL DEFAULT 0,
    dur_ms   INTEGER NOT NULL DEFAULT 0,
    asn      INTEGER NOT NULL DEFAULT 0,
    country  TEXT    NOT NULL DEFAULT '',
    account  TEXT    NOT NULL DEFAULT '',
    -- Actor class: 0 pending, 1 human, 2 known bot, 3 uptime probe, 4 unknown automated.
    cls      INTEGER NOT NULL DEFAULT 0,
    props    TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX event_site_ts ON event (site_id, ts);
CREATE INDEX event_site_actor ON event (site_id, ip_key, ts);

-- Collector counters per site and hour (unix hour). site_id 0 holds what arrived without a site the collector
-- could trust (no Origin, unknown domain). accepted, rate_limited and dropped count events; rejected and
-- unknown_domain count requests (a refused body is never parsed into events). accepted_server is the part of
-- accepted that came through the feed: when a source last delivered, without scanning events.
CREATE TABLE ingest_stat (
    site_id         INTEGER NOT NULL,
    hour            INTEGER NOT NULL,
    accepted        INTEGER NOT NULL DEFAULT 0,
    accepted_server INTEGER NOT NULL DEFAULT 0,
    rate_limited   INTEGER NOT NULL DEFAULT 0,
    rejected       INTEGER NOT NULL DEFAULT 0,
    dropped        INTEGER NOT NULL DEFAULT 0,
    unknown_domain INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, hour)
);

-- The class of one actor (address key) on one UTC day (unix day), with the reasons shown in the dashboard.
CREATE TABLE actor_class (
    site_id    INTEGER NOT NULL REFERENCES site (id),
    ip_key     TEXT    NOT NULL,
    day        INTEGER NOT NULL,
    class      TEXT    NOT NULL CHECK (class IN ('human', 'bot', 'monitor', 'unknown')),
    reasons    TEXT    NOT NULL DEFAULT '[]',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (site_id, ip_key, day)
);
CREATE INDEX actor_class_day ON actor_class (site_id, day);

-- The model's briefing per site and range, keyed by the hash of the facts it was given.
CREATE TABLE llm_briefing (
    site_id    INTEGER NOT NULL REFERENCES site (id),
    range      TEXT    NOT NULL,
    facts_hash TEXT    NOT NULL,
    as_of      INTEGER NOT NULL,
    body       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (site_id, range)
);

-- The model's reading of one behaviour signature (not one address: equal behaviour shares a reading).
CREATE TABLE llm_reading (
    signature  TEXT    PRIMARY KEY,
    body       TEXT    NOT NULL,
    basis_from INTEGER NOT NULL,
    basis_to   INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

-- Short labels for one value: a probed path template ('path') or an unknown user agent ('ua').
CREATE TABLE llm_label (
    kind       TEXT    NOT NULL CHECK (kind IN ('path', 'ua')),
    value      TEXT    NOT NULL,
    label      TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (kind, value)
);

-- Model calls and tokens per site and UTC day (unix day), the daily budget's ledger.
CREATE TABLE llm_usage (
    site_id       INTEGER NOT NULL,
    day           INTEGER NOT NULL,
    calls         INTEGER NOT NULL DEFAULT 0,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, day)
);

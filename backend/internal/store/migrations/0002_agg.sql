-- Figures kept after raw events are purged (agg_addr goes with them by age, the size cap may cut any): built per
-- site and UTC day by internal/agg two days after the day ended, read for a day only while frozen = 1 and dirty = 0
-- (raw otherwise). This file only creates tables; the backfill is a background job.

-- One row per site and UTC day the job built. version counts raw changes to the day (store.InsertEvents,
-- store.SetActorClass, retention's deletes): a build comes out clean only if version is still what its read saw.
-- dirty: 0 clean; 1 raw changed since the rows were written; 2 the day before changed the sessions it carries; 3 a
-- write in progress, or with frozen = 0 rows the size cap cut (rows maybe partial: never read, never cleared; rebuilt
-- only while the day's raw is whole). builder is the build code's
-- version: a release with a higher one rebuilds the days whose raw still exists. raw = 0 once any of the day's events
-- are purged.
CREATE TABLE agg_day (
    site_id  INTEGER NOT NULL REFERENCES site (id),
    day      INTEGER NOT NULL,
    frozen   INTEGER NOT NULL DEFAULT 0,
    dirty    INTEGER NOT NULL DEFAULT 0,
    version  INTEGER NOT NULL DEFAULT 0,
    builder  INTEGER NOT NULL DEFAULT 0,
    raw      INTEGER NOT NULL DEFAULT 1,
    events   INTEGER NOT NULL DEFAULT 0,
    built_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, day)
) WITHOUT ROWID;

-- Per site, UTC hour and class (event.cls 1-4, pending folded into 4 as the dashboard counts it). Sketches
-- (internal/agg): browser_ids the script's own visitor ids, addrs every row's address key, server_addrs the keys
-- with a server row.
CREATE TABLE agg_hour (
    site_id      INTEGER NOT NULL REFERENCES site (id),
    hour         INTEGER NOT NULL,
    cls          INTEGER NOT NULL CHECK (cls BETWEEN 1 AND 4),
    events       INTEGER NOT NULL DEFAULT 0,
    views        INTEGER NOT NULL DEFAULT 0,  -- kind = 'page_view'
    actions      INTEGER NOT NULL DEFAULT 0,  -- browser, not page_view/session_start/engagement/vital
    starts       INTEGER NOT NULL DEFAULT 0,  -- kind = 'session_start'
    requests     INTEGER NOT NULL DEFAULT 0,  -- source = 'server'
    refused      INTEGER NOT NULL DEFAULT 0,  -- server, status >= 400
    errors       INTEGER NOT NULL DEFAULT 0,  -- js_error, or server status >= 500
    browser_ids  BLOB,
    addrs        BLOB,
    server_addrs BLOB,
    PRIMARY KEY (site_id, hour, cls)
) WITHOUT ROWID;

-- Per site, list dimension, UTC day and part (1 people: cls 1; 2 automated: every other class). Each part keeps
-- the union of its top 500 entries by every measure the dashboard ranks or thresholds on; the rest goes into one
-- Other row (other = 1, key '') per parent and sub. n is the entry's exact distinct count that day (an Other row: the
-- sum over its entries, addr_n that of their address counts); ids the dimension's identity sketch, addrs (part 2,
-- visitor and actor dimensions) the address keys for the bots audience; entries and keys (Other rows) the collapsed
-- keys' count and their sorted 64-bit hashes, keys NULL past 50,000.
CREATE TABLE agg_list (
    site_id INTEGER NOT NULL REFERENCES site (id),
    dim     TEXT    NOT NULL,
    day     INTEGER NOT NULL,
    part    INTEGER NOT NULL CHECK (part IN (1, 2)),
    other   INTEGER NOT NULL DEFAULT 0,
    parent  TEXT    NOT NULL DEFAULT '',
    key     TEXT    NOT NULL,
    sub     TEXT    NOT NULL DEFAULT '',
    a       INTEGER NOT NULL DEFAULT 0,
    b       INTEGER NOT NULL DEFAULT 0,
    n       INTEGER NOT NULL DEFAULT 0,
    addr_n  INTEGER NOT NULL DEFAULT 0,
    ids     BLOB,
    addrs   BLOB,
    entries INTEGER NOT NULL DEFAULT 0,
    keys    BLOB,
    PRIMARY KEY (site_id, dim, day, part, other, parent, key, sub)
) WITHOUT ROWID;

-- Per site, UTC day, address key and part: the visitors table, a visitor's sums, refusals and spikes by address.
-- sessions are the key's distinct sids that day, carried those it also had the day before (visits over days =
-- sum(sessions) - sum(carried) after the first day). ua, asn, country and cls are the day's latest row's.
CREATE TABLE agg_addr (
    site_id  INTEGER NOT NULL REFERENCES site (id),
    day      INTEGER NOT NULL,
    ip_key   TEXT    NOT NULL,
    part     INTEGER NOT NULL CHECK (part IN (1, 2)),
    events   INTEGER NOT NULL DEFAULT 0,
    views    INTEGER NOT NULL DEFAULT 0,
    actions  INTEGER NOT NULL DEFAULT 0,
    requests INTEGER NOT NULL DEFAULT 0,
    refused  INTEGER NOT NULL DEFAULT 0,
    sessions INTEGER NOT NULL DEFAULT 0,
    carried  INTEGER NOT NULL DEFAULT 0,
    browser  INTEGER NOT NULL DEFAULT 0,
    human    INTEGER NOT NULL DEFAULT 0,
    first_ts INTEGER NOT NULL,
    last_ts  INTEGER NOT NULL,
    ua       TEXT    NOT NULL DEFAULT '',
    asn      INTEGER NOT NULL DEFAULT 0,
    country  TEXT    NOT NULL DEFAULT '',
    cls      INTEGER NOT NULL DEFAULT 0,  -- raw code, pending kept: actorClass's human rule needs it
    PRIMARY KEY (site_id, day, ip_key, part)
) WITHOUT ROWID;
CREATE INDEX agg_addr_key ON agg_addr (site_id, ip_key, day);
CREATE INDEX agg_addr_requests ON agg_addr (site_id, day, requests);

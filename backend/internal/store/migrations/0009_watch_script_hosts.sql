-- The watch keeps script hosts only: path_state goes with the path findings, and the other tables take the shape
-- below. Dropped, not copied: every row is derived from raw, and the job walks it again from the oldest raw day (a
-- backfill finds nothing, the learning window starts again). No release carried 0007: only a development database
-- had a finding to lose. cut counted templates too, and path_state kept client addresses past their raw.
DROP TABLE path_state;
DROP TABLE watch_open;
DROP TABLE script_host;
DROP TABLE watch_site;

-- The watch job's state (internal/watch): what each script host did over the whole raw history, kept past the raw it
-- was walked from. Every value is the first, the last, the most or set once, so a day walked twice changes nothing.
-- Age retention cuts the rows idle past BACKEND_RETENTION_MONTHS; the size cap never touches them (bounded per site,
-- and a row it cut would come back as new).

-- One row per site: the walk's cursor and what the findings are judged against. Days are unix days, times unix ms.
-- from_day: the first day walked. done_day: the last closed day merged. ready_at: when the walk first caught up (0:
-- still backfilling, no findings). scripts_from: the first day a page reported its script hosts, where the learning
-- window starts. cut: hosts the caps left out, each counted once a day it was.
CREATE TABLE watch_site (
    site_id      INTEGER PRIMARY KEY REFERENCES site (id),
    from_day     INTEGER NOT NULL,
    done_day     INTEGER NOT NULL,
    ready_at     INTEGER NOT NULL DEFAULT 0,
    scripts_from INTEGER NOT NULL DEFAULT 0,
    cut          INTEGER NOT NULL DEFAULT 0
);

-- Per site and host a page loaded scripts from (the browser script's report on its engagement event). addrs: the
-- most distinct address keys of one day that reported it (a visitor id is the browser's word, an address is not);
-- nets: the most distinct networks of one day (IPv4 /24, IPv6 /48), what makes it new. found_at: when the job found
-- it new (0: never); found_first: its first sight on the day it passed as new, the finding's time (set with found_at
-- 0: past that day's bound, found on a later day).
CREATE TABLE script_host (
    site_id     INTEGER NOT NULL REFERENCES site (id),
    host        TEXT    NOT NULL,
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    addrs       INTEGER NOT NULL DEFAULT 0,
    nets        INTEGER NOT NULL DEFAULT 0,
    found_at    INTEGER NOT NULL DEFAULT 0,
    found_first INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, host)
) WITHOUT ROWID;
CREATE INDEX script_host_found ON script_host (site_id, found_at);

-- The findings of the days not closed yet (today, and yesterday until 02:00 UTC), recomputed whole on every pass
-- and replacing the site's rows: provisional, a later pass may drop one. Beside them (kind host, no finding) the
-- script hosts those days reported, as the closed state merged with them would hold them: script_host's last_seen
-- moves only when a day closes. key: the host; first_seen the finding's time, or the host's first sight; detail its
-- state as JSON.
CREATE TABLE watch_open (
    site_id    INTEGER NOT NULL REFERENCES site (id),
    kind       TEXT    NOT NULL CHECK (kind IN ('script', 'host')),
    key        TEXT    NOT NULL,
    first_seen INTEGER NOT NULL,
    detail     TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (site_id, kind, key)
) WITHOUT ROWID;

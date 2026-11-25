-- The watch job's state (internal/watch): what each template and script host did over the whole raw history, kept
-- past the raw it was walked from. Every value is the first, the last, the most, a bounded set or set once, so a day
-- walked twice changes nothing; but a soft day keeps no answer of a path refused before (judged the next day), and
-- walked again after that path's answer it would keep it. Age retention cuts the rows idle past
-- BACKEND_RETENTION_MONTHS; the size cap never touches them (bounded per site, and a row it cut would come back as new).

-- One row per site: the walk's cursor and what the findings are judged against. Days are unix days, times unix ms.
-- from_day: the first day walked, where the learning window starts. done_day: the last closed day merged. ready_at:
-- when the walk first caught up (0: still backfilling, no findings). routed_day: the last day a server row named a
-- route. soft_day: the last day that answered many new unrouted paths 2xx (no path findings that day). shell_bytes:
-- the last size many unrouted paths shared on a day (a single-page app's shell). scripts_from: the first day a page
-- reported its script hosts. cut: templates and hosts the caps left out, each counted once a day it was.
CREATE TABLE watch_site (
    site_id      INTEGER PRIMARY KEY REFERENCES site (id),
    from_day     INTEGER NOT NULL,
    done_day     INTEGER NOT NULL,
    ready_at     INTEGER NOT NULL DEFAULT 0,
    routed_day   INTEGER NOT NULL DEFAULT 0,
    soft_day     INTEGER NOT NULL DEFAULT 0,
    shell_bytes  INTEGER NOT NULL DEFAULT 0,
    scripts_from INTEGER NOT NULL DEFAULT 0,
    cut          INTEGER NOT NULL DEFAULT 0
);

-- Per site and request template ('GET /x.php'): its refusals (404, or 401/403 without a route) and its answers (2xx
-- without error, without a route or by a method other than GET/HEAD/OPTIONS, not of a shell's or its refusals'
-- size). first/last are times, addrs the most distinct address keys of one day, lo/hi the answers' sizes in bytes (0:
-- none reported). refused_sizes: the refusals' distinct sizes as a JSON array, ascending, a few at most (two error
-- pages of a path are two sizes, not the span between them). plain_first: the first answer to an address not mostly
-- refused that day; hit_first and hit_addr the first to one that was. found: the finding the first answer made (flip,
-- hit, endpoint), found_at when the job made it ('' and 0: none; found set and found_at 0: a day's findings past its
-- bound, made on a later day).
CREATE TABLE path_state (
    site_id       INTEGER NOT NULL REFERENCES site (id),
    template      TEXT    NOT NULL,
    refused_first INTEGER NOT NULL DEFAULT 0,
    refused_last  INTEGER NOT NULL DEFAULT 0,
    refused_addrs INTEGER NOT NULL DEFAULT 0,
    refused_sizes TEXT    NOT NULL DEFAULT '[]',
    ok_first      INTEGER NOT NULL DEFAULT 0,
    ok_last       INTEGER NOT NULL DEFAULT 0,
    ok_addrs      INTEGER NOT NULL DEFAULT 0,
    ok_lo         INTEGER NOT NULL DEFAULT 0,
    ok_hi         INTEGER NOT NULL DEFAULT 0,
    plain_first   INTEGER NOT NULL DEFAULT 0,
    hit_first     INTEGER NOT NULL DEFAULT 0,
    hit_addr      TEXT    NOT NULL DEFAULT '',
    found         TEXT    NOT NULL DEFAULT '' CHECK (found IN ('', 'flip', 'hit', 'endpoint')),
    found_at      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, template)
) WITHOUT ROWID;
CREATE INDEX path_state_found ON path_state (site_id, found_at);

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
-- moves only when a day closes. key: the template or host; first_seen when it was first answered or seen; detail its
-- state as JSON.
CREATE TABLE watch_open (
    site_id    INTEGER NOT NULL REFERENCES site (id),
    kind       TEXT    NOT NULL CHECK (kind IN ('flip', 'hit', 'endpoint', 'script', 'host')),
    key        TEXT    NOT NULL,
    first_seen INTEGER NOT NULL,
    detail     TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (site_id, kind, key)
) WITHOUT ROWID;

-- The class 'api' (event.cls 5): a program the product signed in. Two CHECKs refused it, and SQLite changes a CHECK
-- only by building the table anew: the one copy of rows a migration makes, since the hourly aggregates outlive the raw
-- they were built from and cannot be a job's work. No other table references either.

-- The class of one actor (address key) on one UTC day (unix day), with the reasons shown in the dashboard.
CREATE TABLE actor_class_new (
    site_id    INTEGER NOT NULL REFERENCES site (id),
    ip_key     TEXT    NOT NULL,
    day        INTEGER NOT NULL,
    class      TEXT    NOT NULL CHECK (class IN ('human', 'bot', 'monitor', 'unknown', 'api')),
    reasons    TEXT    NOT NULL DEFAULT '[]',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (site_id, ip_key, day)
);
INSERT INTO actor_class_new (site_id, ip_key, day, class, reasons, updated_at)
    SELECT site_id, ip_key, day, class, reasons, updated_at FROM actor_class;
DROP TABLE actor_class;
ALTER TABLE actor_class_new RENAME TO actor_class;
CREATE INDEX actor_class_day ON actor_class (site_id, day);

-- Per site, UTC hour and class (event.cls 1-5, pending folded into 4 as the dashboard counts it). Sketches
-- (internal/agg): browser_ids the script's own visitor ids, addrs every row's address key, server_addrs the keys
-- with a server row.
CREATE TABLE agg_hour_new (
    site_id      INTEGER NOT NULL REFERENCES site (id),
    hour         INTEGER NOT NULL,
    cls          INTEGER NOT NULL CHECK (cls BETWEEN 1 AND 5),
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
INSERT INTO agg_hour_new (site_id, hour, cls, events, views, actions, starts, requests, refused, errors, browser_ids,
        addrs, server_addrs)
    SELECT site_id, hour, cls, events, views, actions, starts, requests, refused, errors, browser_ids, addrs,
        server_addrs FROM agg_hour;
DROP TABLE agg_hour;
ALTER TABLE agg_hour_new RENAME TO agg_hour;

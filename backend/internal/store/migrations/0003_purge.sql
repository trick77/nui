-- Markers the server keeps for its background jobs, one row per key. raw_purge_armed_at (unix ms): the first start
-- of the release that purges raw events after BACKEND_RAW_RETENTION_DAYS; the purge deletes nothing for a week
-- after it. Written by the server at start, never by a migration.
CREATE TABLE agg_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) WITHOUT ROWID;

-- The carried sessions a rebuild keeps from a day's stored agg_addr rows when the day before has no raw left: saved
-- here before the rewrite deletes a row, deleted once the day is written. A rewrite cut short leaves the day
-- AggWriting with part of its rows; its retry takes the carried sessions from here, never from those rows. Only
-- keys with carried > 0, plus one row with an empty ip_key and part 0 that says the copy is whole.
CREATE TABLE agg_carried (
    site_id INTEGER NOT NULL REFERENCES site (id),
    day     INTEGER NOT NULL,
    ip_key  TEXT    NOT NULL,
    part    INTEGER NOT NULL,
    carried INTEGER NOT NULL,
    PRIMARY KEY (site_id, day, ip_key, part)
) WITHOUT ROWID;

-- addrs_cut: the size cap deleted the day's agg_addr rows, all of them in the transaction that set it.
ALTER TABLE agg_day ADD COLUMN addrs_cut INTEGER NOT NULL DEFAULT 0;

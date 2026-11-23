-- What a request did and why it failed, as the product's backend names them (fixed names of its own, cleaned by the
-- collector): op an operation inside the route (an MCP tool), err a failure the status does not show (a JSON-RPC error
-- answered 200). Both '' before the feed carried them.
ALTER TABLE event ADD COLUMN op TEXT NOT NULL DEFAULT '';
ALTER TABLE event ADD COLUMN err TEXT NOT NULL DEFAULT '';
-- 1: the product's router matched nothing (or its catch-all did), so the template is the path's. 0 also before the
-- collector noted it.
ALTER TABLE event ADD COLUMN unrouted INTEGER NOT NULL DEFAULT 0;

-- Calls are server requests but 404s and what was refused without a route (agg.Called). Per hour and class: how many failed (err, or a status from 400), the histogram
-- of their durations, the sketches of their address keys and accounts (internal/agg).
ALTER TABLE agg_hour ADD COLUMN failed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agg_hour ADD COLUMN durations BLOB;
ALTER TABLE agg_hour ADD COLUMN callers BLOB;
ALTER TABLE agg_hour ADD COLUMN accounts BLOB;

-- The entry's duration histogram: endpoints only.
ALTER TABLE agg_list ADD COLUMN durations BLOB;

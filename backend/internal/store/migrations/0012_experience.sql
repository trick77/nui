-- What people experienced, per hour and class (builder 4, internal/agg's vital.go defines every value): engaged the
-- engagements' active milliseconds, each a whole number clamped to an hour (anything else 0); v_lcp to v_ttfb the
-- histograms (agg.Hist) of the vitals measured the standard way (p.v = 2), in ms, CLS ×1000. v_: cls is the class.
-- Days an older builder built hold 0 and NULL.
ALTER TABLE agg_hour ADD COLUMN engaged INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agg_hour ADD COLUMN v_lcp BLOB;
ALTER TABLE agg_hour ADD COLUMN v_inp BLOB;
ALTER TABLE agg_hour ADD COLUMN v_cls BLOB;
ALTER TABLE agg_hour ADD COLUMN v_fcp BLOB;
ALTER TABLE agg_hour ADD COLUMN v_ttfb BLOB;

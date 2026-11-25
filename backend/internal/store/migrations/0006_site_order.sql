-- The sites file's order: a site's place among its product's in the dashboard's menu. 0: none set (listed after the
-- ordered ones, by domain).
ALTER TABLE site ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0;

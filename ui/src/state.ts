// The page's state lives in the URL, so a reload or a link reproduces the
// view: /dashboard/?site=…&range=7d&aud=bots&cmp=yoy, the log at
// /dashboard/log?site=…&addr=…, the alerts at /dashboard/alerts?site=…&all=1.
// What the URL leaves out is the browser's last: the audience, the domain,
// and each domain's range.

import type { Audience, Compare, LogFilter, Range } from "./api";
import { SEP } from "./format";

export const RANGES: Range[] = ["24h", "7d", "30d", "90d", "12mo"];
export const AUDIENCES: Audience[] = ["all", "humans", "bots"];
export const COMPARES: Compare[] = ["prev", "yoy", "off"];
export const LOG_KEYS = [
  "addr",
  "asn",
  "path",
  "class",
  "kind",
  "status",
  "calls",
  "name",
  "endpoint",
  "account",
  "op",
  "failure",
] as const;

export type Page = "dash" | "log" | "alerts";

// A view as the URL says it: site and range null where it does not.
export type View = {
  page: Page;
  site: string | null;
  range: Range | null;
  aud: Audience;
  cmp: Compare;
  log: LogFilter;
  // The alerts of every site and the instance's, not the domain's alone.
  all: boolean;
};

// A view of a listed domain, nothing left open.
export type Shown = View & { site: string; range: Range };

const AUD_KEY = "app.aud";
const SITE_KEY = "app.site";
// A key per domain, not one map: two tabs never overwrite each other's.
const rangeKey = (site: string) => `app.range.${site}`;

// What storage does not take, kept until the page is left: a domain switched
// away from and back to has its range with storage blocked too.
const unstored = new Map<string, string>();

function stored(key: string): string | null {
  const own = unstored.get(key);
  if (own != null) return own;
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function store(key: string, v: string) {
  try {
    localStorage.setItem(key, v);
    unstored.delete(key);
  } catch {
    unstored.set(key, v);
  }
}

export function storedAudience(): Audience {
  const v = stored(AUD_KEY) as Audience;
  return AUDIENCES.includes(v) ? v : "all";
}

export function storeAudience(a: Audience) {
  store(AUD_KEY, a);
}

export function storedRange(site: string): Range {
  const v = stored(rangeKey(site)) as Range;
  return RANGES.includes(v) ? v : "7d";
}

// What is shown is what the next visit starts with, a deep link included.
export function storeShown(site: string, range: Range) {
  store(SITE_KEY, site);
  store(rangeKey(site), range);
}

// The pages beside the dashboard, by their path under the mount.
const SUBPAGE = /\/(log|alerts)\/?$/;

// The app's mount: /dashboard/ in production, / under the dev server.
export function basePath(pathname: string): string {
  const p = pathname.replace(SUBPAGE, "/");
  return p.endsWith("/") ? p : `${p}/`;
}

export function parseView(pathname: string, search: string): View {
  const q = new URLSearchParams(search);
  const range = q.get("range") as Range;
  const aud = q.get("aud") as Audience;
  const cmp = q.get("cmp") as Compare;
  const log: LogFilter = {};
  for (const k of LOG_KEYS) {
    const v = q.get(k);
    if (v) log[k] = v;
  }
  const page = (SUBPAGE.exec(pathname)?.[1] ?? "dash") as Page;
  return {
    page,
    site: q.get("site") || null,
    range: RANGES.includes(range) ? range : null,
    aud: AUDIENCES.includes(aud) ? aud : storedAudience(),
    cmp: COMPARES.includes(cmp) ? cmp : "prev",
    log,
    all: page === "alerts" && q.get("all") === "1",
  };
}

// The view with what the URL left open filled in: a domain it does not name
// or that is not listed is the last one shown here, else the first; a range
// it does not name is that domain's last. A view without a gap comes back as
// it is, the same object. Null: no domain is listed.
export function resolveView(v: View, sites: readonly string[]): Shown | null {
  const listed = (s: string | null) => s != null && sites.includes(s);
  if (listed(v.site) && v.range) return v as Shown;
  const last = stored(SITE_KEY);
  const site = listed(v.site) ? v.site : listed(last) ? last : sites[0];
  if (!site) return null;
  return { ...v, site, range: v.range ?? storedRange(site) };
}

export const PRODUCT = "Trakk";

// The tab names the domain shown as it is called and, on the log, the address or network it
// follows or the log itself, on the alerts page that (without the domain when
// they are every site's): several tabs open side by side stay apart. Range,
// audience and the other filters stay out, a tab has no room for them.
export function tabTitle(site: string | null, v: View): string {
  if (!site) return PRODUCT;
  if (v.page === "alerts")
    return v.all
      ? `${PRODUCT}${SEP}Alerts`
      : [PRODUCT, site, "Alerts"].join(SEP);
  const parts = [PRODUCT, site];
  if (v.page === "log") {
    const asn = v.log.asn && `AS${v.log.asn.replace(/^as/i, "")}`;
    parts.push(v.log.addr || asn || "Log");
  }
  return parts.join(SEP);
}

export function viewHref(base: string, v: View): string {
  const q = new URLSearchParams();
  // Site and range are always written, the audience on the dashboard, the
  // defaults too: left out, a reload or a link would take the browser's last.
  if (v.site) q.set("site", v.site);
  if (v.range) q.set("range", v.range);
  if (v.page === "dash") {
    q.set("aud", v.aud);
    if (v.cmp !== "prev") q.set("cmp", v.cmp);
  } else if (v.page === "log") {
    for (const k of LOG_KEYS) {
      const val = v.log[k];
      if (val) q.set(k, val);
    }
  } else if (v.all) q.set("all", "1");
  const s = q.toString();
  return `${base}${v.page === "dash" ? "" : v.page}${s ? `?${s}` : ""}`;
}

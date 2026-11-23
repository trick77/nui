// The page's state lives in the URL, so a reload or a link reproduces the
// view: /dashboard/?site=…&range=7d&aud=bots&cmp=yoy, the log at
// /dashboard/log?site=…&addr=…. The audience is also remembered per
// browser.

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
] as const;

export type Page = "dash" | "log";

export type View = {
  page: Page;
  site: string | null;
  range: Range;
  aud: Audience;
  cmp: Compare;
  log: LogFilter;
};

const AUD_KEY = "app.aud";

export function storedAudience(): Audience {
  try {
    const v = localStorage.getItem(AUD_KEY) as Audience | null;
    return v && AUDIENCES.includes(v) ? v : "all";
  } catch {
    return "all";
  }
}

export function storeAudience(a: Audience) {
  try {
    localStorage.setItem(AUD_KEY, a);
  } catch {
    // storage blocked: the URL still carries it
  }
}

// The app's mount: /dashboard/ in production, / under the dev server.
export function basePath(pathname: string): string {
  const p = pathname.replace(/\/log\/?$/, "/");
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
  return {
    page: /\/log\/?$/.test(pathname) ? "log" : "dash",
    site: q.get("site") || null,
    range: RANGES.includes(range) ? range : "7d",
    aud: AUDIENCES.includes(aud) ? aud : storedAudience(),
    cmp: COMPARES.includes(cmp) ? cmp : "prev",
    log,
  };
}

export const PRODUCT = "Stakeout";

// The tab names the domain shown and, on the log, the address or network it
// follows or the log itself: several tabs open side by side stay apart.
// Range, audience and the other filters stay out, a tab has no room for them.
export function tabTitle(site: string | null, v: View): string {
  if (!site) return PRODUCT;
  const parts = [PRODUCT, site];
  if (v.page === "log") {
    const asn = v.log.asn && `AS${v.log.asn.replace(/^as/i, "")}`;
    parts.push(v.log.addr || asn || "Log");
  }
  return parts.join(SEP);
}

export function viewHref(base: string, v: View): string {
  const q = new URLSearchParams();
  if (v.site) q.set("site", v.site);
  if (v.range !== "7d") q.set("range", v.range);
  if (v.page === "dash") {
    // Always written: left out, a reload would take another tab's choice.
    q.set("aud", v.aud);
    if (v.cmp !== "prev") q.set("cmp", v.cmp);
  } else {
    for (const k of LOG_KEYS) {
      const val = v.log[k];
      if (val) q.set(k, val);
    }
  }
  const s = q.toString();
  return `${base}${v.page === "log" ? "log" : ""}${s ? `?${s}` : ""}`;
}

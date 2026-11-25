// Synthetic answers in the dashboard API's shapes, for the tests and for the
// dev server's fixture mode (npm run dev:fixtures). Never part of the
// production bundle: main.tsx imports it only under import.meta.env.DEV.
// The numbers stand in for the backend's, so this file may compute; the
// UI never does.

import type {
  Alert,
  Alerts,
  Axis,
  Classes,
  ClassKey,
  Clients,
  Dim,
  Domain,
  ErrorBreakdown,
  ErrorCount,
  ErrorDetail,
  ErrorWhere,
  List,
  ListRow,
  Log,
  LogRow,
  Noteworthy,
  Notify,
  Overview,
  SiteInfo,
  Tile,
  TrailItem,
  Visitor,
  VisitorRow,
  Visitors,
  Watch,
  WatchHost,
  WatchPath,
} from "./api";
import { joinSep } from "./format";

export const SITES: SiteInfo[] = [
  {
    site: "lens.example.com",
    product: "Example Lens",
    kind: "site",
    browser: { last: "2026-11-22T13:12:00Z" },
    server: { last: "2026-11-22T13:20:00Z" },
    feed: true,
  },
  {
    site: "console.example.com",
    product: "Example Lens",
    kind: "site",
    browser: { last: "2026-11-22T12:58:00Z" },
    server: null,
    feed: false,
  },
  // Nothing delivered yet, its server feed configured (FEEDS).
  {
    site: "blog.example.org",
    product: "Blog",
    kind: "site",
    browser: null,
    server: null,
    feed: true,
  },
  // A server feed only: the script never loaded.
  {
    site: "api.example.com",
    product: "Example Lens",
    kind: "api",
    browser: null,
    server: { last: "2026-11-22T13:19:00Z" },
    feed: true,
  },
];

// The sites with a server feed configured, delivered or not: the sites list
// shows only the delivered ones, the domain footer says it of all.
const FEEDS = ["lens.example.com", "blog.example.org", "api.example.com"];

const NOW = new Date("2026-11-22T13:20:00Z");
// Each range's start: 24 hours before now, else a midnight in Zurich.
const FROM: Record<string, string> = {
  "24h": "2026-11-21T13:20:00Z",
  "7d": "2026-11-15T23:00:00Z",
  "30d": "2026-10-23T22:00:00Z",
  "90d": "2026-08-24T22:00:00Z",
  "12mo": "2025-11-30T23:00:00Z",
};
// The oldest raw event: 90 d and 12 mo start before it, so what is read
// from raw (a trail, the log, a template's paths) begins later than they
// do. The figures go back further (FIGURES_SINCE).
export const DETAIL_SINCE = "2026-09-03T08:00:00Z";
const FIGURES_SINCE = "2025-10-20T07:00:00Z";
// detail_since as the server sends it: only where the range starts before
// raw does, null where raw covers it.
const detailSince = (range: string | null) =>
  Date.parse(FROM[range || "7d"] ?? FROM["7d"]) < Date.parse(DETAIL_SINCE)
    ? DETAIL_SINCE
    : null;
const MONTHS = "Jan Feb Mar Apr May Jun Jul Aug Sep Oct Nov Dec".split(" ");

function rng(seed: number) {
  let s = seed >>> 0;
  return () => (s = (Math.imul(s, 1664525) + 1013904223) >>> 0) / 4294967296;
}

const HOUR = 3_600_000;
// Each range's slots and bars (hours each), as the backend cuts them.
const GRID: Record<
  string,
  { slot: string; mean: string; hours: number; window: number; bar: number }
> = {
  "24h": { slot: "10 min", mean: "1 h", hours: 1 / 6, window: 6, bar: 1 },
  "7d": { slot: "hour", mean: "6 h", hours: 1, window: 6, bar: 6 },
  "30d": { slot: "3 hours", mean: "24 h", hours: 3, window: 8, bar: 24 },
  "90d": { slot: "6 hours", mean: "24 h", hours: 6, window: 4, bar: 24 },
  "12mo": { slot: "day", mean: "7 d", hours: 24, window: 7, bar: 168 },
};
const grid = (range: string) => GRID[range] ?? GRID["7d"];
// An instant on Zurich's winter clock, which the fixtures take all year.
const local = (ms: number) => new Date(ms + HOUR);
const two = (n: number) => String(n).padStart(2, "0");
const dayOf = (d: Date) =>
  `${two(d.getUTCDate())}.${two(d.getUTCMonth() + 1)}.`;
const clockOf = (d: Date) =>
  `${two(d.getUTCHours())}:${two(d.getUTCMinutes())}`;
const WEEKDAYS = "Sun Mon Tue Wed Thu Fri Sat".split(" ");

// Where a range begins on that clock.
function startOf(range: string): number {
  const at = Date.parse(FROM[range] ?? FROM["7d"]);
  if (range === "24h") return at;
  const d = local(at + HOUR / 2);
  return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate()) - HOUR;
}

// The bounds of a range's slots of so many hours, up to the one now is in.
function bounds(range: string, hours: number): number[] {
  const out = [startOf(range)];
  while (out[out.length - 1] < NOW.getTime())
    out.push(out[out.length - 1] + hours * HOUR);
  return out;
}

// The time axis of bounds, as the backend places it.
function axisOf(range: string, b: number[]): Axis {
  const at = (t: number) =>
    Math.max(0, Math.min(b.length - 1, (t - b[0]) / (b[1] - b[0])));
  const end = b[b.length - 1];
  const ticks: Axis["ticks"] = [];
  const weekends: Axis["weekends"] = [];
  if (range === "24h") {
    for (let t = Math.ceil(b[0] / HOUR) * HOUR; t < end; t += HOUR) {
      const d = local(t);
      if (d.getUTCHours() % 2 === 0)
        ticks.push({ at: at(t), label: clockOf(d), line: !d.getUTCHours() });
    }
    return { ticks, weekends };
  }
  for (let t = b[0]; t < end; t += 24 * HOUR) {
    const d = local(t);
    const monday = d.getUTCDay() === 1;
    const month = d.getUTCDate() === 1;
    if (range === "7d")
      ticks.push({
        at: at(t),
        label: `${WEEKDAYS[d.getUTCDay()]} ${dayOf(d)}`,
        line: true,
      });
    else if (range === "30d")
      ticks.push({ at: at(t), label: dayOf(d), line: monday });
    else if (range === "90d" && (monday || month))
      ticks.push({ at: at(t), label: monday ? dayOf(d) : "", line: month });
    else if (range === "12mo" && month)
      ticks.push({ at: at(t), label: MONTHS[d.getUTCMonth()], line: true });
    if (range !== "12mo" && d.getUTCDay() === 6)
      weekends.push([at(t), at(t + 48 * HOUR)]);
  }
  return { ticks, weekends };
}

// The names of a range's slots, or of its bars.
function names(range: string, b: number[], bars = false): string[] {
  return b.slice(0, -1).map((t, i) => {
    const d = local(t);
    const e = local(b[i + 1]);
    const day = `${WEEKDAYS[d.getUTCDay()]} ${dayOf(d)}`;
    if (range === "24h")
      return bars ? `hour to ${clockOf(e)}` : `${clockOf(d)}–${clockOf(e)}`;
    if (range === "12mo")
      return bars
        ? `${dayOf(d)}–${dayOf(local(b[i + 1] - 24 * HOUR))}`
        : `${day}${d.getUTCFullYear()}`;
    if (bars && range !== "7d") return day;
    return `${day} ${clockOf(d)}–${clockOf(e)}`;
  });
}

const sum = (a: number[]) => a.reduce((x, y) => x + y, 0);
const median = (a: number[]) => {
  const s = [...a].sort((x, y) => x - y);
  const m = s.length >> 1;
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
};
const round1 = (v: number) => Math.round(v * 10) / 10;

// How much of the last slot is over: all of 24h's, which ends now.
function lastPart(b: number[]): number {
  const n = b.length - 1;
  return (NOW.getTime() - b[n - 1]) / (b[n] - b[n - 1]);
}

// base is a day's figure; a slot has its hours' part of it, by day and night.
function series(b: number[], base: number, seed: number, spike = 1) {
  const n = b.length - 1;
  const r = rng(seed * 7919 + n);
  const cur: number[] = [];
  const prev: number[] = [];
  const at = Math.floor(n * 0.62);
  for (let i = 0; i < n; i++) {
    const hours = (b[i + 1] - b[i]) / HOUR;
    const mid = local(b[i] + (hours * HOUR) / 2);
    const week = [0.62, 1.05, 1.12, 1.1, 1.06, 0.98, 0.66][mid.getUTCDay()];
    const day =
      hours >= 24
        ? 1
        : 1 + 0.7 * Math.sin(((mid.getUTCHours() - 9) / 24) * 2 * Math.PI);
    const part = i === n - 1 ? lastPart(b) : 1;
    const grow = 0.86 + (0.28 * i) / n;
    const v = (base * hours * week * day) / 24;
    cur.push(
      Math.round(
        v * grow * (0.88 + 0.24 * r()) * part * (i === at ? spike : 1),
      ),
    );
    prev.push(Math.round(v * 0.9 * (0.88 + 0.24 * r())));
  }
  return { cur, prev };
}

// The mean of the window around each slot, moved in at the edges; none for
// the slots from end on.
const moving = (a: number[], window: number, end: number) =>
  a.map((_, i) => {
    if (i >= end) return null;
    const at = Math.min(i - Math.floor(window / 2), end - window);
    const w = a.slice(Math.max(0, at), Math.max(0, at) + window);
    return round1(sum(w) / w.length);
  });

function overview(
  range: string,
  site: string,
  aud: string,
  cmp: string,
): Overview {
  const feed = SITES.find((s) => s.site === site)?.server != null;
  const k = aud === "humans" ? 0.8 : aud === "bots" ? 0.25 : 1;
  const base = 410 * k;
  const g = grid(range);
  const bs = bounds(range, g.hours);
  const open = range !== "24h";
  const mk = (
    key: Tile["key"],
    label: string,
    b: number,
    seed: number,
    opt: Partial<Tile> & {
      rate?: boolean;
      lowerBetter?: boolean;
      spike?: number;
    } = {},
  ): Tile => {
    const s = series(bs, b, seed, opt.spike);
    // To now, against the prior period cut at the same time of day.
    let value = sum(s.cur);
    let prev: number | null =
      sum(s.prev.slice(0, -1)) +
      Math.round(s.prev[s.prev.length - 1] * lastPart(bs));
    let cur = s.cur;
    let before = s.prev;
    if (opt.rate) {
      const perSlot = (b * g.hours) / 24;
      cur = s.cur.map((v, i) => round1((v / perSlot) * 4 + (i % 3)));
      before = s.prev.map((v, i) => round1((v / perSlot) * 4 + ((i + 1) % 3)));
      value = round1(median(cur));
      prev = round1(median(before));
    }
    const over = open ? cur.length - 1 : cur.length;
    const mean = moving(cur, g.window, over);
    let prevMean = moving(before, g.window, over);
    const diff = opt.rate
      ? round1(value - prev)
      : round1(((value - prev) / prev) * 100);
    const up = diff > 0;
    // Flat below 2 % or half a point, as the backend judges.
    const verdict =
      Math.abs(diff) < (opt.rate ? 0.5 : 2)
        ? "flat"
        : up !== Boolean(opt.lowerBetter)
          ? "good"
          : "bad";
    // As the backend answers with the comparison off: an empty series, a
    // flat verdict.
    if (cmp === "off") prevMean = [];
    return {
      key,
      label,
      unit: opt.unit ?? (opt.rate ? "%" : ""),
      kind: opt.rate ? "rate" : "sum",
      value,
      prev: cmp === "off" ? null : prev,
      change: cmp === "off" ? null : opt.rate ? { pp: diff } : { pct: diff },
      verdict: cmp === "off" ? "flat" : verdict,
      series: cur,
      mean,
      prev_mean: prevMean,
      delta: prevMean.map((p, i) => {
        const m = mean[i];
        if (m == null || p == null || (!opt.rate && !p)) return null;
        return round1(opt.rate ? m - p : ((m - p) / p) * 100);
      }),
      low: false,
      server: opt.server ?? false,
    };
  };
  const isApi = SITES.find((s) => s.site === site)?.kind === "api";
  const tiles: Tile[] = isApi
    ? [
        mk("calls", "Calls", base * 9.2, 4, { server: true }),
        mk("accounts", "Accounts", base * 0.05, 1, { server: true }),
        mk("callers", "Addresses", base * 0.2, 2, { server: true }),
        mk("failed", "Failed", base, 5, {
          rate: true,
          lowerBetter: true,
          server: true,
        }),
        mk("p95", "95th percentile", base * 0.3, 6, {
          lowerBetter: true,
          server: true,
          unit: "ms",
        }),
      ]
    : siteTiles();
  function siteTiles() {
    return [
      mk("visitors", aud === "bots" ? "Addresses" : "Unique visitors", base, 1),
      mk("views", "Page views", base * 2.34, 2),
      mk("events", "Events", base * 0.43, 3),
      mk("requests", "Requests", base * 9.2, 4, { server: true }),
      mk("auto_share", "Unknown automated", base, 5, {
        rate: true,
        lowerBetter: true,
        server: true,
        spike: 2.5,
      }),
      mk("errors", "Errors", base * 0.12, 6, {
        lowerBetter: true,
        spike: 3,
      }),
    ].filter((t) => feed || !t.server);
  }
  return {
    period: {
      from: FROM[range] ?? FROM["7d"],
      to: NOW.toISOString(),
      slot: g.slot,
      mean: g.mean,
      labels: names(range, bs),
      ...axisOf(range, bs),
      // Empty with the comparison off.
      prev_label:
        cmp === "off"
          ? ""
          : cmp === "yoy"
            ? "last year"
            : `prior ${range.replace(/(\d+)/, "$1 ")}`,
      open,
      first: null,
      low: null,
      since: null,
    },
    live: { humans: 7, automated: feed ? 3 : 1 },
    tiles,
  };
}

function noteworthy(site: string, watchFirst = false): Noteworthy {
  const feed = SITES.find((s) => s.site === site)?.server != null;
  // The backend's ids, units and texts; the briefing's lines (gen) first,
  // the computed facts after them. A line over several findings of one
  // kind lists them in ids and detail ("<figure> <unit> <text>" each), its
  // figure their sum where they count the same thing, else empty.
  const items: Noteworthy["items"] = [
    {
      id: 'err:TypeError: cannot read "prefixes"',
      ids: ['err:TypeError: cannot read "prefixes"'],
      kind: "problem",
      figure: "14",
      unit: "times",
      gen: "Safari breaks on AS pages since the deploy",
      text: 'New error: TypeError: cannot read "prefixes"',
      detail: [],
      target: { panel: "errors", filter: {} },
    },
    {
      id: "spike:addr:198.51.100.7",
      ids: ["spike:addr:198.51.100.7"],
      kind: "automation",
      figure: "2338",
      unit: "requests from",
      gen: "One operator is walking the IP lookup API",
      text: "198.51.100.7 (AS64510 Example Cloud)",
      detail: ["88.4 % of all requests, 0 in the prior 7 d"],
      target: { panel: "visitors", filter: { key: "198.51.100.7" } },
    },
    {
      id: "probe:GET /.env",
      ids: ["probe:GET /.env", "probe:GET /wp-login.php"],
      kind: "automation",
      figure: "283",
      unit: "probes of",
      gen: "Scanners look for secrets and logins",
      text: "New probed path: GET /.env, and 1 related",
      detail: [
        "212 probes of New probed path: GET /.env",
        "71 probes of New probed path: GET /wp-login.php",
      ],
      target: { panel: "probed", filter: {} },
    },
    {
      id: "ref:news.ycombinator.com",
      ids: ["ref:news.ycombinator.com", "trend:visitors:up"],
      kind: "people",
      figure: "",
      unit: "",
      gen: "Hacker News brought a wave of new people",
      text: "New referrer: news.ycombinator.com, and 1 related",
      detail: [
        "749 visitors from New referrer: news.ycombinator.com",
        "+24 % visitors Visitors up against the prior 7 d (2841, was 2291)",
      ],
      target: { panel: "referrers", filter: {} },
    },
    {
      id: "spike:net:64510",
      ids: ["spike:net:64510"],
      kind: "automation",
      figure: "2340",
      unit: "requests from",
      gen: null,
      text: "AS64510 Example Cloud",
      detail: ["88.5 % of all requests, 0 in the prior 7 d"],
      target: { panel: "log", filter: { kind: "request", asn: "64510" } },
    },
    {
      id: "ref:reddit.com",
      ids: ["ref:reddit.com"],
      kind: "people",
      figure: "12",
      unit: "visitors from",
      gen: null,
      text: "New referrer: reddit.com",
      detail: [],
      target: { panel: "referrers", filter: {} },
    },
  ];
  // The watch's outweigh the rest: first.
  const shown = (watchFirst ? [...WATCH_FINDINGS, ...items] : items)
    .filter(
      (f) =>
        feed ||
        f.id.startsWith("err:") ||
        f.id.startsWith("ref:reddit") ||
        f.id.startsWith("script:"),
    )
    // Without a model: computed texts only.
    .map((f) => (feed ? f : { ...f, gen: null }));
  return {
    headline: feed
      ? {
          gen: "Good week for people, busy one for scripts.",
          // One per finding: the items' ids.
          text: "8 findings against the prior 7 d",
        }
      : { gen: null, text: "2 findings against the prior 7 d" },
    // The range's closed end: 7 d in Europe/Zurich.
    as_of: "2026-11-21T23:00:00Z",
    items: shown.slice(0, 5),
    more: Math.max(0, shown.length - 5),
  };
}

// The watch's findings as Noteworthy words them (FixtureOptions.nw):
// answered probes, a new target, a new script host, and a refused path
// answered before raw was kept, whose line opens the card.
const WATCH_FINDINGS: Noteworthy["items"] = [
  {
    id: "flip:GET /wp-content/plugins/hello/up.php",
    ids: ["flip:GET /wp-content/plugins/hello/up.php"],
    kind: "problem",
    figure: "1",
    unit: "address",
    gen: null,
    text: "Refused path now answered: GET /wp-content/plugins/hello/up.php",
    detail: [
      "Refused since 2 Oct 10:14, answered 21 Nov 04:12 (up to 14 addresses refused a day)",
      "Answered 18.4 kB, refused 512 B or 4.5 kB",
    ],
    target: {
      panel: "log",
      filter: { kind: "request", path: "GET /wp-content/plugins/hello/up.php" },
    },
  },
  {
    id: "hit:GET /.git/config",
    ids: ["hit:GET /.git/config"],
    kind: "problem",
    figure: "1",
    unit: "address",
    gen: null,
    text: "A scanner got an answer: GET /.git/config",
    detail: [
      "Answered 20 Nov 23:41 to 198.51.100.201, mostly refused that day",
    ],
    target: { panel: "visitors", filter: { key: "198.51.100.201" } },
  },
  {
    id: "script:cdn.evil.example",
    ids: ["script:cdn.evil.example"],
    kind: "problem",
    figure: "4",
    unit: "addresses loaded",
    gen: null,
    text: "New script host: cdn.evil.example",
    detail: ["First seen 21 Nov 18:02, pages load scripts from it"],
    target: { panel: "scripts", filter: { key: "cdn.evil.example" } },
  },
  {
    id: "flip:GET /cgi-bin/status",
    ids: ["flip:GET /cgi-bin/status"],
    kind: "problem",
    figure: "2",
    unit: "addresses",
    gen: null,
    text: "Refused path now answered: GET /cgi-bin/status (its requests are no longer kept)",
    detail: ["Refused since 2 Jul 06:00, answered 28 Aug 11:00"],
    target: { panel: "answered", filter: { key: "GET /cgi-bin/status" } },
  },
  {
    id: "endpoint:POST /xmlrpc.php",
    ids: ["endpoint:POST /xmlrpc.php"],
    kind: "automation",
    figure: "3",
    unit: "addresses",
    gen: null,
    text: "New target answered: POST /xmlrpc.php",
    detail: [
      "First answered 22 Nov 12:05",
      "Provisional: judged on the day so far",
    ],
    target: {
      panel: "log",
      filter: { kind: "request", path: "POST /xmlrpc.php" },
    },
  },
];

const WATCH_PATHS: WatchPath[] = [
  {
    id: "endpoint:POST /xmlrpc.php",
    kind: "endpoint",
    key: "POST /xmlrpc.php",
    at: "2026-11-22T11:05:00Z",
    provisional: true,
    raw: true,
    refused_first: null,
    refused_last: null,
    refused_addrs: 0,
    refused_bytes: null,
    ok_first: "2026-11-22T11:05:00Z",
    ok_last: "2026-11-22T13:02:00Z",
    ok_addrs: 3,
    ok_bytes: null,
    hit_at: null,
    hit_addr: "",
    detail: ["First answered 22 Nov 12:05"],
  },
  {
    id: "flip:GET /wp-content/plugins/hello/up.php",
    kind: "flip",
    key: "GET /wp-content/plugins/hello/up.php",
    at: "2026-11-21T03:12:00Z",
    provisional: false,
    raw: true,
    refused_first: "2026-10-02T08:14:00Z",
    refused_last: "2026-11-20T21:40:00Z",
    refused_addrs: 14,
    refused_bytes: [512, 4515],
    ok_first: "2026-11-21T03:12:00Z",
    ok_last: "2026-11-22T09:30:00Z",
    ok_addrs: 1,
    ok_bytes: [18400, 18400],
    hit_at: null,
    hit_addr: "",
    detail: [
      "Refused since 2 Oct 10:14, answered 21 Nov 04:12 (up to 14 addresses refused a day)",
      "Answered 18.4 kB, refused 512 B or 4.5 kB",
    ],
  },
  {
    id: "hit:GET /.git/config",
    kind: "hit",
    key: "GET /.git/config",
    at: "2026-11-20T22:41:00Z",
    provisional: false,
    raw: true,
    refused_first: "2026-09-14T02:00:00Z",
    refused_last: "2026-11-20T22:40:00Z",
    refused_addrs: 6,
    refused_bytes: null,
    ok_first: "2026-11-20T22:41:00Z",
    ok_last: "2026-11-20T22:41:00Z",
    ok_addrs: 1,
    ok_bytes: null,
    hit_at: "2026-11-20T22:41:00Z",
    hit_addr: "198.51.100.201",
    detail: [
      "Answered 20 Nov 23:41 to 198.51.100.201, mostly refused that day",
    ],
  },
  {
    id: "flip:GET /cgi-bin/status",
    kind: "flip",
    key: "GET /cgi-bin/status",
    // Within 90 d, before raw begins (DETAIL_SINCE).
    at: "2026-08-28T09:00:00Z",
    provisional: false,
    raw: false,
    refused_first: "2026-07-02T04:00:00Z",
    refused_last: "2026-08-27T23:10:00Z",
    refused_addrs: 3,
    refused_bytes: null,
    ok_first: "2026-08-28T09:00:00Z",
    ok_last: "2026-08-29T10:00:00Z",
    ok_addrs: 2,
    ok_bytes: null,
    hit_at: null,
    hit_addr: "",
    detail: ["Refused since 2 Jul 06:00, answered 28 Aug 11:00"],
  },
];

const WATCH_HOSTS: WatchHost[] = [
  {
    host: "cdn.evil.example",
    first_seen: "2026-11-21T17:02:00Z",
    last_seen: "2026-11-22T12:48:00Z",
    addrs: 4,
    new: true,
    provisional: false,
  },
  {
    host: "pay.example.net",
    first_seen: "2026-11-22T10:15:00Z",
    last_seen: "2026-11-22T12:58:00Z",
    addrs: 3,
    new: true,
    provisional: true,
  },
  ...[
    "cdn.jsdelivr.example",
    "fonts.example.net",
    "maps.example.com",
    "stats.example.org",
    "widgets.example.net",
    "player.example.com",
    "consent.example.org",
  ].map((host, i) => ({
    host,
    first_seen: "2026-08-04T09:00:00Z",
    last_seen: "2026-11-22T13:0" + i + ":00Z",
    addrs: 40 - i * 5,
    new: false,
    provisional: false,
  })),
];

// The watch's answer: a feed site's answered probes, every site's script
// hosts but the API's (it has no browser); the blog still being read.
function watch(site: string, range: string): Watch {
  const from = Date.parse(FROM[range] ?? FROM["7d"]);
  const feed = FEEDS.includes(site);
  const ready = site !== "blog.example.org";
  const browser = SITES.find((s) => s.site === site)?.browser != null;
  return {
    paths:
      feed && ready ? WATCH_PATHS.filter((p) => Date.parse(p.at) >= from) : [],
    hosts:
      browser && ready
        ? WATCH_HOSTS.filter((h) => Date.parse(h.last_seen) >= from)
        : [],
    feed,
    ready,
    learning_until: ready ? "2026-06-08T00:00:00Z" : null,
    scripts_learning_until: ready && browser ? "2026-07-01T00:00:00Z" : null,
    soft_day: site === "lens.example.com" ? "2026-11-18" : null,
    cut: site === "lens.example.com" ? 37 : 0,
  };
}

const CLASS_ROWS: Classes["rows"] = [
  {
    class: "human",
    requests: 14554,
    share: 44,
    addresses: 132,
    change: { pct: 9 },
    verdict: "good",
  },
  {
    class: "bot",
    requests: 8120,
    share: 25,
    addresses: 17,
    change: { pct: 2 },
    verdict: "flat",
  },
  {
    class: "monitor",
    requests: 4060,
    share: 12,
    addresses: 7,
    change: { pct: 0 },
    verdict: "flat",
  },
  {
    class: "unknown",
    requests: 6210,
    share: 19,
    addresses: 38,
    change: { pct: 64 },
    verdict: "bad",
  },
  {
    class: "api",
    requests: 40,
    share: 0.1,
    addresses: 1,
    change: { new: true },
    verdict: "flat",
  },
];

function classes(range: string): Classes {
  const bs = bounds(range, grid(range).bar);
  const s = (b: number, seed: number, spike = 1) =>
    series(bs, b, seed, spike).cur;
  const open = range !== "24h";
  const axis = axisOf(range, bs);
  // A bar that is a day has its label under it.
  if (grid(range).bar === 24)
    axis.ticks = axis.ticks.map((t) => ({ ...t, at: t.at + 0.5 }));
  return {
    labels: names(range, bs, true),
    ...axis,
    open,
    partial: bs.slice(1).map((_, i) => open && i === bs.length - 2),
    series: {
      human: s(2500, 11),
      bot: s(1400, 12),
      monitor: s(700, 13),
      unknown: s(410, 14, 6.5),
      api: s(6, 15),
    },
    rows: CLASS_ROWS,
  };
}

const row = (
  key: string,
  value: number,
  second: number | null,
  change: ListRow["change"],
  verdict: ListRow["verdict"],
  more: Partial<ListRow> = {},
): ListRow => ({
  key,
  label: key,
  sub: "",
  gen: null,
  value,
  // Always a figure: views, requests or events beside the value.
  second: second ?? Math.round(value * 1.3),
  share: 0,
  change,
  verdict,
  expandable: false,
  ...more,
});

const LISTS: Record<string, ListRow[]> = {
  pages: [
    // entry: percent of all visits that began there; every top-level row
    // opens into its raw paths.
    row("/", 1210, 1890, { pct: 3 }, "good", { expandable: true, entry: 43.3 }),
    row("/AS:n", 840, 2310, { pct: 21 }, "good", {
      expandable: true,
      entry: 27,
    }),
    row("/:ipv4", 512, 1104, { pct: 9 }, "good", {
      expandable: true,
      entry: 17.8,
    }),
    row("/:text", 388, 902, { pct: -4 }, "bad", {
      expandable: true,
      entry: 6.6,
    }),
    row("/:cidr", 141, 260, { pct: 2 }, "flat", {
      expandable: true,
      entry: 3.4,
    }),
    row("/:ipv6", 62, 71, { new: true }, "good", {
      expandable: true,
      entry: 1.8,
    }),
    row("/about", 31, 33, { pct: -11 }, "bad", {
      expandable: true,
      entry: 0.1,
    }),
    row("/privacy", 12, 12, null, "flat", { expandable: true, entry: 0 }),
  ],
  entry: [
    // entry as on pages: the backend sets it for both dimensions.
    row("/", 980, 1210, { pct: 4 }, "good", { entry: 43.3 }),
    row("/AS:n", 610, 840, { pct: 25 }, "good", {
      expandable: true,
      entry: 27,
    }),
    row("/:ipv4", 402, 512, { pct: 11 }, "good", {
      expandable: true,
      entry: 17.8,
    }),
    row("/:text", 150, 388, { pct: -8 }, "bad", {
      expandable: true,
      entry: 6.6,
    }),
    row("/:cidr", 77, 141, { pct: 1 }, "flat", {
      expandable: true,
      entry: 3.4,
    }),
    row("/:ipv6", 40, 62, { new: true }, "good", {
      expandable: true,
      entry: 1.8,
    }),
    row("/about", 3, 31, { pct: -50 }, "bad", { entry: 0.1 }),
  ],
  events: [
    row("form", 912, 640, { pct: 4 }, "good", { sub: "auto" }),
    row("outbound_click", 486, 301, { pct: 18 }, "good", { sub: "auto" }),
    row("search", 388, 240, { pct: 11 }, "good", { sub: "auto" }),
    row("download", 133, 97, { pct: -9 }, "bad", { sub: "auto" }),
    row("copy_prefix_list", 74, 51, { new: true }, "good", { sub: "custom" }),
    row("js_error", 21, 14, { pct: 5 }, "bad", { sub: "auto" }),
    row("share_link", 9, 9, { pct: -20 }, "bad", { sub: "custom" }),
  ],
  // By the link's host.
  outbound: [
    row("github.com", 271, 208, { pct: 12 }, "good"),
    row("console.example.com", 140, 101, { pct: 31 }, "good"),
    row("ipverse.net", 40, 33, { new: true }, "good"),
  ],
  // Only visits with a referrer: "direct" is no row.
  referrers: [
    row("github.com", 690, null, { pct: 12 }, "good"),
    row("google.com", 402, null, { pct: -6 }, "bad"),
    row("news.ycombinator.com", 188, null, { new: true }, "good"),
    row("duckduckgo.com", 96, null, { pct: 9 }, "good"),
    row("console.example.com", 71, null, { pct: -14 }, "bad"),
    row("bing.com", 22, null, { pct: 1 }, "flat"),
    row("reddit.com", 12, null, { new: true }, "good"),
  ],
  // By AS number: its name as label, its network kind as sub.
  networks: [
    row("64500", 410, null, { pct: 6 }, "good", {
      label: "AS64500 Example Broadband",
      sub: "isp",
    }),
    row("64501", 330, null, { pct: -3 }, "bad", {
      label: "AS64501 ExampleNet Fiber",
      sub: "isp",
    }),
    row("64502", 260, null, { pct: 22 }, "good", {
      label: "AS64502 Example Mobile",
      sub: "isp",
    }),
    row("64503", 140, null, { pct: 41 }, "good", {
      label: "AS64503 Example University",
      sub: "education",
    }),
    row("64504", 90, null, { pct: 0 }, "flat", {
      label: "AS64504 Example Telecom",
      sub: "isp",
    }),
    row("64505", 61, null, { new: true }, "good", {
      label: "AS64505 Example Cable",
      sub: "isp",
    }),
    row("64510", 30, null, { pct: 2 }, "flat", {
      label: "AS64510 Example Cloud",
      sub: "hosting",
    }),
  ],
  // A method and template, its most frequent status as sub.
  probed: [
    row("GET /api/v1/ip/{addr}", 2340, 2, { pct: 640 }, "bad", {
      gen: "API walked in order",
      sub: "401",
    }),
    row("GET /.env", 96, 9, { new: true }, "bad", {
      gen: "looking for leaked secrets",
      sub: "404",
    }),
    row("GET /wp-login.php", 71, 7, { pct: 12 }, "bad", {
      gen: "WordPress login scan",
      sub: "404",
    }),
    row("GET /.git/config", 45, 5, { new: true }, "bad", {
      gen: null,
      sub: "403",
    }),
    row("GET /admin", 31, 4, { pct: -30 }, "good", {
      gen: "admin panel guess",
      sub: "404",
    }),
  ],
  refused: [
    row("198.51.100.7", 2338, null, { pct: 640 }, "bad", {
      sub: "AS64510 Example Cloud",
    }),
    row("198.51.100.201", 301, null, { new: true }, "bad", {
      sub: "AS64511 Example Hosting",
    }),
    row("192.0.2.66", 14, null, { pct: -8 }, "good", {
      sub: "AS15169 Google LLC",
    }),
    row("2001:db8:4f2:1a00::/64", 2, null, { pct: 0 }, "flat", {
      sub: "AS64501 ExampleNet Fiber",
    }),
  ],
  // A template, with the operation after it (and as sub).
  endpoints: [
    row("POST /mcp whois", 1840, 41, { pct: 12 }, "good", {
      sub: "whois",
      failed: 2.4,
      p50: 18,
      p95: 96,
    }),
    row("POST /mcp search", 610, 23, { pct: -4 }, "bad", {
      sub: "search",
      failed: 11.1,
      p50: 210,
      p95: 1480,
    }),
    row("POST /api/v1/whois/batch", 96, 4, { new: true }, "good", {
      failed: 0,
      p50: 130,
      p95: 620,
    }),
  ],
  accounts: [
    row("a91k2m4p", 1620, 6, { pct: 8 }, "good", { failed: 1.2 }),
    row("b03x7q1z", 730, 3, { pct: 40 }, "good", { failed: 14.5 }),
  ],
  agents: [
    row("lens-cli/2.0.1", 1480, 8, { pct: 6 }, "good", { failed: 3.1 }),
    row("python-httpx/0.28.1", 870, 5, { new: true }, "good", { failed: 9.4 }),
  ],
  // The product's code, or the status where it says it all.
  failures: [
    row("HTTP 429", 61, 2, { pct: 20 }, "bad", {}),
    row("invalid_params", 44, 5, { new: true }, "bad", {}),
    row("HTTP 401", 12, 7, { pct: 0 }, "flat", {}),
  ],
  errors: [
    // A script error by its message, a server one by its request.
    row('TypeError: cannot read "prefixes"', 84, 14, { new: true }, "bad", {
      sub: "js",
    }),
    row("GET /api/v1/search", 37, 21, { pct: 12 }, "bad", {
      sub: "server 500",
    }),
    row("Unhandled rejection: AbortError", 11, 9, { pct: 0 }, "flat", {
      sub: "js",
    }),
  ],
  countries: [
    ["CH", 720],
    ["DE", 610],
    ["US", 560],
    ["NL", 240],
    ["FR", 190],
    ["GB", 120],
    ["CA", 90],
    ["JP", 70],
    ["AU", 60],
    ["IN", 55],
    ["ES", 52],
    ["IT", 48],
    ["SE", 45],
    ["BR", 40],
    ["PL", 38],
  ].map(([cc, v], i) =>
    row(
      cc as string,
      v as number,
      null,
      { pct: 5 - i },
      i < 5 ? "good" : "bad",
      {
        label: cc === "CH" ? "Switzerland" : (cc as string),
        level: i < 3 ? 4 : i < 6 ? 3 : i < 10 ? 2 : 1,
      },
    ),
  ),
};

const WITHIN: Record<string, string[]> = {
  "/AS:n": [
    "/AS15169",
    "/AS13335",
    "/AS64500",
    "/AS3320",
    "/AS8075",
    "/AS16509",
    "/AS32934",
  ],
  "/:ipv4": ["/8.8.8.8", "/1.1.1.1", "/203.0.113.9", "/198.51.100.7"],
  "/:text": ["/google", "/cloudflare", "/swisscom"],
  "/:cidr": ["/192.0.2.0/24", "/2001:db8::/32"],
  "/:ipv6": ["/2001:db8::1"],
};

const SERVER_DIMS: Dim[] = [
  "probed",
  "refused",
  "endpoints",
  "accounts",
  "failures",
  "agents",
];

function list(q: URLSearchParams, feed: boolean): List | null {
  const dim = q.get("dim") as Dim;
  const server = SERVER_DIMS.includes(dim);
  if (server && !feed) return null;
  const limit = Number(q.get("limit") || 6);
  const term = (q.get("q") || "").toLowerCase();
  const within = q.get("within");
  let rows: ListRow[];
  if (within) {
    // A template's raw paths carry their own entry share too.
    rows = (WITHIN[within] ?? []).map((p, i) =>
      row(
        p,
        Math.round(300 / (i + 1)),
        Math.round(700 / (i + 1)),
        { pct: 10 - 3 * i },
        i < 3 ? "good" : "bad",
        dim === "pages" || dim === "entry"
          ? { entry: round1(12 / (i + 1)) }
          : {},
      ),
    );
  } else rows = [...(LISTS[dim] ?? [])];
  if (dim === "errors" && !feed) rows = rows.filter((r) => r.sub === "js");
  if (q.get("sub")) rows = rows.filter((r) => r.sub === q.get("sub"));
  // Shares are of every row the query matched, before the search narrows
  // them, as the backend sums them.
  const total = sum(rows.map((r) => r.value));
  // The search matches key and label, not sub.
  if (term)
    rows = rows.filter((r) =>
      `${r.key} ${r.label}`.toLowerCase().includes(term),
    );
  if (q.get("sort") === "change") {
    const mag = (r: ListRow) =>
      !r.change
        ? -1
        : "new" in r.change
          ? 1e9
          : Math.abs("pct" in r.change ? r.change.pct : r.change.pp);
    rows.sort((a, b) => mag(b) - mag(a));
  }
  rows = rows.map((r) => ({
    ...r,
    share: Math.round((r.value / (total || 1)) * 100),
  }));
  const shown = rows.slice(0, limit);
  const rest = rows.slice(limit);
  return {
    rows: shown,
    other: { count: rest.length, value: sum(rest.map((r) => r.value)) },
    total: rows.length,
    server,
    // A template's paths only, as the backend answers.
    ...(within ? { detail_since: detailSince(q.get("range")) } : {}),
  };
}

const VISITORS: VisitorRow[] = [
  {
    key: "198.51.100.7",
    display: "198.51.100.7",
    network: "AS64510 Example Cloud",
    network_kind: "hosting",
    country: "US",
    class: "unknown",
    client: "python-requests/2.32.3",
    client_gen: "Python scraping script",
    visits: null,
    views: 0,
    requests: 2340,
    refused: 2338,
    refused_share: 99.9,
    events: 0,
    last: "2026-11-22T13:19:42Z",
    // The classifier's reasons, verbatim.
    signals: [
      "client python-requests/2.32.3",
      "mostly refused",
      "API only, no page loaded",
      "rapid requests",
      "hosting network",
    ],
    does: {
      gen: "Walks the IP lookup API in address order with an invalid key: a scraping attempt.",
      text: "Unknown automated, mostly GET /api/v1/ip/{addr} → 401 (2338×)",
    },
  },
  {
    key: "203.0.113.42",
    display: "203.0.113.42",
    network: "AS64500 Example Broadband",
    network_kind: "isp",
    country: "CH",
    class: "human",
    client: joinSep(["Chrome", "macOS"]),
    client_gen: null,
    visits: 14,
    views: 61,
    requests: 318,
    refused: 2,
    refused_share: 0.6,
    events: 9,
    last: "2026-11-22T13:12:08Z",
    signals: [
      "script beacon",
      "engaged",
      "assets loaded",
      "also unknown automated: curl/8.4.0, 12 of 330 events (client curl/8.4.0)",
    ],
    does: {
      gen: "Looked up one AS after a search, then left for the GitHub data repository.",
      text: "Person, mostly view /AS:n (40×)",
    },
  },
  {
    key: "192.0.2.66",
    display: "192.0.2.66",
    network: "AS15169 Google LLC",
    network_kind: "business",
    country: "US",
    class: "bot",
    client: "Googlebot/2.1",
    client_gen: null,
    // Its page views came with the script: visits are counted (null only
    // for an address without browser events).
    visits: 64,
    views: 212,
    requests: 1904,
    refused: 14,
    refused_share: 0.7,
    events: 0,
    last: "2026-11-22T13:17:20Z",
    signals: ["Googlebot address range"],
    does: { gen: null, text: "Known bot, mostly view /AS:n (1520×)" },
  },
  {
    key: "2001:db8:4f2:1a00::/64",
    display: "2001:db8:4f2:1a00::/64",
    network: "AS64501 ExampleNet Fiber",
    network_kind: "isp",
    country: "DE",
    class: "human",
    client: joinSep(["Firefox", "Windows"]),
    client_gen: null,
    visits: 6,
    views: 22,
    requests: 141,
    refused: 0,
    refused_share: 0,
    events: 4,
    last: "2026-11-22T12:58:40Z",
    signals: ["script beacon", "engaged", "assets loaded"],
    does: {
      gen: "Checked an IPv6 address, then went on to the console.",
      text: "Person, mostly view /:ipv6 (12×)",
    },
  },
  {
    key: "192.0.2.120",
    display: "192.0.2.120",
    network: "AS64512 Example Monitoring",
    network_kind: "hosting",
    country: "US",
    class: "monitor",
    client: "UptimeRobot/2.0",
    client_gen: null,
    visits: null,
    views: 0,
    requests: 1440,
    refused: 0,
    refused_share: 0,
    events: 0,
    last: "2026-11-22T13:20:00Z",
    signals: ["UptimeRobot address range"],
    does: {
      gen: "Uptime check of the front page, once a minute.",
      text: "Uptime probe, mostly HEAD / → 200 (1440×)",
    },
  },
  {
    key: "198.51.100.30",
    display: "198.51.100.30",
    network: "AS64511 Example Hosting",
    network_kind: "hosting",
    country: "NL",
    class: "api",
    client: "example-agent/0.14.8",
    client_gen: null,
    visits: null,
    views: 0,
    requests: 40,
    refused: 0,
    refused_share: 0,
    events: 0,
    last: "2026-11-22T13:18:02Z",
    signals: ["signed-in API client"],
    does: { gen: null, text: "API client, mostly POST /mcp → 200 (40×)" },
  },
  {
    key: "198.51.100.201",
    display: "198.51.100.201",
    network: "AS64511 Example Hosting",
    network_kind: "hosting",
    country: "NL",
    class: "unknown",
    client: "HeadlessChrome/141",
    client_gen: null,
    visits: 3,
    views: 97,
    requests: 512,
    refused: 301,
    refused_share: 58.8,
    events: 0,
    last: "2026-11-22T11:41:09Z",
    signals: ["navigator.webdriver set", "hosting network"],
    does: {
      gen: "Headless Chrome stepping through AS pages one per second: a scraper.",
      text: "Unknown automated, mostly view /AS:n (97×)",
    },
  },
  {
    key: "203.0.113.9",
    display: "203.0.113.9",
    network: "AS64500 Example Broadband",
    network_kind: "isp",
    country: "CH",
    class: "human",
    client: joinSep(["Safari", "iOS"]),
    client_gen: null,
    visits: 2,
    views: 5,
    requests: 44,
    refused: 0,
    refused_share: 0,
    events: 1,
    last: "2026-11-22T08:30:11Z",
    signals: ["script beacon", "assets loaded"],
    does: {
      gen: "Looked up a single address on a phone.",
      text: "Person, mostly view /:ipv4 (3×)",
    },
  },
  ...Array.from({ length: 9 }, (_, i): VisitorRow => ({
    key: `192.0.2.${10 + i}`,
    display: `192.0.2.${10 + i}`,
    network: "AS64502 Example Mobile",
    network_kind: "isp",
    country: "AT",
    class: "human",
    client: joinSep(["Chrome", "Android"]),
    client_gen: null,
    visits: 1,
    views: 3 - (i % 3),
    requests: 30 - i,
    refused: 0,
    refused_share: 0,
    events: i % 2,
    last: "2026-11-21T18:00:00Z",
    signals: ["script beacon"],
    does: { gen: null, text: `Person, mostly view /AS:n (${3 - (i % 3)}×)` },
  })),
];

function visitors(q: URLSearchParams, feed: boolean): Visitors {
  let rows = VISITORS.map((v) =>
    feed ? v : { ...v, requests: null, refused: null, refused_share: null },
  ).filter((v) => feed || v.views > 0);
  const cls = q.get("class");
  if (cls) rows = rows.filter((v) => v.class === cls);
  const aud = q.get("aud");
  if (aud === "humans") rows = rows.filter((v) => v.class === "human");
  if (aud === "bots") rows = rows.filter((v) => v.class !== "human");
  // Any run of spaces matches any other, en spaces included, as the
  // backend folds them.
  const fold = (s: string) => s.toLowerCase().trim().replace(/\s+/g, " ");
  const term = fold(q.get("q") || "");
  if (term)
    rows = rows.filter((v) =>
      fold(`${v.display} ${v.network} ${v.client}`).includes(term),
    );
  const sort = q.get("sort") || "requests";
  const key = (v: VisitorRow) =>
    sort === "refused"
      ? (v.refused ?? 0)
      : sort === "events"
        ? v.events
        : sort === "views"
          ? v.views
          : (v.requests ?? 0);
  rows.sort((a, b) => key(b) - key(a));
  const limit = Number(q.get("limit") || 7);
  // The backend's keyset cursor: base64url JSON of the range, the last
  // row's sort value and key.
  const cursorOf = (v: VisitorRow) =>
    btoa(
      JSON.stringify({
        f: 1794783600000,
        t: 1795353600000,
        v: key(v),
        k: v.key,
      }),
    )
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");
  const cursor = q.get("cursor") || "";
  const from = cursor ? rows.findIndex((v) => cursorOf(v) === cursor) + 1 : 0;
  const page = rows.slice(from, from + limit);
  return {
    rows: page,
    total: rows.length,
    next: from + limit < rows.length ? cursorOf(page[page.length - 1]) : "",
    from: Date.parse(FROM[q.get("range") || "7d"]),
    detail_since: detailSince(q.get("range")),
  };
}

// cursor: TRAIL_NEXT asks for the person's second trail page.
function visitor(key: string, feed: boolean, cursor: string): Visitor | null {
  const v = VISITORS.find((x) => x.key === key);
  if (!v) return null;
  const later = cursor === TRAIL_NEXT;
  const r = rng(key.length * 31);
  const raw = Array.from({ length: 24 }, (_, h) => {
    if (v.class === "monitor") return 3;
    if (v.class === "bot") return 1 + Math.floor(r() * 3);
    if (v.class === "unknown")
      return h === 11 ? 2 : h === 12 || h === 14 ? 4 : 0;
    return h >= 8 && h <= 22 && r() > 0.62 ? 1 + Math.floor(r() * 3) : 0;
  });
  // Levels 0-4, the busiest hour 4, as the backend scales them.
  const top = Math.max(...raw);
  const hours = raw.map((x) => (top ? Math.round((x * 4) / top) : 0));
  const scrape = v.class === "unknown";
  return {
    key: v.key,
    class: v.class,
    network: v.network,
    network_kind: v.network_kind,
    country: v.country,
    client: v.client,
    client_gen: v.client_gen ?? null,
    signals: v.signals,
    does: v.does,
    requests: feed ? v.requests : null,
    addresses: v.key.endsWith("/64") ? 3 : 1,
    reading:
      v.key === "203.0.113.9" || v.class === "api"
        ? null
        : {
            // One of the backend's fixed verdicts.
            verdict: scrape
              ? "Scraping attempt"
              : v.class === "bot"
                ? "Search crawler"
                : v.class === "monitor"
                  ? "Uptime check"
                  : "Person",
            suspicious: scrape,
            evidence: scrape
              ? [
                  "Looks up addresses in sequence, 0.8 s apart",
                  "Every request refused with 401",
                  "Never loaded the page it started from",
                ]
              : ["Came from GitHub, searched once", "Read one AS page to 90 %"],
            related: scrape ? ["198.51.100.201"] : [],
            basis: { from: "2026-11-22T10:02:17Z", to: "2026-11-22T13:19:42Z" },
          },
    hours,
    trail: scrape
      ? [
          {
            t: "2026-11-22T13:19:42Z",
            kind: "request",
            what: "GET /api/v1/ip/{addr} → 401",
            count: 2338,
            from: "2026-11-22T10:02:17Z",
            to: "2026-11-22T13:19:42Z",
            interval_s: 0.8,
            continues: false,
            continued: false,
          },
          single("2026-11-22T10:02:17Z", "request", "GET / → 200"),
        ]
      : v.class === "api"
        ? [
            {
              t: "2026-11-22T13:18:02Z",
              kind: "request",
              what: "POST /mcp → 200",
              count: 40,
              from: "2026-11-22T13:05:41Z",
              to: "2026-11-22T13:18:02Z",
              interval_s: 19,
              continues: false,
              continued: false,
            },
          ]
        : PERSON_TRAIL[later ? 1 : 0],
    // The backend's keyset cursor, "<ms>.<row id>".
    trail_next: scrape || v.class === "api" || later ? "" : TRAIL_NEXT,
    from: Date.parse(FROM["7d"]),
    detail_since: DETAIL_SINCE,
  };
}

export const TRAIL_NEXT = "1795352951000.48177";

// A trail line of one event: its span is its own moment.
const single = (t: string, kind: string, what: string): TrailItem => ({
  t,
  kind,
  what,
  count: 1,
  from: t,
  to: t,
  interval_s: 0,
  continues: false,
  continued: false,
});

// A person's trail in two pages, a run of views split by the page bound:
// the first page's last line continues, the second's first continued.
const PERSON_TRAIL: TrailItem[][] = [
  [
    single("2026-11-22T13:12:09Z", "vital", "vital CLS 0.042"),
    single(
      "2026-11-22T13:12:08Z",
      "outbound_click",
      "outbound_click github.com",
    ),
    single("2026-11-22T13:11:31Z", "engagement", "engagement"),
    single("2026-11-22T13:10:55Z", "page_view", "view /AS:n"),
    {
      t: "2026-11-22T13:10:12Z",
      kind: "page_view",
      what: "view /:text",
      count: 3,
      from: "2026-11-22T13:09:52Z",
      to: "2026-11-22T13:10:12Z",
      interval_s: 10,
      continues: true,
      continued: false,
    },
  ],
  [
    {
      t: "2026-11-22T13:09:41Z",
      kind: "page_view",
      what: "view /:text",
      count: 2,
      from: "2026-11-22T13:09:21Z",
      to: "2026-11-22T13:09:41Z",
      interval_s: 20,
      continues: false,
      continued: true,
    },
    single("2026-11-22T13:09:02Z", "session_start", "session_start"),
  ],
];

const LOG_CLIENTS: [string, ClassKey, string, string, LogRow["source"]][] = [
  // what as the backend describes an event: a request by its template.
  ["192.0.2.120", "monitor", "request", "HEAD / → 200", "server"],
  [
    "198.51.100.7",
    "unknown",
    "request",
    "GET /api/v1/ip/{addr} → 401",
    "server",
  ],
  ["192.0.2.66", "bot", "page_view", "view /AS:n", "browser"],
  [
    "203.0.113.42",
    "human",
    "outbound_click",
    "outbound_click github.com",
    "browser",
  ],
  ["203.0.113.42", "human", "vital", "vital LCP 2.1 s", "browser"],
  ["203.0.113.42", "human", "page_view", "view /AS:n", "browser"],
  [
    "2001:db8:4f2:1a00::7c",
    "human",
    "outbound_click",
    "outbound_click console.example.com",
    "browser",
  ],
];
// Now and then that visitor gets a script error instead.
const LOG_ERROR = ["js_error", "js_error Script error."];
// A browser as browser and OS joined by SEP, a tool by its product token.
const CLIENT_NAME: Record<string, string> = {
  "192.0.2.120": "UptimeRobot/2.0",
  "198.51.100.7": "python-requests/2.32.3",
  "192.0.2.66": "Googlebot/2.1",
  "203.0.113.42": joinSep(["Chrome", "macOS"]),
  "2001:db8:4f2:1a00::7c": joinSep(["Firefox", "Windows"]),
};

// Each address's AS number, for the log's asn filter.
const LOG_ASN: Record<string, number> = {
  "192.0.2.120": 64512,
  "198.51.100.7": 64510,
  "192.0.2.66": 15169,
  "203.0.113.42": 64500,
  "2001:db8:4f2:1a00::7c": 64501,
};

// The backend's log page and keyset cursor ("<ms>.<row id>").
const LOG_PAGE = 100;
const LOG_ROWS = 250;

function log(q: URLSearchParams, feed: boolean, script: boolean): Log | null {
  let rows: LogRow[] = Array.from({ length: LOG_ROWS }, (_, i) => {
    const [addr, cls, k, w, source] = LOG_CLIENTS[i % LOG_CLIENTS.length];
    const [kind, what] =
      i % (10 * LOG_CLIENTS.length) === 10 ? LOG_ERROR : [k, w];
    return {
      t: new Date(NOW.getTime() - i * 47_000).toISOString(),
      addr,
      class: cls,
      kind,
      what,
      client: CLIENT_NAME[addr],
      source,
      account: i % (2 * LOG_CLIENTS.length) === 1 ? "a91k2m4p" : "",
      dur_ms: source === "server" ? 3 + (i % 90) : null,
      error:
        kind === "js_error"
          ? { origin: "withheld", host: "", place: "" }
          : null,
      page: kind === "js_error" ? "/AS13335" : "",
    };
  });
  if (!feed) rows = rows.filter((r) => r.source === "browser");
  if (!script) rows = rows.filter((r) => r.source === "server");
  const addr = q.get("addr");
  // An address or a /64; anything else is refused.
  if (addr && /\/(?!64$)/.test(addr)) return null;
  if (addr)
    rows = rows.filter((r) => r.addr.startsWith(addr.replace(/:?:?\/64$/, "")));
  // An AS number, with or without its "AS"; anything else is refused.
  const asn = q.get("asn");
  if (asn) {
    const n = /^(?:AS)?(\d+)$/i.exec(asn.trim());
    if (!n) return null;
    rows = rows.filter((r) => LOG_ASN[r.addr] === Number(n[1]));
  }
  const path = q.get("path");
  if (path) rows = rows.filter((r) => r.what.includes(path));
  const cls = q.get("class");
  if (cls) rows = rows.filter((r) => r.class === cls);
  // kind: an event kind, or a whole source.
  const kind = q.get("kind");
  if (kind)
    rows = rows.filter((r) =>
      kind === "browser" || kind === "server"
        ? r.source === kind
        : r.kind === kind,
    );
  const status = q.get("status");
  if (status) rows = rows.filter((r) => r.what.includes(`→ ${status[0]}`));
  // Exact filters.
  const name = q.get("name");
  if (name) rows = rows.filter((r) => r.what === `${r.kind} ${name}`);
  const account = q.get("account");
  if (account) rows = rows.filter((r) => r.account === account);
  const endpoint = q.get("endpoint");
  if (endpoint)
    rows = rows.filter(
      (r) => r.kind === "request" && r.what.startsWith(`${endpoint} → `),
    );
  const failure = q.get("failure");
  if (failure)
    rows = rows.filter(
      (r) =>
        r.what.endsWith(`→ ${failure.replace("HTTP ", "")}`) ||
        r.what.endsWith(`, ${failure}`),
    );
  const op = q.get("op");
  if (op) rows = rows.filter((r) => r.what.includes(` ${op} → `));
  // Row i's id counts down from the newest.
  const id = (r: LogRow) => `${Date.parse(r.t)}.${48211 - rows.indexOf(r)}`;
  const cursor = q.get("cursor") || "";
  if (cursor && !/^\d+\.\d+$/.test(cursor)) return null;
  const from = cursor ? rows.findIndex((r) => id(r) === cursor) + 1 : 0;
  const page = rows.slice(from, from + LOG_PAGE);
  return {
    rows: page,
    total: rows.length,
    next:
      page.length === LOG_PAGE && from + LOG_PAGE < rows.length
        ? id(page[page.length - 1])
        : "",
    from: Date.parse(FROM[q.get("range") || "7d"]),
    detail_since: detailSince(q.get("range")),
  };
}

// One script error: a page's own by its place, "Script error." without one.
function errorDetail(name: string, range: string): ErrorDetail {
  const withheld = name === "Script error.";
  const none: ErrorWhere = { origin: "withheld", host: "", place: "" };
  const own = (place: string): ErrorWhere => ({
    origin: "own",
    host: "",
    place,
  });
  const of = (...rows: ErrorCount[]): ErrorBreakdown => ({ rows, more: 0 });
  const at = withheld ? none : own("/assets/index-C4v1rT2s.js:1:48113");
  return {
    count: 84,
    visitors: 14,
    sessions: 16,
    viewers: 1310,
    share: 1.1,
    first: new Date(NOW.getTime() - 30 * 3600_000).toISOString(),
    last: new Date(NOW.getTime() - 600_000).toISOString(),
    // Another host's is in the two places cut.
    origins: withheld ? ["withheld"] : ["own", "other", "extension"],
    places: withheld
      ? of({ key: "withheld ", where: none, count: 84, visitors: 14 })
      : {
          rows: [
            { key: "own a", where: at, count: 74, visitors: 12 },
            {
              key: "extension",
              where: {
                origin: "extension",
                host: "",
                place: "chrome-extension://abc/content.js:4:1",
              },
              count: 4,
              visitors: 1,
            },
          ],
          more: 2,
        },
    pages: of(
      { key: "/AS:n", count: 70, visitors: 12 },
      { key: "/:ipv4", count: 14, visitors: 3 },
    ),
    clients: of({
      key: joinSep(["Firefox", "iOS"]),
      count: 84,
      visitors: 14,
    }),
    classes: of({ key: "human", count: 84, visitors: 14 }),
    recent: [
      {
        t: new Date(NOW.getTime() - 600_000).toISOString(),
        addr: "203.0.113.42",
        key: "203.0.113.42",
        class: "human",
        client: joinSep(["Firefox", "iOS"]),
        page: "/AS13335",
        where: at,
      },
    ],
    type: withheld ? "" : "TypeError",
    stack: withheld
      ? []
      : [
          "at Lookup (https://lens.example.com/assets/index-C4v1rT2s.js:1:48113)",
          "at render (https://lens.example.com/assets/index-C4v1rT2s.js:1:9021)",
        ],
    detail_since: detailSince(range),
  };
}

function domain(site: string, feed: boolean, script: boolean): Domain {
  return {
    counters: {
      accepted: 48211,
      rate_limited: 312,
      rejected: 27,
      dropped: 0,
      unknown_domain: 1904,
    },
    sources: {
      // null: the script never delivered.
      browser: script
        ? { state: "receiving", last: "2026-11-22T13:12:00Z" }
        : null,
      server: feed
        ? { state: "receiving", last: "2026-11-22T13:20:00Z" }
        : null,
      feed: FEEDS.includes(site),
      script: "/trakk.js",
      build: "3f2a91c",
      first: FIGURES_SINCE,
    },
    storage: {
      events: feed ? 1284551 : 398211,
      oldest: DETAIL_SINCE,
      raw_days: 90,
      purge_from: null,
      figures_since: FIGURES_SINCE,
      bytes: feed ? 432013312 : 134217728,
      retention_months: 25,
      identity: "address + user agent, plus browser id",
    },
    rules: {
      rate: joinSep(["60 events / min per address or /64", "600 per /48"]),
      batch: joinSep(["32 KiB", "50 events"]),
      botlist: { services: 24, updated: "2026-11-22T03:00:00Z" },
      networks: { updated: "2026-11-22T03:00:00Z" },
      tz: "Europe/Zurich",
      llm: { calls_today: 41, budget: 200, configured: true },
    },
  };
}

const ago = (min: number) =>
  new Date(NOW.getTime() - min * 60_000).toISOString();

// Where alerts go: three channels by severity, the pager's token gone bad.
// The mail's last error is older than its last delivery: no warning.
function notify(state: FixtureOptions["notify"]): Notify | undefined {
  if (state === "missing") return undefined;
  if (state === "unconfigured")
    return {
      configured: false,
      enabled: false,
      faults: [],
      rules: { active: 0, off: 0 },
      channels: [],
    };
  if (state === "bare")
    return {
      configured: true,
      enabled: true,
      faults: [],
      rules: { active: 3, off: 0 },
      channels: [],
    };
  return {
    configured: true,
    enabled: state !== "off",
    faults: ['rule "api-latency": unknown channel "sms", the rule is off'],
    rules: { active: 9, off: 1 },
    channels: [
      {
        name: "ops",
        service: "ntfy",
        min_severity: "low",
        last_ok: ago(4),
        last_error: null,
      },
      {
        name: "mail",
        service: "email",
        min_severity: "normal",
        last_ok: ago(37),
        last_error: { at: ago(300), text: "SMTP 421, try again later" },
      },
      {
        name: "pager",
        service: "pushover",
        min_severity: "high",
        last_ok: ago(2 * 24 * 60),
        last_error: {
          at: ago(37),
          text: "HTTP 401: invalid application token",
        },
      },
    ],
  };
}

type Sent = Alert["delivery"];
const OPS = { name: "ops", ok: true };
const MAIL = { name: "mail", ok: true };
const sent = (min: number, ...channels: Sent["channels"]): Sent => ({
  state: channels.every((c) => c.ok)
    ? "sent"
    : channels.some((c) => c.ok)
      ? "partial"
      : "failed",
  at: ago(min),
  channels,
});
const NONE: Sent = { state: "none", at: null, channels: [] };

// An alert raised min minutes ago and still so then (last), cleared since
// when cleared is set.
function alert(
  id: number,
  site: string,
  kind: string,
  severity: Alert["severity"],
  min: number,
  more: Partial<Alert> & { last?: number; cleared?: number },
): Alert {
  const { last, cleared, ...rest } = more;
  return {
    id,
    site,
    rule: kind.replace(".", "-"),
    kind,
    key: "",
    severity,
    state: cleared != null ? "cleared" : "once",
    text: "",
    detail: "",
    figure: null,
    unit: "",
    first_at: ago(min),
    last_at: ago(last ?? min),
    cleared_at: cleared != null ? ago(cleared) : null,
    delivery: sent(min - 1, OPS),
    target: null,
    ...rest,
  };
}

// The history, newest first: every kind, state and delivery; the instance's
// own under site ""; blog.example.org has none. lens.example.com has more
// than a page.
const ALERTS: Alert[] = [
  alert(412, "lens.example.com", "error.new", "high", 38, {
    key: 'TypeError: cannot read "prefixes"',
    state: "open",
    last: 4,
    text: 'New error: TypeError: cannot read "prefixes"',
    detail: "Safari on /AS:n pages, since build 3f2a91c",
    figure: 14,
    unit: "times",
    delivery: sent(37, OPS, MAIL, { name: "pager", ok: false }),
    target: { panel: "errors", filter: {} },
  }),
  alert(411, "lens.example.com", "spike.addr", "normal", 52, {
    key: "198.51.100.7",
    state: "open",
    last: 6,
    text: "198.51.100.7 (AS64510 Example Cloud)",
    detail: "88.4 % of all requests, 0 in the prior 7 d",
    figure: 2338,
    unit: "requests",
    delivery: sent(51, OPS, MAIL),
    target: { panel: "visitors", filter: { key: "198.51.100.7" } },
  }),
  alert(410, "", "collector.unknown_domain", "normal", 75, {
    state: "open",
    last: 9,
    text: "Events for no configured site, or with a wrong feed key",
    detail: "most name staging.example.com",
    figure: 1904,
    unit: "events",
    delivery: sent(74, { name: "ops", ok: false }, { name: "mail", ok: false }),
    target: { panel: "domain", filter: {} },
  }),
  alert(409, "api.example.com", "level.failed", "high", 96, {
    state: "open",
    text: "Failed calls over 5 % in the last hour",
    figure: 11.1,
    unit: "%",
    delivery: { state: "pending", at: null, channels: [] },
    target: { panel: "failures", filter: {} },
  }),
  alert(408, "lens.example.com", "spike.net", "normal", 180, {
    last: 95,
    cleared: 60,
    text: "AS64510 Example Cloud",
    detail: "88.5 % of all requests, 0 in the prior 7 d",
    figure: 2340,
    unit: "requests",
    delivery: sent(179, OPS, MAIL),
    target: { panel: "log", filter: { kind: "request", asn: "64510" } },
  }),
  alert(407, "lens.example.com", "probe.new", "low", 240, {
    key: "GET /.env",
    text: "New probed path: GET /.env",
    figure: 212,
    unit: "probes",
    target: { panel: "probed", filter: {} },
  }),
  alert(406, "console.example.com", "source.silent", "high", 420, {
    key: "browser",
    last: 400,
    cleared: 375,
    text: "No browser events for 45 minutes",
    figure: 45,
    unit: "minutes",
    delivery: sent(419, OPS, MAIL, { name: "pager", ok: false }),
    target: { panel: "domain", filter: {} },
  }),
  alert(405, "", "job.failed", "high", 610, {
    key: "aggregate",
    text: "The daily aggregate build failed",
    detail: "database is locked",
    delivery: sent(609, OPS, MAIL, { name: "pager", ok: true }),
  }),
  alert(404, "lens.example.com", "referrer.new", "low", 900, {
    key: "news.ycombinator.com",
    text: "New referrer: news.ycombinator.com",
    figure: 749,
    unit: "visitors",
    delivery: NONE,
    target: { panel: "referrers", filter: {} },
  }),
  alert(403, "lens.example.com", "trend.visitors", "low", 1300, {
    text: "Visitors up against the prior 7 d",
    detail: "2841, was 2291",
    figure: 24,
    unit: "%",
  }),
  alert(402, "", "llm.budget", "normal", 1500, {
    last: 1500,
    cleared: 1200,
    text: "The model's budget for today is used up",
    detail: "computed texts only until midnight",
    figure: 200,
    unit: "calls",
    delivery: sent(1499, OPS, MAIL),
    target: { panel: "domain", filter: {} },
  }),
  alert(401, "", "db.size_cap", "normal", 1700, {
    state: "open",
    text: "The database is near its size cap",
    detail: "412 MB of 448 MB",
    figure: 92,
    unit: "%",
    delivery: sent(1699, OPS, MAIL),
  }),
  alert(400, "api.example.com", "endpoint.new", "low", 1900, {
    key: "POST /api/v1/whois/batch",
    text: "New endpoint: POST /api/v1/whois/batch",
    figure: 96,
    unit: "calls",
    target: { panel: "endpoints", filter: {} },
  }),
  alert(399, "api.example.com", "failure.new", "normal", 2000, {
    key: "invalid_params",
    text: "New failure reason: invalid_params",
    figure: 44,
    unit: "calls",
    delivery: sent(1999, OPS, MAIL),
    target: { panel: "log", filter: { failure: "invalid_params" } },
  }),
  alert(398, "api.example.com", "trend.calls", "low", 2100, {
    text: "Calls up against the prior 7 d",
    figure: 12,
    unit: "%",
    delivery: NONE,
  }),
  alert(397, "api.example.com", "trend.failed", "normal", 2300, {
    last: 2250,
    cleared: 2200,
    text: "Failed calls up against the prior 7 d",
    figure: 3.2,
    unit: "pp",
    delivery: sent(2299, OPS, MAIL),
  }),
  alert(396, "lens.example.com", "trend.auto_share", "normal", 2500, {
    last: 2400,
    cleared: 2350,
    text: "Unknown automated share up against the prior 7 d",
    figure: 31.4,
    unit: "%",
    delivery: sent(2499, OPS, MAIL),
  }),
  alert(395, "", "collector.refused", "normal", 2700, {
    last: 2690,
    cleared: 2680,
    text: "Events refused by the rate limit",
    figure: 312,
    unit: "events",
    delivery: sent(2699, OPS, MAIL),
    target: { panel: "domain", filter: {} },
  }),
  alert(394, "", "collector.dropped", "high", 2900, {
    last: 2890,
    cleared: 2880,
    text: "Events dropped, the queue was full",
    figure: 87,
    unit: "events",
    delivery: sent(2899, OPS, MAIL, { name: "pager", ok: true }),
    target: { panel: "domain", filter: {} },
  }),
  alert(393, "api.example.com", "level.p95", "normal", 3100, {
    last: 3000,
    cleared: 2950,
    text: "95th percentile over 1 s in the last hour",
    figure: 1480,
    unit: "ms",
    delivery: sent(3099, OPS, MAIL),
    target: { panel: "endpoints", filter: {} },
  }),
  alert(392, "lens.example.com", "level.requests", "low", 3300, {
    text: "Requests over 2000 in the last hour",
    figure: 9120,
    unit: "requests",
  }),
  alert(391, "lens.example.com", "level.visitors", "low", 3500, {
    text: "Visitors over 500 today",
    figure: 2841,
    unit: "visitors",
  }),
  // A kind this UI does not know yet: shown by its id.
  alert(390, "lens.example.com", "country.new", "low", 3700, {
    key: "IS",
    text: "New country: Iceland",
    figure: 3,
    unit: "visitors",
  }),
  // Older, routine ones: a second page.
  ...Array.from({ length: 48 }, (_, i) =>
    alert(
      389 - i,
      "lens.example.com",
      "level.requests",
      "low",
      4000 + i * 420,
      {
        last: 3990 + i * 420,
        cleared: 3960 + i * 420,
        text: "Requests over 2000 in the last hour",
        figure: 2100 + ((i * 37) % 400),
        unit: "requests",
      },
    ),
  ),
];

// The backend's page: newest first, the cursor the last id shown.
function alerts(q: URLSearchParams): Alerts | null {
  const before = q.get("before");
  if (before && !/^\d+$/.test(before)) return null;
  const limit = Math.min(Number(q.get("limit") || 50), 200);
  const site = q.get("site");
  const rows = ALERTS.filter(
    (a) => (!site || a.site === site) && (!before || a.id < Number(before)),
  );
  const page = rows.slice(0, limit);
  return {
    alerts: page,
    next: rows.length > limit ? String(page[page.length - 1].id) : null,
  };
}

const clients: Clients = {
  ip_version: [
    { key: "IPv4", value: 1950, share: 68 },
    { key: "IPv6", value: 917, share: 32 },
  ],
  browsers: [
    // Whole percents, as the backend rounds them.
    { key: "Chrome", value: 1480, share: 52 },
    { key: "Firefox", value: 620, share: 22 },
    { key: "Safari", value: 510, share: 18 },
    { key: "Edge", value: 170, share: 6 },
    { key: "Other", value: 87, share: 3 },
  ],
  os: [
    { key: "macOS", value: 980, share: 34 },
    { key: "Windows", value: 860, share: 30 },
    { key: "Linux", value: 540, share: 19 },
    { key: "iOS", value: 310, share: 11 },
    { key: "Other", value: 177, share: 6 },
  ],
  devices: [
    { key: "Desktop", value: 2290, share: 80 },
    { key: "Mobile", value: 470, share: 16 },
    { key: "Tablet", value: 107, share: 4 },
  ],
};

const json = (
  body: unknown,
  status = 200,
  headers: Record<string, string> = {},
) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });

export type FixtureOptions = {
  me?: 200 | 401 | 403;
  // What /api/auth/methods offers; both by default.
  methods?: { oidc: boolean; token: boolean };
  // The answer to a token sign-in; 204 signs in ("fixture-token" only).
  token?: 204 | 400 | 401 | 403 | 404 | 413 | 429 | 500;
  // false: a 204 whose session the browser does not keep (/api/me stays
  // 401).
  keep?: boolean;
  // Answer these paths (e.g. "/api/dash/classes") with a 500.
  fail?: string[];
  delayMs?: number;
  // Rows a full list holds past the ones it sends: a list cut at its limit.
  beyond?: number;
  // true: no model configured.
  noModel?: boolean;
  // "watch": the briefing leads with the watch's findings.
  nw?: "watch";
  // The domain's notifications: on with a fault and a failing channel by
  // default; switched off, never set up, on without channels or faults, or
  // a backend older than alerts that sends none.
  notify?: "on" | "off" | "unconfigured" | "bare" | "missing";
};

// A fetch that answers the dashboard API from the fixtures above.
export function fixtureFetch(opt: FixtureOptions = {}) {
  let me = opt.me;
  return async (input: string, init?: RequestInit): Promise<Response> => {
    if (opt.delayMs) await new Promise((r) => setTimeout(r, opt.delayMs));
    if (init?.signal?.aborted) throw new DOMException("aborted", "AbortError");
    const u = new URL(input, "http://fixture.invalid");
    const q = u.searchParams;
    if (opt.fail?.includes(u.pathname))
      return json({ detail: "fixture failure" }, 500);
    if (u.pathname === "/api/me")
      return me && me !== 200
        ? json(
            {
              detail:
                me === 401 ? "not signed in" : "not in the dashboard group",
            },
            me,
          )
        : json({ name: "dev" });
    if (u.pathname === "/api/auth/methods")
      return json(opt.methods ?? { oidc: true, token: true });
    if (u.pathname === "/api/auth/token") {
      // As the server reads it: a form post with one field.
      const body = init?.body;
      if (init?.method !== "POST" || !(body instanceof URLSearchParams))
        return json({ detail: "not a form post" }, 400);
      const st = opt.token ?? 401;
      if (st === 204 && body.get("token") === "fixture-token") {
        if (opt.keep !== false) me = 200;
        return new Response(null, { status: 204 });
      }
      if (st === 429)
        return json({ detail: "slow down" }, 429, { "Retry-After": "30" });
      return json({ detail: "refused" }, st === 204 ? 401 : st);
    }
    if (u.pathname === "/api/auth/logout")
      return new Response(null, { status: 204 });
    if (me === 401) return json({ detail: "not signed in" }, 401);
    if (me === 403) return json({ detail: "not in the dashboard group" }, 403);
    const site = q.get("site") || "";
    const feed = SITES.find((s) => s.site === site)?.server != null;
    const script = SITES.find((s) => s.site === site)?.browser != null;
    const range = q.get("range") || "7d";
    switch (u.pathname) {
      case "/api/dash/sites":
        return json({ sites: SITES });
      case "/api/dash/overview":
        return json(
          overview(range, site, q.get("aud") || "all", q.get("cmp") || "prev"),
        );
      case "/api/dash/noteworthy":
        return json(noteworthy(site, opt.nw === "watch"));
      case "/api/dash/watch":
        return json(watch(site, range));
      case "/api/dash/classes":
        return feed
          ? json(classes(range))
          : json({ detail: `${site} has no server feed` }, 404);
      case "/api/dash/status":
        return feed
          ? json({
              rows: [
                { class: "2xx", value: 9120, share: 73.1 },
                { class: "3xx", value: 610, share: 4.9 },
                { class: "4xx", value: 2710, share: 21.7 },
                { class: "5xx", value: 37, share: 0.3 },
              ],
            })
          : json({ detail: `${site} has no server feed` }, 404);
      case "/api/dash/list": {
        const l = list(q, feed);
        if (l && opt.beyond && Number(q.get("limit")) >= 500)
          l.total += opt.beyond;
        return l
          ? json(l)
          : json({ detail: `${site} has no server feed` }, 404);
      }
      case "/api/dash/clients":
        return json(clients);
      case "/api/dash/visitors":
        return json(visitors(q, feed));
      case "/api/dash/visitor": {
        const v = visitor(q.get("key") || "", feed, q.get("cursor") || "");
        return v
          ? json({ ...v, detail_since: detailSince(range) })
          : json({ detail: "no events from this address in the range" }, 404);
      }
      case "/api/dash/log": {
        const l = log(q, feed, script);
        return l ? json(l) : json({ detail: "bad log filter or cursor" }, 400);
      }
      case "/api/dash/error":
        return q.get("key")
          ? json(errorDetail(q.get("key")!, range))
          : json({ detail: "key is a script error's message" }, 400);
      case "/api/dash/domain": {
        const d = domain(site, feed, script);
        if (opt.noModel) d.rules.llm.configured = false;
        d.notify = notify(opt.notify);
        return json(d);
      }
      case "/api/dash/alerts": {
        const a = alerts(q);
        return a ? json(a) : json({ detail: "bad cursor" }, 400);
      }
    }
    return json({ detail: "no such endpoint" }, 404);
  };
}

// Synthetic answers in the dashboard API's shapes, for the tests and for the
// dev server's fixture mode (npm run dev:fixtures). Never part of the
// production bundle: main.tsx imports it only under import.meta.env.DEV.
// The numbers stand in for the backend's, so this file may compute; the
// UI never does.

import type {
  Classes,
  ClassKey,
  Clients,
  Dim,
  Domain,
  List,
  ListRow,
  Log,
  LogRow,
  Noteworthy,
  Overview,
  SiteInfo,
  Tile,
  TrailItem,
  Visitor,
  VisitorRow,
  Visitors,
} from "./api";

export const SITES: SiteInfo[] = [
  {
    site: "lens.example.com",
    product: "Example Lens",
    kind: "site",
    browser: { last: "2026-11-22T13:12:00Z" },
    server: { last: "2026-11-22T13:20:00Z" },
  },
  {
    site: "console.example.com",
    product: "Example Lens",
    kind: "site",
    browser: { last: "2026-11-22T12:58:00Z" },
    server: null,
  },
  // Nothing delivered yet, its server feed configured (FEEDS).
  {
    site: "blog.example.org",
    product: "Blog",
    kind: "site",
    browser: null,
    server: null,
  },
  // A server feed only: the script never loaded.
  {
    site: "api.example.com",
    product: "Example Lens",
    kind: "api",
    browser: null,
    server: { last: "2026-11-22T13:19:00Z" },
  },
];

// The sites with a server feed configured, delivered or not: the sites list
// shows only the delivered ones, the domain footer says it of all.
const FEEDS = ["lens.example.com", "blog.example.org", "api.example.com"];

const N: Record<string, number> = {
  "24h": 24,
  "7d": 7,
  "30d": 30,
  "90d": 90,
  "12mo": 12,
};
const NOW = new Date("2026-11-22T13:20:00Z");
const MONTHS = "Jan Feb Mar Apr May Jun Jul Aug Sep Oct Nov Dec".split(" ");

function rng(seed: number) {
  let s = seed >>> 0;
  return () => (s = (Math.imul(s, 1664525) + 1013904223) >>> 0) / 4294967296;
}

function labels(range: string): string[] {
  const n = N[range] ?? 7;
  return Array.from({ length: n }, (_, i) => {
    const back = n - 1 - i;
    const d = new Date(NOW);
    if (range === "24h") {
      d.setUTCHours(d.getUTCHours() - back);
      return `${String(d.getUTCHours()).padStart(2, "0")}:00`;
    }
    // As the backend writes them: "Jan", "02.01.", "15:00".
    if (range === "12mo") {
      d.setUTCMonth(d.getUTCMonth() - back);
      return MONTHS[d.getUTCMonth()];
    }
    d.setUTCDate(d.getUTCDate() - back);
    const two = (n: number) => String(n).padStart(2, "0");
    return `${two(d.getUTCDate())}.${two(d.getUTCMonth() + 1)}.`;
  });
}

const sum = (a: number[]) => a.reduce((x, y) => x + y, 0);
const median = (a: number[]) => {
  const s = [...a].sort((x, y) => x - y);
  const m = s.length >> 1;
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
};
const round1 = (v: number) => Math.round(v * 10) / 10;

function series(range: string, base: number, seed: number, spike = 1) {
  const n = N[range] ?? 7;
  const r = rng(seed * 7919 + n);
  const cur: number[] = [];
  const prev: number[] = [];
  const at = Math.floor(n * 0.62);
  for (let i = 0; i < n; i++) {
    const season =
      range === "24h"
        ? 0.35 + 0.9 * Math.max(0, Math.sin(((i - 6) / 24) * 2 * Math.PI))
        : range === "12mo"
          ? 1
          : [0.62, 1.05, 1.12, 1.1, 1.06, 0.98, 0.66][i % 7];
    const part = i === n - 1 ? 0.56 : 1;
    const grow = 0.86 + (0.28 * i) / n;
    cur.push(
      Math.round(
        base *
          season *
          grow *
          (0.88 + 0.24 * r()) *
          part *
          (i === at ? spike : 1),
      ),
    );
    prev.push(Math.round(base * season * 0.9 * (0.88 + 0.24 * r())));
  }
  return { cur, prev };
}

function overview(
  range: string,
  site: string,
  aud: string,
  cmp: string,
): Overview {
  const feed = SITES.find((s) => s.site === site)?.server != null;
  const scale = range === "12mo" ? 30 : range === "24h" ? 0.05 : 1;
  const k = aud === "humans" ? 0.8 : aud === "bots" ? 0.25 : 1;
  const base = 410 * scale * k;
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
    const s = series(range, b, seed, opt.spike);
    const closed = s.cur.slice(0, -1);
    const pclosed = s.prev.slice(0, -1);
    let value = sum(closed);
    let prev: number | null = sum(pclosed);
    let cur = s.cur;
    let prevSeries: number[] | null = s.prev;
    if (opt.rate) {
      cur = s.cur.map((v, i) => round1((v / (b * 10)) * 4 + (i % 3)));
      prevSeries = s.prev.map((v, i) =>
        round1((v / (b * 10)) * 4 + ((i + 1) % 3)),
      );
      value = round1(median(cur.slice(0, -1)));
      prev = round1(median(prevSeries.slice(0, -1)));
    }
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
    if (cmp === "off") prevSeries = [];
    return {
      key,
      label,
      unit: opt.rate ? "%" : "",
      kind: opt.rate ? "rate" : "sum",
      value,
      prev: cmp === "off" ? null : prev,
      change: cmp === "off" ? null : opt.rate ? { pp: diff } : { pct: diff },
      verdict: cmp === "off" ? "flat" : verdict,
      series: cur,
      prev_series: prevSeries,
      median: opt.rate ? value : Math.round(median(cur.slice(0, -1))),
      server: opt.server ?? false,
    };
  };
  const tiles: Tile[] = [
    mk("visitors", aud === "bots" ? "Addresses" : "Visitors", base, 1),
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
  const ls = labels(range);
  return {
    period: {
      from: "2026-11-15T23:00:00Z",
      to: NOW.toISOString(),
      bucket: range === "24h" ? "hour" : range === "12mo" ? "month" : "day",
      labels: ls,
      // Empty with the comparison off.
      prev_label:
        cmp === "off"
          ? ""
          : cmp === "yoy"
            ? "last year"
            : `prior ${range.replace(/(\d+)/, "$1 ")}`,
      last_bucket_open: true,
    },
    live: { humans: 7, automated: feed ? 3 : 1 },
    tiles,
  };
}

function noteworthy(site: string): Noteworthy {
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
  const shown = items
    .filter(
      (f) => feed || f.id.startsWith("err:") || f.id.startsWith("ref:reddit"),
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
];

function classes(range: string): Classes {
  const ls = labels(range);
  const scale = range === "12mo" ? 30 : range === "24h" ? 0.05 : 1;
  const s = (b: number, seed: number, spike = 1) =>
    series(range, b * scale, seed, spike).cur;
  return {
    labels: ls,
    series: {
      human: s(2500, 11),
      bot: s(1400, 12),
      monitor: s(700, 13),
      unknown: s(410, 14, 6.5),
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

function list(q: URLSearchParams, feed: boolean): List | null {
  const dim = q.get("dim") as Dim;
  if ((dim === "probed" || dim === "refused") && !feed) return null;
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
    server: dim === "probed" || dim === "refused",
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
    client: "Chrome · macOS",
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
    client: "Firefox · Windows",
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
    client: "Safari · iOS",
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
    client: "Chrome · Android",
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
  const term = (q.get("q") || "").toLowerCase();
  if (term)
    rows = rows.filter((v) =>
      `${v.display} ${v.network} ${v.client}`.toLowerCase().includes(term),
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
      v.key === "203.0.113.9"
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
      : PERSON_TRAIL[later ? 1 : 0],
    // The backend's keyset cursor, "<ms>.<row id>".
    trail_next: scrape || later ? "" : TRAIL_NEXT,
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
  ["203.0.113.42", "human", "engagement", "engagement", "browser"],
  ["203.0.113.42", "human", "page_view", "view /AS:n", "browser"],
  [
    "2001:db8:4f2:1a00::7c",
    "human",
    "outbound_click",
    "outbound_click console.example.com",
    "browser",
  ],
];
// A browser as "<browser> · <OS>", a tool by its product token.
const CLIENT_NAME: Record<string, string> = {
  "192.0.2.120": "UptimeRobot/2.0",
  "198.51.100.7": "python-requests/2.32.3",
  "192.0.2.66": "Googlebot/2.1",
  "203.0.113.42": "Chrome · macOS",
  "2001:db8:4f2:1a00::7c": "Firefox · Windows",
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
    const [addr, cls, kind, what, source] = LOG_CLIENTS[i % LOG_CLIENTS.length];
    return {
      t: new Date(NOW.getTime() - i * 47_000).toISOString(),
      addr,
      class: cls,
      kind,
      what,
      client: CLIENT_NAME[addr],
      source,
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
      script: "/nui.js",
      build: "3f2a91c",
      first: "2026-09-03T08:00:00Z",
    },
    storage: {
      events: feed ? 1284551 : 398211,
      bytes: feed ? 432013312 : 134217728,
      retention_months: 25,
      oldest: "2026-09-03T08:00:00Z",
      identity: "address + user agent, plus browser id",
    },
    rules: {
      rate: "60 events / min per address or /64 · 600 per /48",
      batch: "32 KiB · 50 events",
      botlist: { services: 24, updated: "2026-11-22T03:00:00Z" },
      networks: { updated: "2026-11-22T03:00:00Z" },
      tz: "Europe/Zurich",
      llm: { calls_today: 41, budget: 200, configured: true },
    },
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
        return json(noteworthy(site));
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
          ? json(v)
          : json({ detail: "no events from this address in the range" }, 404);
      }
      case "/api/dash/log": {
        const l = log(q, feed, script);
        return l ? json(l) : json({ detail: "bad log filter or cursor" }, 400);
      }
      case "/api/dash/domain":
        return json(domain(site, feed, script));
    }
    return json({ detail: "no such endpoint" }, 404);
  };
}

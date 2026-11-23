// The dashboard API's wire types and a typed client. Every number is computed by the backend; the UI formats
// and draws only. JSON keys are snake_case, times RFC 3339 UTC.

export type Range = "24h" | "7d" | "30d" | "90d" | "12mo";
export type Audience = "all" | "humans" | "bots";
export type Compare = "prev" | "yoy" | "off";
export type ClassKey = "human" | "bot" | "monitor" | "unknown";
export type Verdict = "good" | "bad" | "flat";

// pct for sums, pp for rates, new when the previous period had none; null
// when there is nothing to compare.
export type Change = { pct: number } | { pp: number } | { new: true } | null;

export type Me = { name: string };

export type AuthMethods = { oidc: boolean; token: boolean };

export type SiteInfo = {
  site: string;
  product: string;
  kind: "site" | "api";
  // null: the source has never delivered (for the server: or has no feed).
  browser: { last: string } | null;
  server: { last: string } | null;
};

export type Period = {
  from: string;
  to: string;
  bucket: "hour" | "day" | "month";
  labels: string[];
  prev_label: string;
  last_bucket_open: boolean;
};

export type TileKey =
  "visitors" | "views" | "events" | "requests" | "auto_share" | "errors";

export type Tile = {
  key: TileKey;
  label: string;
  unit: string;
  kind: "sum" | "rate";
  value: number;
  prev: number | null;
  change: Change;
  // flat with the comparison off; prev_series then empty.
  verdict: Verdict;
  series: number[];
  prev_series: number[];
  median: number;
  server: boolean;
};

export type Overview = {
  period: Period;
  live: { humans: number; automated: number };
  tiles: Tile[];
};

// Model text with the computed text beside it: gen is null when no model
// wrote any, and the UI falls back to text.
export type Gen = { gen: string | null; text: string };

export type FindingKind = "problem" | "automation" | "people";
export type Panel =
  "errors" | "visitors" | "referrers" | "probed" | "events" | "domain" | "log";

// A briefing line may cover several findings of one kind (ids): its text
// is then "<lead text>, and N related", detail names each finding with its
// own figure, and figure and unit are their sum or both empty.
export type Finding = {
  id: string;
  ids: string[];
  kind: FindingKind;
  figure: string;
  unit: string;
  gen: string | null;
  text: string;
  detail: string[];
  target: { panel: Panel; filter: Record<string, string> };
};

export type Noteworthy = {
  headline: Gen;
  as_of: string | null;
  items: Finding[];
  more: number;
};

export type ClassRow = {
  class: ClassKey;
  requests: number;
  share: number;
  addresses: number;
  change: Change;
  verdict: Verdict;
};

export type Classes = {
  labels: string[];
  series: Record<ClassKey, number[]>;
  rows: ClassRow[];
};

export type Dim =
  | "pages"
  | "entry"
  | "events"
  | "outbound"
  | "referrers"
  | "networks"
  | "probed"
  | "refused"
  | "errors"
  | "countries";

export type ListRow = {
  key: string;
  label: string;
  sub: string;
  gen: string | null;
  value: number;
  second: number;
  share: number;
  change: Change;
  verdict: Verdict;
  expandable: boolean;
  // Optional: for pages and entry pages the percent of visits that began
  // there, the map shade for countries (1 lightest to 4). Shown only when
  // the backend sends them.
  entry?: number;
  level?: number;
};

export type List = {
  rows: ListRow[];
  // The matching rows past the limit asked for: { 0, 0 } when none.
  other: { count: number; value: number };
  total: number;
  server: boolean;
  // A template's paths (within) only: they are raw events, which reach
  // back to here (null: raw covers the range).
  detail_since?: string | null;
};

export type StatusRow = { class: string; value: number; share: number };
export type Status = { rows: StatusRow[] };

export type ShareRow = { key: string; value: number; share: number };
export type Clients = {
  ip_version: ShareRow[];
  browsers: ShareRow[];
  os: ShareRow[];
  devices: ShareRow[];
};

export type VisitorRow = {
  key: string;
  display: string;
  network: string;
  network_kind: string;
  country: string;
  class: ClassKey;
  client: string;
  visits: number | null;
  views: number;
  requests: number | null;
  refused: number | null;
  refused_share: number | null;
  events: number;
  last: string;
  signals: string[];
  does: Gen;
  // The model's label for an unknown automated client's user agent; null
  // when there is none, and client stands.
  client_gen: string | null;
};

// detail_since, here and below: where raw events begin, the oldest one or
// now when none is left (null: raw covers the range). Sums cover the
// whole range; what is read from raw events (what it does, a trail, the
// log) reaches back to it.
export type Visitors = {
  rows: VisitorRow[];
  total: number;
  next: string;
  detail_since: string | null;
};

export type TrailItem = {
  t: string;
  kind: string;
  what: string;
  count: number;
  // A single event's span is its own moment, its interval 0.
  from: string;
  to: string;
  interval_s: number;
  // A page of the trail may end inside a run: continues marks a run that
  // goes on into the next, older page, continued one that began on the
  // previous page. count, span and interval are this page's part only.
  continues: boolean;
  continued: boolean;
};

export type Reading = {
  verdict: string;
  suspicious: boolean;
  evidence: string[];
  related: string[];
  basis: { from: string; to: string };
};

export type Visitor = {
  key: string;
  class: ClassKey;
  network: string;
  network_kind: string;
  country: string;
  client: string;
  // The model's label for an unknown automated client, as in the table.
  client_gen: string | null;
  signals: string[];
  // What it does in one sentence, as in the table.
  does: Gen;
  requests: number | null;
  addresses: number;
  reading: Reading | null;
  hours: number[];
  trail: TrailItem[];
  trail_next: string;
  detail_since: string | null;
};

export type LogRow = {
  t: string;
  addr: string;
  class: ClassKey;
  kind: string;
  what: string;
  client: string;
  source: "browser" | "server";
};

export type Log = {
  rows: LogRow[];
  total: number;
  next: string;
  detail_since: string | null;
};

export type SourceState = { state: "receiving" | "idle"; last: string };

export type Domain = {
  counters: {
    accepted: number;
    rate_limited: number;
    rejected: number;
    dropped: number;
    unknown_domain: number;
  };
  // null: the source never delivered (the server's: or the site has no
  // feed, which feed tells apart).
  sources: {
    browser: SourceState | null;
    server: SourceState | null;
    feed: boolean;
    script: string;
    build: string;
    // The first figure: figures_since.
    first: string | null;
  };
  storage: {
    // Raw events: how many, the oldest, how long they are kept.
    events: number;
    oldest: string | null;
    raw_days: number;
    // During the purge's grace after an upgrade: when raw older than
    // raw_days starts to go, if the site holds any; null otherwise.
    purge_from: string | null;
    // The oldest figure, from the aggregates or raw: it may be older than
    // the oldest raw event.
    figures_since: string | null;
    bytes: number;
    retention_months: number;
    identity: string;
  };
  rules: {
    rate: string;
    batch: string;
    botlist: { services: number; updated: string | null };
    networks: { updated: string | null };
    tz: string;
    llm: { calls_today: number; budget: number; configured: boolean };
  };
};

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

// The fetch the client goes through: the browser's, or the fixture router
// in the dev fixture mode and in tests.
type Fetcher = (url: string, init?: RequestInit) => Promise<Response>;
let fetcher: Fetcher = (url, init) => fetch(url, init);

export function setFetcher(f: Fetcher | null) {
  fetcher = f ?? ((url, init) => fetch(url, init));
}

// A session that expires while the page is open: the app swaps to the
// sign-in or no-access screen instead of showing an error per panel.
let onAuth: ((status: 401 | 403) => void) | null = null;
export function setAuthListener(f: ((status: 401 | 403) => void) | null) {
  onAuth = f;
}

async function getJSON<T>(url: string, signal?: AbortSignal): Promise<T> {
  const res = await fetcher(url, {
    signal,
    headers: { Accept: "application/json" },
  });
  if ((res.status === 401 || res.status === 403) && url.startsWith("/api/dash"))
    onAuth?.(res.status);
  if (!res.ok) {
    let detail = res.statusText || `HTTP ${res.status}`;
    try {
      // {"detail"} from the API; a panic's 500 from kit's Recover says
      // {"error"}.
      const body = (await res.json()) as { detail?: unknown; error?: unknown };
      if (typeof body.detail === "string") detail = body.detail;
      else if (typeof body.error === "string") detail = body.error;
    } catch {
      // not JSON: keep the status text (an auth proxy's page, say)
    }
    throw new ApiError(res.status, detail);
  }
  return (await res.json()) as T;
}

// The viewer's time zone: the backend cuts buckets there.
export function viewerZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
}

// What every dashboard query carries.
export type Scope = {
  site: string;
  range: Range;
  aud: Audience;
  cmp: Compare;
};

function query(
  scope: Scope | null,
  extra: Record<string, string | number | undefined>,
): string {
  const q = new URLSearchParams();
  if (scope) {
    q.set("site", scope.site);
    q.set("range", scope.range);
    q.set("tz", viewerZone());
    q.set("aud", scope.aud);
    q.set("cmp", scope.cmp);
  }
  for (const [k, v] of Object.entries(extra))
    if (v !== undefined && v !== "") q.set(k, String(v));
  const s = q.toString();
  return s ? `?${s}` : "";
}

const dash = <T>(
  path: string,
  scope: Scope | null,
  extra: Record<string, string | number | undefined>,
  signal?: AbortSignal,
) => getJSON<T>(`/api/dash/${path}${query(scope, extra)}`, signal);

export const api = {
  me: (signal?: AbortSignal) => getJSON<Me>("/api/me", signal),
  // How this server signs people in; public.
  authMethods: (signal?: AbortSignal) =>
    getJSON<AuthMethods>("/api/auth/methods", signal),
  // The admin token as a form post: URLSearchParams makes the browser send
  // it form-encoded. The answer's status is all that is read.
  tokenLogin: (token: string) =>
    fetcher("/api/auth/token", {
      method: "POST",
      credentials: "same-origin",
      body: new URLSearchParams({ token }),
    }),
  logout: () =>
    fetcher("/api/auth/logout", { method: "POST", credentials: "same-origin" }),
  sites: (signal?: AbortSignal) =>
    dash<{ sites: SiteInfo[] }>("sites", null, {}, signal),
  overview: (s: Scope, signal?: AbortSignal) =>
    dash<Overview>("overview", s, {}, signal),
  noteworthy: (s: Scope, signal?: AbortSignal) =>
    dash<Noteworthy>("noteworthy", s, {}, signal),
  classes: (s: Scope, signal?: AbortSignal) =>
    dash<Classes>("classes", s, {}, signal),
  list: (
    s: Scope,
    p: {
      dim: Dim;
      sort?: "volume" | "change";
      limit?: number;
      within?: string;
      // The full table's search term and the Events tab's custom-event
      // filter.
      q?: string;
      sub?: string;
    },
    signal?: AbortSignal,
  ) =>
    dash<List>(
      "list",
      s,
      {
        dim: p.dim,
        sort: p.sort ?? "volume",
        limit: p.limit ?? 6,
        within: p.within,
        q: p.q,
        sub: p.sub,
      },
      signal,
    ),
  status: (s: Scope, signal?: AbortSignal) =>
    dash<Status>("status", s, {}, signal),
  clients: (s: Scope, signal?: AbortSignal) =>
    dash<Clients>("clients", s, {}, signal),
  visitors: (
    s: Scope,
    p: {
      sort: "requests" | "refused" | "events" | "views";
      class?: string;
      q?: string;
      limit?: number;
      cursor?: string;
    },
    signal?: AbortSignal,
  ) =>
    dash<Visitors>(
      "visitors",
      s,
      {
        sort: p.sort,
        class: p.class,
        q: p.q,
        limit: p.limit ?? 7,
        cursor: p.cursor,
      },
      signal,
    ),
  // cursor: a previous answer's trail_next, for earlier trail entries.
  visitor: (s: Scope, key: string, cursor?: string, signal?: AbortSignal) =>
    dash<Visitor>("visitor", s, { key, cursor }, signal),
  log: (s: Scope, f: LogFilter, cursor?: string, signal?: AbortSignal) =>
    dash<Log>("log", s, { ...f, cursor }, signal),
  domain: (s: Scope, signal?: AbortSignal) =>
    dash<Domain>("domain", s, {}, signal),
};

export type LogFilter = {
  addr?: string;
  // An AS number, "64500" or "AS64500": a network spike opens the log on it.
  asn?: string;
  path?: string;
  class?: string;
  kind?: string;
  status?: string;
};

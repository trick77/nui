// The dashboard API's wire types and a typed client. Every number is computed by the backend; the UI formats
// and draws only. JSON keys are snake_case, times RFC 3339 UTC.

export type Range = "24h" | "7d" | "30d" | "90d" | "12mo";
export type Audience = "all" | "humans" | "bots";
export type Compare = "prev" | "yoy" | "off";
export type ClassKey = "human" | "api" | "bot" | "monitor" | "unknown";
export type Verdict = "good" | "bad" | "flat";

// pct for sums, pp for rates, new when the previous period had none; null
// when there is nothing to compare.
export type Change = { pct: number } | { pp: number } | { new: true } | null;

export type Me = { name: string };

export type AuthMethods = { oidc: boolean; token: boolean };

export type SiteInfo = {
  // The domain: what every request and link names.
  site: string;
  // What it is called, the domain without a configured name. Display only.
  name: string;
  product: string;
  kind: "site" | "api";
  // null: the source has never delivered (for the server: or has no feed).
  browser: { last: string } | null;
  server: { last: string } | null;
  // A server feed is configured, delivered or not.
  feed: boolean;
};

// A mark on a chart's time axis. A place counts the chart's slots (or bars)
// from its left edge.
export type Tick = {
  at: number;
  label: string;
  // Something begins there: a rule across the chart.
  line: boolean;
};

// A chart's time axis: its ticks, and the weekends from place to place.
export type Axis = { ticks: Tick[]; weekends: [number, number][] };

// What a chart's points are, the same for every tile drawn on it.
export type Frame = Axis & {
  // What a point of the chart covers ("3 hours"), and the moving mean's
  // window ("24 h").
  slot: string;
  mean: string;
  // The slots' names.
  labels: string[];
  // The last slot is still filling.
  open: boolean;
  // The place the domain's figures begin at, null when before the range.
  first: number | null;
};

export type Period = Frame & {
  from: string;
  to: string;
  prev_label: string;
  // The frame of the tiles with too little in a slot (low), null without one.
  low: Frame | null;
  // Where the figures begin, when that is within the range; null otherwise.
  since: string | null;
};

export type TileKey =
  | "visitors"
  | "visits"
  | "views"
  | "events"
  | "requests"
  | "auto_share"
  | "errors"
  // An API's.
  | "calls"
  | "accounts"
  | "callers"
  | "failed"
  | "p95";

export type Tile = {
  key: TileKey;
  label: string;
  unit: string;
  kind: "sum" | "rate";
  // null, here and in the series: there is no such figure (a percentile of
  // no calls, a bucket without figures).
  value: number | null;
  prev: number | null;
  change: Change;
  // flat with the comparison off.
  verdict: Verdict;
  // A figure per slot, and the moving mean of those over.
  series: (number | null)[];
  mean: (number | null)[];
  // The comparison's mean, and the one against the other (percent, points of
  // a rate); both empty when there is none to draw.
  prev_mean: (number | null)[];
  delta: (number | null)[];
  // They are on the period's low frame.
  low: boolean;
  server: boolean;
};

export type Overview = {
  period: Period;
  live: { humans: number; automated: number };
  tiles: Tile[];
};

// The frame a tile's series are on.
export const frameOf = (p: Period, t: Tile | undefined): Frame =>
  (t?.low && p.low) || p;

// Model text with the computed text beside it: gen is null when no model
// wrote any, and the UI falls back to text.
export type Gen = { gen: string | null; text: string };

export type FindingKind = "problem" | "automation" | "people";
export type Panel =
  | "errors"
  | "visitors"
  | "referrers"
  | "probed"
  | "events"
  | "endpoints"
  | "failures"
  | "domain"
  | "log"
  // The script hosts card, a row named by the filter's key.
  | "scripts";

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

export type Classes = Axis & {
  // The bars' names.
  labels: string[];
  // The last bar is still filling.
  open: boolean;
  // The bars that are less than a whole one: the open one, a week the range
  // cuts.
  partial: boolean[];
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
  | "countries"
  | "endpoints"
  | "accounts"
  | "failures"
  | "agents"
  | "services";

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
  // Endpoints and accounts: the percent of calls that failed. Endpoints:
  // their durations' percentiles in ms.
  failed?: number;
  p50?: number;
  p95?: number;
  // Services, from today's bot list: the purpose as the list spells it
  // ("ai-training"), the service behind an agent named otherwise, whether
  // the purpose is only what the client declares (the service's addresses
  // serve several), and the https host its clients name in Signature-Agent,
  // unverified.
  category?: string;
  service?: string;
  declared?: boolean;
  says?: string;
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
  // Services: the purpose groups of all rows in the range ("ai",
  // "search"), what cat picks; absent without one.
  groups?: string[];
  // Where the figures begin, when that is within the range; the same of
  // the comparison, whose rows then carry no change. null otherwise.
  since?: string | null;
  prev_since?: string | null;
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
  // Where the span begins, unix ms.
  from: number;
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
  from: number;
  detail_since: string | null;
  // The hosts its events name in Signature-Agent, at most three, the most
  // frequent first: a claim nothing checks, never a signal.
  says?: string[];
};

export type LogRow = {
  t: string;
  addr: string;
  class: ClassKey;
  kind: string;
  what: string;
  client: string;
  source: "browser" | "server";
  // A server request's: the account it acted as ("" for none), its duration.
  account: string;
  dur_ms: number | null;
  // A script error's: where it happened (null for any other event), on
  // which page.
  error: ErrorWhere | null;
  page: string;
};

export type ErrorOrigin =
  "withheld" | "extension" | "own" | "other" | "named" | "unknown";

export type ErrorWhere = {
  origin: ErrorOrigin;
  // Another host's name (other).
  host: string;
  // file:line:col, "" when the browser reported none.
  place: string;
};

export type ErrorCount = {
  key: string;
  // Of places only.
  where?: ErrorWhere;
  count: number;
  visitors: number;
};

export type ErrorBreakdown = { rows: ErrorCount[]; more: number };

// One script error, by its message.
export type ErrorDetail = {
  count: number;
  visitors: number;
  sessions: number;
  // Visitors with a page view in the same span, and the share of them that
  // got the error.
  viewers: number;
  share: number | null;
  first: string | null;
  last: string | null;
  // Of every place, the cut ones too, the most frequent first.
  origins: ErrorOrigin[];
  places: ErrorBreakdown;
  pages: ErrorBreakdown;
  clients: ErrorBreakdown;
  classes: ErrorBreakdown;
  recent: {
    t: string;
    addr: string;
    key: string;
    class: ClassKey;
    client: string;
    page: string;
    where: ErrorWhere;
  }[];
  // The newest occurrence's with a stack; "" and empty without one.
  type: string;
  stack: string[];
  detail_since: string | null;
};

export type Log = {
  rows: LogRow[];
  total: number;
  next: string;
  from: number;
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
    llm: {
      calls_today: number;
      budget: number;
      spent: boolean;
      configured: boolean;
    };
  };
  // Missing from a backend older than alerts: as not configured.
  notify?: Notify;
};

// A host the pages loaded scripts from; addrs the most addresses of one day
// that reported it. new: first seen in the range, a finding; provisional:
// that finding is of a day still open.
export type WatchHost = {
  host: string;
  first_seen: string;
  last_seen: string;
  addrs: number;
  new: boolean;
  provisional: boolean;
};

export type Watch = {
  // New ones first.
  hosts: WatchHost[];
  // The kept requests have been read once.
  ready: boolean;
  // Until then the hosts are only learned. Once set, never cleared: compare
  // with now.
  scripts_learning_until: string | null;
  // Hosts the limits left out, each counted once a day it was: the site's
  // whole time.
  cut: number;
};

export type Severity = "low" | "normal" | "high";

// Never a URL: the backend keeps a channel's address to itself.
export type NotifyChannel = {
  name: string;
  service: string;
  min_severity: Severity;
  last_ok: string | null;
  last_error: { at: string; text: string } | null;
};

export type Notify = {
  configured: boolean;
  enabled: boolean;
  faults: string[];
  rules: { active: number; off: number };
  channels: NotifyChannel[];
};

export type AlertState = "open" | "cleared" | "once";
export type DeliveryState = "sent" | "partial" | "failed" | "pending" | "none";

// One alert as the history keeps it. text and detail are plain text, shown
// as such; site "" is the instance's own.
export type Alert = {
  id: number;
  site: string;
  kind: string;
  severity: Severity;
  state: AlertState;
  text: string;
  detail: string;
  figure: number | null;
  unit: string;
  first_at: string;
  last_at: string;
  cleared_at: string | null;
  delivery: {
    state: DeliveryState;
    at: string | null;
    channels: { name: string; ok: boolean }[];
  };
  target: { panel: Panel; filter: Record<string, string> } | null;
};

export type Alerts = { alerts: Alert[]; next: string | null };

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
      // The services' purpose group, one of an answer's groups.
      cat?: string;
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
        cat: p.cat,
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
  // cursor: a previous answer's trail_next, for earlier trail entries. from:
  // the table's, so a row shown opens however long ago the table loaded (the
  // log's likewise: its first page's, for the next).
  visitor: (
    s: Scope,
    key: string,
    cursor?: string,
    signal?: AbortSignal,
    from?: number,
  ) => dash<Visitor>("visitor", s, { key, cursor, from }, signal),
  log: (
    s: Scope,
    f: LogFilter,
    cursor?: string,
    signal?: AbortSignal,
    from?: number,
  ) => dash<Log>("log", s, { ...f, cursor, from }, signal),
  error: (s: Scope, key: string, signal?: AbortSignal) =>
    dash<ErrorDetail>("error", s, { key }, signal),
  domain: (s: Scope, signal?: AbortSignal) =>
    dash<Domain>("domain", s, {}, signal),
  // Script hosts: domain and range apply, never the audience or the
  // comparison.
  watch: (s: Scope, signal?: AbortSignal) =>
    dash<Watch>("watch", { ...s, aud: "all", cmp: "prev" }, {}, signal),
  // The history spans no range. site "" (or none): every site's and the
  // instance's. before: a previous answer's next.
  alerts: (
    p: { site?: string; before?: string; limit?: number },
    signal?: AbortSignal,
  ) =>
    dash<Alerts>(
      "alerts",
      null,
      { site: p.site, before: p.before, limit: p.limit },
      signal,
    ),
};

export type LogFilter = {
  addr?: string;
  // An AS number, "64500" or "AS64500": a network spike opens the log on it.
  asn?: string;
  path?: string;
  class?: string;
  kind?: string;
  status?: string;
  // "1": the calls among the events (an account's row counts those).
  calls?: string;
  // Exact, set by a click: an event's name (a script error's message), an
  // endpoint, an account, an operation, a reason to fail, a bot list name
  // (its server requests).
  name?: string;
  endpoint?: string;
  account?: string;
  op?: string;
  failure?: string;
  service?: string;
};

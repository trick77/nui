// The browser script: collects page views, engagement, clicks, forms,
// errors and web vitals and sends them in small batches to the collector it
// was loaded from.
//
// It runs on someone else's page, so it must never harm it or the collector:
// every callback is wrapped, events are capped per page and per minute, the
// queue drops instead of growing, a failing collector is backed off and
// finally left alone. The host page enforces Trusted Types for scripts: no
// script elements, no innerHTML, no eval, no string timers. No unload
// handler either (it breaks the back/forward cache).

type Props = Record<string, unknown>;

type Rec = {
  t: number;
  k: string;
  n?: string;
  // The session it belongs to: a batch carries one.
  s: string;
  p: Props;
  // Where the event happened; sent only when it differs from the batch ctx.
  u: string;
  ti: string;
};

// The collector refuses a whole batch when one value is over a limit
// (backend/internal/ingest/collector.go, checkValue): every value sent is
// held to these.
export const LIMITS = {
  // The collector rejects a body over 32 KiB: stay below it with room.
  batchBytes: 31 * 1024,
  // The keepalive bodies a browser lets a page have in flight together.
  quota: 64 * 1024,
  batchEvents: 50,
  // Strings under the keys url, ref, src and href; every other string str.
  url: 2048,
  str: 512,
  // Map keys, entries per map, entries per array, nesting.
  key: 64,
  keys: 64,
  items: 50,
  depth: 8,
  // Values visited per walk of a host page's props.
  nodes: 200,
  props: 4096,
  // The context without URL and referrer, as the collector encodes it.
  ctx: 4096,
  // What the collector stores (store.Event.Size), held as an upper bound:
  // a batch's rows at most `stored`, an event's own row at most `row`.
  // Every row repeats the page's URL with its path; the first one also
  // holds the context and the request's headers (at most `headers`).
  stored: 64 * 1024,
  row: 16 * 1024,
  headers: 2048,
  // A row's fixed part: overhead, source, visitor and session ids, an IPv6
  // address and its /64 key twice, a template, a country.
  rowFixed: 96 + 7 + 32 + 32 + 39 + 2 * 24 + 200 + 2,
  // An event name the collector derives (a host, a message, a URL).
  name: 200,
  queue: 100,
  perPage: 300,
  perDocument: 1000,
  perMinute: 60,
  flushAfterMs: 5000,
  flushAt: 20,
  backoffMs: 2000,
  backoffMaxMs: 5 * 60 * 1000,
  // Every delay, Retry-After included: browsers fire a timer over 2^31-1 ms
  // at once.
  retryMaxMs: 60 * 60 * 1000,
  failures: 5,
  sessionMs: 30 * 60 * 1000,
  sameError: 3,
  // Distinct error messages per page, and per document over all its pages.
  errorKinds: 20,
  errorKindsPerDocument: 100,
};

const STORE_VID = "nui.vid";
const STORE_SID = "nui.sid";
const STORE_SEEN = "nui.seen";
const DOWNLOAD =
  /\.(zip|gz|tgz|tar|xz|bz2|7z|rar|pdf|csv|tsv|json|txt|xlsx?|docx?|pptx?|dmg|exe|msi|pkg|deb|rpm|apk|iso|mmdb|mrt)$/i;
const SEARCH_NAMES = /^(q|query|search|s|term)$/i;
// Kinds the collector names by a host, a message or a URL (LIMITS.name);
// every other kind but custom by at most 16 bytes, or not at all.
const NAMED = /^(outbound_click|download|js_error|resource_error)$/;
const ID = /^[\da-f]{32}$/;

// UTF-8 length without TextEncoder, which old browsers lack.
export function utf8Len(s: string): number {
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x80) n += 1;
    else if (c < 0x800) n += 2;
    else if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) {
      n += 4;
      i++;
    } else n += 3;
  }
  return n;
}

// A JSON text's length once the collector has decoded and encoded it again:
// its UTF-8 length, but U+2028 and U+2029, which JSON.stringify leaves as
// they are, escaped (six bytes, not three). Never less than utf8Len.
export function goLen(json: string): number {
  let n = utf8Len(json);
  for (let i = 0; i < json.length; i++) {
    const c = json.charCodeAt(i);
    if (c === 0x2028 || c === 0x2029) n += 3;
  }
  return n;
}

// s cut to at most max UTF-8 bytes, never inside a surrogate pair.
// The work is bounded by max, not by the input: the result has at most max
// code units, and one more keeps a pair at the cut whole.
export function clip(v: unknown, max: number): string {
  const s = (v == null ? "" : String(v)).slice(0, max + 1);
  if (s.length * 3 <= max || utf8Len(s) <= max) return s;
  let n = 0;
  let i = 0;
  for (; i < s.length; i++) {
    const c = s.charCodeAt(i);
    const pair = c >= 0xd800 && c <= 0xdbff && i + 1 < s.length;
    const w = c < 0x80 ? 1 : c < 0x800 ? 2 : pair ? 4 : 3;
    if (n + w > max) break;
    n += w;
    if (pair) i++;
  }
  return s.slice(0, i);
}

const URL_KEY = /^(url|ref|src|href)$/;

// A URL as it is sent. The fragment goes unless it is a client-side route
// ("#/", "#!"): sign-in callbacks and magic links carry tokens there. Go's
// url.Parse refuses a malformed escape in the path or fragment, so a stray
// "%" is escaped and one cut in half by the clip is dropped. A browser
// leaves "|" and "^" in a path as they are; Go then escapes the whole path
// again, up to three times as long as sent: they go escaped.
export function clipURL(v: unknown): string {
  return clip(
    clip(v, LIMITS.url)
      .replace(/#(?![/!])[^]*$/, "")
      .replace(/%(?![\da-f]{2})/gi, "%25")
      .replace(/[|^]/g, (c) => (c === "|" ? "%7C" : "%5E")),
    LIMITS.url,
  ).replace(/%[\da-f]?$/i, "");
}

// Values bound() may still visit in the current walk: a host page's object
// costs the same whatever its size.
let budget = 0;

// A copy of v within the collector's limits: strings clipped by their key,
// maps and arrays cut, too deep a container null, anything not JSON null.
// key is the name the value is checked under, depth where the collector
// counts it (0 for an event's p and for each ctx value).
export function bound(v: unknown, key: string, depth: number): unknown {
  if (--budget < 0) return null;
  if (typeof v === "string")
    return URL_KEY.test(key) ? clipURL(v) : clip(v, LIMITS.str);
  if (typeof v === "number") return isFinite(v) ? v : null;
  if (typeof v === "boolean") return v;
  if (!v || typeof v !== "object" || depth >= LIMITS.depth) return null;
  // Typed arrays by index too: for-in would first list every element.
  if (Array.isArray(v) || ArrayBuffer.isView(v)) {
    const l = v as ArrayLike<unknown>;
    const a: unknown[] = [];
    for (let i = 0; i < l.length && i < LIMITS.items && budget > 0; i++)
      a.push(bound(l[i], key, depth + 1));
    return a;
  }
  const out: Props = {};
  let n = 0;
  for (const k in v as Props) {
    if (++n > LIMITS.keys || budget <= 0) break;
    if (!Object.prototype.hasOwnProperty.call(v, k)) continue;
    const c = clip(k, LIMITS.key);
    out[c] = bound((v as Props)[k], c, depth + 1);
  }
  return out;
}

// An event's props within the collector's limits; over the per-event
// budget they are dropped, not the event.
function fit(p: unknown): Props {
  budget = LIMITS.nodes;
  const out = bound(p, "p", 0) as Props;
  // As sent and as stored: the collector takes 4 KiB of both.
  return goLen(safeJSON(out)) > LIMITS.props ? { dropped: true } : out;
}

// Shallow, flat, bounded copy of free-form props: strings clipped, numbers
// and booleans kept, anything else stringified.
export function cleanProps(p: unknown): Props {
  const out: Props = {};
  if (!p || typeof p !== "object") return out;
  let keys = 0;
  budget = LIMITS.nodes;
  for (const k in p as Props) {
    if (!Object.prototype.hasOwnProperty.call(p, k)) continue;
    if (++keys > 32) break;
    const v = (p as Props)[k];
    const key = clip(k, LIMITS.key);
    out[key] =
      v && typeof v === "object" ? safeJSON(bound(v, key, 1)) : (v as unknown);
  }
  return fit(out);
}

function safeJSON(v: unknown): string {
  try {
    return JSON.stringify(v) ?? "";
  } catch {
    return "";
  }
}

function randomId(win: Window): string {
  try {
    const b = new Uint8Array(16);
    win.crypto.getRandomValues(b);
    let s = "";
    for (let i = 0; i < b.length; i++) s += (b[i] + 256).toString(16).slice(1);
    return s;
  } catch {
    let s = "";
    for (let i = 0; i < 32; i++)
      s += Math.floor(Math.random() * 16).toString(16);
    return s;
  }
}

export type Tracker = {
  event: (name: unknown, props?: unknown) => void;
  flush: (final?: boolean) => void;
  stop: () => void;
  // For tests and diagnosis.
  state: () => {
    queued: number;
    disabled: boolean;
    failures: number;
    backoff: number;
    dropped: number;
    errorKeys: number;
    sid: string;
    vid: string;
  };
};

type NuiGlobal = {
  q?: unknown;
  event?: unknown;
  _t?: Tracker;
};

// start wires the script into win. scriptSrc is the script's own URL: the
// collector lives on its origin. Returns null when it cannot run here.
export function start(
  win: Window & typeof globalThis,
  scriptSrc: string | null | undefined,
): Tracker | null {
  const doc = win.document;
  const nav = win.navigator;
  const loc = win.location;
  let src = scriptSrc;
  if (!src) {
    const el = doc.querySelector<HTMLScriptElement>('script[src*="/nui.js"]');
    src = el ? el.src : "";
  }
  if (!src) return null;
  const endpoint = new URL(src, loc.href).origin + "/api/in";
  const site = loc.hostname;

  const queue: Rec[] = [];
  const minute: number[] = [];
  const errorsSeen = new Map<string, number>();
  let docErrors = 0;
  const offs: (() => void)[] = [];
  let pageCount = 0;
  let docCount = 0;
  let dropped = 0;
  let disabled = false;
  let failures = 0;
  let backoff = 0;
  let notBefore = 0;
  // The pause a 429 named with Retry-After: the collector asked for it, so
  // the final flush on leaving waits for it too. The own back-off after a
  // network error, a 5xx or a 408 does not hold the final flush back: the
  // end of the visit would be lost, and with a failed first batch its start.
  let asked = 0;
  let timer: ReturnType<typeof setTimeout> | 0 = 0;
  // Body bytes of the requests not yet answered: browsers refuse a
  // keepalive request once those in flight would pass 64 KiB together.
  let carried = 0;
  // The latest page view, its title still to be taken (see pageView).
  let view: Rec | undefined;
  const settle = () => {
    if (view) view.ti = clip(doc.title, LIMITS.str);
    view = undefined;
  };

  const now = () => Date.now();

  // ---- storage: visitor and session ids; memory when storage is blocked
  const mem: Record<string, string> = {};
  const get = (k: string): string | null => {
    try {
      const v = win.localStorage.getItem(k);
      if (v != null) return v;
    } catch {
      // blocked storage
    }
    return mem[k] ?? null;
  };
  const put = (k: string, v: string) => {
    mem[k] = v;
    try {
      win.localStorage.setItem(k, v);
    } catch {
      // blocked or full storage
    }
  };
  // The host page can write these: anything but an id of ours is replaced,
  // or one bad value would get every batch refused.
  const id = (k: string) => {
    const v = get(k);
    return v && ID.test(v) ? v : "";
  };
  let vid = id(STORE_VID);
  if (!vid) {
    vid = randomId(win);
    put(STORE_VID, vid);
  }
  let sid = id(STORE_SID);

  // ---- context, sent with every batch
  const inputs = { pointer: 0, key: 0, touch: 0, scroll: 0 };
  let uach: Props | undefined;
  const navEntry = () => {
    try {
      return win.performance.getEntriesByType("navigation")[0] as
        | (PerformanceNavigationTiming & { activationStart?: number })
        | undefined;
    } catch {
      return undefined;
    }
  };
  const navType = navEntry()?.type || "";
  // A page the browser prerenders reports nothing until it is shown: the
  // visitor may never open it.
  const pre = () =>
    !!(doc as Document & { prerendering?: boolean }).prerendering;
  let visAtLoad = doc.visibilityState;
  try {
    const d = (nav as Navigator & { userAgentData?: UAData }).userAgentData;
    if (d) {
      uach = {
        mobile: d.mobile,
        platform: d.platform,
        brands: (d.brands || []).slice(0, 8).map((b) => [b.brand, b.version]),
      };
      d.getHighEntropyValues?.([
        "platformVersion",
        "model",
        "architecture",
        "bitness",
      ])
        .then((v) => {
          if (!uach) return;
          uach.platform_version = v.platformVersion;
          uach.model = v.model;
          uach.arch = v.architecture;
          uach.bitness = v.bitness;
        })
        .catch(() => {});
    }
  } catch {
    // no client hints
  }

  const media = (q: string): boolean | undefined => {
    try {
      return win.matchMedia(q).matches;
    } catch {
      return undefined;
    }
  };

  const ctx = (): Props => {
    const c: Props = {};
    const set = (k: string, f: () => unknown) => {
      try {
        const v = f();
        if (v !== undefined) c[k] = v;
      } catch {
        // a missing browser API leaves the key out
      }
    };
    set("url", () => loc.href);
    set("ref", () => doc.referrer);
    set("title", () => doc.title);
    set("lang", () => nav.language);
    set("langs", () => (nav.languages || []).slice(0, 8));
    set("screen", () => [win.screen.width, win.screen.height]);
    set("viewport", () => [win.innerWidth, win.innerHeight]);
    set("dpr", () => win.devicePixelRatio);
    set("tz", () => Intl.DateTimeFormat().resolvedOptions().timeZone);
    set("tzo", () => new Date().getTimezoneOffset());
    set("uach", () => uach);
    set("conn", () => {
      const k = (nav as Navigator & { connection?: Conn }).connection;
      return k
        ? {
            type: k.effectiveType,
            downlink: k.downlink,
            rtt: k.rtt,
            save_data: k.saveData,
          }
        : undefined;
    });
    set("touch", () => nav.maxTouchPoints);
    set("hc", () => nav.hardwareConcurrency);
    set(
      "mem",
      () => (nav as Navigator & { deviceMemory?: number }).deviceMemory,
    );
    set("dark", () => media("(prefers-color-scheme: dark)"));
    set("reduced_motion", () => media("(prefers-reduced-motion: reduce)"));
    set("webdriver", () => nav.webdriver);
    set("vis", () => visAtLoad);
    set("nav", () => navType || undefined);
    set("cookies", () => nav.cookieEnabled);
    set("inputs", () => ({ ...inputs }));
    // Each value is checked at depth 0, like an event's p.
    budget = LIMITS.nodes;
    const out = bound(c, "", -1) as Props;
    // Over the context's limit only with odd client hints or an odd title:
    // the longest values go, in this order, until it fits; what is left is
    // the browser's short figures.
    for (const k of ["uach", "langs", "title", "lang", "tz", "conn"])
      if (ctxLen(out) > LIMITS.ctx) delete out[k];
    return out;
  };
  // The context as the collector stores it: without URL and referrer, a
  // comma counted for every key.
  const ctxLen = (c: Props) => {
    let n = 2;
    for (const k in c)
      if (k !== "url" && k !== "ref")
        n += utf8Len(k) + 4 + goLen(safeJSON(c[k]));
    return n;
  };

  // ---- the queue and its guards
  const schedule = (ms: number) => {
    if (timer) return;
    try {
      timer = setTimeout(
        () => {
          timer = 0;
          guard(() => flush(false));
        },
        Math.min(Math.max(ms, 0) || 0, LIMITS.retryMaxMs),
      );
    } catch {
      timer = 0;
    }
  };

  // One session per browser: the id is read back from storage for every
  // event, so a tab follows the one another tab started. It lives while
  // anything counts as activity: an event, input, visible and focused time.
  // liveFrom is where the current visible, focused stretch began: one that
  // began within a live session keeps it alive up to now. A closing event
  // (engagement, vital) never opens a session nor extends an expired one;
  // activity after the session expired starts a new one there (resume).
  let liveFrom = 0;
  let touched = 0;
  const alive = (t: number): boolean => {
    const last = Number(get(STORE_SEEN)) || 0;
    return (
      last > 0 &&
      (t - last <= LIMITS.sessionMs ||
        (liveFrom > 0 && liveFrom - last <= LIMITS.sessionMs))
    );
  };
  const session = (t: number, closing: boolean): boolean => {
    sid = id(STORE_SID) || sid;
    const live = alive(t);
    let fresh = false;
    if (!sid || (!live && !closing)) {
      sid = randomId(win);
      put(STORE_SID, sid);
      fresh = true;
    }
    if (live || fresh) {
      put(STORE_SEEN, String(t));
      touched = t;
    }
    return fresh;
  };
  // Input keeps the session alive; storage at most every few seconds.
  // Input after the session expired starts a new one (resume); the end of a
  // visible, focused stretch (force) only records how long it lived.
  const touch = (force?: boolean) => {
    const t = now();
    if (!force && t - touched < 5000) return;
    touched = t;
    if (!sid) return;
    if (alive(t)) put(STORE_SEEN, String(t));
    else if (!force) resume();
  };
  // Activity on a page whose session expired: what the page holds so far
  // goes out as a closing engagement of the expired session, then a new
  // session starts here, with its session_start, and takes what follows.
  // Before the first page view there is nothing to close: the view itself
  // starts the session.
  let begun = false;
  let resuming = false;
  const resume = () => {
    if (!begun || resuming || disabled || pre()) return;
    resuming = true;
    try {
      engagement();
      record("session_start", {});
    } finally {
      resuming = false;
    }
  };

  // Checked before any work for an event: once a cap is reached, a host page
  // calling in a loop costs nothing more.
  const full = (t: number): boolean => {
    while (minute.length && t - minute[0] >= 60000) minute.shift();
    if (
      pageCount >= LIMITS.perPage ||
      docCount >= LIMITS.perDocument ||
      minute.length >= LIMITS.perMinute ||
      queue.length >= LIMITS.queue
    ) {
      dropped++;
      return true;
    }
    return false;
  };
  const admit = (t: number): boolean => {
    if (full(t)) return false;
    pageCount++;
    docCount++;
    minute.push(t);
    return true;
  };

  // at: the URL and title the event happened on, when the page has moved on.
  const record = (
    k: string,
    p: Props,
    n?: string,
    at?: [string, string],
  ): Rec | undefined => {
    if (disabled || pre()) return;
    const t = now();
    if (full(t)) return;
    if (
      session(t, k === "engagement" || k === "vital") &&
      k !== "session_start"
    )
      record("session_start", {});
    if (!admit(t)) return;
    const r: Rec = {
      t,
      k,
      s: sid,
      p: fit(p),
      u: at ? at[0] : clipURL(loc.href),
      ti: clip(at ? at[1] : doc.title, LIMITS.str),
    };
    if (n != null) r.n = clip(n, LIMITS.str);
    queue.push(r);
    if (queue.length >= LIMITS.flushAt) flush(false);
    else schedule(LIMITS.flushAfterMs);
    return r;
  };

  // ---- sending
  // paced: a 429, the collector's pace and not a failure (it charges a
  // batch by what it stores, so a page with long URLs meets it in normal
  // use): wait as long as it says, or the own back-off without a
  // Retry-After, and never switch off for it.
  const fail = (retryMs: number, paced = false) => {
    backoff = Math.min(
      LIMITS.backoffMaxMs,
      backoff ? backoff * 2 : LIMITS.backoffMs,
    );
    if (!paced && ++failures >= LIMITS.failures) {
      disabled = true;
      queue.length = 0;
      return;
    }
    const ra = retryMs > 0 ? retryMs : 0;
    notBefore =
      now() +
      Math.min(LIMITS.retryMaxMs, paced && ra ? ra : Math.max(backoff, ra));
    asked = paced && ra ? notBefore : 0;
  };

  const requeue = (batch: Rec[]) => {
    const room = LIMITS.queue - queue.length;
    if (room <= 0) {
      dropped += batch.length;
      return;
    }
    dropped += Math.max(0, batch.length - room);
    queue.unshift(...batch.slice(0, room));
  };

  // The next batch, its body at most room bytes; null when nothing fits.
  const take = (room: number): [string, Rec[]] | null => {
    settle();
    const c = ctx();
    const url = c.url;
    const s = queue[0].s;
    const head =
      safeJSON({
        v: 1,
        site,
        vid,
        sid: s,
        sent: now(),
        ctx: c,
      }).slice(0, -1) + ',"events":[';
    let size = utf8Len(head) + 2;
    // What the rows will store, as store.Event.Size counts it: the first
    // row's props hold the context and the headers ("ctx":…,"hdr":…,), then
    // every row its fixed part, kind, name, user agent (the header is
    // navigator.userAgent), URL and its path (never longer: see clipURL),
    // props ({"p":…,"title":…}) and, on page views and session starts, the
    // referrer and its host.
    let stored = 14 + ctxLen(c) + LIMITS.headers;
    const fixed = LIMITS.rowFixed + utf8Len(clip(nav.userAgent, LIMITS.str));
    const refLen = 2 * utf8Len(String(c.ref || ""));
    const parts: string[] = [];
    const batch: Rec[] = [];
    while (
      queue.length &&
      batch.length < LIMITS.batchEvents &&
      queue[0].s === s
    ) {
      const r = queue[0];
      const e: Props = { t: r.t, k: r.k };
      if (r.n != null) e.n = r.n;
      if (r.u !== url) {
        e.url = r.u;
        e.title = r.ti;
      }
      e.p = r.p;
      const j = safeJSON(e);
      const w = utf8Len(j) + (parts.length ? 1 : 0);
      const cost =
        fixed +
        r.k.length +
        (r.k === "custom"
          ? utf8Len(r.n || "")
          : NAMED.test(r.k)
            ? LIMITS.name
            : 16) +
        2 * utf8Len(r.u) +
        6 +
        goLen(safeJSON(r.p)) +
        (e.title ? 9 + goLen(safeJSON(e.title)) : 0) +
        (r.k === "page_view" || r.k === "session_start" ? refLen : 0);
      // An own row over its bound cannot happen with the bounds above (the
      // largest is about 12.5 KiB); should one of them grow, it is dropped.
      if (
        size + w > LIMITS.batchBytes ||
        stored + cost > LIMITS.stored ||
        cost > LIMITS.row
      ) {
        if (!parts.length || cost > LIMITS.row) {
          // Cannot fit even alone: drop it rather than block the queue.
          queue.shift();
          dropped++;
          continue;
        }
        break;
      }
      // Within the limits but not within what the browser has left: it
      // waits for a later request.
      if (size + w > room) break;
      size += w;
      stored += cost;
      parts.push(j);
      batch.push(queue.shift()!);
    }
    return batch.length ? [head + parts.join(",") + "]}", batch] : null;
  };

  const deliver = (body: string, batch: Rec[], final: boolean) => {
    const f = win.fetch;
    if (typeof f !== "function") {
      // A beacon cannot report a status; one refusal counts as a failure.
      const ok = (() => {
        try {
          return nav.sendBeacon ? nav.sendBeacon(endpoint, body) : false;
        } catch {
          return false;
        }
      })();
      if (ok) {
        failures = 0;
        backoff = 0;
      } else if (!nav.sendBeacon) {
        disabled = true;
        queue.length = 0;
      } else fail(0);
      return;
    }
    const n = utf8Len(body);
    carried += n;
    let p: Promise<Response>;
    try {
      p = f.call(win, endpoint, {
        method: "POST",
        body,
        keepalive: true,
        credentials: "omit",
        mode: "cors",
      });
    } catch {
      carried -= n;
      requeue(batch);
      fail(0);
      return;
    }
    p.then(
      (res) => {
        carried -= n;
        const st = res.status;
        // An answer's body unread holds its request's bytes against the
        // browser's 64 KiB keepalive quota: two refused batches left so
        // made every later send fail at once (seen with 429s in Chromium).
        try {
          res.body?.cancel().catch(() => {});
        } catch {
          // no stream to cancel
        }
        if (st >= 200 && st < 300) {
          failures = 0;
          backoff = 0;
          notBefore = asked = 0;
        } else if (st === 403) {
          // Unknown site: nothing here will ever be accepted.
          disabled = true;
          queue.length = 0;
          return;
        } else if (st === 408 || st === 429 || st >= 500) {
          // 408: the body came in too slowly, the batch itself is fine.
          let ra = 0;
          try {
            // Seconds or an HTTP date; anything else (NaN) counts as 0.
            const h = res.headers.get("Retry-After") || "";
            const s = Number(h);
            ra = isFinite(s) ? s * 1000 : Date.parse(h) - now();
          } catch {
            // no readable headers: the own back-off alone
          }
          requeue(batch);
          fail(ra, st === 429);
        } else {
          // 400, 413: this batch will never pass; do not resend it. The
          // tracker never builds one over a limit (see take).
          dropped += batch.length;
          fail(0);
        }
        if (queue.length && !disabled)
          schedule(Math.max(LIMITS.flushAfterMs, notBefore - now()));
      },
      () => {
        carried -= n;
        requeue(batch);
        // A final send shares the browser's keepalive quota with what the
        // host page sends as it closes: refused there, it is the browser's
        // refusal, not the collector's failure. It goes again later.
        if (!final) fail(0);
        if (queue.length && !disabled)
          schedule(
            Math.max(final ? LIMITS.flushAfterMs : 0, notBefore - now()),
          );
      },
    ).catch(() => {});
  };

  const flush = (final: boolean) => {
    if (disabled || !queue.length) return;
    // In normal operation one request at a time; its answer sends the rest.
    if (carried && !final) return;
    if (now() < (final ? asked : notBefore)) {
      if (!final) schedule(notBefore - now());
      return;
    }
    if (timer) {
      clearTimeout(timer);
      timer = 0;
    }
    // A final flush sends two batches at most, within what the browser's
    // keepalive quota has left beside the requests still in flight: a
    // batch over it would be refused.
    for (
      let rounds = final ? 2 : 1;
      rounds-- > 0 && queue.length && !disabled;
    ) {
      const b = take(LIMITS.quota - carried);
      if (!b) break;
      deliver(b[0], b[1], final);
    }
    if (queue.length && !disabled && !carried) schedule(LIMITS.flushAfterMs);
  };

  // ---- capture
  const guard = (f: () => void) => {
    try {
      f();
    } catch {
      // never into the host page
    }
  };
  const on = (
    target: EventTarget | null | undefined,
    type: string,
    fn: (e: Event) => void,
    opts?: AddEventListenerOptions | boolean,
  ) => {
    if (!target) return;
    const h = (e: Event) => guard(() => fn(e));
    try {
      target.addEventListener(type, h, opts);
      offs.push(() => target.removeEventListener(type, h, opts));
    } catch {
      // ignore
    }
  };

  // Engagement: visible and focused time, deepest scroll, input counts.
  let activeSince = 0;
  let activeMs = 0;
  let maxScroll = 0;
  let pageInputs = 0;
  const isActive = () => {
    try {
      return doc.visibilityState === "visible" && doc.hasFocus();
    } catch {
      return doc.visibilityState === "visible";
    }
  };
  const tick = () => {
    const t = now();
    if (activeSince) activeMs += t - activeSince;
    activeSince = isActive() ? t : 0;
    if (activeSince) {
      if (!liveFrom) {
        liveFrom = t;
        // A visible, focused stretch begins: activity, as input is.
        if (sid && !alive(t)) resume();
      }
    } else if (liveFrom) {
      // The stretch ends: the session was alive until now.
      touch(true);
      liveFrom = 0;
    }
  };
  tick();
  const engagement = (at?: [string, string]) => {
    tick();
    if (activeMs <= 0 && maxScroll <= 0 && pageInputs <= 0) return;
    record(
      "engagement",
      {
        ms: Math.round(activeMs),
        scroll: Math.round(maxScroll),
        inputs: { ...inputs },
      },
      undefined,
      at,
    );
    activeMs = 0;
    maxScroll = 0;
    pageInputs = 0;
  };
  const scrolled = () => {
    const el = doc.documentElement;
    const h = Math.max(el.scrollHeight, doc.body ? doc.body.scrollHeight : 0);
    if (h <= 0) return;
    const d = Math.min(100, ((win.scrollY + win.innerHeight) / h) * 100);
    if (d > maxScroll) maxScroll = d;
  };
  const count = (k: keyof typeof inputs) => () => {
    // First: input that resumes an expired session belongs to the new one.
    touch();
    if (inputs[k] < 1e6) inputs[k]++;
    pageInputs++;
    if (k === "scroll") scrolled();
  };
  on(doc, "pointerdown", count("pointer"), { capture: true, passive: true });
  on(doc, "keydown", count("key"), { capture: true, passive: true });
  on(doc, "touchstart", count("touch"), { capture: true, passive: true });
  on(win, "scroll", count("scroll"), { passive: true });
  on(win, "focus", tick);
  on(win, "blur", tick);

  // Page views, SPA navigation included. A navigation changes the path or
  // a client-side route in the fragment, never the query alone: a search box
  // that writes its text into the URL is one page. Each page has its own
  // budget of events, the document a higher one.
  // The view is queued at once, so closing the page cannot lose it, and its
  // title is taken when the page is left or the batch is built: routers set
  // it after pushState. The page being left is where the URL was before the
  // change (here), with the title it still shows.
  let lastPage = "";
  let here = "";
  const pageKey = () =>
    loc.pathname +
    (/^#[/!]/.test(loc.hash) ? loc.hash.replace(/\?[^]*$/, "") : "");
  const pageView = (force: boolean) => {
    const k = pageKey();
    const was = here;
    here = clipURL(loc.href);
    if (!force && k === lastPage) return;
    if (lastPage) {
      settle();
      engagement([was, doc.title]);
    }
    lastPage = k;
    pageCount = 0;
    errorsSeen.clear();
    view = record("page_view", {});
  };
  const hist = win.history;
  const orig: [string, unknown][] = [];
  for (const m of ["pushState", "replaceState"] as const) {
    try {
      const o = hist[m];
      if (typeof o !== "function") continue;
      orig.push([m, o]);
      hist[m] = function (
        this: History,
        ...a: Parameters<History["pushState"]>
      ) {
        const r = o.apply(this, a);
        guard(() => pageView(false));
        return r;
      };
    } catch {
      // a frozen history object
    }
  }
  on(win, "popstate", () => pageView(false));
  on(win, "hashchange", () => pageView(false));
  on(win, "pageshow", (e) => {
    if ((e as PageTransitionEvent).persisted) {
      activeSince = 0;
      tick();
      pageView(true);
    }
  });

  // Leaving or hiding the page: engagement, the vitals, then out.
  let vitalsSent = false;
  const hidden = () => {
    if (pre()) return;
    engagement();
    // A left page is no longer read: without this the visibilitychange
    // after a pagehide sends a second engagement of the moments between.
    activeSince = 0;
    if (!vitalsSent) {
      vitalsSent = true;
      vitalsFinal();
    }
    flush(true);
  };
  on(doc, "visibilitychange", () => {
    if (doc.visibilityState === "hidden") hidden();
    else tick();
  });
  on(win, "pagehide", hidden);

  // Outbound links and downloads: one listener on the document.
  const linkClick = (e: Event) => {
    const t = e.target as Element | null;
    const a =
      t && t.closest ? (t.closest("a[href]") as HTMLAnchorElement) : null;
    if (!a) return;
    const u = new URL(a.href, loc.href);
    if (u.protocol !== "http:" && u.protocol !== "https:") return;
    if (a.hasAttribute("download") || DOWNLOAD.test(u.pathname)) {
      record("download", { url: u.href });
    } else if (u.host !== loc.host) {
      record("outbound_click", {
        url: u.href,
        text: clip((a.textContent || "").trim().replace(/\s+/g, " "), 100),
      });
    }
  };
  on(doc, "click", linkClick, true);
  on(doc, "auxclick", linkClick, true);

  // Forms: first focus and submit; a search box's term. A form's controls
  // shadow its properties (an input named "id" is f.id): attributes only.
  // The only value ever read is a text or search input's, as a search term.
  const started = new WeakSet<object>();
  const formInfo = (f: HTMLFormElement): Props => ({
    id: clip(f.getAttribute("id"), 64),
    name: clip(f.getAttribute("name"), 64),
    // A URL without its fragment, then held to 512 bytes as "action".
    action: clipURL(f.getAttribute("action")),
  });
  on(
    doc,
    "focusin",
    (e) => {
      const f = (e.target as HTMLInputElement | null)?.form;
      if (!f || started.has(f)) return;
      started.add(f);
      record("form", { phase: "start", ...formInfo(f) });
    },
    true,
  );
  on(
    doc,
    "submit",
    (e) => {
      const f = e.target;
      if (!(f instanceof win.HTMLFormElement)) return;
      let term: string | null = null;
      for (const el of Array.from(f.elements) as HTMLInputElement[]) {
        if (
          el.tagName === "INPUT" &&
          (el.type === "search" ||
            (el.type === "text" && SEARCH_NAMES.test(el.name)))
        ) {
          term = el.value;
          break;
        }
      }
      if (term != null) record("search", { term: clip(term, 200) });
      else record("form", { phase: "submit", ...formInfo(f) });
    },
    true,
  );

  // Errors: script errors, failed resources, unhandled rejections.
  // Each message a few times; a bounded number of distinct ones per page
  // (the table starts over on a navigation), so a page throwing ever new
  // messages cannot grow the table, and a long single-page visit still
  // reports what breaks on its later pages, within a ceiling per document.
  const jsError = (msg: string, extra: Props) => {
    if (disabled || full(now())) return;
    const key = clip(msg, 200);
    const n = errorsSeen.get(key) || 0;
    if (
      n >= LIMITS.sameError ||
      (!n &&
        (errorsSeen.size >= LIMITS.errorKinds ||
          docErrors >= LIMITS.errorKindsPerDocument))
    )
      return;
    if (!n) docErrors++;
    errorsSeen.set(key, n + 1);
    record("js_error", { msg, ...extra });
  };
  on(
    win,
    "error",
    (e) => {
      const ev = e as ErrorEvent;
      const t = e.target as (Element & { src?: string; href?: string }) | null;
      if (t && t !== (win as unknown) && t.tagName) {
        const u = t.src || t.href || "";
        if (u) record("resource_error", { url: u });
        return;
      }
      jsError(ev.message || "Error", {
        src: ev.filename,
        line: ev.lineno || 0,
        col: ev.colno || 0,
      });
    },
    true,
  );
  on(win, "unhandledrejection", (e) => {
    const r = (e as PromiseRejectionEvent).reason as
      { message?: unknown } | undefined;
    jsError(
      "Unhandled rejection: " +
        String(r && typeof r === "object" && "message" in r ? r.message : r),
      {},
    );
  });

  // Web vitals, from the browser's own observers where it has them.
  let lcp = 0;
  let cls = 0;
  let inp = 0;
  const observers: PerformanceObserver[] = [];
  const observe = (
    type: string,
    cb: (list: PerformanceEntry[]) => void,
    extra?: Record<string, unknown>,
  ) => {
    try {
      const PO = win.PerformanceObserver;
      if (!PO || !(PO.supportedEntryTypes || []).includes(type)) return;
      const o = new PO((l) => guard(() => cb(l.getEntries())));
      o.observe({ type, buffered: true, ...extra } as PerformanceObserverInit);
      observers.push(o);
    } catch {
      // unsupported
    }
  };
  // Callers send only vitals that exist. Times since navigation start count
  // from activation for a prerendered page, never below 0.
  const vital = (name: string, value: number, shifted?: boolean) =>
    record("vital", {
      name,
      value:
        Math.round(
          Math.max(
            0,
            value - (shifted ? navEntry()?.activationStart || 0 : 0),
          ) * 1000,
        ) / 1000,
    });
  // First paint is kept: during prerendering it is sent on activation.
  let fcp = 0;
  let fcpSent = false;
  const sendFCP = () => {
    if (fcp && !fcpSent && !pre()) {
      fcpSent = true;
      vital("FCP", fcp, true);
    }
  };
  observe("paint", (l) => {
    for (const e of l)
      if (e.name === "first-contentful-paint") {
        fcp = e.startTime;
        sendFCP();
      }
  });
  observe("largest-contentful-paint", (l) => {
    for (const e of l) lcp = e.startTime;
  });
  observe("layout-shift", (l) => {
    for (const e of l) {
      const s = e as PerformanceEntry & {
        value: number;
        hadRecentInput: boolean;
      };
      if (!s.hadRecentInput) cls += s.value;
    }
  });
  observe(
    "event",
    (l) => {
      for (const e of l) {
        const x = e as PerformanceEntry & { interactionId?: number };
        if (x.interactionId && e.duration > inp) inp = e.duration;
      }
    },
    { durationThreshold: 40 },
  );
  const vitalsFinal = () => {
    if (lcp) vital("LCP", lcp, true);
    if (inp) vital("INP", inp);
    vital("CLS", cls);
  };

  // ---- the public API, and calls queued before the script loaded
  const event = (name: unknown, props?: unknown) =>
    guard(() => {
      if (disabled || full(now())) return;
      const n = clip(name, 128);
      if (n) record("custom", cleanProps(props), n);
    });
  const g = win as unknown as { nui?: NuiGlobal };
  const stub = g.nui;
  const pending = stub && Array.isArray(stub.q) ? (stub.q as unknown[]) : [];
  const api: Tracker = {
    event,
    flush: (final = false) => guard(() => flush(final)),
    stop: () => {
      for (const off of offs.splice(0)) guard(off);
      for (const o of observers.splice(0)) guard(() => o.disconnect());
      for (const [m, o] of orig.splice(0))
        guard(() => {
          (hist as unknown as Record<string, unknown>)[m] = o;
        });
      if (timer) clearTimeout(timer);
      timer = 0;
      disabled = true;
    },
    state: () => ({
      queued: queue.length,
      disabled,
      failures,
      backoff,
      dropped,
      errorKeys: errorsSeen.size,
      sid,
      vid,
    }),
  };
  try {
    g.nui = {
      event,
      q: { push: (a: unknown) => replay(a) },
      _t: api,
    };
  } catch {
    // a non-writable window.nui: custom events stay unavailable
  }
  function replay(a: unknown) {
    if (a && typeof a === "object" && "length" in a) {
      const args = a as ArrayLike<unknown>;
      event(args[0], args[1]);
    }
  }

  // ---- first page view, time to first byte; for a prerendered page once
  // it is shown, its time counted from then.
  const begin = () => {
    visAtLoad = doc.visibilityState;
    activeSince = liveFrom = 0;
    tick();
    pageView(true);
    begun = true;
    const n = navEntry();
    if (n && n.responseStart > 0) vital("TTFB", n.responseStart, true);
    sendFCP();
    for (const a of pending.slice(0, LIMITS.batchEvents))
      guard(() => replay(a));
  };
  if (pre()) on(doc, "prerenderingchange", begin, { once: true });
  else begin();
  return api;
}

type UAData = {
  mobile: boolean;
  platform: string;
  brands?: { brand: string; version: string }[];
  getHighEntropyValues?: (h: string[]) => Promise<{
    platformVersion?: string;
    model?: string;
    architecture?: string;
    bitness?: string;
  }>;
};

type Conn = {
  effectiveType?: string;
  downlink?: number;
  rtt?: number;
  saveData?: boolean;
};

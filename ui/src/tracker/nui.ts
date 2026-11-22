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
  errorKinds: 20,
};

const STORE_VID = "nui.vid";
const STORE_SID = "nui.sid";
const STORE_SEEN = "nui.seen";
const DOWNLOAD =
  /\.(zip|gz|tgz|tar|xz|bz2|7z|rar|pdf|csv|tsv|json|txt|xlsx?|docx?|pptx?|dmg|exe|msi|pkg|deb|rpm|apk|iso|mmdb|mrt)$/i;
const SEARCH_NAMES = /^(q|query|search|s|term)$/i;
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
// "%" is escaped and one cut in half by the clip is dropped.
export function clipURL(v: unknown): string {
  return clip(
    clip(v, LIMITS.url)
      .replace(/#(?![/!])[^]*$/, "")
      .replace(/%(?![\da-f]{2})/gi, "%25"),
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
  if (Array.isArray(v)) {
    const a: unknown[] = [];
    for (let i = 0; i < v.length && i < LIMITS.items && budget > 0; i++)
      a.push(bound(v[i], key, depth + 1));
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
  return utf8Len(safeJSON(out)) > LIMITS.props ? { dropped: true } : out;
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
  const offs: (() => void)[] = [];
  let pageCount = 0;
  let docCount = 0;
  let dropped = 0;
  let disabled = false;
  let failures = 0;
  let backoff = 0;
  let notBefore = 0;
  let timer: ReturnType<typeof setTimeout> | 0 = 0;
  let inFlight = false;
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
  const navType = (() => {
    try {
      const e = win.performance.getEntriesByType(
        "navigation",
      )[0] as PerformanceNavigationTiming;
      return e ? e.type : "";
    } catch {
      return "";
    }
  })();
  const visAtLoad = doc.visibilityState;
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
    return bound(c, "", -1) as Props;
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
  // event, so a tab follows the one another tab started.
  const session = (t: number): boolean => {
    sid = id(STORE_SID) || sid;
    const last = Number(get(STORE_SEEN) || 0);
    let fresh = false;
    if (!sid || !last || t - last > LIMITS.sessionMs) {
      sid = randomId(win);
      put(STORE_SID, sid);
      fresh = true;
    }
    put(STORE_SEEN, String(t));
    return fresh;
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
    if (disabled) return;
    const t = now();
    if (full(t)) return;
    if (session(t) && k !== "session_start") record("session_start", {});
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
  const fail = (retryMs: number) => {
    failures++;
    backoff = Math.min(
      LIMITS.backoffMaxMs,
      backoff ? backoff * 2 : LIMITS.backoffMs,
    );
    if (failures >= LIMITS.failures) {
      disabled = true;
      queue.length = 0;
      return;
    }
    notBefore =
      now() +
      Math.min(LIMITS.retryMaxMs, Math.max(backoff, retryMs > 0 ? retryMs : 0));
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

  const take = (): { body: string; batch: Rec[] } | null => {
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
      const s = safeJSON(e);
      const w = utf8Len(s) + (parts.length ? 1 : 0);
      if (size + w > LIMITS.batchBytes) {
        if (!parts.length) {
          // Cannot fit even alone: drop it rather than block the queue.
          queue.shift();
          dropped++;
          continue;
        }
        break;
      }
      size += w;
      parts.push(s);
      batch.push(queue.shift()!);
    }
    if (!batch.length) return null;
    return { body: head + parts.join(",") + "]}", batch };
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
    inFlight = !final;
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
      inFlight = false;
      requeue(batch);
      fail(0);
      return;
    }
    p.then(
      (res) => {
        inFlight = false;
        const st = res.status;
        if (st >= 200 && st < 300) {
          failures = 0;
          backoff = 0;
          notBefore = 0;
        } else if (st === 403) {
          // Unknown site: nothing here will ever be accepted.
          disabled = true;
          queue.length = 0;
          return;
        } else if (st === 429 || st >= 500) {
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
          fail(ra);
        } else {
          // 400, 413: this batch will never pass; do not resend it.
          dropped += batch.length;
          fail(0);
        }
        if (queue.length && !disabled)
          schedule(Math.max(LIMITS.flushAfterMs, notBefore - now()));
      },
      () => {
        inFlight = false;
        requeue(batch);
        fail(0);
        if (queue.length && !disabled) schedule(Math.max(0, notBefore - now()));
      },
    ).catch(() => {});
  };

  const flush = (final: boolean) => {
    if (disabled || !queue.length) return;
    if (inFlight && !final) return;
    if (now() < notBefore) {
      if (!final) schedule(notBefore - now());
      return;
    }
    if (timer) {
      clearTimeout(timer);
      timer = 0;
    }
    // One request in normal operation. A final flush sends two at most:
    // browsers cap the keepalive bodies in flight at 64 KiB together, and
    // a third batch would be refused and count as a failure.
    let rounds = final ? 2 : 1;
    while (rounds-- > 0 && queue.length && !disabled) {
      const b = take();
      if (!b) return;
      deliver(b.body, b.batch, final);
      if (inFlight) break;
    }
    if (queue.length && !disabled) schedule(LIMITS.flushAfterMs);
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
  };
  activeSince = isActive() ? now() : 0;
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
      activeSince = isActive() ? now() : 0;
      pageView(true);
    }
  });

  // Leaving or hiding the page: engagement, the vitals, then out.
  let vitalsSent = false;
  const hidden = () => {
    engagement();
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
    action: f.getAttribute("action"),
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
  // Each message a few times; a bounded number of distinct ones, so a page
  // throwing ever new messages cannot grow the table.
  const jsError = (msg: string, extra: Props) => {
    if (disabled || full(now())) return;
    const key = clip(msg, 200);
    const n = errorsSeen.get(key) || 0;
    if (n >= LIMITS.sameError || (!n && errorsSeen.size >= LIMITS.errorKinds))
      return;
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
  const vital = (name: string, value: number) => {
    if (value > 0 || name === "CLS")
      record("vital", { name, value: Math.round(value * 1000) / 1000 });
  };
  observe("paint", (l) => {
    for (const e of l)
      if (e.name === "first-contentful-paint") vital("FCP", e.startTime);
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
    if (lcp) vital("LCP", lcp);
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

  // ---- first page view, time to first byte
  pageView(true);
  try {
    const n = win.performance.getEntriesByType(
      "navigation",
    )[0] as PerformanceNavigationTiming;
    if (n && n.responseStart > 0) vital("TTFB", n.responseStart);
  } catch {
    // no navigation timing
  }
  for (const a of pending.slice(0, LIMITS.batchEvents)) guard(() => replay(a));
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

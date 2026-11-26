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
  // Strings under the keys url, ref, src and href; every other string.
  urlBytes: 2048,
  strBytes: 512,
  // Map keys, entries per map, entries per array, nesting.
  keyBytes: 64,
  mapKeys: 64,
  arrayItems: 50,
  maxDepth: 8,
  // Values visited per walk of a host page's props.
  nodes: 200,
  propsBytes: 4096,
  // The context without URL and referrer, as the collector encodes it.
  ctxBytes: 4096,
  // What the collector stores (store.Event.Size), held as an upper bound:
  // a batch's rows at most `stored`, an event's own row at most
  // `rowBytes`. Every row repeats the page's URL with its path; the first
  // one also holds the context and the request's headers (at most
  // `hdrBytes`).
  stored: 64 * 1024,
  rowBytes: 16 * 1024,
  hdrBytes: 2048,
  // A row's fixed part: overhead, source, visitor and session ids, an IPv6
  // address and its /64 key twice, a template, a country.
  rowFixed: 96 + 7 + 32 + 32 + 39 + 2 * 24 + 200 + 2,
  // An event name the collector derives (a host, a message, a URL).
  nameBytes: 200,
  queueMax: 100,
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
  // An error's stack: frames, bytes per frame.
  stackFrames: 8,
  frameBytes: 160,
  // How long a wrapped call's throw may take to the window's error event.
  // Nothing of the browser's lies between, but the page's finally blocks, its
  // microtasks (a framework's render) and its own error listeners do: a long
  // task's 50 ms, not the few an idle page needs. A throw held up longer is
  // reported as the browser's stand-in, never as another error.
  errorAgeMs: 50,
};

// On the product's origin, so namespaced, but never by this product's name
// (user): a rename would count every browser as new.
const STORE_VID = "analytics.vid";
const STORE_SID = "analytics.sid";
const STORE_SEEN = "analytics.seen";
const STORE_GL = "analytics.gl";
// Globals an automation tool leaves in the page, by their exact names (a
// page's own __pwa… or cdc_config is no tool's), one group per token sent.
// ChromeDriver's is cdc_ or $cdc_ and 22 letters, then a kind.
const AUTO_KEY = new RegExp(
  "^(?:(__playwright__binding__)|(\\$?cdc_[a-zA-Z0-9]{22}_[a-zA-Z]+)|" +
    "(\\$wdc_[a-zA-Z0-9]{22}_[a-zA-Z]+)|(domAutomation|domAutomationController)|" +
    "(_phantom|callPhantom)|(__nightmare)|" +
    "(_Selenium_IDE_Recorder|__selenium_evaluate|__selenium_unwrapped)|" +
    "(__webdriver_evaluate|__webdriver_script_fn|__webdriver_script_func|__webdriver_unwrapped|" +
    "__driver_evaluate|__driver_unwrapped|__fxdriver_evaluate|__fxdriver_unwrapped)|" +
    "(__lastWatirAlert))$",
);
const AUTO_TOKEN = [
  "playwright-binding",
  "chromedriver",
  "webdriver-wdc",
  "dom-automation",
  "phantomjs",
  "nightmare",
  "selenium",
  "webdriver-js",
  "watir",
];
// The version of the probes' meaning, sent with every context: a change to what one counts raises it, so figures
// taken before and after are told apart.
const PROBES = 2;
// The version of the vitals' definitions, sent with every vital: a change to what one measures raises it. 2 is Web
// Vitals' own (CLS the worst session window, INP the 98th percentile); a vital without one is from before: CLS the
// whole visit's sum, INP the slowest interaction.
const VITALS = 2;
// How long a measured renderer stands; a new browser version ends it sooner.
const GL_KEEP_MS = 30 * 24 * 60 * 60 * 1000;
// The input counts, per page and per document: scroll counts a script's scrolling too, the wheel a person's own;
// synth is the part of pointer, key and touch a script dispatched (isTrusted
// false). Password managers type this way too, and CDP-driven input is
// trusted: a figure to weigh, no verdict.
const noInputs = () => ({
  pointer: 0,
  key: 0,
  touch: 0,
  scroll: 0,
  move: 0,
  mdown: 0,
  wheel: 0,
  synth: 0,
});
const DOWNLOAD =
  /\.(zip|gz|tgz|tar|xz|bz2|7z|rar|pdf|csv|tsv|json|txt|xlsx?|docx?|pptx?|dmg|exe|msi|pkg|deb|rpm|apk|iso|mmdb|mrt)$/i;
const SEARCH_NAMES = /^(q|query|search|s|term)$/i;
// Kinds the collector names by a host, a message or a URL (LIMITS.nameBytes);
// every other kind but custom by at most 16 bytes, or not at all.
const NAMED = /^(outbound_click|download|js_error|resource_error)$/;
// A stack's frame is a head and a spot. The head is Chromium's "    at f ("
// or "    at " (no space is in a URL: the last " (" opens the spot), or
// Firefox's and WebKit's "f@", where a name with a space in it ("global
// code@", "setTimeout handler*f@") needs a URL behind it; older WebKit prints
// a nameless function's URL alone. The spot is, up to the line's end, a URL
// (or a sourceURL's bare name), what is cut off it (query, fragment) and the
// position; or it names no file. Each is tried at one place in a line, and
// nothing in them nests: hostile text costs its length.
const V8_HEAD = /^ +at (?:.* \(|async )?/;
const AT_HEAD =
  /^(?:(?=[a-z][\w+.-]*:\/\/)|.*?@(?=[a-z][\w+.-]*:\/\/)|[^@\s]*@)/i;
// Positions: ":1:2", a wasm offset, Firefox's " line 5 > eval:1:1" and
// Chromium's "https://…:5:6), <anonymous>:1:1)", both behind the place that
// called eval.
const SPOT = new RegExp(
  "^(?:" +
    "((?:[a-z][\\w+.-]*:\\/\\/|(?!\\S*@)[^\\s\\d:?#()<[])[^\\s?#]*)(?:[?#]\\S*)?" +
    "((?: line \\d{1,9} > [\\w.]+)*" +
    "(?::\\d{1,9}:\\d{1,9}(?:\\){1,9}, <anonymous>:\\d+:\\d+)?|:wasm-function\\[\\d+\\]:0x[\\da-f]+)\\)?)" +
    "|(?:<anonymous>|native|index \\d+|unknown \\w+|\\[(?:native|wasm) code\\])(?::\\d+:\\d+)?" +
    "(?:\\){1,9}, <anonymous>(?::\\d+:\\d+)?)?\\)?" +
    ")$",
  "i",
);
// A URL in a function's name.
const NAME_QUERY = /(:\/\/[^\s?#]*)[?#]\S*?(?=@?$|\s)/g;
const ID = /^[\da-f]{32}$/;
// Script hosts per document, those the first report may take, and the bytes
// of one: 20 at their longest (quoted, a comma) are 2,060 bytes, about half
// of LIMITS.propsBytes in one report. The hosts a document remembers.
const SCRIPT_HOSTS = 20;
const FIRST_HOSTS = 16;
const HOST_BYTES = 100;
const SEEN_HOSTS = 100;
// A host name as the URL parser gives it: labels of letters, digits, inner
// hyphens and underscores (hosts do carry them). The backend's hostName.
const HOSTNAME =
  /^[a-z\d_]([a-z\d_-]*[a-z\d_])?(\.[a-z\d_]([a-z\d_-]*[a-z\d_])?)*$/;
const IPV6 = /^\[[\da-f:]+\]$/;

// A URL's hostname as it is sent, "" for none: lower case without a final
// dot, an IPv6 address in its brackets (the parser's canonical form), a
// name past HOST_BYTES cut to its last labels (the domain stays, a name
// stays a name). The backend (watch.scriptHost) keeps the same forms.
export function scriptHost(hostname: string): string {
  let h = hostname.toLowerCase().replace(/\.$/, "");
  if (IPV6.test(h)) return h;
  if (!HOSTNAME.test(h)) return "";
  while (h.length > HOST_BYTES) {
    const i = h.indexOf(".");
    if (i < 0) return "";
    h = h.slice(i + 1);
  }
  return h;
}

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
    clip(v, LIMITS.urlBytes)
      .replace(/#(?![/!])[^]*$/, "")
      .replace(/%(?![\da-f]{2})/gi, "%25")
      .replace(/[|^]/g, (c) => (c === "|" ? "%7C" : "%5E")),
    LIMITS.urlBytes,
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
    return URL_KEY.test(key) ? clipURL(v) : clip(v, LIMITS.strBytes);
  if (typeof v === "number") return isFinite(v) ? v : null;
  if (typeof v === "boolean") return v;
  if (!v || typeof v !== "object" || depth >= LIMITS.maxDepth) return null;
  // Typed arrays by index too: for-in would first list every element.
  if (Array.isArray(v) || ArrayBuffer.isView(v)) {
    const l = v as ArrayLike<unknown>;
    const a: unknown[] = [];
    for (let i = 0; i < l.length && i < LIMITS.arrayItems && budget > 0; i++)
      a.push(bound(l[i], key, depth + 1));
    return a;
  }
  const out: Props = {};
  let n = 0;
  for (const k in v as Props) {
    if (++n > LIMITS.mapKeys || budget <= 0) break;
    if (!Object.prototype.hasOwnProperty.call(v, k)) continue;
    const c = clip(k, LIMITS.keyBytes);
    out[c] = bound((v as Props)[k], c, depth + 1);
  }
  return out;
}

// A thrown value's type, only where it is an error and its name could be a
// class's: any other object's name (a file's, a person's) is data. Another
// frame's error is no instanceof, one built on the prototype alone (an ES5
// subclass) has no tag. No stack is asked for: reading it makes the browser
// format it, and this is part of the message a report is counted by.
const TAG = /^\[object (?:Error|DOMException)\]$/;
const TYPE = /^[a-z_$][\w$]{0,63}$/i;
function errorType(v: unknown): string {
  try {
    if (v instanceof Error || TAG.test(Object.prototype.toString.call(v))) {
      const n = (v as Error).name as unknown;
      if (typeof n === "string" && TYPE.test(n)) return n;
    }
  } catch {
    // no type
  }
  return "";
}

// A thrown value's type and the top of its stack, as far as it has them. It
// is the page's: any getter may throw. Only lines that are frames: Chromium
// puts the message first, which would go out twice, unclipped. With own (this
// script's URL in a stack), where no event says it, its place too: the first
// frame in a file that is not own. A line that has a URL and is not understood
// ends the search, or its caller would be blamed.
export function errorProps(v: unknown, own?: string): Props {
  const out: Props = {};
  if (!v || typeof v !== "object") return out;
  const e = v as { message?: unknown; stack?: unknown };
  const type = errorType(v);
  if (type) out.type = type;
  try {
    const raw = e.stack;
    if (typeof raw !== "string") return out;
    let s = raw;
    const msg = e.message;
    if (typeof msg === "string" && msg)
      for (const i of [0, s.slice(0, 256).indexOf(": ") + 2]) {
        const end = i + msg.length;
        if (s.startsWith(msg, i) && /^(?:\n|$)/.test(s.slice(end, end + 1))) {
          s = s.slice(end);
          break;
        }
      }
    const lines = s.slice(0, 8192).split(/\n/);
    // The last one may be cut.
    if (s.length > 8192) lines.pop();
    const v8 = lines.some((l) => V8_HEAD.test(l));
    const stack: string[] = [];
    let placed = own == null;
    for (const l of lines) {
      const h = (v8 ? V8_HEAD : AT_HEAD).exec(l);
      const m = h && SPOT.exec(l.slice(h[0].length));
      if (!h || !m) {
        if ((h || !v8) && /:\/\/./.test(l)) placed = true;
        continue;
      }
      if (!placed && m[1] && m[1] !== own) {
        placed = true;
        out.src = m[1];
        const at = /^(?::| line )(\d+)(?::(\d+))?/.exec(m[2]);
        out.line = at ? +at[1] : 0;
        out.col = at ? +(at[2] || 0) : 0;
      }
      // Too long a frame keeps its position.
      const tail = m[2] || "";
      const text =
        clip(h[0].trimStart(), LIMITS.frameBytes).replace(NAME_QUERY, "$1") +
        (m[1] || m[0]);
      stack.push(
        utf8Len(text + tail) > LIMITS.frameBytes
          ? clip(
              clip(text, LIMITS.frameBytes - 3 - tail.length) + "..." + tail,
              LIMITS.frameBytes,
            )
          : text + tail,
      );
      if (stack.length >= LIMITS.stackFrames) break;
    }
    if (stack.length) out.stack = stack;
  } catch {
    // no stack
  }
  return out;
}

// An event's props within the collector's limits; over the per-event
// budget they are dropped, not the event.
function fit(p: unknown): Props {
  budget = LIMITS.nodes;
  const out = bound(p, "p", 0) as Props;
  // As sent and as stored: the collector takes 4 KiB of both.
  return goLen(safeJSON(out)) > LIMITS.propsBytes ? { dropped: true } : out;
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
    const key = clip(k, LIMITS.keyBytes);
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

type TrakkGlobal = {
  q?: unknown;
  event?: unknown;
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
    const el = doc.querySelector<HTMLScriptElement>('script[src*="/trakk.js"]');
    src = el ? el.src : "";
  }
  if (!src) return null;
  const own = new URL(src, loc.href);
  const endpoint = own.origin + "/api/in";
  // This script as a stack's frames name it.
  const ownFile = own.origin + own.pathname;
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
    if (view) view.ti = clip(doc.title, LIMITS.strBytes);
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
  const inputs = noInputs();
  let uach: Props | undefined;
  const entries = (type: string): PerformanceEntry[] => {
    try {
      return win.performance.getEntriesByType(type);
    } catch {
      return [];
    }
  };
  const navEntry = () =>
    entries("navigation")[0] as
      (PerformanceNavigationTiming & { activationStart?: number }) | undefined;
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

  // ---- automation probes, read by the classifier. Only tokens of our own
  // go out, never a name the page chose. Taken again with every context:
  // ChromeDriver marks the document once it first runs a command.
  const autoSeen: string[] = [];
  const autoHit = (t: string) => {
    if (autoSeen.indexOf(t) < 0 && autoSeen.length < 16) autoSeen.push(t);
  };
  const autoTake = () => {
    for (const o of [win, doc] as object[])
      for (const k of Object.getOwnPropertyNames(o)) {
        const m = AUTO_KEY.exec(k);
        if (m)
          autoHit(AUTO_TOKEN[m.findIndex((g, i) => i > 0 && g != null) - 1]);
      }
    const el = doc.documentElement;
    for (const a of ["webdriver", "selenium", "driver"])
      if (el && el.getAttribute(a) != null) autoHit("selenium-attr");
    return autoSeen.slice();
  };
  // Where navigator.webdriver is defined: on the prototype as every browser
  // has it, on the instance (patched), or nowhere (deleted).
  const wdd = () =>
    Object.getOwnPropertyDescriptor(nav, "webdriver")
      ? "own"
      : Object.getOwnPropertyDescriptor(Object.getPrototypeOf(nav), "webdriver")
        ? "proto"
        : "none";
  // Notification permission against the Permissions API's answer: headless
  // Chrome and stealth plugins disagree with themselves.
  let perm: [string, string] | undefined;
  // The WebGL renderer: SwiftShader and llvmpipe are no person's GPU, a
  // renderer not in ANGLE's form on desktop Chrome a spoofed one. Kept per
  // site in each browser, with the time and the browser it was taken in: a
  // remote desktop once, or an old version, is not the browser's renderer for
  // good. "-" in the vendor's place marks a browser where asking threw.
  let gl: [string, string] | undefined;
  // Why there is no renderer, sent in its place.
  let glw = "";
  const browser = clip(nav.userAgent, 256);
  try {
    const v = JSON.parse(get(STORE_GL) || "null") as unknown;
    if (
      Array.isArray(v) &&
      v.length === 4 &&
      typeof v[0] === "string" &&
      typeof v[1] === "string" &&
      v[3] === browser &&
      typeof v[2] === "number" &&
      now() - v[2] >= 0 &&
      now() - v[2] < GL_KEEP_MS
    ) {
      if (v[0] === "-") glw = "threw";
      else gl = [clip(v[0], 128), clip(v[1], 128)];
    }
  } catch {
    // the page wrote something else there
  }
  const glKeep = (ven: string, ren: string) =>
    put(STORE_GL, JSON.stringify([ven, ren, now(), browser]));
  const glTake = () => {
    if (gl || disabled) return;
    // An offscreen canvas: no element in the page's document.
    const OC = (
      win as { OffscreenCanvas?: new (w: number, h: number) => OffscreenCanvas }
    ).OffscreenCanvas;
    if (!OC) {
      glw = "no-canvas";
      return;
    }
    try {
      const g = new OC(1, 1).getContext(
        "webgl",
      ) as WebGLRenderingContext | null;
      if (!g) {
        // No context: for now (the GPU process restarting, too many
        // contexts) or for good (Chrome without a GPU and its software
        // renderer): said for this page, asked again on the next.
        gl = ["", ""];
        return;
      }
      let ven = String(g.getParameter(g.VENDOR) ?? "");
      let ren = String(g.getParameter(g.RENDERER) ?? "");
      // Chrome and Safari mask RENDERER; Firefox gives the real one there
      // and warns when the debug extension is asked for.
      if (/^WebKit/.test(ren)) {
        const x = g.getExtension("WEBGL_debug_renderer_info");
        if (x) {
          ven = String(g.getParameter(x.UNMASKED_VENDOR_WEBGL) ?? "");
          ren = String(g.getParameter(x.UNMASKED_RENDERER_WEBGL) ?? "");
        }
      }
      // Released at once: the page's own contexts count against the
      // browser's limit, the oldest lost beyond it.
      g.getExtension("WEBGL_lose_context")?.loseContext();
      // A context lost as it was made names none.
      if (!ren) {
        glw = "lost";
        return;
      }
      gl = [clip(ven, 128), clip(ren, 128)];
      glKeep(gl[0], gl[1]);
    } catch {
      glw = "threw";
      glKeep("-", "");
    }
  };
  // Started with the first page view: never for a prerendered page nobody
  // opened. Making a WebGL context took up to 411 ms (the GPU process
  // starts): only in an idle period with most of its time left, never
  // forced; without idle callbacks (Safari) not at all.
  const probeStart = () => {
    try {
      const np = (win as { Notification?: { permission?: string } })
        .Notification?.permission;
      nav.permissions
        ?.query({ name: "notifications" as PermissionName })
        .then((s) => {
          perm = [String(np ?? ""), s.state];
        })
        .catch(() => {});
    } catch {
      // no Permissions API
    }
    const w = win as {
      requestIdleCallback?: (f: (d: IdleDeadline) => void) => number;
      cancelIdleCallback?: (h: number) => void;
    };
    if (gl || glw) return;
    if (!w.requestIdleCallback || !w.cancelIdleCallback) {
      glw = "no-idle";
      return;
    }
    glw = "pending";
    let h = 0;
    let tries = 0;
    const run = (d: IdleDeadline) =>
      guard(() => {
        if (d.timeRemaining() >= 40) {
          glw = "";
          glTake();
        } else if (++tries < 20) h = w.requestIdleCallback!.call(win, run);
        else glw = "busy";
      });
    h = w.requestIdleCallback.call(win, run);
    offs.push(() => w.cancelIdleCallback!.call(win, h));
  };
  // Clicks a script made (element.click() fires no pointer or key event). A
  // page's own code does that too: a figure to weigh.
  let sclick = 0;

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
    set("auto", autoTake);
    set("wdd", wdd);
    set("outer", () => [win.outerWidth, win.outerHeight]);
    set("plg", () => [
      nav.plugins ? nav.plugins.length : null,
      (nav as Navigator & { pdfViewerEnabled?: boolean }).pdfViewerEnabled ??
        null,
    ]);
    set("perm", () => perm);
    set("gl", () => gl);
    set("glw", () => (gl ? undefined : glw || undefined));
    set("synth", () => inputs.synth);
    set("sclick", () => sclick);
    // Whether the page was ever activated: Playwright's evaluate does it
    // without any input (measured), a press in an iframe or before the script
    // loaded too.
    set(
      "act",
      () =>
        (nav as Navigator & { userActivation?: { hasBeenActive: boolean } })
          .userActivation?.hasBeenActive,
    );
    set("pv", () => PROBES);
    // Each value is checked at depth 0, like an event's p.
    budget = LIMITS.nodes;
    const out = bound(c, "", -1) as Props;
    // Over the context's limit only with odd client hints or an odd title:
    // the longest values go, in this order, until it fits; what is left is
    // the browser's short figures. The probes go last: the classifier reads
    // them.
    for (const k of [
      "uach",
      "langs",
      "title",
      "lang",
      "tz",
      "conn",
      "gl",
      "auto",
    ])
      if (ctxLen(out) > LIMITS.ctxBytes) delete out[k];
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
  // The session of the document's first page view.
  let arrival = "";
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
      queue.length >= LIMITS.queueMax
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
      ti: clip(at ? at[1] : doc.title, LIMITS.strBytes),
    };
    if (n != null) r.n = clip(n, LIMITS.strBytes);
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
    const room = LIMITS.queueMax - queue.length;
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
    // The referrer brought the visitor to the document's first session; a
    // later one is a return, not an arrival from there.
    if (s !== arrival) delete c.ref;
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
    let stored = 14 + ctxLen(c) + LIMITS.hdrBytes;
    const fixed =
      LIMITS.rowFixed + utf8Len(clip(nav.userAgent, LIMITS.strBytes));
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
            ? LIMITS.nameBytes
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
        cost > LIMITS.rowBytes
      ) {
        if (!parts.length || cost > LIMITS.rowBytes) {
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
  // The page's input counts; the context carries the document's.
  let pageIn = noInputs();
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
  // The hosts the page's scripts came from, so a new one (an injected
  // skimmer) stands out. The host only, never the URL: a query can carry a
  // key. Each goes out once per document, with the next engagement; the
  // page's own and the collector's are no news.
  // The first report takes at most FIRST_HOSTS: the rest of the 20 wait for
  // hosts that show up later, an injected script's. Every host seen before
  // that report is remembered (up to SEEN_HOSTS), sent or not, so the
  // page's own overflow never takes a later place.
  const scriptHosts: string[] = [];
  const seenHosts: string[] = [];
  let scriptsSent = 0;
  let firstReport = true;
  const known = [site, new URL(src, loc.href).hostname].map((h) =>
    h.toLowerCase().replace(/\.$/, ""),
  );
  const addScript = (u: unknown) => {
    // The URL whole, never cut: a cut inside a long userinfo would name
    // another host. Only http(s) is parsed: no inline script's "", no data:
    // URL.
    const s = String(u);
    if (scriptHosts.length >= SCRIPT_HOSTS || !/^https?:/i.test(s)) return;
    let h: string;
    try {
      const x = new URL(s);
      if (x.protocol !== "http:" && x.protocol !== "https:") return;
      h = scriptHost(x.hostname);
    } catch {
      return;
    }
    if (
      !h ||
      known.indexOf(h) >= 0 ||
      seenHosts.indexOf(h) >= 0 ||
      scriptHosts.indexOf(h) >= 0
    )
      return;
    if (firstReport) {
      if (seenHosts.length >= SEEN_HOSTS) return;
      seenHosts.push(h);
      if (scriptHosts.length < FIRST_HOSTS) scriptHosts.push(h);
      return;
    }
    if (seenHosts.length < SEEN_HOSTS) seenHosts.push(h);
    scriptHosts.push(h);
  };
  const engagement = (at?: [string, string]) => {
    tick();
    measure();
    if (activeMs <= 0 && maxScroll <= 0 && pageInputs <= 0) {
      // Mouse travel alone is no engagement, nor the next page's; on a page
      // that stays it belongs to the press to come.
      if (at) pageIn = noInputs();
      return;
    }
    // The script elements too: where the browser has no resource entries,
    // or its buffer of them was full before this script began.
    guard(() => {
      const all = doc.scripts;
      for (let i = 0; i < all.length && scriptHosts.length < SCRIPT_HOSTS; i++)
        addScript(all[i].src);
    });
    const p: Props = {
      ms: Math.round(activeMs),
      scroll: Math.round(maxScroll),
      inputs: pageIn,
    };
    const fresh = scriptHosts.slice(scriptsSent);
    if (fresh.length) p.scripts = fresh;
    // Sent only if the props kept them: over the budget fit drops them all
    // (not reached today, 20 hosts at their longest are half of it). A batch
    // the collector refuses later is not sent again for them.
    const r = record("engagement", p, undefined, at);
    if (r && (!fresh.length || r.p.scripts)) {
      scriptsSent += fresh.length;
      firstReport = false;
    }
    activeMs = 0;
    maxScroll = 0;
    pageInputs = 0;
    pageIn = noInputs();
  };
  // The scroll depth is measured at most once a frame, and when the
  // engagement is taken; never in the scroll handler: the page's height
  // and scroll position force a layout, on every event of a page whose own
  // scroll handler changes the DOM.
  let scrolled = false;
  const measure = () => {
    if (!scrolled) return;
    scrolled = false;
    const el = doc.documentElement;
    const h = Math.max(el.scrollHeight, doc.body ? doc.body.scrollHeight : 0);
    if (h <= 0) return;
    const d = Math.min(100, ((win.scrollY + win.innerHeight) / h) * 100);
    if (d > maxScroll) maxScroll = d;
  };
  const bump = (k: keyof typeof inputs) => {
    if (inputs[k] < 1e6) inputs[k]++;
    if (pageIn[k] < 1e6) pageIn[k]++;
  };
  const count = (k: keyof typeof inputs) => (e: Event) => {
    // First: input that resumes an expired session belongs to the new one.
    touch();
    bump(k);
    if (k !== "scroll" && !e.isTrusted) {
      bump("synth");
    }
    // A mouse press, beside every press: a person's hand travels to what it
    // clicks, a script's mouse jumps there in one move.
    if (k === "pointer" && (e as PointerEvent).pointerType === "mouse")
      bump("mdown");
    pageInputs++;
    if (k === "scroll" && !scrolled) {
      scrolled = true;
      try {
        win.requestAnimationFrame(() => guard(measure));
      } catch {
        // no frames: measured with the engagement
      }
    }
  };
  on(doc, "pointerdown", count("pointer"), { capture: true, passive: true });
  on(doc, "keydown", count("key"), { capture: true, passive: true });
  on(doc, "touchstart", count("touch"), { capture: true, passive: true });
  on(win, "scroll", count("scroll"), { passive: true });
  // Mouse travel: counted only, no activity of its own (a resting hand
  // jitters), and as cheap as a handler can be: it fires on every frame.
  on(
    doc,
    "pointermove",
    (e) => {
      if ((e as PointerEvent).pointerType === "mouse") bump("move");
    },
    { capture: true, passive: true },
  );
  // The wheel tells a person's scrolling from a script's. Activity of its
  // own: in a nested box or at the page's end no scroll event follows.
  on(
    doc,
    "wheel",
    (e) => {
      if (!e.isTrusted) return;
      touch();
      bump("wheel");
      pageInputs++;
    },
    { capture: true, passive: true },
  );
  // A click by a script. The stack of one made inside Playwright's evaluate
  // holds its helper's frame (measured; Chromium only, WebKit names no
  // class): only the token goes out, never the stack. Looked at for a few
  // clicks, then no more: a page may click by the thousand.
  on(
    doc,
    "click",
    (e) => {
      if (e.isTrusted) return;
      if (sclick < 1e6) sclick++;
      if (sclick <= 10 && autoSeen.indexOf("playwright-evaluate") < 0) {
        // The default ten frames may all be the page's own wrappers'.
        const V8 = Error as { stackTraceLimit?: number };
        const limit = V8.stackTraceLimit;
        const raise = (n: number) =>
          guard(() => {
            if (typeof limit === "number") V8.stackTraceLimit = n;
          });
        raise(Math.max(limit || 0, 50));
        guard(() => {
          // As text, or the frames a page's prepareStackTrace returns.
          if (
            /(?:^|[\s,])UtilityScript\.evaluate \(<anonymous>:/.test(
              String(new Error().stack),
            )
          )
            autoHit("playwright-evaluate");
        });
        raise(limit || 0);
      }
    },
    { capture: true, passive: true },
  );
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
  // The page the vitals belong to, the document's first (or the one shown
  // again from the back/forward cache), with the title it showed last while
  // still shown: a change of the query alone leaves it shown.
  let landing: [string, string] = ["", ""];
  let onLanding = false;
  const land = () => {
    landing = [here, doc.title];
    onLanding = true;
  };
  const landed = () => {
    if (onLanding) landing[1] = doc.title;
  };
  const pageKey = () =>
    loc.pathname +
    (/^#[/!]/.test(loc.hash) ? loc.hash.replace(/\?[^]*$/, "") : "");
  const pageView = (force: boolean) => {
    const k = pageKey();
    landed();
    const was = here;
    here = clipURL(loc.href);
    if (!force && k === lastPage) return;
    onLanding = false;
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
  // What a wrapped call threw last, boxed (anything can be thrown): with
  // this script in the call stack the browser hides it from the window's
  // error event that follows if nobody catches it. One, the latest, and only
  // until its task is over: the value is the page's, and a caught one must
  // not explain a later error. A microtask is too early: listeners and timers
  // run theirs before the exception is reported. One timer is enough however
  // often a task throws: none fires inside it. A hidden tab's fires up to a
  // minute late, so what was thrown longer ago than errorAgeMs is ignored
  // too. The limit: after a caught throw, another hidden error within that
  // time, in that task or in one that runs ahead of the timer, takes its text
  // (measured with timers due together).
  let thrown: [unknown, number] | undefined;
  let thrownTimer: ReturnType<typeof setTimeout> | 0 = 0;
  const keep = (v: unknown) => {
    // First the timer: without one nothing is kept.
    if (!thrownTimer)
      thrownTimer = setTimeout(() => {
        thrownTimer = 0;
        thrown = undefined;
      }, 0);
    thrown = [v, now()];
  };
  for (const m of ["pushState", "replaceState"] as const) {
    try {
      const o = hist[m];
      if (typeof o !== "function") continue;
      orig.push([m, o]);
      hist[m] = function (
        this: History,
        ...a: Parameters<History["pushState"]>
      ) {
        let r;
        try {
          r = o.apply(this, a);
        } catch (e) {
          guard(() => keep(e));
          throw e;
        }
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
      restored();
    }
  });

  // Leaving or hiding the page: engagement, the vitals, then out. Paints
  // after the page was first hidden are no one's experience (Web Vitals).
  let vitalsSent = false;
  let firstHidden = Infinity;
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
  on(doc, "visibilitychange", (e) => {
    if (doc.visibilityState === "hidden") {
      firstHidden = Math.min(firstHidden, e.timeStamp);
      hidden();
    } else tick();
  });
  on(win, "pagehide", hidden);

  // Outbound links and downloads: one listener on the document.
  const linkClick = (e: Event) => {
    // auxclick is every other button's: the middle one opens the link, the
    // right one a menu.
    if (e.type === "auxclick" && (e as MouseEvent).button !== 1) return;
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
  // The message is the page's to make and the rest its stack to read (which
  // the browser then formats, with the page's prepareStackTrace): neither
  // once it would not be sent.
  const jsError = (text: () => string, rest: () => Props) => {
    if (disabled || full(now())) return;
    const msg = text();
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
    const p: Props = { msg, ...rest() };
    // Over the props' limit (a long URL): the stack goes, not the message.
    if (fit(p).dropped) delete p.stack;
    record("js_error", p);
  };
  // A thrown value's text; like its getters, the page's to throw in.
  const said = (v: unknown) =>
    String(v && typeof v === "object" && "message" in v ? v.message : v);
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
      const src = ev.filename || "";
      // Any error uses up what a wrapped call threw; only a hidden one may
      // be it, and only one that follows it at once (a clock set back tells
      // nothing).
      let hid = thrown;
      thrown = undefined;
      const age = hid ? now() - hid[1] : -1;
      if (
        age < 0 ||
        age > LIMITS.errorAgeMs ||
        src ||
        ev.lineno ||
        !/^script error\.?$/i.test(ev.message)
      )
        hid = undefined;
      // Out of a wrapped call the event names this script (WebKit, for a
      // script's top-level code): the place is its caller's, or not known.
      const ours = hid || src.replace(/[?#][^]*$/, "") === ownFile;
      jsError(
        () => {
          if (!hid) return ev.message || "Error";
          const v = hid[0];
          let msg = "";
          guard(() => (msg = said(v)));
          // As the browsers word it.
          return [errorType(v), msg].filter(Boolean).join(": ") || "Error";
        },
        () => ({
          src: ours ? "" : src,
          line: (!ours && ev.lineno) || 0,
          col: (!ours && ev.colno) || 0,
          ...errorProps(hid ? hid[0] : ev.error, ours ? ownFile : undefined),
        }),
      );
    },
    true,
  );
  on(win, "unhandledrejection", (e) => {
    const r = (e as PromiseRejectionEvent).reason as unknown;
    if (thrown && thrown[0] === r) thrown = undefined;
    jsError(
      () => "Unhandled rejection: " + said(r),
      () => errorProps(r, ownFile),
    );
  });

  // Web vitals, from the browser's own observers where it has them, as Web
  // Vitals defines them (VITALS). Each observer keeps its callback to drain
  // what is still buffered when the page is hidden.
  let lcp = 0;
  const observers: [PerformanceObserver, () => void][] = [];
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
      return observers.push([o, () => guard(() => cb(o.takeRecords()))]);
    } catch {
      // unsupported
    }
  };
  // Callers send only vitals that exist, for the landing page. Times since
  // navigation start count from activation for a prerendered page, never
  // below 0.
  const vital = (name: string, value: number, shifted?: boolean) => {
    landed();
    record(
      "vital",
      {
        name,
        value:
          Math.round(
            Math.max(
              0,
              value - (shifted ? navEntry()?.activationStart || 0 : 0),
            ) * 1000,
          ) / 1000,
        v: VITALS,
      },
      undefined,
      landing,
    );
  };
  // First paint is kept: during prerendering it is sent on activation.
  let fcp = 0;
  let fcpSent = false;
  const sendFCP = () => {
    if (fcp && !fcpSent && !pre() && fcp < firstHidden) {
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
    for (const e of l) if (e.startTime < firstHidden) lcp = e.startTime;
  });
  // CLS: the worst session window, shifts less than 1 s apart within 5 s,
  // those right after input left out. Sent only after a first paint or a
  // restore.
  let cls = 0;
  let sum = 0;
  let from = 0;
  let last = 0;
  const shifts = observe("layout-shift", (l) => {
    for (const e of l) {
      const s = e as PerformanceEntry & {
        value: number;
        hadRecentInput: boolean;
      };
      if (s.hadRecentInput) continue;
      const t = s.startTime;
      if (sum && t - last < 1000 && t - from < 5000) sum += s.value;
      else {
        sum = s.value;
        from = t;
      }
      last = t;
      if (sum > cls) cls = sum;
    }
  });
  // INP: the ten longest interactions, each at its longest entry; one in 50
  // interactions is left out from the top, so the value is the 98th
  // percentile. Where the browser counts no interactions their ids do,
  // about 7 apart, every interaction's, the fast ones included (as the Web
  // Vitals polyfill). Only where entries carry an interaction id.
  const longest: [number, number][] = [];
  let lo = Infinity;
  let hi = 0;
  let base = 0;
  const perf = win.performance as Performance & { interactionCount?: number };
  const interactions = () =>
    perf.interactionCount ?? (hi ? (hi - lo) / 7 + 1 : 0);
  const idOf = (e: PerformanceEntry) =>
    (e as PerformanceEntry & { interactionId?: number }).interactionId;
  const interact = (l: PerformanceEntry[]) => {
    for (const e of l) {
      const id = idOf(e);
      if (!id) continue;
      const x = longest.find((a) => a[0] === id);
      if (!x) longest.push([id, e.duration]);
      else if (e.duration > x[1]) x[1] = e.duration;
      longest.sort((a, b) => b[1] - a[1]).splice(10);
    }
  };
  const PET = (win as { PerformanceEventTiming?: { prototype: object } })
    .PerformanceEventTiming;
  if ("interactionId" in (PET?.prototype || {})) {
    observe("event", interact, { durationThreshold: 40 });
    observe("first-input", interact);
    if (!("interactionCount" in perf))
      observe(
        "event",
        (l) => {
          for (const e of l) {
            const id = idOf(e);
            if (id) {
              lo = Math.min(lo, id);
              hi = Math.max(hi, id);
            }
          }
        },
        { durationThreshold: 0 },
      );
  }
  // Scripts as they load, those before the tracker included (buffered).
  observe("resource", (l) => {
    if (scriptHosts.length >= SCRIPT_HOSTS) return;
    for (const e of l)
      if ((e as PerformanceResourceTiming).initiatorType === "script")
        addScript(e.name);
  });
  const vitalsFinal = () => {
    for (const o of observers) o[1]();
    if (lcp && lcp < firstHidden) vital("LCP", lcp, true);
    if (longest.length)
      vital(
        "INP",
        longest[
          Math.min(longest.length - 1, Math.floor((interactions() - base) / 50))
        ][1],
      );
    if (shifts && fcpSent) vital("CLS", cls);
  };
  // Back from the back/forward cache: CLS and INP start over and go out
  // again when the page is hidden, for the page shown now; the paints and
  // the first byte were the load's. A restored page is shown: CLS needs no
  // paint of its own, even where the load, hidden, sent none.
  const restored = () => {
    lcp = cls = sum = 0;
    longest.length = 0;
    base = interactions();
    land();
    fcpSent = true;
    vitalsSent = false;
  };

  // ---- the public API, and calls queued before the script loaded
  const event = (name: unknown, props?: unknown) =>
    guard(() => {
      if (disabled || full(now())) return;
      const n = clip(name, 128);
      if (n) record("custom", cleanProps(props), n);
    });
  const g = win as unknown as { trakk?: TrakkGlobal };
  const stub = g.trakk;
  const pending = stub && Array.isArray(stub.q) ? (stub.q as unknown[]) : [];
  try {
    g.trakk = {
      event,
      q: { push: (a: unknown) => replay(a) },
    };
  } catch {
    // a non-writable window.trakk: custom events stay unavailable
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
    arrival = sid;
    begun = true;
    land();
    // Hidden before the script ran: the browser's record of it where it
    // keeps one, the hiding during prerendering left out; else hidden now
    // is taken as hidden from the start.
    const n = navEntry();
    firstHidden =
      entries("visibility-state").find(
        (e) => e.name === "hidden" && e.startTime >= (n?.activationStart || 0),
      )?.startTime ?? (doc.visibilityState === "hidden" ? 0 : Infinity);
    if (n && n.responseStart > 0) vital("TTFB", n.responseStart, true);
    sendFCP();
    for (const a of pending.slice(0, LIMITS.batchEvents))
      guard(() => replay(a));
    guard(probeStart);
  };
  if (pre()) on(doc, "prerenderingchange", begin, { once: true });
  else begin();
  // The handle is for tests: nothing on a page can reach it, so the
  // shipped script carries none.
  if (import.meta.env.MODE === "production") return {} as Tracker;
  return {
    event,
    flush: (final = false) => guard(() => flush(final)),
    stop: () => {
      for (const off of offs.splice(0)) guard(off);
      for (const [o] of observers.splice(0)) guard(() => o.disconnect());
      for (const [m, o] of orig.splice(0))
        guard(() => {
          (hist as unknown as Record<string, unknown>)[m] = o;
        });
      if (timer) clearTimeout(timer);
      if (thrownTimer) clearTimeout(thrownTimer);
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

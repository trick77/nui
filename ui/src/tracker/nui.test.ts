import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import source from "./nui.ts?raw";
import { LIMITS, cleanProps, clip, start, utf8Len, type Tracker } from "./nui";

type Body = {
  v: number;
  site: string;
  vid: string;
  sid: string;
  ctx: Record<string, unknown>;
  events: {
    k: string;
    n?: string;
    url?: string;
    p: Record<string, unknown>;
  }[];
};

const SRC = "https://collector.example.com/nui.js";
const win = window as Window & typeof globalThis;

let fetchMock: ReturnType<typeof vi.fn>;
let tracker: Tracker | null = null;

const bodies = (): Body[] =>
  fetchMock.mock.calls.map((c) =>
    JSON.parse((c[1] as RequestInit).body as string),
  );
const kinds = () => bodies().flatMap((b) => b.events.map((e) => e.k));
const reply = (status: number, headers?: Record<string, string>) =>
  Promise.resolve(new Response(null, { status, headers }));

function boot(): Tracker {
  tracker = start(win, SRC);
  expect(tracker).not.toBeNull();
  return tracker!;
}

beforeEach(() => {
  vi.useFakeTimers({ now: new Date("2026-11-22T12:00:00Z") });
  localStorage.clear();
  delete (win as { nui?: unknown }).nui;
  fetchMock = vi.fn(() => reply(204));
  vi.stubGlobal("fetch", fetchMock);
  history.replaceState(null, "", "/");
  document.title = "Home";
});

afterEach(() => {
  tracker?.stop();
  tracker = null;
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe("strings", () => {
  it("counts UTF-8 bytes", () => {
    expect(utf8Len("abc")).toBe(3);
    expect(utf8Len("ä")).toBe(2);
    expect(utf8Len("€")).toBe(3);
    expect(utf8Len("😀")).toBe(4);
  });

  it("clips by bytes without splitting a pair", () => {
    expect(clip("x".repeat(10), 4)).toBe("xxxx");
    expect(clip("😀😀", 6)).toBe("😀");
    expect(clip("ää", 3)).toBe("ä");
    expect(clip(null, 5)).toBe("");
    expect(clip(42, 5)).toBe("42");
  });

  it("keeps props flat and bounded", () => {
    expect(cleanProps({ a: 1, b: "x", c: true, d: null, e: { f: 1 } })).toEqual(
      {
        a: 1,
        b: "x",
        c: true,
        d: null,
        e: '{"f":1}',
      },
    );
    expect(cleanProps({ n: Infinity })).toEqual({ n: null });
    expect(cleanProps("nope")).toEqual({});
    const big: Record<string, string> = {};
    for (let i = 0; i < 12; i++) big["k" + i] = "x".repeat(500);
    expect(cleanProps(big)).toEqual({ dropped: true });
    const many: Record<string, number> = {};
    for (let i = 0; i < 40; i++) many["k" + i] = i;
    expect(Object.keys(cleanProps(many))).toHaveLength(32);
  });
});

describe("sending", () => {
  it("sends a page view with context to the script's origin", async () => {
    const t = boot();
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("https://collector.example.com/api/in");
    expect(init.method).toBe("POST");
    expect(init.keepalive).toBe(true);
    expect(init.credentials).toBe("omit");
    expect(init.headers).toBeUndefined();
    expect(typeof init.body).toBe("string");
    const b = bodies()[0];
    expect(b.v).toBe(1);
    expect(b.site).toBe(location.hostname);
    expect(b.vid).toMatch(/^[0-9a-f]{32}$/);
    expect(b.sid).toMatch(/^[0-9a-f]{32}$/);
    expect(b.ctx.url).toBe(location.href);
    expect(b.ctx.title).toBe("Home");
    expect(b.ctx.viewport).toHaveLength(2);
    expect(b.events.map((e) => e.k)).toEqual(["session_start", "page_view"]);
    // The event happened at the ctx URL: no url of its own.
    expect(b.events[1].url).toBeUndefined();
  });

  it("flushes on its own after a few seconds", async () => {
    boot();
    expect(fetchMock).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(LIMITS.flushAfterMs);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("keeps the visitor id and the session across loads", async () => {
    const a = boot();
    const { vid, sid } = a.state();
    a.stop();
    vi.advanceTimersByTime(10 * 60 * 1000);
    const b = boot();
    expect(b.state().vid).toBe(vid);
    expect(b.state().sid).toBe(sid);
    b.flush();
    await vi.advanceTimersByTimeAsync(0);
    // The second load continues the session: no session_start.
    expect(
      bodies()
        .at(-1)!
        .events.map((e) => e.k),
    ).not.toContain("session_start");
  });

  it("starts a new session after 30 minutes without activity", async () => {
    const t = boot();
    const first = t.state().sid;
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    vi.advanceTimersByTime(LIMITS.sessionMs + 1000);
    t.event("later");
    expect(t.state().sid).not.toBe(first);
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(
      bodies()
        .at(-1)!
        .events.map((e) => e.k),
    ).toEqual(["session_start", "custom"]);
  });

  it("caps events per minute", async () => {
    const t = boot();
    fetchMock.mockImplementation(() => new Promise(() => {}));
    for (let i = 0; i < 200; i++) t.event("e" + i);
    const s = t.state();
    expect(s.queued).toBeLessThanOrEqual(LIMITS.perMinute);
    expect(s.dropped).toBeGreaterThan(100);
  });

  // A host-page value whose every read is counted: as wide and as deep as
  // anyone walks it.
  const endless = () => {
    const reads = { n: 0 };
    const keys = Array.from({ length: 100 }, (_, i) => "k" + i);
    const node = (): object =>
      new Proxy(
        {},
        {
          ownKeys: () => keys,
          getOwnPropertyDescriptor: () => ({
            enumerable: true,
            configurable: true,
          }),
          get: () => {
            reads.n++;
            return node();
          },
        },
      );
    return { reads, value: node() };
  };

  it("checks the caps before doing any work for an event", () => {
    const t = boot();
    for (let i = 0; i < LIMITS.perMinute; i++) t.event("fill");
    const get = vi.spyOn(Storage.prototype, "getItem");
    const set = vi.spyOn(Storage.prototype, "setItem");
    const { reads, value } = endless();
    const name = { toString: vi.fn(() => "x") };
    t.event(name, { state: value });
    expect(reads.n).toBe(0);
    expect(name.toString).not.toHaveBeenCalled();
    expect(get).not.toHaveBeenCalled();
    expect(set).not.toHaveBeenCalled();
  });

  it("bounds the work one event's props can cost", () => {
    const t = boot();
    const { reads, value } = endless();
    t.event("x", { state: value, other: value });
    expect(reads.n).toBeLessThan(400);
    expect(t.state().queued).toBe(3);
    // clip reads at most max + 1 code units and still never splits a pair.
    const tail = "x".repeat(1_000_000);
    expect(clip("a".repeat(511) + "😀" + tail, 512)).toBe("a".repeat(511));
    expect(clip("a".repeat(508) + "😀" + tail, 512)).toBe(
      "a".repeat(508) + "😀",
    );
  });

  it("caps events per page and bounds the queue", async () => {
    const t = boot();
    // A collector that never answers: everything stays queued.
    fetchMock.mockImplementation(() => new Promise(() => {}));
    t.flush();
    for (let m = 0; m < 10; m++) {
      for (let i = 0; i < 80; i++) t.event("e");
      vi.advanceTimersByTime(61_000);
    }
    expect(t.state().queued).toBeLessThanOrEqual(LIMITS.queue);
    expect(t.state().dropped).toBeGreaterThan(800 - LIMITS.perPage);
  });

  it("keeps every batch under the size limit", async () => {
    const t = boot();
    const props: Record<string, string> = {};
    for (let i = 0; i < 7; i++) props["k" + i] = "y".repeat(500);
    for (let i = 0; i < 30; i++) t.event("big", props);
    // The 20th event already flushed one batch.
    const before = fetchMock.mock.calls.length;
    t.flush(true);
    await vi.advanceTimersByTimeAsync(0);
    // Two at once at most: the keepalive quota is 64 KiB in flight.
    expect(fetchMock.mock.calls.length - before).toBe(2);
    expect(t.state().queued).toBeGreaterThan(0);
    await vi.advanceTimersByTimeAsync(LIMITS.flushAfterMs * 3);
    expect(t.state().queued).toBe(0);
    for (const c of fetchMock.mock.calls) {
      const body = (c[1] as RequestInit).body as string;
      expect(utf8Len(body)).toBeLessThanOrEqual(LIMITS.batchBytes);
      expect(JSON.parse(body).events.length).toBeLessThanOrEqual(
        LIMITS.batchEvents,
      );
    }
    expect(kinds().filter((k) => k === "custom")).toHaveLength(30);
  });

  it("honours Retry-After on 429 and sends again later", async () => {
    const t = boot();
    fetchMock.mockImplementationOnce(() =>
      reply(429, { "Retry-After": "120" }),
    );
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(t.state().failures).toBe(1);
    expect(t.state().queued).toBe(2);
    await vi.advanceTimersByTimeAsync(60_000);
    t.flush();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(61_000);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(t.state().failures).toBe(0);
    expect(t.state().queued).toBe(0);
  });

  it("caps every delay, whatever Retry-After says", async () => {
    const delays: number[] = [];
    const real = globalThis.setTimeout;
    vi.spyOn(globalThis, "setTimeout").mockImplementation(((
      f: () => void,
      ms?: number,
    ) => {
      delays.push(ms ?? 0);
      return real(f, ms);
    }) as typeof setTimeout);
    for (const ra of ["1e10", "Infinity", "-5", "soon", "999999999999"]) {
      tracker?.stop();
      localStorage.clear();
      fetchMock.mockReset();
      fetchMock.mockImplementationOnce(() => reply(429, { "Retry-After": ra }));
      fetchMock.mockImplementation(() => reply(204));
      const t = boot();
      t.flush();
      await vi.advanceTimersByTimeAsync(0);
      const armed = delays.length;
      await vi.advanceTimersByTimeAsync(1000);
      // A delay past 2^31-1 ms fires at once in browsers: a busy loop.
      expect(delays.length - armed).toBeLessThan(3);
      for (const d of delays) {
        expect(Number.isFinite(d)).toBe(true);
        expect(d).toBeGreaterThanOrEqual(0);
        expect(d).toBeLessThanOrEqual(LIMITS.retryMaxMs);
      }
      await vi.advanceTimersByTimeAsync(LIMITS.retryMaxMs);
      expect(t.state().queued).toBe(0);
    }
  });

  it("reads Retry-After as an HTTP date", async () => {
    const t = boot();
    const at = new Date(Date.now() + 120_000).toUTCString();
    fetchMock.mockImplementationOnce(() => reply(429, { "Retry-After": at }));
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(100_000);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(25_000);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("switches itself off on 403", async () => {
    const t = boot();
    fetchMock.mockImplementation(() => reply(403));
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(t.state().disabled).toBe(true);
    t.event("x");
    expect(t.state().queued).toBe(0);
  });

  it("backs off on errors and switches off after repeated failures", async () => {
    const t = boot();
    fetchMock.mockImplementation(() => reply(503));
    let last = 0;
    for (let i = 0; i < LIMITS.failures; i++) {
      t.event("x");
      t.flush();
      await vi.advanceTimersByTimeAsync(0);
      const s = t.state();
      if (!s.disabled) {
        expect(s.backoff).toBeGreaterThan(last);
        last = s.backoff;
      }
      await vi.advanceTimersByTimeAsync(s.backoff + 1);
    }
    expect(t.state().disabled).toBe(true);
    const calls = fetchMock.mock.calls.length;
    t.event("y");
    t.flush(true);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchMock.mock.calls.length).toBe(calls);
  });

  it("drops a batch the collector refuses as malformed", async () => {
    const t = boot();
    fetchMock.mockImplementationOnce(() => reply(400));
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(t.state().queued).toBe(0);
    expect(t.state().dropped).toBe(2);
  });

  it("requeues after a network error", async () => {
    const t = boot();
    fetchMock.mockImplementationOnce(() =>
      Promise.reject(new TypeError("offline")),
    );
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(t.state().queued).toBe(2);
    await vi.advanceTimersByTimeAsync(LIMITS.backoffMs + 10);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(t.state().queued).toBe(0);
  });

  it("survives a fetch that throws", async () => {
    const t = boot();
    fetchMock.mockImplementation(() => {
      throw new Error("blocked");
    });
    expect(() => t.flush()).not.toThrow();
    expect(t.state().failures).toBe(1);
  });

  it("falls back to sendBeacon without fetch, and gives up without either", async () => {
    vi.stubGlobal("fetch", undefined);
    const beacon = vi.fn(() => true);
    Object.defineProperty(navigator, "sendBeacon", {
      value: beacon,
      configurable: true,
    });
    const t = boot();
    t.flush();
    expect(beacon).toHaveBeenCalledTimes(1);
    t.stop();
    Object.defineProperty(navigator, "sendBeacon", {
      value: undefined,
      configurable: true,
    });
    const u = boot();
    expect(() => u.flush()).not.toThrow();
    expect(u.state().disabled).toBe(true);
  });

  it("runs with storage and observers missing", async () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("denied");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("denied");
    });
    vi.stubGlobal("PerformanceObserver", undefined);
    vi.stubGlobal("matchMedia", undefined);
    const t = boot();
    expect(t.state().vid).toMatch(/^[0-9a-f]{32}$/);
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(kinds()).toContain("page_view");
  });

  it("finds its own script tag when currentScript is gone", () => {
    const s = document.createElement("script");
    s.setAttribute("src", "https://other.example.org/nui.js");
    document.head.appendChild(s);
    tracker = start(win, null);
    expect(tracker).not.toBeNull();
    s.remove();
    tracker!.stop();
    tracker = null;
    expect(start(win, null)).toBeNull();
  });
});

describe("capture", () => {
  const flushed = async (t: Tracker) => {
    t.flush(true);
    await vi.advanceTimersByTimeAsync(0);
    return bodies().flatMap((b) => b.events);
  };

  it("tracks SPA navigation with each view's own URL", async () => {
    const t = boot();
    history.pushState(null, "", "/AS15169");
    history.replaceState(null, "", "/AS15169");
    history.pushState(null, "", "/8.8.8.8");
    const ev = (await flushed(t)).filter((e) => e.k === "page_view");
    expect(ev).toHaveLength(3);
    expect(ev[0].url).toBe(location.origin + "/");
    expect(ev[1].url).toBe(location.origin + "/AS15169");
    expect(ev[2].url).toBeUndefined();
  });

  it("restores history on stop", () => {
    const before = history.pushState;
    const t = boot();
    expect(history.pushState).not.toBe(before);
    t.stop();
    tracker = null;
    expect(history.pushState).toBe(before);
  });

  it("records a page view when restored from the back/forward cache", async () => {
    const t = boot();
    const e = new Event("pageshow") as Event & { persisted: boolean };
    Object.defineProperty(e, "persisted", { value: true });
    window.dispatchEvent(e);
    expect((await flushed(t)).filter((x) => x.k === "page_view")).toHaveLength(
      2,
    );
  });

  it("records outbound clicks and downloads, not internal links", async () => {
    const t = boot();
    document.body.innerHTML =
      '<a id="out" href="https://github.com/ipverse/as-ip-blocks"> as-ip-blocks </a>' +
      '<a id="dl" href="/data/as.csv">csv</a><a id="in" href="/about">about</a>' +
      '<a id="mail" href="mailto:x@example.com">mail</a>';
    for (const id of ["out", "dl", "in", "mail"])
      document
        .getElementById(id)!
        .dispatchEvent(new MouseEvent("click", { bubbles: true }));
    const ev = await flushed(t);
    const out = ev.find((e) => e.k === "outbound_click")!;
    expect(out.p).toEqual({
      url: "https://github.com/ipverse/as-ip-blocks",
      text: "as-ip-blocks",
    });
    expect(ev.find((e) => e.k === "download")!.p.url).toBe(
      location.origin + "/data/as.csv",
    );
    expect(
      ev.filter((e) => e.k === "outbound_click" || e.k === "download"),
    ).toHaveLength(2);
    document.body.innerHTML = "";
  });

  it("records forms and search terms", async () => {
    const t = boot();
    document.body.innerHTML =
      '<form id="f" name="signup" action="/join"><input id="mail" name="mail"></form>' +
      '<form id="s"><input type="search" name="q" value="AS15169"></form>';
    document
      .getElementById("mail")!
      .dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    document
      .getElementById("mail")!
      .dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    document
      .getElementById("f")!
      .dispatchEvent(new Event("submit", { bubbles: true }));
    document
      .getElementById("s")!
      .dispatchEvent(new Event("submit", { bubbles: true }));
    const ev = await flushed(t);
    const forms = ev.filter((e) => e.k === "form");
    expect(forms.map((f) => f.p.phase)).toEqual(["start", "submit"]);
    expect(forms[1].p).toMatchObject({
      id: "f",
      name: "signup",
      action: "/join",
    });
    expect(ev.find((e) => e.k === "search")!.p).toEqual({ term: "AS15169" });
    document.body.innerHTML = "";
  });

  it("records script errors, at most a few of the same", async () => {
    const t = boot();
    for (let i = 0; i < 6; i++)
      window.dispatchEvent(
        new ErrorEvent("error", {
          message: "TypeError: x",
          filename: "/app.js",
          lineno: 3,
          colno: 7,
        }),
      );
    const rej = new Event("unhandledrejection") as Event & { reason: unknown };
    rej.reason = new Error("AbortError");
    window.dispatchEvent(rej);
    const img = document.createElement("img");
    img.src = "https://cdn.example.com/x.png";
    document.body.appendChild(img);
    img.dispatchEvent(new Event("error"));
    const ev = await flushed(t);
    const errs = ev.filter((e) => e.k === "js_error");
    expect(errs).toHaveLength(LIMITS.sameError + 1);
    expect(errs[0].p).toEqual({
      msg: "TypeError: x",
      src: "/app.js",
      line: 3,
      col: 7,
    });
    expect(errs.at(-1)!.p.msg).toBe("Unhandled rejection: AbortError");
    expect(ev.find((e) => e.k === "resource_error")!.p.url).toBe(
      "https://cdn.example.com/x.png",
    );
    img.remove();
  });

  it("remembers a bounded number of distinct errors", async () => {
    const t = boot();
    const fire = (n: number) => {
      for (let i = 0; i < n; i++)
        window.dispatchEvent(
          new ErrorEvent("error", { message: "E" + i + "x".repeat(300) }),
        );
    };
    fire(500);
    expect(t.state().errorKeys).toBeLessThanOrEqual(LIMITS.errorKinds);
    t.stop();
    tracker = null;
    const u = boot();
    fetchMock.mockImplementation(() => reply(403));
    u.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(u.state().disabled).toBe(true);
    fire(10);
    expect(u.state().errorKeys).toBe(0);
  });

  it("sends engagement and vitals when the page is hidden", async () => {
    boot();
    window.dispatchEvent(new Event("scroll"));
    document.dispatchEvent(new Event("pointerdown"));
    document.dispatchEvent(new KeyboardEvent("keydown"));
    vi.advanceTimersByTime(4000);
    Object.defineProperty(document, "visibilityState", {
      value: "hidden",
      configurable: true,
    });
    document.dispatchEvent(new Event("visibilitychange"));
    Object.defineProperty(document, "visibilityState", {
      value: "visible",
      configurable: true,
    });
    await vi.advanceTimersByTimeAsync(0);
    const ev = bodies().flatMap((b) => b.events);
    const eng = ev.find((e) => e.k === "engagement")!;
    expect(eng.p.inputs).toEqual({ pointer: 1, key: 1, touch: 0, scroll: 1 });
    expect(ev.find((e) => e.k === "vital" && e.p.name === "CLS")).toBeTruthy();
    // A second hide does not repeat the vitals.
    window.dispatchEvent(new Event("pagehide"));
    await vi.advanceTimersByTimeAsync(0);
    const cls = bodies()
      .flatMap((b) => b.events)
      .filter((e) => e.k === "vital" && e.p.name === "CLS");
    expect(cls).toHaveLength(1);
  });

  it("takes custom events, including those queued before it loaded", async () => {
    (win as { nui?: unknown }).nui = {
      q: [["copy_prefix_list", { asn: 15169 }]],
    };
    const t = boot();
    const g = (
      win as unknown as {
        nui: {
          event: (n: string, p?: unknown) => void;
          q: { push: (a: unknown) => void };
        };
      }
    ).nui;
    g.event("signup", { plan: "free" });
    g.q.push(["late", {}]);
    g.event("", {});
    const ev = (await flushed(t)).filter((e) => e.k === "custom");
    expect(ev.map((e) => [e.n, e.p])).toEqual([
      ["copy_prefix_list", { asn: 15169 }],
      ["signup", { plan: "free" }],
      ["late", {}],
    ]);
  });
});

// The collector's limits (backend/internal/ingest/collector.go): a breach
// anywhere refuses the whole batch.
const bytes = (s: string) => new TextEncoder().encode(s).length;
const URL_KEYS = new Set(["url", "ref", "src", "href"]);
function overLimit(key: string, v: unknown, depth: number): string | null {
  if (depth > 8) return key + ": too deep";
  if (typeof v === "string")
    return bytes(v) > (URL_KEYS.has(key) ? 2048 : 512) ? key + ": long" : null;
  if (typeof v === "number") return isFinite(v) ? null : key + ": number";
  if (Array.isArray(v)) {
    if (v.length > 50) return key + ": array";
    for (const x of v) {
      const e = overLimit(key, x, depth + 1);
      if (e) return e;
    }
  } else if (v && typeof v === "object") {
    const ks = Object.keys(v);
    if (ks.length > 64) return key + ": map";
    for (const k of ks) {
      if (bytes(k) > 64) return k + ": key";
      const e = overLimit(k, (v as Record<string, unknown>)[k], depth + 1);
      if (e) return e;
    }
  }
  return null;
}
// Go's url.Parse refuses a bad escape in the path or the fragment.
const badEscape = (u: string) =>
  /%(?![0-9a-f]{2})/i.test(u.replace(/\?[^#]*/, ""));
function batchProblems(raw: string): string[] {
  const out: string[] = [];
  const b = JSON.parse(raw) as Body & { events: { title?: string }[] };
  if (bytes(raw) > 32 * 1024) out.push("body");
  if (b.events.length > 50) out.push("events");
  if (bytes(b.vid) > 512 || bytes(b.sid) > 512) out.push("ids");
  for (const [k, v] of Object.entries(b.ctx)) {
    const e = overLimit(k, v, 0);
    if (e) out.push("ctx." + e);
  }
  if (badEscape(String(b.ctx.url))) out.push("ctx.url escape");
  for (const e of b.events) {
    if (bytes(e.url ?? "") > 2048) out.push(e.k + ".url");
    if (e.url && badEscape(e.url)) out.push(e.k + ".url escape");
    if (bytes(e.title ?? "") > 512) out.push(e.k + ".title");
    if (bytes(e.n ?? "") > 512) out.push(e.k + ".n");
    if (bytes(JSON.stringify(e.p)) > 4096) out.push(e.k + ".p size");
    const p = overLimit("p", e.p, 0);
    if (p) out.push(e.k + "." + p);
  }
  return out;
}

describe("collector limits", () => {
  afterEach(() => {
    document.body.innerHTML = "";
    delete (navigator as { userAgentData?: unknown }).userAgentData;
  });

  it("keeps every property it sends within the collector's limits", async () => {
    localStorage.setItem("nui.vid", "v".repeat(2000));
    localStorage.setItem("nui.sid", "s".repeat(2000));
    const long = "é".repeat(3000);
    Object.defineProperty(navigator, "userAgentData", {
      configurable: true,
      value: {
        mobile: false,
        platform: long,
        brands: Array.from({ length: 80 }, () => ({
          brand: long,
          version: "1",
        })),
      },
    });
    // A path that ends in a cut escape once clipped, and a stray "%".
    history.replaceState(
      null,
      "",
      "/%zz/" + "%C3%A9".repeat(400) + "?q=" + long,
    );
    document.title = long;
    const t = boot();
    const hugeAttr = "a".repeat(5000);
    document.body.innerHTML =
      `<form id="${hugeAttr}" name="${hugeAttr}" action="/join/${"x".repeat(3000)}">` +
      `<input id="i" name="mail"></form>` +
      `<form id="s"><input type="search" name="q" value="${hugeAttr}"></form>` +
      `<a id="out" href="https://github.com/${"p".repeat(3000)}">${hugeAttr}</a>` +
      `<a id="dl" href="/f/${"d".repeat(3000)}.csv">x</a>`;
    document
      .getElementById("i")!
      .dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    document
      .getElementById(hugeAttr)!
      .dispatchEvent(new Event("submit", { bubbles: true }));
    document
      .getElementById("s")!
      .dispatchEvent(new Event("submit", { bubbles: true }));
    for (const id of ["out", "dl"])
      document
        .getElementById(id)!
        .dispatchEvent(new MouseEvent("click", { bubbles: true }));
    window.dispatchEvent(
      new ErrorEvent("error", { message: long, filename: "/" + long }),
    );
    const img = document.createElement("img");
    img.src = "https://cdn.example.com/" + "i".repeat(3000);
    document.body.appendChild(img);
    img.dispatchEvent(new Event("error"));
    const deep: Record<string, unknown> = {};
    let cur = deep;
    for (let i = 0; i < 20; i++) cur = cur["d"] = {};
    const wide: Record<string, string> = {};
    for (let i = 0; i < 100; i++) wide["k".repeat(80) + i] = long;
    t.event(long, {
      url: "https://example.com/" + "u".repeat(3000),
      [long]: 1,
      deep,
      big: BigInt(1),
      list: Array.from({ length: 100 }, () => long),
    });
    t.event("wide", wide);
    history.pushState(null, "", "/next/" + "n".repeat(3000));
    vi.advanceTimersByTime(61_000);
    t.flush(true);
    await vi.advanceTimersByTimeAsync(LIMITS.flushAfterMs * 4);
    const raws = fetchMock.mock.calls.map(
      (c) => (c[1] as RequestInit).body as string,
    );
    const sent = raws.flatMap((r) => JSON.parse(r).events as Body["events"]);
    for (const k of [
      "form",
      "search",
      "outbound_click",
      "download",
      "js_error",
      "resource_error",
      "custom",
      "page_view",
    ])
      expect(sent.map((e) => e.k)).toContain(k);
    for (const r of raws) expect(batchProblems(r)).toEqual([]);
    // Every string key outside the URL ones is held to 512 bytes, the form's
    // action included.
    const form = sent.find((e) => e.k === "form" && e.p.phase === "submit")!;
    expect(bytes(form.p.action as string)).toBeLessThanOrEqual(512);
    expect(JSON.parse(raws[0]).vid).toMatch(/^[0-9a-f]{32}$/);
    expect(JSON.parse(raws[0]).sid).toMatch(/^[0-9a-f]{32}$/);
  });
});

describe("host page safety", () => {
  it("never builds scripts or markup from strings", () => {
    const code = source
      .replace(/\/\*[\s\S]*?\*\//g, "")
      .replace(/\/\/.*$/gm, "");
    expect(code).not.toMatch(
      /innerHTML|outerHTML|insertAdjacentHTML|document\.write/,
    );
    expect(code).not.toMatch(/\beval\(|new Function|createElement\(/);
    expect(code).not.toMatch(/setTimeout\(\s*["'`]|setInterval/);
    expect(code).not.toMatch(/["']unload["']|beforeunload/);
  });
});

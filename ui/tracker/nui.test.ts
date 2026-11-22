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
    for (let i = 0; i < 18; i++) t.event("big", props);
    t.flush(true);
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
    for (const c of fetchMock.mock.calls) {
      const body = (c[1] as RequestInit).body as string;
      expect(utf8Len(body)).toBeLessThanOrEqual(LIMITS.batchBytes);
      expect(JSON.parse(body).events.length).toBeLessThanOrEqual(
        LIMITS.batchEvents,
      );
    }
    expect(kinds().filter((k) => k === "custom")).toHaveLength(18);
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

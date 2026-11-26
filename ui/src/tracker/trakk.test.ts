import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import source from "./trakk.ts?raw";
import {
  LIMITS,
  cleanProps,
  clip,
  errorProps,
  scriptHost,
  start,
  utf8Len,
  type Tracker,
} from "./trakk";

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

const SRC = "https://collector.example.com/trakk.js";
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
  delete (win as { trakk?: unknown }).trakk;
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

  it("walks a typed array by index, never listing all its elements", () => {
    expect(cleanProps({ buf: new Uint8Array([1, 2, 3]) })).toEqual({
      buf: "[1,2,3]",
    });
    const huge = cleanProps({ buf: new Float64Array(5_000_000) });
    expect(JSON.parse(huge.buf as string)).toHaveLength(LIMITS.arrayItems);
    expect(cleanProps({ v: new DataView(new ArrayBuffer(8)) })).toEqual({
      v: "[]",
    });
  });
});

describe("automation probes", () => {
  // Idle callbacks the test runs by hand, with the idle time left it names.
  const stubIdle = () => {
    let next: ((d: { timeRemaining: () => number }) => void)[] = [];
    vi.stubGlobal("requestIdleCallback", (f: (typeof next)[number]) =>
      next.push(f),
    );
    vi.stubGlobal("cancelIdleCallback", () => {
      next = [];
    });
    return {
      run: (left: number) => {
        const now = next;
        next = [];
        for (const f of now) f({ timeRemaining: () => left });
      },
      queued: () => next.length,
    };
  };
  const ctxOf = async (t: Tracker) => {
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    return bodies().at(-1)!.ctx;
  };

  it("sends its own tokens for automation globals, never the page's names", async () => {
    const w = win as unknown as Record<string, unknown>;
    const d = document as unknown as Record<string, unknown>;
    w.__playwright__binding__ = {};
    d.$cdc_asdjflasutopfhvcZLmcfl_Symbol = {};
    // The page's own, with a tool's prefix: none of them a hit.
    w.__pwaInstallPrompt = 1;
    w.cdc_config = 1;
    w.__driver_state = 1;
    document.documentElement.setAttribute("webdriver", "");
    try {
      const ctx = await ctxOf(boot());
      expect(ctx.auto).toEqual([
        "playwright-binding",
        "chromedriver",
        "selenium-attr",
      ]);
    } finally {
      for (const k of [
        "__playwright__binding__",
        "__pwaInstallPrompt",
        "cdc_config",
        "__driver_state",
      ])
        delete w[k];
      delete d.$cdc_asdjflasutopfhvcZLmcfl_Symbol;
      document.documentElement.removeAttribute("webdriver");
    }
  });

  it("sends nothing in auto for a plain page", async () => {
    const ctx = await ctxOf(boot());
    expect(ctx.auto).toEqual([]);
    expect(ctx.outer).toHaveLength(2);
    expect(ctx.plg).toHaveLength(2);
    expect(ctx.synth).toBe(0);
  });

  it("tells where navigator.webdriver is defined", async () => {
    expect((await ctxOf(boot())).wdd).toMatch(/^(proto|none)$/);
    tracker!.stop();
    Object.defineProperty(navigator, "webdriver", {
      get: () => false,
      configurable: true,
    });
    try {
      expect((await ctxOf(boot())).wdd).toBe("own");
    } finally {
      delete (navigator as { webdriver?: boolean }).webdriver;
    }
  });

  it("sends the notification permission beside the Permissions API's answer", async () => {
    vi.stubGlobal("Notification", { permission: "denied" });
    Object.defineProperty(navigator, "permissions", {
      value: { query: () => Promise.resolve({ state: "prompt" }) },
      configurable: true,
    });
    try {
      const t = boot();
      await vi.advanceTimersByTimeAsync(0);
      expect((await ctxOf(t)).perm).toEqual(["denied", "prompt"]);
    } finally {
      delete (navigator as { permissions?: unknown }).permissions;
    }
  });

  it("sends a kept renderer while it stands, and ignores what the page wrote there", async () => {
    const day = 24 * 60 * 60 * 1000;
    const ua = navigator.userAgent;
    const keep = async (v: unknown) => {
      tracker?.stop();
      localStorage.setItem("analytics.gl", JSON.stringify(v));
      return ctxOf(boot());
    };
    expect(
      (await keep(["Apple Inc.", "Apple GPU", Date.now() - 29 * day, ua])).gl,
    ).toEqual(["Apple Inc.", "Apple GPU"]);
    // Too old, dated ahead, another browser version, an earlier format, the
    // page's own: each measured again.
    for (const v of [
      ["Apple Inc.", "Apple GPU", Date.now() - 31 * day, ua],
      ["Apple Inc.", "Apple GPU", Date.now() + day, ua],
      ["Apple Inc.", "Apple GPU", Date.now(), ua + "x"],
      ["Apple Inc.", "Apple GPU", "x", ua],
      ["Apple Inc.", "Apple GPU", String(Date.now()), ua],
      ["Apple Inc.", "Apple GPU"],
      "-",
      { x: 1 },
    ])
      expect((await keep(v)).gl, JSON.stringify(v)).toBeUndefined();
  });

  it("says why there is no renderer", async () => {
    vi.stubGlobal("requestIdleCallback", undefined);
    expect((await ctxOf(boot())).glw).toBe("no-idle");
    tracker!.stop();
    vi.stubGlobal("OffscreenCanvas", undefined);
    let idle = stubIdle();
    let t = boot();
    for (let i = 0; i < 25; i++) idle.run(5);
    expect((await ctxOf(t)).glw).toBe("busy");
    tracker!.stop();
    idle = stubIdle();
    t = boot();
    idle.run(49);
    expect((await ctxOf(t)).glw).toBe("no-canvas");
    tracker!.stop();
    vi.stubGlobal(
      "OffscreenCanvas",
      class {
        getContext() {
          return null;
        }
      },
    );
    idle = stubIdle();
    t = boot();
    idle.run(49);
    const ctx = await ctxOf(t);
    expect(ctx.gl).toEqual(["", ""]);
    expect(ctx.glw).toBeUndefined();
    expect(localStorage.getItem("analytics.gl")).toBeNull();
    tracker!.stop();
    // A context lost as it was made.
    vi.stubGlobal(
      "OffscreenCanvas",
      class {
        getContext() {
          return { getParameter: () => null, getExtension: () => null };
        }
      },
    );
    idle = stubIdle();
    t = boot();
    expect((await ctxOf(t)).glw).toBe("pending");
    idle.run(49);
    t.event("e");
    const lost = await ctxOf(t);
    expect(lost.gl).toBeUndefined();
    expect(lost.glw).toBe("lost");
    expect(localStorage.getItem("analytics.gl")).toBeNull();
  });

  it("counts clicks a script made, and names Playwright's evaluate by its stack", async () => {
    const t = boot();
    document.body.innerHTML = '<button id="b">b</button>';
    const b = document.getElementById("b")!;
    b.click();
    let ctx = await ctxOf(t);
    expect(ctx.sclick).toBe(1);
    expect(ctx.synth).toBe(0);
    expect(ctx.auto).toEqual([]);
    // A page's own class of that name is no Playwright's.
    new Function(
      "b",
      "class UtilityScript { evaluate() { b.click(); } } new UtilityScript().evaluate();",
    )(b);
    t.event("e");
    ctx = await ctxOf(t);
    expect(ctx.sclick).toBe(2);
    expect(ctx.auto).toEqual([]);
    // The frame as Chromium prints it, below a limit raised for the look.
    const V8 = Error as unknown as {
      stackTraceLimit: number;
      prepareStackTrace?: unknown;
    };
    const { stackTraceLimit: limit, prepareStackTrace: prepare } = V8;
    let seen = 0;
    V8.prepareStackTrace = () => {
      seen = V8.stackTraceLimit;
      return "Error\n    at f (x.js:1:1)\n    at UtilityScript.evaluate (<anonymous>:372:16)";
    };
    try {
      b.click();
      expect(seen).toBe(Math.max(limit, 50));
      expect(V8.stackTraceLimit).toBe(limit);
      seen = 0;
      b.click();
      // Once named, no stack is taken any more.
      expect(seen).toBe(0);
    } finally {
      V8.prepareStackTrace = prepare;
      V8.stackTraceLimit = limit;
      document.body.innerHTML = "";
    }
    t.event("e");
    ctx = await ctxOf(t);
    expect(ctx.sclick).toBe(4);
    expect(ctx.auto).toEqual(["playwright-evaluate"]);
  });

  it("sends the probes' version and whether the page was activated", async () => {
    Object.defineProperty(navigator, "userActivation", {
      value: { hasBeenActive: true },
      configurable: true,
    });
    try {
      const ctx = await ctxOf(boot());
      expect(ctx.pv).toBe(2);
      expect(ctx.act).toBe(true);
    } finally {
      delete (navigator as { userActivation?: unknown }).userActivation;
    }
    tracker!.stop();
    expect((await ctxOf(boot())).act).toBeUndefined();
  });

  it("measures the renderer when idle, offscreen, and keeps it", async () => {
    const getParameter = (p: unknown) =>
      ({
        1: "WebKit",
        2: "WebKit WebGL",
        3: "Google Inc. (Google)",
        4: "ANGLE (SwiftShader)",
      })[p as number];
    const loseContext = vi.fn();
    const ctx3d = {
      VENDOR: 1,
      RENDERER: 2,
      getParameter,
      getExtension: () => ({
        UNMASKED_VENDOR_WEBGL: 3,
        UNMASKED_RENDERER_WEBGL: 4,
        loseContext,
      }),
    };
    vi.stubGlobal(
      "OffscreenCanvas",
      class {
        getContext() {
          return ctx3d;
        }
      },
    );
    const idle = stubIdle();
    const t = boot();
    // A short idle period is passed over, a long one taken.
    idle.run(10);
    expect(loseContext).not.toHaveBeenCalled();
    idle.run(49);
    expect((await ctxOf(t)).gl).toEqual([
      "Google Inc. (Google)",
      "ANGLE (SwiftShader)",
    ]);
    expect(loseContext).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem("analytics.gl")).toContain("SwiftShader");
  });

  it("asks no more where asking threw, and not at all without idle callbacks", async () => {
    const getContext = vi.fn(() => {
      throw new Error("no");
    });
    vi.stubGlobal(
      "OffscreenCanvas",
      class {
        getContext = getContext;
      },
    );
    const idle = stubIdle();
    boot();
    idle.run(49);
    expect(JSON.parse(localStorage.getItem("analytics.gl")!)[0]).toBe("-");
    tracker!.stop();
    const again = boot();
    expect(idle.queued()).toBe(0);
    expect((await ctxOf(again)).glw).toBe("threw");
    tracker!.stop();
    localStorage.clear();
    vi.stubGlobal("requestIdleCallback", undefined);
    const t = boot();
    await vi.advanceTimersByTimeAsync(10_000);
    expect((await ctxOf(t)).gl).toBeUndefined();
    expect(getContext).toHaveBeenCalledTimes(1);
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
    // Visible but not focused: no time that counts as engagement.
    vi.spyOn(document, "hasFocus").mockReturnValue(false);
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

  describe("session life", () => {
    const show = (state: "visible" | "hidden") => {
      Object.defineProperty(document, "visibilityState", {
        value: state,
        configurable: true,
      });
      document.dispatchEvent(new Event("visibilitychange"));
    };
    afterEach(() => {
      Object.defineProperty(document, "visibilityState", {
        value: "visible",
        configurable: true,
      });
      document.body.innerHTML = "";
    });
    const sessions = () =>
      bodies().flatMap((b) =>
        b.events.map((e) => [b.sid, e.n ?? e.k] as [string, string]),
      );

    it("stays alive through a long visible read", async () => {
      const t = boot();
      const sid = t.state().sid;
      vi.advanceTimersByTime(40 * 60 * 1000);
      show("hidden");
      await vi.advanceTimersByTimeAsync(0);
      expect(sessions().filter(([, k]) => k === "session_start")).toHaveLength(
        1,
      );
      expect(sessions().find(([, k]) => k === "engagement")).toEqual([
        sid,
        "engagement",
      ]);
      // Right after: the same session still.
      show("visible");
      t.event("back");
      expect(t.state().sid).toBe(sid);
    });

    it("stays alive while the visitor scrolls an unfocused page", () => {
      vi.spyOn(document, "hasFocus").mockReturnValue(false);
      const t = boot();
      const sid = t.state().sid;
      for (let i = 0; i < 4; i++) {
        vi.advanceTimersByTime(10 * 60 * 1000);
        window.dispatchEvent(new Event("scroll"));
      }
      t.event("later");
      expect(t.state().sid).toBe(sid);
    });

    it("ends after an absence, whatever happens on return", async () => {
      const t = boot();
      const sid = t.state().sid;
      show("hidden");
      vi.advanceTimersByTime(40 * 60 * 1000);
      show("visible");
      window.dispatchEvent(new Event("scroll"));
      document.dispatchEvent(new Event("pointerdown"));
      t.event("click");
      expect(t.state().sid).not.toBe(sid);
    });

    it("starts a new session where activity resumes after it expired", async () => {
      for (const resume of [
        () => window.dispatchEvent(new Event("scroll")),
        () => document.dispatchEvent(new Event("pointerdown")),
        () => document.dispatchEvent(new KeyboardEvent("keydown")),
        () => {
          vi.spyOn(document, "hasFocus").mockReturnValue(true);
          window.dispatchEvent(new Event("focus"));
        },
      ]) {
        tracker?.stop();
        localStorage.clear();
        fetchMock.mockClear();
        vi.spyOn(document, "hasFocus").mockReturnValue(false);
        const t = boot();
        const old = t.state().sid;
        // Read before the absence: it belongs to the old session.
        window.dispatchEvent(new Event("scroll"));
        vi.advanceTimersByTime(40 * 60 * 1000);
        resume();
        const fresh = t.state().sid;
        expect(fresh).not.toBe(old);
        vi.advanceTimersByTime(10_000);
        document.dispatchEvent(new Event("pointerdown"));
        show("hidden");
        await vi.advanceTimersByTimeAsync(0);
        show("visible");
        const s = sessions().filter(
          ([, k]) => k === "session_start" || k === "engagement",
        );
        expect(s).toEqual([
          [old, "session_start"],
          [old, "engagement"],
          [fresh, "session_start"],
          [fresh, "engagement"],
        ]);
        const engaged = bodies()
          .flatMap((b) => b.events)
          .filter((e) => e.k === "engagement");
        // The resuming input counts for the new session, not the old.
        expect(engaged[0].p.inputs).toMatchObject({ scroll: 1, pointer: 0 });
        expect(t.state().sid).toBe(fresh);
      }
    });

    it("sends the document's referrer with its first session only", async () => {
      const from = "https://search.example.org/?q=trakk";
      Object.defineProperty(document, "referrer", {
        configurable: true,
        value: from,
      });
      let t: Tracker;
      try {
        // A return: input, or an event of the page's own.
        for (const later of [
          () => document.dispatchEvent(new Event("pointerdown")),
          () => t.event("later"),
        ]) {
          tracker?.stop();
          localStorage.clear();
          fetchMock.mockClear();
          vi.spyOn(document, "hasFocus").mockReturnValue(false);
          t = boot();
          const first = t.state().sid;
          t.flush();
          await vi.advanceTimersByTimeAsync(0);
          vi.advanceTimersByTime(40 * 60 * 1000);
          later();
          history.pushState(null, "", "/next");
          show("hidden");
          await vi.advanceTimersByTimeAsync(0);
          show("visible");
          const sent = bodies();
          expect(sent[0].sid).toBe(first);
          expect(sent[0].ctx.ref).toBe(from);
          const back = sent.filter((b) => b.sid !== first);
          expect(back.flatMap((b) => b.events.map((e) => e.k))).toEqual(
            expect.arrayContaining(["session_start", "page_view"]),
          );
          for (const b of back) expect(b.ctx.ref).toBeUndefined();
          history.replaceState(null, "", "/");
        }
      } finally {
        delete (document as { referrer?: string }).referrer;
      }
    });

    it("never opens a session for a closing engagement or vital", async () => {
      vi.spyOn(document, "hasFocus").mockReturnValue(false);
      const t = boot();
      const sid = t.state().sid;
      window.dispatchEvent(new Event("scroll"));
      vi.advanceTimersByTime(40 * 60 * 1000);
      show("hidden");
      await vi.advanceTimersByTimeAsync(0);
      const closing = sessions().filter(
        ([, k]) => k === "engagement" || k === "vital",
      );
      expect(closing.length).toBeGreaterThan(0);
      for (const [s] of closing) expect(s).toBe(sid);
      expect(sessions().filter(([, k]) => k === "session_start")).toHaveLength(
        1,
      );
      // Nor does it extend the expired one.
      show("visible");
      t.event("next");
      expect(t.state().sid).not.toBe(sid);
    });
  });

  it("follows the session another tab started", async () => {
    const t = boot();
    const first = t.state().sid;
    t.event("before");
    // Another tab of the browser idled out first and started a new one.
    const other = "ab".repeat(16);
    localStorage.setItem("analytics.sid", other);
    localStorage.setItem("analytics.seen", String(Date.now()));
    t.event("after");
    expect(t.state().sid).toBe(other);
    t.flush(true);
    await vi.advanceTimersByTimeAsync(0);
    // Events leave with the session they were recorded in.
    const sent = bodies().map((b) => [b.sid, b.events.map((e) => e.n ?? e.k)]);
    expect(sent).toEqual([
      [first, ["session_start", "page_view", "before"]],
      [other, ["after"]],
    ]);
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
    expect(t.state().queued).toBeLessThanOrEqual(LIMITS.queueMax);
    expect(t.state().dropped).toBeGreaterThan(800 - LIMITS.perPage);
  });

  it("keeps every batch under the size limit", async () => {
    const t = boot();
    const props: Record<string, string> = {};
    for (let i = 0; i < 7; i++) props["k" + i] = "y".repeat(500);
    for (let i = 0; i < 30; i++) t.event("big", props);
    // The 20th event already flushed one batch, still unanswered here.
    expect(fetchMock).toHaveBeenCalledTimes(1);
    t.flush(true);
    // Beside it, what fits the keepalive quota of 64 KiB in flight.
    const sizes = fetchMock.mock.calls.map((c) =>
      utf8Len((c[1] as RequestInit).body as string),
    );
    expect(sizes.length).toBeGreaterThan(1);
    expect(sizes.reduce((a, b) => a + b, 0)).toBeLessThanOrEqual(64 * 1024);
    await vi.advanceTimersByTimeAsync(0);
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

  it("budgets the final flush against what is still in flight", async () => {
    const t = boot();
    const props: Record<string, string> = {};
    for (let i = 0; i < 7; i++) props["k" + i] = "y".repeat(500);
    // A normal batch near its 31 KiB bound that the collector never answers.
    let answer: (r: Response) => void = () => {};
    fetchMock.mockImplementationOnce(
      () => new Promise<Response>((r) => (answer = r)),
    );
    for (let i = 0; i < 19; i++) t.event("big", props);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    for (let i = 0; i < 19; i++) t.event("big", props);
    // Leaving: the browser refuses whatever would pass 64 KiB in flight.
    const refused: number[] = [];
    let inFlight = utf8Len(
      (fetchMock.mock.calls[0][1] as RequestInit).body as string,
    );
    fetchMock.mockImplementation((_u: string, init: RequestInit) => {
      const n = utf8Len(init.body as string);
      if (inFlight + n > 64 * 1024) {
        refused.push(n);
        return Promise.reject(new TypeError("Failed to fetch"));
      }
      inFlight += n;
      return reply(204).then((r) => {
        inFlight -= n;
        return r;
      });
    });
    t.flush(true);
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
    await vi.advanceTimersByTimeAsync(0);
    expect(refused).toEqual([]);
    expect(t.state().failures).toBe(0);
    // A refusal by the quota (the host page's own requests used it up) is
    // no failure of the collector: the batch waits for a later request.
    fetchMock.mockImplementation(() =>
      Promise.reject(new TypeError("Failed to fetch")),
    );
    t.event("late");
    const queued = t.state().queued;
    t.flush(true);
    await vi.advanceTimersByTimeAsync(0);
    expect(t.state().failures).toBe(0);
    expect(t.state().queued).toBe(queued);
    answer(new Response(null, { status: 204 }));
    inFlight = 0;
    fetchMock.mockImplementation(() => reply(204));
    await vi.advanceTimersByTimeAsync(LIMITS.flushAfterMs * 10);
    expect(t.state().queued).toBe(0);
    expect(t.state().dropped).toBe(0);
  });

  it("honours Retry-After on 429 and sends again later", async () => {
    const t = boot();
    fetchMock.mockImplementationOnce(() =>
      reply(429, { "Retry-After": "120" }),
    );
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    // The collector's pace, not a failure.
    expect(t.state().failures).toBe(0);
    expect(t.state().queued).toBe(2);
    await vi.advanceTimersByTimeAsync(60_000);
    t.flush();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(61_000);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(t.state().failures).toBe(0);
    expect(t.state().queued).toBe(0);
  });

  it("lets go of every answer's body", async () => {
    const t = boot();
    const cancel = vi.fn(() => Promise.resolve());
    fetchMock.mockImplementationOnce(() =>
      Promise.resolve({
        status: 429,
        headers: new Headers({ "Retry-After": "1" }),
        body: { cancel },
      }),
    );
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(cancel).toHaveBeenCalledTimes(1);
    // Without a body to cancel (a 204, an old browser) nothing breaks.
    await vi.advanceTimersByTimeAsync(LIMITS.flushAfterMs);
    expect(t.state().queued).toBe(0);
  });

  it("never switches itself off for the collector's pace", async () => {
    const t = boot();
    fetchMock.mockImplementation(() => reply(429, { "Retry-After": "1" }));
    for (let i = 0; i < LIMITS.failures * 2; i++) {
      t.flush();
      await vi.advanceTimersByTimeAsync(1001);
    }
    expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(
      LIMITS.failures * 2,
    );
    expect(t.state().disabled).toBe(false);
    expect(t.state().queued).toBe(2);
    fetchMock.mockImplementation(() => reply(204));
    await vi.advanceTimersByTimeAsync(LIMITS.flushAfterMs);
    expect(t.state().queued).toBe(0);
    expect(t.state().dropped).toBe(0);
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

  it("sends again a batch whose body came in too slowly", async () => {
    const t = boot();
    fetchMock.mockImplementationOnce(() => reply(408));
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(t.state().queued).toBe(2);
    expect(t.state().dropped).toBe(0);
    await vi.advanceTimersByTimeAsync(LIMITS.flushAfterMs + 10);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(t.state().queued).toBe(0);
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

  it("sends the end of a visit despite its own back-off, not despite Retry-After", async () => {
    const hide = () => {
      Object.defineProperty(document, "visibilityState", {
        value: "hidden",
        configurable: true,
      });
      document.dispatchEvent(new Event("visibilitychange"));
      Object.defineProperty(document, "visibilityState", {
        value: "visible",
        configurable: true,
      });
    };
    for (const [failure, sent] of [
      [() => reply(503), true],
      [() => reply(408), true],
      [() => Promise.reject(new TypeError("offline")), true],
      [() => reply(429), true],
      [() => reply(429, { "Retry-After": "120" }), false],
    ] as [() => Promise<Response>, boolean][]) {
      tracker?.stop();
      localStorage.clear();
      fetchMock.mockReset();
      fetchMock.mockImplementationOnce(failure);
      fetchMock.mockImplementation(() => reply(204));
      const t = boot();
      // The first batch fails: the visit's start waits in the queue.
      t.flush();
      await vi.advanceTimersByTimeAsync(0);
      expect(t.state().queued).toBe(2);
      document.dispatchEvent(new Event("pointerdown"));
      vi.advanceTimersByTime(1000);
      hide();
      await vi.advanceTimersByTimeAsync(0);
      if (sent) {
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(bodies()[1].events.map((e) => e.k)).toEqual(
          expect.arrayContaining(["session_start", "page_view", "engagement"]),
        );
        expect(t.state().queued).toBe(0);
      } else {
        // The pause the collector asked for holds.
        expect(fetchMock).toHaveBeenCalledTimes(1);
        expect(t.state().queued).toBeGreaterThan(2);
      }
    }
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
    s.setAttribute("src", "https://other.example.org/trakk.js");
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

  it("counts a change of path or route as a navigation, not of query alone", async () => {
    const t = boot();
    const go = (u: string, replace = false) => {
      vi.advanceTimersByTime(1000);
      history[replace ? "replaceState" : "pushState"](null, "", u);
    };
    for (const q of ["a", "ab", "abc"]) go("/?q=" + q, true);
    go("/?q=abcd");
    go("/#section");
    go("/app#/list");
    go("/app#/list?sort=new");
    go("/app#!/item/7");
    vi.advanceTimersByTime(1000);
    location.hash = "#/item/8";
    await vi.advanceTimersByTimeAsync(0);
    const ev = await flushed(t);
    expect(
      ev.filter((e) => e.k === "page_view").map((e) => e.url ?? "here"),
    ).toEqual([
      location.origin + "/",
      location.origin + "/app#/list",
      location.origin + "/app#!/item/7",
      "here",
    ]);
    expect(ev.filter((e) => e.k === "engagement")).toHaveLength(3);
  });

  it("stamps each page's events with its own URL and title", async () => {
    history.replaceState(null, "", "/a?x=1");
    document.title = "A";
    const t = boot();
    vi.advanceTimersByTime(2000);
    // Routers set the title after pushState.
    history.pushState(null, "", "/b");
    document.title = "B";
    vi.advanceTimersByTime(2000);
    history.pushState(null, "", "/c");
    document.title = "C";
    vi.advanceTimersByTime(2000);
    history.pushState(null, "", "/d");
    // Closed before the router set the title: the view still goes out.
    document.title = "D";
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
    const ev = bodies().flatMap((b) =>
      b.events.map((e) => ({
        k: e.k,
        url: e.url ?? b.ctx.url,
        title: (e as { title?: string }).title ?? b.ctx.title,
      })),
    );
    const at = (p: string) => location.origin + p;
    expect(
      ev.filter((e) => e.k === "page_view" || e.k === "engagement"),
    ).toEqual([
      { k: "page_view", url: at("/a?x=1"), title: "A" },
      { k: "engagement", url: at("/a?x=1"), title: "A" },
      { k: "page_view", url: at("/b"), title: "B" },
      { k: "engagement", url: at("/b"), title: "B" },
      { k: "page_view", url: at("/c"), title: "C" },
      { k: "engagement", url: at("/c"), title: "C" },
      { k: "page_view", url: at("/d"), title: "D" },
    ]);
    expect(t.state().queued).toBe(0);
  });

  it("gives each page its own budget, within a ceiling per document", async () => {
    const t = boot();
    fetchMock.mockImplementation(() => reply(204));
    const views = () => kinds().filter((k) => k === "page_view").length;
    for (let m = 0; m < 6; m++) {
      for (let i = 0; i < LIMITS.perMinute; i++) t.event("e");
      t.flush(true);
      await vi.advanceTimersByTimeAsync(61_000);
    }
    history.pushState(null, "", "/next");
    t.flush(true);
    await vi.advanceTimersByTimeAsync(61_000);
    expect(views()).toBe(2);
    // A loop of navigations stays within the document's ceiling.
    for (let m = 0; m < 40; m++) {
      for (let i = 0; i < LIMITS.perMinute; i++) {
        history.pushState(null, "", "/n" + m + "-" + i);
        t.event("e");
      }
      t.flush(true);
      await vi.advanceTimersByTimeAsync(61_000);
    }
    expect(kinds().length).toBeLessThanOrEqual(LIMITS.perDocument);
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

  it("never reads a password, hidden or other non-text field", async () => {
    const t = boot();
    document.body.innerHTML =
      '<form id="a"><input type="password" name="q" value="SECRET1"></form>' +
      '<form id="b"><input type="hidden" name="s" value="SECRET2"></form>' +
      '<form id="c"><input type="checkbox" name="term" value="SECRET3" checked></form>' +
      '<form id="d"><textarea name="query">SECRET4</textarea></form>' +
      '<form id="e"><input name="id" type="password" value="SECRET5">' +
      '<input name="tagName" value="SECRET6"><input id="x" name="query" value="ok"></form>';
    for (const id of ["a", "b", "c", "d"])
      document
        .getElementById(id)!
        .dispatchEvent(new Event("submit", { bubbles: true }));
    document
      .getElementById("x")!
      .dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    document
      .querySelectorAll("form")[4]
      .dispatchEvent(new Event("submit", { bubbles: true }));
    const ev = await flushed(t);
    const sent = fetchMock.mock.calls
      .map((c) => (c[1] as RequestInit).body as string)
      .join();
    expect(sent).not.toMatch(/SECRET/);
    expect(
      ev.filter((e) => e.k === "form" && e.p.phase === "submit"),
    ).toHaveLength(4);
    expect(ev.filter((e) => e.k === "search").map((e) => e.p)).toEqual([
      { term: "ok" },
    ]);
    expect(ev.find((e) => e.k === "form" && e.p.phase === "start")!.p.id).toBe(
      "e",
    );
    document.body.innerHTML = "";
  });

  it("drops a URL's fragment unless it is a client-side route", async () => {
    history.replaceState(null, "", "/cb?state=1#access_token=SECRET");
    const t = boot();
    history.pushState(null, "", "/app#/route/7");
    history.pushState(null, "", "/bang#!/x");
    document.body.innerHTML =
      '<a id="o1" href="https://auth.example.org/x?y=1#token=SECRET">a</a>' +
      '<a id="o2" href="https://other.example.org/#/r/1">b</a>' +
      '<a id="d1" href="/f.pdf#page=SECRET">c</a>' +
      '<form id="f" action="/reset?u=1#code=SECRET"><input id="i"></form>';
    for (const id of ["o1", "o2", "d1"])
      document
        .getElementById(id)!
        .dispatchEvent(new MouseEvent("click", { bubbles: true }));
    document
      .getElementById("i")!
      .dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    const ev = await flushed(t);
    expect(ev.find((e) => e.k === "form")!.p.action).toBe("/reset?u=1");
    const bs = bodies();
    expect(JSON.stringify(bs)).not.toMatch(/SECRET/);
    const views = ev.filter((e) => e.k === "page_view");
    expect(views[0].url).toBe(location.origin + "/cb?state=1");
    expect(views[1].url).toBe(location.origin + "/app#/route/7");
    expect(bs.at(-1)!.ctx.url).toBe(location.origin + "/bang#!/x");
    const links = ev.filter(
      (e) => e.k === "outbound_click" || e.k === "download",
    );
    expect(links.map((e) => e.p.url)).toEqual([
      "https://auth.example.org/x?y=1",
      "https://other.example.org/#/r/1",
      location.origin + "/f.pdf",
    ]);
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

  it("takes an error's type and stack, frames only, without queries", () => {
    const chrome = new TypeError("boom me@example.com\nsecret line");
    chrome.stack =
      "TypeError: boom me@example.com\nsecret line\n" +
      "    at Foo.#bar (https://x.example.com/a.js?token=1#f:12:34)\n" +
      "    at https://x.example.com/b.js?t=2:5:6\n    at <anonymous>";
    expect(errorProps(chrome)).toEqual({
      type: "TypeError",
      stack: [
        "at Foo.#bar (https://x.example.com/a.js:12:34)",
        "at https://x.example.com/b.js:5:6",
        "at <anonymous>",
      ],
    });
    const webkit = Object.assign(new Error(), {
      name: "SecurityError",
      stack:
        "pushState@[native code]\n@https://x.example.com/app.js?v=3:6:53\nnot a@frame",
    });
    expect(errorProps(webkit)).toEqual({
      type: "SecurityError",
      stack: ["pushState@[native code]", "@https://x.example.com/app.js:6:53"],
    });
    // What is no error, and one whose getters throw.
    for (const v of [null, undefined, "x", 3, {}, { stack: 7, name: 1 }])
      expect(errorProps(v)).toEqual({});
    const get = () => {
      throw new Error("no");
    };
    for (const v of [{}, new Error()]) {
      Object.defineProperties(v, { name: { get }, stack: { get } });
      expect(errorProps(v)).toEqual({});
    }
    // Bounded, whatever the page throws.
    const deep = {
      stack: ("    at f" + "x".repeat(5000) + " (u:1:1)\n").repeat(50),
    };
    const s = errorProps(deep).stack as string[];
    expect(s.length).toBeLessThanOrEqual(LIMITS.stackFrames);
    for (const f of s) expect(utf8Len(f)).toBeLessThanOrEqual(160);
  });

  it("takes a type from an error only, and only a class's name", () => {
    const frame = document.createElement("iframe");
    document.body.append(frame);
    const Foreign = (frame.contentWindow as typeof win).DOMException;
    frame.remove();
    expect(Foreign).not.toBe(DOMException);
    const type = (v: object) =>
      errorProps(Object.defineProperty(v, "stack", { value: undefined })).type;
    const proto = Object.create(null, { name: { value: "RangeError" } });
    for (const [v, want] of [
      [new TypeError("m"), "TypeError"],
      [new DOMException("m", "AbortError"), "AbortError"],
      // Another frame's.
      [new Foreign("m", "SecurityError"), "SecurityError"],
      [Object.setPrototypeOf(new Error("m"), proto), "RangeError"],
      // An ES5 subclass's.
      [Object.create(Error.prototype, { name: { value: "Old" } }), "Old"],
      [{ name: "Alice", message: "m" }, undefined],
      [new File([], "Alice"), undefined],
      [{ [Symbol.toStringTag]: "Person", name: "Alice" }, undefined],
      ...["Alice Example", "me@example.com", "a-b", "", "x".repeat(65), 3].map(
        (name) => [Object.assign(new Error("m"), { name }), undefined],
      ),
    ] as [object, string | undefined][])
      expect(type(v)).toBe(want);
  });

  describe("a stack's frames", () => {
    const own = "https://collector.example.com/trakk.js";
    const of = (...lines: string[]) =>
      errorProps({ stack: lines.join("\n") }, own);
    // Chromium's frames, below the message.
    const v8 = (...lines: string[]) =>
      of("Error: m", ...lines.map((l) => "    " + l));

    it("loses everything from the URL's ? or # to the position", () => {
      for (const [frame, want] of [
        [
          "at f (https://x/a.js?t=12:30:45&token=secret:5:6)",
          "at f (https://x/a.js:5:6)",
        ],
        [
          "at f (https://x/a.js?q=f(x)&token=secret:5:6)",
          "at f (https://x/a.js:5:6)",
        ],
        ["at https://x/a.js?q=f(x)&token=secret:5:6", "at https://x/a.js:5:6"],
        [
          "at f (https://x/app/(shop)/page.js?dpl=secret:5:6)",
          "at f (https://x/app/(shop)/page.js:5:6)",
        ],
        [
          "at https://x/app/(shop)/page.js?dpl=secret:5:6",
          "at https://x/app/(shop)/page.js:5:6",
        ],
        [
          "at f (https://x/a.js?u=https://y/b.js?z:1:2#secret(:3:4):5:6)",
          "at f (https://x/a.js:5:6)",
        ],
        [
          "at f (https://x/a.js#a?token=secret:5:6)",
          "at f (https://x/a.js:5:6)",
        ],
        [
          "at eval (eval at f (https://x/a.js?token=secret:5:6), <anonymous>:1:2)",
          "at eval (eval at f (https://x/a.js:5:6), <anonymous>:1:2)",
        ],
        [
          "at Foo.#bar (https://x/a.js?token=secret:5:6)",
          "at Foo.#bar (https://x/a.js:5:6)",
        ],
        ["at f (page.js?token=secret:5:6)", "at f (page.js:5:6)"],
        [
          "at Object.https://x/y?token=secret (https://x/a.js:5:6)",
          "at Object.https://x/y (https://x/a.js:5:6)",
        ],
      ])
        expect(v8(frame).stack).toEqual([want]);
      for (const [frame, want] of [
        [
          "f@https://x/a.js?t=12:30:45&token=secret:5:6",
          "f@https://x/a.js:5:6",
        ],
        [
          "https://x/app/(shop)/page.js?dpl=secret:5:6",
          "https://x/app/(shop)/page.js:5:6",
        ],
        [
          "f@https://x/a.js?to=me@https://y/b:1:2&token=secret:5:6",
          "f@https://x/a.js:5:6",
        ],
        [
          "f@https://x/a.js?token=secret line 5 > eval:1:2",
          "f@https://x/a.js line 5 > eval:1:2",
        ],
        [
          'a["https://x/y?token=secret"]@https://x/a.js:5:6',
          'a["https://x/y@https://x/a.js:5:6',
        ],
      ])
        expect(of(frame).stack).toEqual([want]);
    });

    it("has the query nowhere, whatever its place in the line", () => {
      for (const q of [
        "?",
        "#",
        "?a=:1:2&",
        "?(",
        "?)",
        "?a=(b)&",
        "?@",
        "?://",
      ])
        for (const p of ["", "(shop)/", "@scope/", "a:1:2/"]) {
          const url = `https://x/${p}a.js${q}secret:5:6`;
          for (const props of [
            v8(`at f (${url})`),
            v8(`at async f (${url})`),
            v8(`at new Foo (${url})`),
            v8(`at ${url}`),
            v8(`at async ${url}`),
            of("f@" + url),
            of("@" + url),
            of("global code@" + url),
            of(url),
          ]) {
            expect(JSON.stringify(props)).not.toContain("secret");
            expect(props).toMatchObject({
              src: `https://x/${p}a.js`,
              line: 5,
              col: 6,
            });
            expect(props.stack).toHaveLength(1);
          }
        }
    });

    it("places by the whole frame, and keeps a clipped one's position", () => {
      const url = "https://x.example.com/" + "p".repeat(300) + ".js";
      for (const props of [
        v8(`at f (${url}?t=1:7:9)`),
        of(`f@${url}?t=1:7:9`),
      ]) {
        expect(props).toMatchObject({ src: url, line: 7, col: 9 });
        const [frame] = props.stack as string[];
        expect(frame).toMatch(/p\.\.\.:7:9\)?$/);
        expect(utf8Len(frame)).toBe(LIMITS.frameBytes);
      }
      // A query longer than a line of the stack is stored.
      expect(v8(`at f (https://x/a.js?${"q".repeat(3000)}:7:9)`)).toEqual({
        stack: ["at f (https://x/a.js:7:9)"],
        src: "https://x/a.js",
        line: 7,
        col: 9,
      });
      const named = v8(`at ${"ü".repeat(300)} (https://x/a.js:7:9)`);
      expect(named.src).toBe("https://x/a.js");
      const [frame] = named.stack as string[];
      expect(frame).toMatch(/^at ü+\.\.\.:7:9\)$/);
      expect(utf8Len(frame)).toBeLessThanOrEqual(LIMITS.frameBytes);
      // The last line read may be a frame cut in half.
      expect(
        of("f@https://x/a.js:1:2", "g@https://x/" + "a:3:4".repeat(2000)).stack,
      ).toEqual(["f@https://x/a.js:1:2"]);
    });

    it("may have a name with spaces where a URL follows", () => {
      const frames = [
        "global code@https://x/a.js:1:2",
        "module code@https://x/a.js:1:2",
        "eval code@https://x/a.js:1:2",
        "promise callback*f@https://x/a.js:1:2",
        "setTimeout handler*f@https://x/a.js:1:2",
        "f@https://cdn.example/@scope/pkg.js:1:2",
      ];
      expect(of(...frames).stack).toEqual(frames);
      expect(
        of(
          "write to me@example.com",
          "me@example.com",
          "mail me@example.com:1:2",
          "see me@https://x/a.js",
          "me@12:30:45",
          "eval code@",
        ),
      ).toEqual({});
      // Chromium's stack has no such frames, only message lines like them.
      expect(
        v8("at f (https://x/a.js:1:2)", "me@example.com:1:2").stack,
      ).toEqual(["at f (https://x/a.js:1:2)"]);
    });

    it("are Chromium's by their place, never the message's lines", () => {
      const frames = [
        "at f (https://x/a.js:1:2)",
        "at https://x/a.js:1:2",
        "at async f (https://x/a.js:1:2)",
        "at async https://x/a.js:1:2",
        "at new Foo (https://x/a.js:1:2)",
        "at get size (https://x/a.js:1:2)",
        "at Foo.bar [as baz] (https://x/a.js:1:2)",
      ];
      expect(v8(...frames).stack).toEqual(frames);
      const rest = [
        "at Array.map (<anonymous>)",
        "at <anonymous>",
        "at <anonymous>:1:2",
        "at g (<anonymous>:1:2)",
        "at h (native)",
        "at async Promise.all (index 0)",
        "at eval (eval at f (https://x/a.js:5:6), <anonymous>:1:2)",
        "at eval (eval at g (eval at f (https://x/a.js:5:6)), <anonymous>:1:2)",
      ];
      expect(v8(...rest).stack).toEqual(rest);
      const other = [
        "at eval (eval at f (<anonymous>:5:6), <anonymous>:1:2)",
        "at eval (eval at f (unknown source), <anonymous>:1:2)",
        "at w (wasm://wasm/0a1b2c:wasm-function[3]:0x1f)",
        "at about:srcdoc:1:2",
      ];
      expect(v8(...other).stack).toEqual(other);
      const said = [
        "at the end of token secret",
        "at 12:30:45",
        "at me@example.com",
        "at me@example.com:1:2",
        "at noon (see https://x/?token=secret)",
        "at noon (12:30:45)",
        "at native speed",
        "at https://x/?token=secret",
      ];
      expect(v8(...said)).toEqual({});
      expect(v8(...said, frames[0]).stack).toEqual([frames[0]]);
    });

    it("begin below the message, though it looks like them", () => {
      const message = "no\n    at f (https://x/token:1:2)\n    at <anonymous>";
      const frame = "    at g (https://x/a.js:3:4)";
      const want = {
        stack: ["at g (https://x/a.js:3:4)"],
        src: "https://x/a.js",
        line: 3,
        col: 4,
      };
      // Under its name, the name it had when it was made, or none.
      for (const head of ["E: ", "Error: ", ""])
        expect(
          errorProps({ message, stack: head + message + "\n" + frame }, own),
        ).toEqual(want);
      // A message longer than what is read of a stack.
      const long = message.repeat(1000);
      expect(
        errorProps({ message: long, stack: `E: ${long}\n${frame}` }, own),
      ).toEqual(want);
      // Firefox and WebKit have none.
      for (const m of ["f", "f@https", ""])
        expect(
          errorProps({ message: m, stack: "f@https://x/a.js:3:4" }).stack,
        ).toEqual(["f@https://x/a.js:3:4"]);
    });

    it("place an error at the first file not the script's own", () => {
      const at = (src: string, line: number, col: number) => ({
        src,
        line,
        col,
      });
      for (const [props, want] of [
        [
          v8(
            "at history.pushState (<anonymous>)",
            `at ${own}?v=2:2:9998`,
            "at https://x.example/app/(shop)/page.js:5:6",
            "at next (https://x.example/b.js:1:1)",
          ),
          at("https://x.example/app/(shop)/page.js", 5, 6),
        ],
        [
          v8("at f (https://x.example/app/(shop)/page.js:5:6)"),
          at("https://x.example/app/(shop)/page.js", 5, 6),
        ],
        // A path that ends in a parenthesis loses only Chromium's own.
        [
          v8("at f (https://x.example/(a):5:6)"),
          at("https://x.example/(a)", 5, 6),
        ],
        [
          of(
            "pushState@[native code]",
            `@${own}:2:1`,
            "global code@https://x/a.js:5:6",
          ),
          at("https://x/a.js", 5, 6),
        ],
        // Evaluated code: where eval was called.
        [
          v8("at eval (eval at f (https://x/a.js:5:6), <anonymous>:1:2)"),
          at("https://x/a.js", 5, 6),
        ],
        [of("f@https://x/a.js line 5 > eval:1:2"), at("https://x/a.js", 5, 0)],
        // A wasm offset is no line.
        [
          v8(
            "at f (https://x/m.wasm:wasm-function[3]:0x1f)",
            "at https://x/a.js:1:2",
          ),
          at("https://x/m.wasm", 0, 0),
        ],
        // Not understood: no place rather than its caller's.
        [v8("at f (https://x/a b.js:5:6)", "at g (https://x/b.js:1:2)"), {}],
        [of("f@https://x/a b.js:5:6", "g@https://x/b.js:1:2"), {}],
        // Below a message line with a URL that is no frame.
        [
          of("E: see https://x/y", "    at g (https://x/b.js:1:2)"),
          at("https://x/b.js", 1, 2),
        ],
      ] as [Record<string, unknown>, object][]) {
        const { stack, ...place } = props;
        expect(place).toEqual(want);
        expect(stack).not.toEqual([]);
      }
      // With the event's own place there is none to look for.
      expect(errorProps({ stack: "f@https://x/a.js:5:6" })).toEqual({
        stack: ["f@https://x/a.js:5:6"],
      });
    });

    it("cost hostile text no more than its length", () => {
      const parts = [
        "@",
        "(",
        " (",
        ")",
        ":",
        ":1",
        ":1:2",
        "?",
        "#",
        "://",
        "a://",
        "@a://",
        " line 1 > eval",
        " at ",
        "    at ",
        "    at f (",
        " ",
        "1",
        "a",
        "\n",
        "    at (https://x/",
        "eval at f (",
        "), <anonymous>",
        "index 1",
        "\ud800",
        ":wasm-function[1]:0x",
        "?x:1:2",
      ];
      const heads = [
        "",
        "    at ",
        "    at f (",
        "f@",
        "f@https://x/",
        "    at f (https://x/a?",
      ];
      vi.useRealTimers();
      let slowest = 0;
      for (const h of heads)
        for (const p of parts)
          for (const q of ["", "!", ":1:2", ":1:2)"])
            for (const message of [undefined, p.repeat(3000)]) {
              const stack = h + p.repeat(8000 / p.length) + q + "\n" + h + p;
              const began = performance.now();
              const got = errorProps({ message, stack }, own);
              slowest = Math.max(slowest, performance.now() - began);
              for (const f of (got.stack || []) as string[])
                expect(utf8Len(f)).toBeLessThanOrEqual(LIMITS.frameBytes);
            }
      // Measured: under 1 ms.
      expect(slowest).toBeGreaterThan(0);
      expect(slowest).toBeLessThan(50);
    });
  });

  it("places a rejection by its stack, and keeps the message when the props are full", async () => {
    const t = boot();
    const at = (file: string) =>
      `f@[native code]\ng@https://x.example.com/${file}:7:9`;
    const rej = new Event("unhandledrejection") as Event & { reason: unknown };
    rej.reason = Object.assign(new Error("gone"), {
      name: "AbortError",
      stack: at("a.js?q=1"),
    });
    window.dispatchEvent(rej);
    window.dispatchEvent(
      new ErrorEvent("error", {
        message: "M".repeat(900),
        filename: "https://x.example.com/" + "p".repeat(2500),
        lineno: 1,
        error: { name: "E", stack: (at("z".repeat(400)) + "\n").repeat(20) },
      }),
    );
    const errs = (await flushed(t)).filter((e) => e.k === "js_error");
    expect(errs[0].p).toEqual({
      msg: "Unhandled rejection: gone",
      type: "AbortError",
      stack: ["f@[native code]", "g@https://x.example.com/a.js:7:9"],
      src: "https://x.example.com/a.js",
      line: 7,
      col: 9,
    });
    expect(errs[1].p.msg).toBe("M".repeat(LIMITS.strBytes));
    expect(errs[1].p.dropped).toBeUndefined();
    expect(bytes(JSON.stringify(errs[1].p))).toBeLessThanOrEqual(
      LIMITS.propsBytes,
    );
  });

  it("keeps the message where only the stack is too much", async () => {
    const t = boot();
    window.dispatchEvent(
      new ErrorEvent("error", {
        message: '"'.repeat(512),
        filename: "https://x.example.com/" + "p".repeat(2000),
        error: {
          stack: (
            "f@https://x.example.com/" +
            '"'.repeat(130) +
            ":1:1\n"
          ).repeat(8),
        },
      }),
    );
    const e = (await flushed(t)).find((x) => x.k === "js_error")!;
    expect(e.p.msg).toBe('"'.repeat(512));
    expect(e.p.stack).toBeUndefined();
  });

  it("reads nothing of an error it would not send", async () => {
    const t = boot();
    let read: string[] = [];
    const error = (message: string) =>
      Object.defineProperties(
        new Error(),
        Object.fromEntries(
          Object.entries({ name: "E", message, stack: "f@u:1:2" }).map(
            ([k, v]) => [k, { get: () => (read.push(k), v) }],
          ),
        ),
      );
    const fire = (m: string) =>
      window.dispatchEvent(
        new ErrorEvent("error", { message: m, error: error(m) }),
      );
    const reject = (m: string) =>
      window.dispatchEvent(
        Object.assign(new Event("unhandledrejection"), { reason: error(m) }),
      );
    const reads = (f: () => void) => {
      read = [];
      f();
      return read;
    };
    const stacks = (f: () => void) =>
      reads(f).filter((k) => k === "stack").length;
    const times = (n: number, f: (m: string) => void) => () => {
      for (let i = 0; i < n; i++) f("same");
    };
    expect(stacks(times(10, fire))).toBe(LIMITS.sameError);
    expect(stacks(times(10, reject))).toBe(LIMITS.sameError);
    // Too many kinds. A rejection's message says which it is.
    for (let i = 0; i < LIMITS.errorKinds; i++) fire("E" + i);
    expect(t.state().errorKeys).toBe(LIMITS.errorKinds);
    expect(reads(() => fire("another"))).toEqual([]);
    expect(reads(() => reject("another"))).toEqual(["message"]);
    const both = () => {
      fire("E1");
      reject("E1");
    };
    expect(reads(both)).not.toEqual([]);
    // Too many events.
    while (!t.state().dropped) t.event("x");
    expect(reads(both)).toEqual([]);
    // Switched off.
    await vi.advanceTimersByTimeAsync(61_000);
    expect(reads(both)).not.toEqual([]);
    fetchMock.mockImplementation(() => reply(403));
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    expect(t.state().disabled).toBe(true);
    expect(reads(both)).toEqual([]);
  });

  // WebKit and Chromium hide an error that comes out of the wrapped history
  // call: the window's error event is their stand-in.
  describe("what a wrapped history call throws", () => {
    const HIDDEN = { msg: "Script error.", src: "", line: 0, col: 0 };
    const hidden = (init: ErrorEventInit = {}) =>
      window.dispatchEvent(
        new ErrorEvent("error", { message: "Script error.", ...init }),
      );
    const webkit = (message: string) =>
      Object.assign(new Error(message), {
        name: "SecurityError",
        stack:
          "pushState@[native code]\n@https://collector.example.com/trakk.js:2:9998\n" +
          "later@https://x.example.com/app.js?token=1:7:53\nnext@https://x.example.com/b.js:1:1",
      });
    const reported = (message: string) => ({
      msg: "SecurityError: " + message,
      type: "SecurityError",
      stack: [
        "pushState@[native code]",
        "@https://collector.example.com/trakk.js:2:9998",
        "later@https://x.example.com/app.js:7:53",
        "next@https://x.example.com/b.js:1:1",
      ],
      src: "https://x.example.com/app.js",
      line: 7,
      col: 53,
    });
    // A page whose history calls throw v, and the script on it.
    const page = (v: unknown, src = SRC) => {
      for (const m of ["pushState", "replaceState"] as const)
        vi.spyOn(history, m).mockImplementation(() => {
          throw v;
        });
      tracker = start(win, src);
      return tracker!;
    };
    const call = (m: "pushState" | "replaceState" = "pushState") => {
      try {
        history[m](null, "", "/x");
      } catch (e) {
        return [e];
      }
      return [];
    };
    const errors = async (t: Tracker) =>
      (await flushed(t)).filter((e) => e.k === "js_error").map((e) => e.p);

    it("reaches the page unchanged, and is reported for the browser's stand-in", async () => {
      const v = webkit("Attempt to use history.pushState()");
      // The script's own frame has no query.
      const t = page(v, SRC + "?v=2");
      for (const m of ["pushState", "replaceState"] as const) {
        expect(call(m)[0]).toBe(v);
        hidden();
      }
      expect(call()[0]).toBe(v);
      hidden({ message: "Script error" });
      expect(await errors(t)).toEqual(
        Array(3).fill(reported("Attempt to use history.pushState()")),
      );
    });

    it("is no error where the page catches it", async () => {
      const t = page(webkit("caught"));
      call();
      await vi.advanceTimersByTimeAsync(0);
      expect(await errors(t)).toEqual([]);
    });

    it("outlives its task's microtasks, never the task", async () => {
      const t = page(webkit("gone"));
      call();
      await Promise.resolve();
      hidden();
      call();
      call();
      await vi.advanceTimersByTimeAsync(0);
      hidden();
      // Armed again by a later task's.
      call();
      hidden();
      call();
      await vi.advanceTimersByTimeAsync(0);
      hidden();
      expect(await errors(t)).toEqual([
        reported("gone"),
        HIDDEN,
        reported("gone"),
        HIDDEN,
      ]);
    });

    it("is too old soon, though its timer is held back", async () => {
      const t = page(webkit("old"));
      // As in a hidden tab: not fired while the clock moves on.
      vi.spyOn(globalThis, "setTimeout").mockImplementationOnce(
        () => 1 as unknown as ReturnType<typeof setTimeout>,
      );
      const began = Date.now();
      for (const age of [0, 50, 51, 900, -1]) {
        vi.setSystemTime(began);
        call();
        vi.setSystemTime(began + age);
        hidden();
      }
      const old = reported("old");
      expect(await errors(t)).toEqual([old, old, HIDDEN, HIDDEN, HIDDEN]);
    });

    it("is used once, the latest of several", async () => {
      const v = webkit("first");
      const t = page(v);
      call();
      v.message = "second";
      call();
      hidden();
      hidden();
      expect(await errors(t)).toEqual([reported("second"), HIDDEN]);
    });

    it("is not read unless it is reported", () => {
      const read = vi.fn();
      const t = page(
        new Proxy(
          {},
          { get: (_, k) => read(k), has: (_, k) => !read("has", k) },
        ),
      );
      call();
      vi.advanceTimersByTime(0);
      expect(read).not.toHaveBeenCalled();
      // Nor once there are too many events.
      while (!t.state().dropped) t.event("x");
      call();
      hidden();
      expect(read).not.toHaveBeenCalled();
    });

    it("has its stack read no more often than it is reported", async () => {
      const v = webkit("again");
      const stack = v.stack;
      const read = vi.fn(() => stack);
      const t = page(Object.defineProperty(v, "stack", { get: read }));
      for (let i = 0; i < 10; i++) {
        call();
        hidden();
      }
      expect(await errors(t)).toEqual(
        Array(LIMITS.sameError).fill(reported("again")),
      );
      expect(read).toHaveBeenCalledTimes(LIMITS.sameError);
    });

    it("leaves a readable error as the event has it, and is used up by it", async () => {
      const t = page(webkit("used up"));
      const readable: ErrorEventInit[] = [
        {
          message: "Uncaught SecurityError: x",
          filename: "https://x.example.com/app.js",
          lineno: 7,
          colno: 3,
        },
        { message: "Script error.", filename: "webkit-masked-url://hidden/" },
        { message: "Script error.", lineno: 4 },
        { message: "another" },
      ];
      for (const init of readable) {
        call();
        hidden(init);
      }
      hidden();
      expect(await errors(t)).toEqual([
        ...readable.map((i) => ({
          msg: i.message,
          src: i.filename ?? "",
          line: i.lineno ?? 0,
          col: i.colno ?? 0,
        })),
        HIDDEN,
      ]);
    });

    it("is the caller's where the browser blames this script for it", async () => {
      const t = page(undefined);
      let n = 0;
      const fire = (filename: string, stack?: string) =>
        hidden({
          message: "SecurityError: " + n++,
          filename,
          lineno: 4,
          colno: 10518,
          error: { stack },
        });
      // As WebKit has it for a script's top-level code.
      const ours = `pushState@[native code]\n@${SRC}?v=2:4:10480`;
      const theirs = "global code@https://x.example.com/app.js:6:18";
      fire(SRC + "?v=2", ours + "\n" + theirs);
      // No place rather than this script's: no frame is the page's, or none is
      // understood, or there is no stack.
      fire(SRC, ours);
      fire(SRC, ours + "\nf@https://x.example.com/a b.js:5:6\n" + theirs);
      fire(SRC);
      // Any other file's error is where the event says.
      fire("https://x.example.com/b.js", theirs);
      expect(
        (await errors(t)).map(({ src, line, col }) => [src, line, col]),
      ).toEqual([
        ["https://x.example.com/app.js", 6, 18],
        ["", 0, 0],
        ["", 0, 0],
        ["", 0, 0],
        ["https://x.example.com/b.js", 4, 10518],
      ]);
    });

    it("can be any value, its getters throwing too", async () => {
      const trap = Object.defineProperties(
        new Error(),
        Object.fromEntries(
          ["name", "message", "stack"].map((k) => [
            k,
            {
              get: () => {
                throw new Error("no");
              },
            },
          ]),
        ),
      );
      const at = { src: "", line: 0, col: 0 };
      for (const [v, want] of [
        ["plain", { msg: "plain" }],
        [undefined, { msg: "undefined" }],
        [null, { msg: "null" }],
        [0, { msg: "0" }],
        ["", { msg: "Error" }],
        [Symbol("s"), { msg: "Symbol(s)" }],
        [Object.create(null), { msg: "Error" }],
        [
          Object.assign(new Error(), { name: "E", stack: undefined }),
          { msg: "E", type: "E" },
        ],
        // No error: its name is data.
        [{ name: "Alice", message: "" }, { msg: "Error" }],
        [{ name: 3, message: "m" }, { msg: "m" }],
        [trap, { msg: "Error" }],
      ] as [unknown, object][]) {
        const t = page(v);
        expect(call()).toEqual([v]);
        hidden();
        fetchMock.mockClear();
        expect(await errors(t)).toEqual([{ ...at, ...want }]);
        t.stop();
        vi.restoreAllMocks();
      }
    });

    it("has no place where no frame is the page's", async () => {
      const t = page(
        Object.assign(webkit("x"), {
          stack: "pushState@[native code]\n@" + SRC + ":2:9998",
        }),
      );
      call();
      hidden();
      expect(await errors(t)).toEqual([
        {
          msg: "SecurityError: x",
          type: "SecurityError",
          stack: ["pushState@[native code]", "@" + SRC + ":2:9998"],
          src: "",
          line: 0,
          col: 0,
        },
      ]);
    });

    it("is reported once where it rejects a promise", async () => {
      const t = page(webkit("async"));
      const rej = new Event("unhandledrejection") as Event & {
        reason: unknown;
      };
      [rej.reason] = call();
      window.dispatchEvent(rej);
      hidden();
      expect(await errors(t)).toEqual([
        { ...reported("async"), msg: "Unhandled rejection: async" },
        HIDDEN,
      ]);
    });

    it("is not kept where its end cannot be timed", async () => {
      const v = webkit("untimed");
      const t = page(v);
      vi.spyOn(globalThis, "setTimeout").mockImplementationOnce(() => {
        throw new Error("no");
      });
      expect(call()[0]).toBe(v);
      hidden();
      expect(await errors(t)).toEqual([HIDDEN]);
    });
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

  it("remembers distinct errors per page, within a ceiling per document", async () => {
    const t = boot();
    for (let page = 0; page < 7; page++) {
      history.pushState(null, "", "/p" + page);
      for (let i = 0; i < 30; i++)
        window.dispatchEvent(
          new ErrorEvent("error", { message: "P" + page + " E" + i }),
        );
      expect(t.state().errorKeys).toBeLessThanOrEqual(LIMITS.errorKinds);
      // Past the per-minute cap, and the batch out.
      await vi.advanceTimersByTimeAsync(61_000);
    }
    const errs = kinds().filter((k) => k === "js_error");
    expect(errs).toHaveLength(LIMITS.errorKindsPerDocument);
    const msgs = bodies()
      .flatMap((b) => b.events)
      .filter((e) => e.k === "js_error")
      .map((e) => String(e.p.msg));
    // A later page's new error is reported: the first page's did not use up
    // the table.
    expect(msgs).toContain("P1 E0");
    expect(msgs).not.toContain("P5 E0");
  });

  it("sends engagement and vitals when the page is hidden", async () => {
    const po = observers();
    boot();
    po.emit("paint", [{ name: "first-contentful-paint", startTime: 300 }]);
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
    expect(eng.p.inputs).toEqual({
      pointer: 1,
      key: 1,
      touch: 0,
      scroll: 1,
      move: 0,
      mdown: 0,
      wheel: 0,
      synth: 2,
    });
    expect(ev.find((e) => e.k === "vital" && e.p.name === "CLS")).toBeTruthy();
    // A second hide does not repeat the vitals.
    window.dispatchEvent(new Event("pagehide"));
    await vi.advanceTimersByTimeAsync(0);
    const cls = bodies()
      .flatMap((b) => b.events)
      .filter((e) => e.k === "vital" && e.p.name === "CLS");
    expect(cls).toHaveLength(1);
  });

  it("counts each page's own input in its engagement", async () => {
    boot();
    document.dispatchEvent(new Event("pointerdown"));
    document.dispatchEvent(new Event("pointerdown"));
    vi.advanceTimersByTime(1000);
    history.pushState(null, "", "/b");
    document.dispatchEvent(new KeyboardEvent("keydown"));
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
    const eng = bodies()
      .flatMap((b) => b.events)
      .filter((e) => e.k === "engagement");
    // Every event a test dispatches is untrusted: synth counts them too.
    const none = { touch: 0, scroll: 0, move: 0, mdown: 0, wheel: 0 };
    expect(eng.map((e) => e.p.inputs)).toEqual([
      { ...none, pointer: 2, key: 0, synth: 2 },
      { ...none, pointer: 0, key: 1, synth: 1 },
    ]);
    // The context keeps the document's totals.
    expect(bodies().at(-1)!.ctx.inputs).toEqual({
      ...none,
      pointer: 2,
      key: 1,
      synth: 3,
    });
  });

  it("counts mouse travel and presses apart, and input a script made", async () => {
    const t = boot();
    const mouse = (type: string) => {
      const e = new Event(type);
      Object.defineProperty(e, "pointerType", { value: "mouse" });
      document.dispatchEvent(e);
    };
    for (let i = 0; i < 5; i++) mouse("pointermove");
    mouse("pointerdown");
    // A wheel event a script dispatched is none of a person's.
    document.dispatchEvent(new Event("wheel"));
    const pen = new Event("pointermove");
    Object.defineProperty(pen, "pointerType", { value: "pen" });
    document.dispatchEvent(pen);
    t.flush();
    await vi.advanceTimersByTimeAsync(0);
    const ctx = bodies().at(-1)!.ctx;
    expect(ctx.inputs).toMatchObject({
      pointer: 1,
      move: 5,
      mdown: 1,
      wheel: 0,
      synth: 1,
    });
    // Every event a test dispatches is untrusted.
    expect(ctx.synth).toBe(1);
  });

  it("measures the scroll depth at most once a frame, never in the handler", async () => {
    const reads = { n: 0 };
    const height = (el: Element, h: number) =>
      Object.defineProperty(el, "scrollHeight", {
        configurable: true,
        get: () => {
          reads.n++;
          return h;
        },
      });
    height(document.documentElement, 2000);
    height(document.body, 1000);
    try {
      for (const frames of [true, false]) {
        tracker?.stop();
        fetchMock.mockClear();
        if (!frames) vi.stubGlobal("requestAnimationFrame", undefined);
        boot();
        reads.n = 0;
        for (let i = 0; i < 30; i++) window.dispatchEvent(new Event("scroll"));
        expect(reads.n).toBe(0);
        await vi.advanceTimersByTimeAsync(50);
        // Once for the frame, both heights.
        expect(reads.n).toBe(frames ? 2 : 0);
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
        expect(reads.n).toBe(2);
        const eng = bodies()
          .flatMap((b) => b.events)
          .find((e) => e.k === "engagement")!;
        expect(eng.p.scroll).toBe(
          Math.round(((window.scrollY + window.innerHeight) / 2000) * 100),
        );
      }
    } finally {
      delete (document.documentElement as { scrollHeight?: number })
        .scrollHeight;
      delete (document.body as { scrollHeight?: number }).scrollHeight;
    }
  });

  it("sends one engagement when a page is left, not one per signal", async () => {
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    boot();
    document.dispatchEvent(new Event("pointerdown"));
    vi.advanceTimersByTime(3000);
    // Leaving: pagehide first, the page still visible, then hidden.
    window.dispatchEvent(new Event("pagehide"));
    vi.advanceTimersByTime(20);
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
    expect(kinds().filter((k) => k === "engagement")).toHaveLength(1);
  });

  it("waits for a prerendered page to be shown, and times vitals from then", async () => {
    type Entry = { name?: string; startTime: number };
    class FakePO {
      static supportedEntryTypes = ["paint", "largest-contentful-paint"];
      static all: FakePO[] = [];
      type = "";
      constructor(public cb: (l: { getEntries: () => Entry[] }) => void) {
        FakePO.all.push(this);
      }
      observe(o: { type: string }) {
        this.type = o.type;
      }
      disconnect() {}
    }
    const emit = (type: string, entries: Entry[]) =>
      FakePO.all
        .filter((o) => o.type === type)
        .forEach((o) => o.cb({ getEntries: () => entries }));
    vi.stubGlobal("PerformanceObserver", FakePO);
    vi.spyOn(performance, "getEntriesByType").mockReturnValue([
      { type: "navigate", responseStart: 500, activationStart: 2000 },
    ] as unknown as PerformanceEntryList);
    Object.defineProperty(document, "prerendering", {
      value: true,
      configurable: true,
    });
    (win as { trakk?: unknown }).trakk = { q: [["early", {}]] };
    try {
      const t = boot();
      emit("paint", [{ name: "first-contentful-paint", startTime: 300 }]);
      emit("largest-contentful-paint", [{ startTime: 2600 }]);
      t.event("during");
      window.dispatchEvent(new ErrorEvent("error", { message: "x" }));
      window.dispatchEvent(new Event("pagehide"));
      t.flush(true);
      await vi.advanceTimersByTimeAsync(LIMITS.flushAfterMs * 2);
      expect(fetchMock).not.toHaveBeenCalled();
      expect(t.state().queued).toBe(0);

      Object.defineProperty(document, "prerendering", {
        value: false,
        configurable: true,
      });
      document.dispatchEvent(new Event("prerenderingchange"));
      vi.advanceTimersByTime(1000);
      window.dispatchEvent(new Event("pagehide"));
      await vi.advanceTimersByTimeAsync(0);
      const ev = bodies().flatMap((b) => b.events);
      expect(ev.map((e) => e.n ?? e.k)).toEqual(
        expect.arrayContaining(["session_start", "page_view", "early"]),
      );
      expect(ev.map((e) => e.n)).not.toContain("during");
      const vitals = Object.fromEntries(
        ev.filter((e) => e.k === "vital").map((e) => [e.p.name, e.p.value]),
      );
      expect(vitals).toMatchObject({ TTFB: 0, FCP: 0, LCP: 600 });
    } finally {
      delete (document as { prerendering?: boolean }).prerendering;
    }
  });

  it("sends no paint of a page shown hidden on activation", async () => {
    const po = observers();
    Object.defineProperty(document, "prerendering", {
      value: true,
      configurable: true,
    });
    try {
      boot();
      po.emit("paint", [{ name: "first-contentful-paint", startTime: 300 }]);
      po.emit("largest-contentful-paint", [{ startTime: 400 }]);
      Object.defineProperty(document, "prerendering", {
        value: false,
        configurable: true,
      });
      setVisibility("hidden");
      document.dispatchEvent(new Event("prerenderingchange"));
      setVisibility("visible");
      hide(5000);
      await vi.advanceTimersByTimeAsync(0);
      expect(vitalsOf().map((e) => e.p.name)).toEqual([]);
    } finally {
      delete (document as { prerendering?: boolean }).prerendering;
    }
  });

  it("takes custom events, including those queued before it loaded", async () => {
    (win as { trakk?: unknown }).trakk = {
      q: [["copy_prefix_list", { asn: 15169 }]],
    };
    const t = boot();
    const g = (
      win as unknown as {
        trakk: {
          event: (n: string, p?: unknown) => void;
          q: { push: (a: unknown) => void };
        };
      }
    ).trakk;
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

type Entry = { startTime: number; [k: string]: unknown };
// The browser's performance observers: entries handed to the callbacks
// (emit), an event's only from its observer's duration threshold up, or
// held until the tracker takes them (buffer).
function observers(
  types = [
    "paint",
    "largest-contentful-paint",
    "layout-shift",
    "event",
    "first-input",
  ],
  interactionIds = true,
) {
  class FakePO {
    static supportedEntryTypes = types;
    static all: FakePO[] = [];
    type = "";
    threshold = 104;
    held: Entry[] = [];
    constructor(public cb: (l: { getEntries: () => Entry[] }) => void) {
      FakePO.all.push(this);
    }
    observe(o: { type: string; durationThreshold?: number }) {
      this.type = o.type;
      this.threshold = o.durationThreshold ?? 104;
    }
    takeRecords() {
      return this.held.splice(0);
    }
    disconnect() {}
  }
  vi.stubGlobal("PerformanceObserver", FakePO);
  vi.stubGlobal(
    "PerformanceEventTiming",
    interactionIds
      ? class {
          get interactionId() {
            return 0;
          }
        }
      : undefined,
  );
  const of = (type: string) => FakePO.all.filter((o) => o.type === type);
  return {
    emit: (type: string, entries: Entry[]) =>
      of(type).forEach((o) =>
        o.cb({
          getEntries: () =>
            type === "event"
              ? entries.filter((e) => Number(e.duration) >= o.threshold)
              : entries,
        }),
      ),
    buffer: (type: string, entries: Entry[]) =>
      of(type).forEach((o) => o.held.push(...entries)),
  };
}
const setVisibility = (v: DocumentVisibilityState) =>
  Object.defineProperty(document, "visibilityState", {
    value: v,
    configurable: true,
  });
// Hidden at timeStamp, on the entries' clock (by default after their
// paints), then visible again without an event.
function hide(timeStamp = 60_000) {
  setVisibility("hidden");
  const e = new Event("visibilitychange");
  Object.defineProperty(e, "timeStamp", { value: timeStamp });
  document.dispatchEvent(e);
  setVisibility("visible");
}
const vitalsOf = () =>
  bodies()
    .flatMap((b) =>
      b.events.map((e) => ({
        ...e,
        url: e.url ?? String(b.ctx.url),
        title: (e as { title?: string }).title ?? String(b.ctx.title),
      })),
    )
    .filter((e) => e.k === "vital");
const vitalValues = () =>
  Object.fromEntries(vitalsOf().map((e) => [e.p.name, e.p.value]));
const FCP = { name: "first-contentful-paint", startTime: 300 };
const shift = (startTime: number, value: number, hadRecentInput = false) => ({
  startTime,
  value,
  hadRecentInput,
});

describe("web vitals", () => {
  it("reports the worst window of layout shifts, not their sum", async () => {
    const po = observers();
    boot();
    po.emit("paint", [FCP]);
    po.emit("layout-shift", [
      shift(1000, 0.1),
      shift(1100, 0.1),
      shift(1150, 1, true),
      shift(1200, 0.1),
      // 2 s later: a window of its own.
      shift(3200, 0.1),
      shift(3300, 0.1),
    ]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues()).toEqual({ FCP: 300, CLS: 0.3 });
  });

  it("ends a window at a gap of a second", async () => {
    const po = observers();
    boot();
    po.emit("paint", [FCP]);
    po.emit("layout-shift", [
      shift(100, 0.1),
      shift(1099, 0.1),
      shift(2099, 0.15),
      shift(2500, 0.1),
    ]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues().CLS).toBe(0.25);
  });

  it("ends a window after five seconds", async () => {
    const po = observers();
    boot();
    po.emit("paint", [FCP]);
    po.emit(
      "layout-shift",
      [10000, 10900, 11800, 12700, 13600, 14500, 15000].map((t) =>
        shift(t, 0.1),
      ),
    );
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues().CLS).toBe(0.6);
  });

  it("sends CLS, 0 included, only where layout shifts are observed and after a first paint", async () => {
    let po = observers(["paint"]);
    boot();
    po.emit("paint", [FCP]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues()).toEqual({ FCP: 300 });

    tracker!.stop();
    fetchMock.mockClear();
    po = observers();
    boot();
    po.emit("layout-shift", [shift(100, 0.1)]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues()).toEqual({});

    tracker!.stop();
    fetchMock.mockClear();
    po = observers();
    boot();
    po.emit("paint", [FCP]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues()).toEqual({ FCP: 300, CLS: 0 });
  });

  it("reports the second longest of 60 interactions, each at its longest entry", async () => {
    const po = observers();
    boot();
    const events = Array.from({ length: 60 }, (_, i) => ({
      startTime: 1000 + i * 100,
      interactionId: 7 * (i + 1),
      duration: 40 + 8 * i,
    }));
    po.emit("event", [
      ...events,
      // The longest interaction's key up, shorter: its key down stands.
      { startTime: 7000, interactionId: 420, duration: 48 },
      // No interaction: never counted.
      { startTime: 7100, duration: 2000 },
    ]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues().INP).toBe(504);
  });

  it("takes the browser's interaction count where it has one", async () => {
    const po = observers();
    Object.defineProperty(performance, "interactionCount", {
      value: 120,
      configurable: true,
    });
    try {
      boot();
      po.emit(
        "event",
        [80, 200, 120, 96].map((duration, i) => ({
          startTime: 1000 + i,
          interactionId: 7 * (i + 1),
          duration,
        })),
      );
      hide();
      await vi.advanceTimersByTimeAsync(0);
      // 120 interactions: the two longest left out.
      expect(vitalValues().INP).toBe(96);
    } finally {
      delete (performance as { interactionCount?: number }).interactionCount;
    }
  });

  it("reports a first input under 40 ms", async () => {
    const po = observers();
    boot();
    po.emit("first-input", [
      { startTime: 500, interactionId: 7, duration: 16 },
    ]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues().INP).toBe(16);
  });

  it("sends no INP without interactions or where entries carry no interaction id", async () => {
    let po = observers();
    boot();
    po.emit("event", [{ startTime: 500, duration: 300 }]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues().INP).toBeUndefined();

    tracker!.stop();
    fetchMock.mockClear();
    po = observers(undefined, false);
    boot();
    po.emit("event", [{ startTime: 500, interactionId: 7, duration: 300 }]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues().INP).toBeUndefined();
  });

  it("takes what the observers still hold when hidden, no paint after it", async () => {
    let po = observers();
    boot();
    po.buffer("paint", [FCP]);
    po.buffer("largest-contentful-paint", [{ startTime: 400 }]);
    po.buffer("layout-shift", [shift(500, 0.2)]);
    po.buffer("event", [{ startTime: 600, interactionId: 7, duration: 64 }]);
    hide(1000);
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues()).toEqual({ FCP: 300, LCP: 400, INP: 64, CLS: 0.2 });

    tracker!.stop();
    fetchMock.mockClear();
    po = observers();
    boot();
    po.buffer("paint", [FCP]);
    po.buffer("largest-contentful-paint", [{ startTime: 400 }]);
    hide(200);
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues()).toEqual({});
  });

  it("sends no paint of a page loaded hidden", async () => {
    const po = observers();
    setVisibility("hidden");
    boot();
    setVisibility("visible");
    po.emit("paint", [FCP]);
    po.emit("largest-contentful-paint", [{ startTime: 400 }]);
    po.emit("layout-shift", [shift(500, 0.2)]);
    hide(5000);
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues()).toEqual({});
  });

  it("files every vital under the landing page, with its version", async () => {
    const po = observers();
    vi.spyOn(performance, "getEntriesByType").mockReturnValue([
      { type: "navigate", responseStart: 120 },
    ] as unknown as PerformanceEntryList);
    history.replaceState(null, "", "/a?x=1");
    document.title = "A";
    boot();
    history.pushState(null, "", "/b");
    document.title = "B";
    po.emit("paint", [FCP]);
    po.emit("largest-contentful-paint", [{ startTime: 400 }]);
    po.emit("layout-shift", [shift(500, 0.05)]);
    po.emit("event", [{ startTime: 600, interactionId: 7, duration: 72 }]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    const ev = vitalsOf();
    expect(ev.map((e) => [e.p.name, e.url, e.title])).toEqual(
      ["TTFB", "FCP", "LCP", "INP", "CLS"].map((n) => [
        n,
        location.origin + "/a?x=1",
        "A",
      ]),
    );
    for (const e of ev)
      expect(Object.keys(e.p)).toEqual(["name", "value", "v"]);
    expect(ev.map((e) => e.p.v)).toEqual([2, 2, 2, 2, 2]);
  });

  it("measures CLS and INP again after the back/forward cache, for the page shown", async () => {
    const po = observers();
    vi.spyOn(performance, "getEntriesByType").mockReturnValue([
      { type: "navigate", responseStart: 120 },
    ] as unknown as PerformanceEntryList);
    history.replaceState(null, "", "/a");
    boot();
    po.emit("paint", [FCP]);
    po.emit("largest-contentful-paint", [{ startTime: 400 }]);
    po.emit("layout-shift", [shift(500, 0.1)]);
    po.emit("event", [{ startTime: 600, interactionId: 7, duration: 200 }]);
    history.pushState(null, "", "/b");
    hide(2000);
    window.dispatchEvent(new Event("pagehide"));
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalsOf().map((e) => e.p.name)).toEqual([
      "TTFB",
      "FCP",
      "LCP",
      "INP",
      "CLS",
    ]);

    fetchMock.mockClear();
    const back = new Event("pageshow");
    Object.defineProperty(back, "persisted", { value: true });
    window.dispatchEvent(back);
    po.emit("largest-contentful-paint", [{ startTime: 8000 }]);
    po.emit("layout-shift", [shift(8100, 0.05)]);
    po.emit("event", [{ startTime: 8200, interactionId: 14, duration: 80 }]);
    hide(9000);
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalsOf().map((e) => [e.p.name, e.p.value, e.url])).toEqual([
      ["INP", 80, location.origin + "/b"],
      ["CLS", 0.05, location.origin + "/b"],
    ]);
  });

  it("sends CLS after the back/forward cache where the load, hidden, sent no paint", async () => {
    const po = observers();
    setVisibility("hidden");
    boot();
    setVisibility("visible");
    po.emit("layout-shift", [shift(500, 0.1)]);
    hide(2000);
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues()).toEqual({});

    const back = new Event("pageshow");
    Object.defineProperty(back, "persisted", { value: true });
    window.dispatchEvent(back);
    po.emit("layout-shift", [shift(8100, 0.05)]);
    hide(9000);
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues()).toEqual({ CLS: 0.05 });
  });

  it("takes the browser's record of a hide before it ran", async () => {
    const at = (
      nav: Record<string, unknown>,
      states: { name: string; startTime: number }[],
    ) =>
      vi
        .spyOn(performance, "getEntriesByType")
        .mockImplementation(
          (t) =>
            (t === "navigation"
              ? [nav]
              : t === "visibility-state"
                ? states
                : []) as unknown as PerformanceEntryList,
        );
    // Opened in the background, shown before the script ran.
    let po = observers();
    at({ type: "navigate" }, [
      { name: "hidden", startTime: 0 },
      { name: "visible", startTime: 250 },
    ]);
    boot();
    po.emit("paint", [FCP]);
    po.emit("largest-contentful-paint", [{ startTime: 400 }]);
    po.emit("layout-shift", [shift(500, 0.1)]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues()).toEqual({});

    // Hidden while prerendered only: before activation.
    tracker!.stop();
    fetchMock.mockClear();
    po = observers();
    at({ type: "navigate", activationStart: 200 }, [
      { name: "hidden", startTime: 100 },
    ]);
    boot();
    po.emit("paint", [FCP]);
    po.emit("largest-contentful-paint", [{ startTime: 400 }]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues()).toMatchObject({ FCP: 100, LCP: 200 });
  });

  it("counts the fast interactions too where the browser counts none", async () => {
    const po = observers();
    boot();
    // 100 interactions, the first three slow: the third longest stands.
    po.emit(
      "event",
      Array.from({ length: 100 }, (_, i) => ({
        startTime: 1000 + i * 100,
        interactionId: 7 * (i + 1),
        duration: [300, 200, 100][i] ?? 16,
      })),
    );
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalValues().INP).toBe(100);
  });

  it("keeps the landing title through a change of the query alone", async () => {
    const po = observers();
    history.replaceState(null, "", "/a");
    document.title = "A";
    boot();
    history.replaceState(null, "", "/a?q=x");
    document.title = "A2";
    history.pushState(null, "", "/b");
    document.title = "B";
    po.emit("paint", [FCP]);
    hide();
    await vi.advanceTimersByTimeAsync(0);
    expect(vitalsOf().map((e) => [e.p.name, e.url, e.title])).toEqual([
      ["FCP", location.origin + "/a", "A2"],
      ["CLS", location.origin + "/a", "A2"],
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
// A vital's and an engagement's fields (checkKind): left out or null pass, but
// a vital's value or v needs its name; a field's key in another case is
// refused (Go's EqualFold: "ſ" is an "s" too).
const VITAL_NAMES = new Set(["TTFB", "FCP", "LCP", "INP", "CLS"]);
function kindProblems(k: string, p: Record<string, unknown>): string[] {
  const out: string[] = [];
  const fields =
    k === "vital"
      ? ["name", "value", "v"]
      : k === "engagement"
        ? ["ms", "scroll"]
        : [];
  for (const key of Object.keys(p)) {
    const f = key.replace(/ſ/g, "s").toLowerCase();
    if (key !== f && fields.includes(f)) out.push(k + "." + key);
  }
  const num = (f: string, lo: number, hi: number, whole = false) => {
    const v = p[f];
    if (
      v != null &&
      (typeof v !== "number" ||
        v < lo ||
        v > hi ||
        (whole && !Number.isInteger(v)))
    )
      out.push(k + "." + f);
  };
  if (k === "vital") {
    const n = p.name;
    if (
      n != null
        ? typeof n !== "string" || !VITAL_NAMES.has(n)
        : p.value != null || p.v != null
    )
      out.push("vital.name");
    num("value", 0, 1e12);
    num("v", 1, 255, true);
  } else if (k === "engagement") {
    num("ms", -1e15, 1e15, true);
    num("scroll", 0, 100);
  }
  return out;
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
    if (e.p) out.push(...kindProblems(e.k, e.p));
  }
  // The context as stored, without the URL and referrer (own columns).
  const ctx = Object.fromEntries(
    Object.entries(b.ctx).filter(([k]) => k !== "url" && k !== "ref"),
  );
  if (goBytes(ctx) > 4096) out.push("ctx size");
  // What the rows store (store.Event.Size): an event's own row at most
  // 16 KiB, the batch at most 64 KiB with the context and the headers (at
  // their 2 KiB bound) on its first row. The address is taken at an IPv6
  // one, the template at its 200-byte bound.
  let stored = 2 + 14 + goBytes(ctx) + 2048;
  for (const e of b.events) {
    const u = e.url ?? String(b.ctx.url);
    // Go keeps a path as sent only when every character may stand
    // unescaped; else it escapes it all again.
    const path = new URL(u).pathname;
    if (/[^\w\-.~!$&'()*+,;=:@[\]%/]/.test(path)) out.push(e.k + " path");
    const ref =
      e.k === "page_view" || e.k === "session_start"
        ? String(b.ctx.ref ?? "")
        : "";
    const props: Record<string, unknown> = { p: e.p };
    if (e.title) props.title = e.title;
    const own =
      96 +
      "browser".length +
      e.k.length +
      bytes(nameOf(e)) +
      2 * 32 +
      39 +
      2 * 24 +
      bytes(navigator.userAgent) +
      bytes(path) +
      200 +
      bytes(u) +
      2 * bytes(ref) +
      goBytes(props);
    if (own > 16 * 1024) out.push(e.k + " stored");
    stored += own;
  }
  if (stored > 64 * 1024) out.push("batch stored");
  return out;
}
// U+2028 and U+2029, kept out of the source as characters: they end a line.
const LS = String.fromCharCode(0x2028);
const SEPARATORS = new RegExp(
  "[" + LS + String.fromCharCode(0x2029) + "]",
  "g",
);
// A value's length as Go encodes it: U+2028 and U+2029 escaped, a lone
// surrogate the three bytes of U+FFFD.
const goBytes = (v: unknown) =>
  bytes(
    JSON.stringify(v)
      .replace(/\\ud[89a-f][\da-f]{2}/g, "uuu")
      .replace(SEPARATORS, "\\u2028"),
  );
// The name the collector gives an event (eventName).
function nameOf(e: Body["events"][number]): string {
  const s = (k: string) => String(e.p[k] ?? "");
  const cut = (x: string) => clip(x, 200);
  switch (e.k) {
    case "custom":
      return e.n ?? "";
    case "outbound_click":
    case "download":
      try {
        return cut(new URL(s("url")).hostname) || cut(s("url"));
      } catch {
        return cut(s("url"));
      }
    case "js_error":
      return cut(s("msg"));
    case "resource_error":
      return cut(s("url").replace(/[?#][^]*$/, ""));
    case "vital":
      return s("name").slice(0, 16);
    case "form":
      return s("phase").slice(0, 16);
  }
  return "";
}

describe("script hosts", () => {
  afterEach(() => {
    for (const s of Array.from(document.querySelectorAll("script"))) s.remove();
  });
  const load = (...urls: string[]) => {
    for (const u of urls) {
      const s = document.createElement("script");
      s.src = u;
      document.head.appendChild(s);
    }
  };
  // Input, then the page hidden and shown again: one engagement.
  const engage = async () => {
    document.dispatchEvent(new Event("pointerdown"));
    vi.advanceTimersByTime(1000);
    for (const v of ["hidden", "visible"]) {
      Object.defineProperty(document, "visibilityState", {
        value: v,
        configurable: true,
      });
      document.dispatchEvent(new Event("visibilitychange"));
    }
    await vi.advanceTimersByTimeAsync(0);
  };
  const sent = () =>
    bodies()
      .flatMap((b) => b.events)
      .filter((e) => e.k === "engagement")
      .map((e) => e.p.scripts);

  it("sends each host once per document, never the page's own or the collector's", async () => {
    load(
      "https://CDN.Example.net/lib.js?key=secret",
      "https://cdn.example.net/other.js",
      location.origin + "/app.js",
      "https://collector.example.com/trakk.js",
      "http://tags.example.org:8080/t.js",
    );
    boot();
    await engage();
    load("https://late.example.org/x.js", "https://cdn.example.net/again.js");
    await engage();
    // A page of a single-page app is no new document.
    history.pushState(null, "", "/b");
    await engage();
    expect(sent()).toEqual([
      ["cdn.example.net", "tags.example.org"],
      ["late.example.org"],
      undefined,
    ]);
    expect(JSON.stringify(bodies())).not.toContain("secret");
  });

  it("drops other schemes and what is no hostname, cuts a long name to its last labels", async () => {
    const long = "a".repeat(60) + "." + "b".repeat(30) + ".example.com";
    expect(long).toHaveLength(103);
    load(
      "data:text/javascript,1",
      "blob:https://blob.example.org/0f2c",
      "chrome-extension://abcdefghijklmnop/content.js",
      "file:///tmp/x.js",
      "ftp://ftp.example.org/x.js",
      "https://" + long + "/x.js",
      "https://" + long.slice(3) + "/x.js",
      "https://a!b.example.org/x.js",
      "https://under_score.example.org/x.js",
      "https://[2001:db8::1]/x.js",
      "https://203.0.113.9/x.js",
      "https://dot.example.org./x.js",
      // The host past a long userinfo, never a part of it.
      "https://not.example.org:" + "0".repeat(2100) + "@far.example.org/x.js",
    );
    boot();
    await engage();
    expect(sent()).toEqual([
      [
        "b".repeat(30) + ".example.com",
        long.slice(3),
        "under_score.example.org",
        "[2001:db8::1]",
        "203.0.113.9",
        "dot.example.org",
        "far.example.org",
      ],
    ]);
  });

  it("names a host as the backend keeps it", () => {
    expect(scriptHost("[2001:DB8::1]")).toBe("[2001:db8::1]");
    expect(scriptHost("A_B.Example.org.")).toBe("a_b.example.org");
    expect(scriptHost("x".repeat(101))).toBe("");
    expect(scriptHost("-x.example.org")).toBe("");
    expect(scriptHost("x".repeat(50) + "." + "y".repeat(50) + ".example")).toBe(
      "y".repeat(50) + ".example",
    );
  });

  it("sends at most 16 at first and keeps the rest of 20 for hosts loaded later, their props whole", async () => {
    const host = (c: string, i: number) =>
      c + String(i).padStart(2, "0") + "-" + "x".repeat(88) + ".example";
    expect(host("h", 0)).toHaveLength(100);
    load(
      ...Array.from(
        { length: 25 },
        (_, i) => "https://" + host("h", i) + "/x.js",
      ),
    );
    boot();
    await engage();
    // The page's own overflow is no later arrival, on any engagement.
    load(
      ...Array.from(
        { length: 5 },
        (_, i) => "https://" + host("l", i) + "/x.js",
      ),
    );
    await engage();
    load("https://later.example.org/x.js");
    await engage();
    const got = sent();
    expect(got[0]).toEqual(Array.from({ length: 16 }, (_, i) => host("h", i)));
    expect(got[1]).toEqual(Array.from({ length: 4 }, (_, i) => host("l", i)));
    expect(got[2]).toBeUndefined();
    for (const r of fetchMock.mock.calls.map(
      (c) => (c[1] as RequestInit).body as string,
    ))
      expect(batchProblems(r)).toEqual([]);
  });

  it("sends no host twice past the hosts it remembers", async () => {
    load(
      ...Array.from(
        { length: 110 },
        (_, i) => "https://cdn" + i + ".example.net/x.js",
      ),
    );
    boot();
    await engage();
    await engage();
    await engage();
    const got = sent();
    // Past 100 the page's own count as later: 4 of them, once.
    expect(got[1]).toHaveLength(4);
    expect(got[2]).toBeUndefined();
    const all = got.flatMap((g) => g ?? []);
    expect(new Set(all).size).toBe(all.length);
  });

  it("reports a script injected after the first report on a page with many hosts", async () => {
    load(
      ...Array.from(
        { length: 30 },
        (_, i) => "https://cdn" + i + ".example.net/x.js",
      ),
    );
    boot();
    await engage();
    load("https://skimmer.example.org/x.js");
    await engage();
    expect(sent()).toEqual([
      Array.from({ length: 16 }, (_, i) => "cdn" + i + ".example.net"),
      ["skimmer.example.org"],
    ]);
  });

  it("takes scripts from resource entries, those no longer in the page included", async () => {
    type Entry = { name: string; initiatorType: string };
    let cb: ((l: { getEntries: () => Entry[] }) => void) | undefined;
    vi.stubGlobal(
      "PerformanceObserver",
      class {
        static supportedEntryTypes = ["resource"];
        constructor(f: typeof cb) {
          cb = f;
        }
        observe() {}
        disconnect() {}
      },
    );
    boot();
    cb!({
      getEntries: () => [
        { name: "https://gone.example.org/x.js", initiatorType: "script" },
        { name: "https://img.example.org/x.png", initiatorType: "img" },
      ],
    });
    await engage();
    expect(sent()).toEqual([["gone.example.org"]]);
  });

  it("keeps hosts for the next engagement when none is sent", async () => {
    load("https://cdn.example.net/x.js");
    boot();
    // Hidden without any input: no engagement.
    for (const v of ["hidden", "visible"]) {
      Object.defineProperty(document, "visibilityState", {
        value: v,
        configurable: true,
      });
      document.dispatchEvent(new Event("visibilitychange"));
    }
    await vi.advanceTimersByTimeAsync(0);
    expect(sent()).toEqual([]);
    await engage();
    expect(sent()).toEqual([["cdn.example.net"]]);
  });
});

describe("collector limits", () => {
  afterEach(() => {
    document.body.innerHTML = "";
    delete (navigator as { userAgentData?: unknown }).userAgentData;
  });

  it("keeps what a batch's rows store within the collector's limit", async () => {
    // Every row repeats the page's URL: a long query fills a batch early.
    history.replaceState(null, "", "/search?q=" + "x".repeat(1900));
    const t = boot();
    fetchMock.mockImplementation(() => new Promise(() => {}));
    for (let i = 0; i < 48; i++) t.event("e" + i, { i });
    t.flush(true);
    await vi.advanceTimersByTimeAsync(0);
    const raws = fetchMock.mock.calls.map(
      (c) => (c[1] as RequestInit).body as string,
    );
    expect(raws.length).toBeGreaterThan(0);
    for (const r of raws) expect(batchProblems(r)).toEqual([]);
  });

  it("never builds a batch the collector refuses for size", async () => {
    // Everything at its bound: a 2 KiB URL whose path Go would escape again,
    // a referrer as long, a title of control characters and line separators
    // (escaped six-fold and two-fold), the largest custom props, 50 events.
    const odd = ("\u0001" + LS).repeat(200);
    history.replaceState(null, "", "/" + "a|^".repeat(700) + "?q=1");
    Object.defineProperty(document, "referrer", {
      configurable: true,
      value: "https://ref.example.org/" + "r".repeat(2100),
    });
    document.title = odd;
    const t = boot();
    // Props just within 4 KiB: as sent, and as Go stores them.
    const big: Record<string, string> = {};
    for (let i = 0; i < 4; i++) big["k" + i] = LS.repeat(150);
    const huge: Record<string, string> = {};
    for (let i = 0; i < 8; i++) huge["k" + i] = "x".repeat(500);
    for (let i = 0; i < 25; i++) {
      t.event("n".repeat(200), i % 2 ? big : huge);
      window.dispatchEvent(
        new ErrorEvent("error", { message: "E" + i + odd, filename: "/x" }),
      );
    }
    // A page left under its own long URL and odd title.
    history.pushState(null, "", "/" + "b|".repeat(1000));
    for (let i = 0; i < 40; i++) {
      t.flush(true);
      await vi.advanceTimersByTimeAsync(LIMITS.flushAfterMs);
    }
    const raws = fetchMock.mock.calls.map(
      (c) => (c[1] as RequestInit).body as string,
    );
    const sent = raws.flatMap((r) => JSON.parse(r).events as Body["events"]);
    expect(sent.length).toBeGreaterThan(45);
    expect(t.state().dropped).toBe(0);
    expect(t.state().queued).toBe(0);
    for (const r of raws) expect(batchProblems(r)).toEqual([]);
    // The props went whole, not dropped as too big.
    expect(sent.filter((e) => e.p.k0 === big.k0)).toHaveLength(12);
    delete (document as { referrer?: string }).referrer;
  });

  it("holds an event's own row within the collector's bound", () => {
    // Every part at its bound: the largest name, user agent, URL with its
    // path, props, a title escaped six-fold, and a referrer with its host.
    const common =
      LIMITS.rowFixed +
      LIMITS.strBytes +
      2 * LIMITS.urlBytes +
      9 +
      6 * LIMITS.strBytes +
      2;
    // An error or a custom event: a name, the largest props.
    const named =
      common +
      "resource_error".length +
      LIMITS.nameBytes +
      6 +
      LIMITS.propsBytes;
    // A page view: empty props, the referrer and its host.
    const view = common + "page_view".length + 16 + 6 + 2 + 2 * LIMITS.urlBytes;
    expect(Math.max(named, view)).toBeLessThanOrEqual(LIMITS.rowBytes);
  });

  it("keeps the context within the collector's limit", async () => {
    Object.defineProperty(navigator, "userAgentData", {
      configurable: true,
      value: {
        mobile: false,
        platform: "p".repeat(500),
        brands: Array.from({ length: 8 }, () => ({
          brand: "b".repeat(500),
          version: "v".repeat(500),
        })),
      },
    });
    document.title = "t".repeat(500);
    const t = boot();
    t.flush(true);
    await vi.advanceTimersByTimeAsync(0);
    const b = bodies()[0];
    expect(batchProblems(JSON.stringify(b))).toEqual([]);
    expect(b.ctx.title).toBe("t".repeat(500));
  });

  it("keeps every property it sends within the collector's limits", async () => {
    localStorage.setItem("analytics.vid", "v".repeat(2000));
    localStorage.setItem("analytics.sid", "s".repeat(2000));
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

  it("never builds a vital or an engagement the collector refuses", async () => {
    const po = observers();
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    boot();
    po.emit("paint", [{ ...FCP, startTime: 300.1234 }]);
    po.emit("largest-contentful-paint", [{ startTime: 1234.5678 }]);
    po.emit("layout-shift", [shift(400, 0.0123), shift(500, 0.2)]);
    po.emit("event", [{ startTime: 900, interactionId: 7, duration: 56.5 }]);
    document.dispatchEvent(new Event("pointerdown"));
    vi.advanceTimersByTime(1500);
    hide();
    // Shown again, then the wall clock set back while the page is active: a
    // negative time.
    document.dispatchEvent(new Event("visibilitychange"));
    document.dispatchEvent(new Event("keydown"));
    vi.setSystemTime(Date.now() - 60_000);
    hide();
    await vi.advanceTimersByTimeAsync(LIMITS.flushAfterMs);
    const raws = fetchMock.mock.calls.map(
      (c) => (c[1] as RequestInit).body as string,
    );
    const sent = raws.flatMap((r) => JSON.parse(r).events as Body["events"]);
    expect(
      new Set(sent.filter((e) => e.k === "vital").map((e) => e.p.name)),
    ).toEqual(new Set(["FCP", "LCP", "INP", "CLS"]));
    const ms = sent.filter((e) => e.k === "engagement").map((e) => e.p.ms);
    expect(ms).toHaveLength(2);
    expect(ms[1]).toBeLessThan(0);
    for (const r of raws) expect(batchProblems(r)).toEqual([]);
  });

  it("knows the shapes the collector refuses for a vital or an engagement", () => {
    const body = (k: string, p: unknown) =>
      JSON.stringify({
        v: 1,
        site: location.hostname,
        vid: "",
        sid: "",
        ctx: { url: location.href },
        events: [{ k, p }],
      });
    for (const [k, p] of [
      ["vital", { name: "LCP", value: 1.5, v: 2 }],
      ["vital", { name: null, value: null, v: null }],
      ["vital", { dropped: true }],
      ["engagement", { ms: -30, scroll: 12.5, inputs: {} }],
      ["engagement", { ms: null, scroll: null }],
      ["engagement", { dropped: true }],
      ["vital", { name: "LCP", value: 1e12 }],
      ["engagement", { ms: 1e15, scroll: 100 }],
      ["engagement", { ms: -1e15 }],
      // Script hosts are values, never keys beside ms.
      [
        "engagement",
        { ms: 1, scripts: ["MS", "Scroll"], inputs: { Scroll: 1 } },
      ],
    ] as const)
      expect(batchProblems(body(k, p))).toEqual([]);
    for (const [k, p, want] of [
      ["vital", { name: "FID", value: 1 }, "vital.name"],
      ["vital", { name: 1, value: 1 }, "vital.name"],
      ["vital", { value: 1 }, "vital.name"],
      ["vital", { v: 2 }, "vital.name"],
      ["vital", { name: null, value: 1 }, "vital.name"],
      ["vital", { name: "LCP", value: 1, VALUE: "x" }, "vital.VALUE"],
      ["vital", { name: "LCP", value: -1 }, "vital.value"],
      ["vital", { name: "LCP", value: 1e12 + 1 }, "vital.value"],
      ["vital", { name: "LCP", value: 1e308 }, "vital.value"],
      ["vital", { name: "LCP", value: "1" }, "vital.value"],
      ["vital", { name: "LCP", value: 1, v: 0 }, "vital.v"],
      ["vital", { name: "LCP", value: 1, v: 256 }, "vital.v"],
      ["vital", { name: "LCP", value: 1, v: 2.5 }, "vital.v"],
      ["vital", { name: "LCP", value: 1, v: "2" }, "vital.v"],
      ["engagement", { ms: 1.5 }, "engagement.ms"],
      ["engagement", { ms: "10" }, "engagement.ms"],
      ["engagement", { ms: 10, scroll: -1 }, "engagement.scroll"],
      ["engagement", { ms: 1e15 + 1 }, "engagement.ms"],
      ["engagement", { ms: -1e15 - 1 }, "engagement.ms"],
      ["engagement", { ms: 9e18 }, "engagement.ms"],
      ["engagement", { ms: 10, scroll: 100.5 }, "engagement.scroll"],
      ["engagement", { MS: 1.5 }, "engagement.MS"],
      ["engagement", { ms: 10, ſcroll: -1 }, "engagement.ſcroll"],
    ] as const)
      expect(batchProblems(body(k, p))).toEqual([want]);
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

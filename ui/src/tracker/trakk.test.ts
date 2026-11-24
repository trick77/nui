import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import source from "./trakk.ts?raw";
import {
  LIMITS,
  cleanProps,
  clip,
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

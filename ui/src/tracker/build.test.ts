// The shipped script, built as `npm run build` builds it: minified to its
// property names, it must still send what the source sends and keep the
// page's window.nui.
import {
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";
import {
  build,
  loadConfigFromFile,
  type InlineConfig,
  type Plugin,
} from "vite";
import source from "./nui.ts?raw";
import { start, type Tracker } from "./nui";

type TrackerApi = {
  build: (out: string, write?: boolean) => InlineConfig;
  props: RegExp;
};

const SRC = "https://collector.example.com/nui.js";
const NOW = new Date("2026-11-22T12:00:00Z");

let code = "";
let props: string[] = [];

beforeAll(async () => {
  // The ui directory's config, as the build finds it.
  const loaded = await loadConfigFromFile({
    command: "build",
    mode: "production",
  });
  const root = loaded!.path.replace(/vite\.config\.ts$/, "");
  const all: unknown[] = [];
  const walk = (p: unknown) =>
    Array.isArray(p) ? p.forEach(walk) : all.push(p);
  walk(loaded!.config.plugins);
  const plugin = all.find(
    (p) => (p as Plugin | null)?.name === "nui-tracker",
  ) as Plugin;
  const api = plugin.api as TrackerApi;
  props = api.props.source.replace(/^\^\(|\)\$$/g, "").split("|");
  const cfg = api.build("dist", false);
  const out = await build({ ...cfg, root });
  const res = Array.isArray(out) ? out[0] : out;
  code = (res as { output: { code: string }[] }).output[0].code;
}, 60_000);

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  vi.useFakeTimers({ now: NOW });
  fetchMock = vi.fn(() => Promise.resolve(new Response(null, { status: 204 })));
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

const show = (state: "visible" | "hidden") => {
  Object.defineProperty(document, "visibilityState", {
    value: state,
    configurable: true,
  });
  document.dispatchEvent(new Event("visibilitychange"));
};

// One visit, the same for the source and the built script; the batches it
// sends, with the random ids named by their order.
async function visit(load: () => void): Promise<unknown[]> {
  vi.setSystemTime(NOW);
  localStorage.clear();
  fetchMock.mockClear();
  history.replaceState(null, "", "/a?x=1");
  document.title = "A";
  document.body.innerHTML =
    '<a id="out" href="https://github.com/ipverse">out</a>' +
    '<form id="s"><input type="search" name="q" value="AS15169"></form>';
  (window as { nui?: unknown }).nui = { q: [["early", { a: 1 }]] };
  load();
  const nui = (
    window as unknown as { nui: { event: (...a: unknown[]) => void } }
  ).nui;
  nui.event("signup", { plan: "free", n: 2 });
  document.dispatchEvent(new Event("pointerdown"));
  document.dispatchEvent(new KeyboardEvent("keydown"));
  window.dispatchEvent(new Event("scroll"));
  await vi.advanceTimersByTimeAsync(2000);
  history.pushState(null, "", "/b");
  document.title = "B";
  document
    .getElementById("out")!
    .dispatchEvent(new MouseEvent("click", { bubbles: true }));
  document
    .getElementById("s")!
    .dispatchEvent(new Event("submit", { bubbles: true }));
  window.dispatchEvent(
    new ErrorEvent("error", { message: "TypeError: x", lineno: 3 }),
  );
  // Past the first flush.
  await vi.advanceTimersByTimeAsync(6000);
  show("hidden");
  show("visible");
  await vi.advanceTimersByTimeAsync(60_000);
  const ids = new Map<string, string>();
  const name = (v: string) => {
    if (!ids.has(v)) ids.set(v, "id" + ids.size);
    return ids.get(v)!;
  };
  return fetchMock.mock.calls.map((c) => {
    expect(c[0]).toBe("https://collector.example.com/api/in");
    const b = JSON.parse((c[1] as RequestInit).body as string);
    return { ...b, vid: name(b.vid), sid: name(b.sid) };
  });
}

const keys = (v: unknown, out: Set<string>) => {
  if (Array.isArray(v)) v.forEach((x) => keys(x, out));
  else if (v && typeof v === "object")
    for (const [k, x] of Object.entries(v)) {
      out.add(k);
      keys(x, out);
    }
  return out;
};

describe("the built script", () => {
  it("sends what the source sends and keeps window.nui", async () => {
    let t: Tracker | null = null;
    const want = await visit(() => (t = start(window, SRC)));
    t!.stop();
    expect(want.length).toBeGreaterThan(0);

    const tag = document.createElement("script");
    tag.src = SRC;
    document.head.appendChild(tag);
    const got = await visit(() => new Function(code)());
    expect(got).toEqual(want);
    const kinds = got.flatMap((b) =>
      (b as { events: { k: string; n?: string }[] }).events.map(
        (e) => e.n ?? e.k,
      ),
    );
    expect(kinds).toEqual(
      expect.arrayContaining([
        "session_start",
        "page_view",
        "early",
        "signup",
        "outbound_click",
        "search",
        "js_error",
        "engagement",
      ]),
    );
    // No property of its own collides with a key on the wire.
    const wire = keys(got, new Set());
    for (const p of props) expect(wire.has(p), p).toBe(false);
  });

  it("mangles no browser API and none of the page's window.nui", () => {
    const owners: object[] = [
      window,
      document,
      navigator,
      location,
      history,
      performance,
      localStorage,
      Window.prototype,
      Document.prototype,
      Node.prototype,
      Element.prototype,
      HTMLElement.prototype,
      HTMLFormElement.prototype,
      HTMLInputElement.prototype,
      HTMLAnchorElement.prototype,
      HTMLScriptElement.prototype,
      Event.prototype,
      ErrorEvent.prototype,
      Response.prototype,
      Headers.prototype,
      Storage.prototype,
      Array.prototype,
      Object.prototype,
      String.prototype,
      Map.prototype,
      Promise.prototype,
    ];
    expect(props.length).toBeGreaterThan(20);
    for (const p of props) {
      for (const o of owners) expect(p in o, p).toBe(false);
      expect(["nui", "event", "q", "push", "length"]).not.toContain(p);
    }
  });

  it("carries no source map, no comment and no internal name", () => {
    expect(code.length).toBeGreaterThan(5000);
    expect(code).not.toMatch(/sourceMappingURL|\/\*|\*\/|\/\//);
    // Names from the source that only the source needs.
    const internal = [
      "LIMITS",
      "STORE_VID",
      "notBefore",
      "requeue",
      "deliver",
      "carried",
      "arrival",
      "pageInputs",
      "errorsSeen",
      // the handle only tests use
      "stop",
      "state",
      "queued",
      "errorKeys",
      ...props,
    ];
    for (const n of internal) {
      expect(source, n).toMatch(new RegExp("\\b" + n + "\\b"));
      expect(code, n).not.toMatch(new RegExp("\\b" + n + "\\b"));
    }
    // Nor the bundle's global name.
    expect(code).not.toContain("nuiTracker");
    // What a page enforcing Trusted Types and a CSP refuses.
    expect(code).not.toMatch(
      /\beval\(|new Function|innerHTML|outerHTML|insertAdjacentHTML|document\.write|createElement\(|setTimeout\(\s*["'`]|setInterval|debugger/,
    );
    // The contract stays readable.
    for (const s of ["nui.vid", "nui.sid", "nui.seen", "/api/in", "__nui"])
      expect(code).toContain(s);
  });
});

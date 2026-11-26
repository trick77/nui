import { afterEach, describe, expect, it, vi } from "vitest";
import shell from "../index.html?raw";
import icon from "../public/icon.svg?raw";
import { ApiError, api, setAuthListener, setFetcher, type Scope } from "./api";
import { SITES, fixtureFetch } from "./fixtures";
import {
  bytes,
  changeText,
  clock,
  countryName,
  date,
  dateTime,
  dayMonth,
  networkKind,
  num,
  pct,
  rawSince,
  SEP,
  seconds,
  signal,
  value,
  when,
} from "./format";
import { evidenceText, signalGroups, signalTexts } from "./signals";
import {
  basePath,
  parseView,
  resolveView,
  storeAudience,
  storeShown,
  storedAudience,
  storedRange,
  tabTitle,
  type View,
  viewHref,
} from "./state";

const scope: Scope = {
  site: "lens.example.com",
  range: "7d",
  aud: "all",
  cmp: "prev",
};

afterEach(() => {
  setFetcher(null);
  setAuthListener(null);
  localStorage.clear();
});

describe("format", () => {
  it("names where raw events begin when the server says they do", () => {
    // The server decides (on its UTC days); the date is the viewer's.
    expect(rawSince("2026-09-03T08:00:00Z", "Europe/Zurich")).toBe("3.9.2026");
    expect(rawSince("2026-10-25T00:30:00Z", "America/New_York")).toBe(
      "24.10.2026",
    );
    expect(rawSince(null, "Europe/Zurich")).toBe(null);
    expect(rawSince(undefined)).toBe(null);
  });

  it("writes numbers the Swiss way", () => {
    // ICU versions differ between ’ and ': both are the Swiss apostrophe.
    expect(num(1234567)).toMatch(/^1['’]234['’]567$/);
    expect(num(12.345, 1)).toBe("12.3");
    expect(num(null)).toBe("–");
    expect(num(NaN)).toBe("–");
    expect(pct(44)).toBe("44 %");
    expect(pct(0.6)).toBe("0.6 %");
    expect(pct(null)).toBe("–");
    expect(value(12.4, "rate")).toBe("12 %");
    expect(value(2211, "sum")).toMatch(/^2['’]211$/);
    expect(bytes(512)).toBe("512 B");
    expect(bytes(432013312)).toBe("412 MB");
    expect(bytes(134217728)).toBe("128 MB");
    expect(bytes(1.5 * 1024 ** 3)).toBe("1.5 GB");
    expect(seconds(0.8)).toBe("0.8 s");
    expect(seconds(90)).toBe("2 min");
    expect(seconds(7200)).toBe("2.0 h");
    expect(seconds(null)).toBe("–");
  });

  it("writes a network's kind after its name", () => {
    expect(
      ["isp", "hosting", "business", "education", "government", ""].map(
        networkKind,
      ),
    ).toEqual(["ISP", "Hosting", "Business", "Education", "Government", ""]);
  });

  it("starts a signal with a capital, a dotted name as it is", () => {
    expect(
      [
        "signed-in API client",
        "client is PetalBot",
        "Googlebot address range",
        "example.org address range",
        "",
      ].map(signal),
    ).toEqual([
      "Signed-in API client",
      "Client is PetalBot",
      "Googlebot address range",
      "example.org address range",
      "",
    ]);
  });

  it("writes changes with an arrow from the sign", () => {
    expect(changeText({ pct: 14.4 })).toBe("↑ 14 %");
    expect(changeText({ pct: -0.7 })).toBe("↓ 0.7 %");
    expect(changeText({ pp: -2.2 })).toBe("↓ 2.2 pp");
    expect(changeText({ pct: 0 })).toBe("→ 0 %");
    expect(changeText({ new: true })).toBe("new");
    expect(changeText(null)).toBe("");
  });

  it("writes dates and times", () => {
    const t = "2026-11-22T13:19:42Z";
    expect(date(t, "UTC")).toBe("22.11.2026");
    const now = new Date(t);
    expect(dayMonth(t, "UTC", now)).toBe("22.11.");
    expect(clock(t, "UTC")).toBe("13:19");
    expect(dateTime(t, "UTC", now)).toBe("22.11. 13:19");
    // Another year than the viewer's current one says it, in every helper:
    // a span across the new year, a list last updated long ago.
    const jan = new Date("2027-01-03T10:00:00Z");
    expect(dateTime("2026-12-30T09:00:00Z", "UTC", jan)).toBe(
      "30.12.2026 09:00",
    );
    expect(dateTime("2027-01-02T09:00:00Z", "UTC", jan)).toBe("2.1. 09:00");
    expect(dayMonth("2025-03-01T09:00:00Z", "UTC", now)).toBe("1.3.2025");
    // The year is the viewer's: 2026 in UTC, already 2027 in Tokyo.
    const eve = new Date("2026-12-31T20:00:00Z");
    expect(dateTime("2026-12-31T10:00:00Z", "Asia/Tokyo", eve)).toBe(
      "31.12.2026 19:00",
    );
    expect(dateTime("2026-12-31T10:00:00Z", "UTC", eve)).toBe("31.12. 10:00");
    expect(when(t, new Date(t), "UTC")).toBe("13:19:42");
    expect(when("2026-11-20T08:00:00Z", new Date(t), "UTC")).toBe(
      "20.11. 08:00",
    );
    // The same day a year earlier is not today, and says its year.
    expect(when("2025-11-22T13:19:42Z", new Date(t), "UTC")).toBe(
      "22.11.2025 13:19",
    );
    // Today in UTC, yesterday in the viewer's zone.
    expect(
      when("2026-11-22T02:00:00Z", new Date(t), "America/Los_Angeles"),
    ).toBe("21.11. 18:00");
    expect(when("2026-11-22T12:00:00Z", new Date(t), "Asia/Tokyo")).toBe(
      "21:00:00",
    );
    expect(date(null)).toBe("–");
    expect(when(null)).toBe("–");
    expect(countryName("CH")).toBe("Switzerland");
    expect(countryName("")).toBe("–");
  });
});

describe("signals", () => {
  const HUMAN = "Human signals";
  const BOT = "Bot signals";
  const OTHER = "Other";
  const grouped = (signals: string[]) =>
    signalGroups(signals).map((g) => [g.heading, ...g.texts]);

  it.each([
    ["signed in", HUMAN, "Signed in"],
    ["script beacon", HUMAN, "Uses a real browser"],
    [
      "script beacon from another address",
      HUMAN,
      "Uses a real browser, from another address",
    ],
    ["engaged", HUMAN, "Active on the page"],
    ["assets loaded", HUMAN, "Loads images and styles"],
    ["no sign of a browser", BOT, "No real browser"],
    ["no page loaded", BOT, "Never opened a page"],
    ["machine pace", BOT, "Machine pace"],
    ["client curl/8.4.0", BOT, "Program: curl/8.4.0"],
    ["client isbot/1.0", BOT, "Program: isbot/1.0"],
    ["no user agent", BOT, "Does not identify itself"],
    ["missing browser headers", BOT, "Missing browser headers"],
    ["navigator.webdriver set", BOT, "Remote-controlled browser"],
    ["claims Googlebot, address is not Googlebot's", BOT, "Poses as Googlebot"],
    ["claims Googlebot, no rDNS name", BOT, "Claims Googlebot, no rDNS name"],
    [
      "claims Googlebot, rDNS check pending",
      BOT,
      "Claims Googlebot, rDNS check pending",
    ],
    ["no script beacon", BOT, "Runs no scripts"],
    ["no assets", BOT, "Loads no images or styles"],
    ["mostly refused", BOT, "Mostly refused"],
    ["API only, no page loaded", BOT, "API calls only"],
    ["rapid requests", BOT, "Rapid requests"],
    ["hosting network", BOT, "From a data center"],
    [
      "automation indicators: window, behaviour",
      BOT,
      "Signs of automation: window, behaviour",
    ],
    [
      "automation indicators: window, behaviour, hosting network",
      BOT,
      "Signs of automation: window, behaviour, from a data center",
    ],
    // A provider: a bot's or an uptime probe's.
    ["client is PetalBot", OTHER, "Client is PetalBot"],
    ["UptimeRobot address range", OTHER, "UptimeRobot address range"],
    [
      "Samplebot address range, rDNS not verified",
      OTHER,
      "Samplebot address range, rDNS not verified",
    ],
    ["Googlebot, rDNS confirmed", OTHER, "Googlebot, rDNS confirmed"],
    ["more clients", OTHER, "More clients"],
    ["signed-in API client", OTHER, "Signed-in API client"],
    [
      "also known bot: Samplebot/1.4, 3 of 9 events (Samplebot address range, no rDNS name)",
      OTHER,
      "Also known bot: Samplebot/1.4, 3 of 9 events (Samplebot address range, no rDNS name)",
    ],
    // The reason it quotes in its own text.
    [
      "also unknown automated: zgrab/0.x, 3 of 9 events (client zgrab/0.x)",
      OTHER,
      "Also unknown automated: zgrab/0.x, 3 of 9 events (Program: zgrab/0.x)",
    ],
    [
      "also API client: agent/1.0, 1 of 9 events (signed-in API client)",
      OTHER,
      "Also API client: agent/1.0, 1 of 9 events (Signed-in API client)",
    ],
    [
      "also unknown automated: HeadlessChrome/142, 4 of 9 events (automation indicators: window, hosting network)",
      OTHER,
      "Also unknown automated: HeadlessChrome/142, 4 of 9 events (Signs of automation: window, from a data center)",
    ],
    // Not known here.
    ["something new", OTHER, "Something new"],
    ["constructor", OTHER, "Constructor"],
  ])("says what %s means", (s, heading, text) => {
    expect(grouped([s])).toEqual([[heading, text]]);
  });

  it("groups them in order", () => {
    expect(
      grouped([
        "something new",
        "rapid requests",
        "client curl/8.4.0",
        "script beacon",
        "engaged",
      ]),
    ).toEqual([
      [HUMAN, "Uses a real browser", "Active on the page"],
      [BOT, "Rapid requests", "Program: curl/8.4.0"],
      [OTHER, "Something new"],
    ]);
    expect(grouped([])).toEqual([]);
  });

  it("keeps the backend's order in one line, a reason that decided first", () => {
    expect(
      signalTexts([
        "client curl/8.4.0",
        "signed in",
        "no assets",
        "something new",
        "assets loaded",
      ]),
    ).toEqual([
      "Program: curl/8.4.0",
      "Signed in",
      "Something new",
      "Loads images and styles",
    ]);
  });

  it("hides a missing beacon and missing assets once signed in", () => {
    expect(
      grouped([
        "signed in",
        "no script beacon",
        "no assets",
        "hosting network",
      ]),
    ).toEqual([
      [HUMAN, "Signed in"],
      [BOT, "From a data center"],
    ]);
    // An API client's account is none of a person's.
    expect(grouped(["signed-in API client", "no script beacon"])).toEqual([
      [BOT, "Runs no scripts"],
      [OTHER, "Signed-in API client"],
    ]);
  });

  it("words a reading's evidence alike", () => {
    expect(
      [
        "Signal: navigator.webdriver set",
        "Signal: no assets",
        "Signal: client is PetalBot",
        "Signal: also unknown automated: curl/8.4.0, 3 of 9 events (client curl/8.4.0)",
        "12 requests, 3 % refused",
      ].map(evidenceText),
    ).toEqual([
      "Remote-controlled browser",
      "Loads no images or styles",
      "Client is PetalBot",
      "Also unknown automated: curl/8.4.0, 3 of 9 events (Program: curl/8.4.0)",
      "12 requests, 3 % refused",
    ]);
  });
});

describe("state", () => {
  it("reads the view from the URL with defaults", () => {
    expect(parseView("/dashboard/", "")).toEqual({
      page: "dash",
      site: null,
      range: null,
      aud: "all",
      cmp: "prev",
      log: {},
      all: false,
    });
    expect(
      parseView("/dashboard/alerts", "?site=a.example.com&all=1"),
    ).toMatchObject({ page: "alerts", site: "a.example.com", all: true });
    // All sites is the alerts' own switch.
    expect(parseView("/dashboard/", "?all=1").all).toBe(false);
    const v = parseView(
      "/dashboard/log",
      "?site=a.example.com&range=12mo&aud=bots&cmp=yoy&addr=192.0.2.1&kind=request&bogus=1",
    );
    expect(v).toMatchObject({
      page: "log",
      site: "a.example.com",
      range: "12mo",
      aud: "bots",
      cmp: "yoy",
      log: { addr: "192.0.2.1", kind: "request" },
    });
    expect(parseView("/", "?range=1y&aud=x&cmp=z")).toMatchObject({
      range: null,
      aud: "all",
      cmp: "prev",
    });
  });

  describe("fills in what the URL leaves open", () => {
    const sites = ["a.example.com", "b.example.com", "c.example.com"];
    const at = (search: string, list = sites) => {
      const v = resolveView(parseView("/dashboard/", search), list);
      return v && [v.site, v.range];
    };

    it("without anything remembered", () => {
      expect(at("")).toEqual(["a.example.com", "7d"]);
      expect(at("?site=b.example.com")).toEqual(["b.example.com", "7d"]);
      expect(at("?range=30d")).toEqual(["a.example.com", "30d"]);
      expect(at("", [])).toBe(null);
      expect(at("?site=a.example.com&range=30d", [])).toBe(null);
    });

    it("with the last domain and each domain's range", () => {
      storeShown("a.example.com", "90d");
      storeShown("b.example.com", "24h");
      expect(at("")).toEqual(["b.example.com", "24h"]);
      expect(at("?site=a.example.com")).toEqual(["a.example.com", "90d"]);
      expect(at("?site=c.example.com")).toEqual(["c.example.com", "7d"]);
      expect(at("?range=12mo")).toEqual(["b.example.com", "12mo"]);
      expect(at("?site=a.example.com&range=12mo")).toEqual([
        "a.example.com",
        "12mo",
      ]);
      // A range that is none, a domain that is not listed: as if left out.
      expect(at("?range=1y")).toEqual(["b.example.com", "24h"]);
      expect(at("?site=x.example.com")).toEqual(["b.example.com", "24h"]);
      expect(at("?site=x.example.com&range=30d")).toEqual([
        "b.example.com",
        "30d",
      ]);
    });

    it("with the first domain once the last is gone", () => {
      storeShown("gone.example.com", "24h");
      expect(at("")).toEqual(["a.example.com", "7d"]);
    });

    it("keeping a view without a gap as it is", () => {
      const v = parseView("/", "?site=c.example.com&range=30d");
      storeShown("a.example.com", "90d");
      expect(resolveView(v, sites)).toBe(v);
    });

    it("with storage blocked or broken", () => {
      localStorage.setItem("app.range.a.example.com", "forever");
      expect(storedRange("a.example.com")).toBe("7d");
      const blocked = () => {
        throw new Error("blocked");
      };
      const get = vi
        .spyOn(Storage.prototype, "getItem")
        .mockImplementation(blocked);
      const set = vi
        .spyOn(Storage.prototype, "setItem")
        .mockImplementation(blocked);
      try {
        expect(at("")).toEqual(["a.example.com", "7d"]);
        expect(storedAudience()).toBe("all");
        // What storage does not take lasts until the page is left.
        storeShown("b.example.com", "24h");
        storeAudience("bots");
        expect(at("")).toEqual(["b.example.com", "24h"]);
        expect(at("?site=a.example.com")).toEqual(["a.example.com", "7d"]);
        expect(storedAudience()).toBe("bots");
      } finally {
        get.mockRestore();
        set.mockRestore();
      }
      // Back, it takes over.
      storeShown("b.example.com", "30d");
      storeAudience("humans");
      expect(localStorage.getItem("app.range.b.example.com")).toBe("30d");
      expect(at("")).toEqual(["b.example.com", "30d"]);
      expect(storedAudience()).toBe("humans");
    });
  });

  it("remembers the audience per browser", () => {
    storeAudience("humans");
    expect(storedAudience()).toBe("humans");
    expect(parseView("/", "").aud).toBe("humans");
    expect(parseView("/", "?aud=bots").aud).toBe("bots");
  });

  it("writes the URL back, the default range and audience too", () => {
    const v = parseView("/dashboard/", "");
    // Before the domains are known there is none to name.
    expect(viewHref("/dashboard/", v)).toBe("/dashboard/?aud=all");
    expect(
      viewHref("/dashboard/", { ...v, site: "a.example.com", range: "7d" }),
    ).toBe("/dashboard/?site=a.example.com&range=7d&aud=all");
    expect(
      viewHref("/dashboard/", {
        ...v,
        site: "a.example.com",
        range: "30d",
        aud: "bots",
        cmp: "off",
      }),
    ).toBe("/dashboard/?site=a.example.com&range=30d&aud=bots&cmp=off");
    // The log keeps domain, range, audience and comparison.
    expect(
      viewHref("/dashboard/", {
        ...v,
        page: "log",
        site: "a.example.com",
        range: "7d",
        aud: "bots",
        cmp: "yoy",
        log: { addr: "2001:db8::/64", path: "/AS:n" },
      }),
    ).toBe(
      "/dashboard/log?site=a.example.com&range=7d&aud=bots&cmp=yoy&addr=2001%3Adb8%3A%3A%2F64&path=%2FAS%3An",
    );
    // The alerts keep them too, never a log filter.
    const alerts = {
      ...v,
      page: "alerts" as const,
      site: "a.example.com",
      range: "30d" as const,
      log: { addr: "192.0.2.1" },
    };
    expect(viewHref("/dashboard/", alerts)).toBe(
      "/dashboard/alerts?site=a.example.com&range=30d&aud=all",
    );
    expect(viewHref("/dashboard/", { ...alerts, all: true })).toBe(
      "/dashboard/alerts?site=a.example.com&range=30d&aud=all&all=1",
    );
    // All sites is the alerts' own, the dashboard never writes it.
    expect(
      viewHref("/dashboard/", { ...alerts, page: "dash", all: true }),
    ).toBe("/dashboard/?site=a.example.com&range=30d&aud=all");
  });

  it("keeps audience and comparison through the log and the alerts", () => {
    // Another audience remembered than the one shown: a URL that drops it
    // would come back with the remembered one.
    storeAudience("humans");
    const dash = parseView(
      "/dashboard/",
      "?site=a.example.com&range=30d&aud=bots&cmp=yoy",
    );
    const back = (v: View) => {
      const u = new URL(viewHref("/dashboard/", v), "https://example.com");
      return parseView(u.pathname, u.search);
    };
    const log = { ...dash, page: "log" as const, log: { addr: "192.0.2.1" } };
    expect(back(log)).toEqual(log);
    const alerts = { ...dash, page: "alerts" as const };
    expect(back(alerts)).toEqual(alerts);
    expect(back({ ...alerts, all: true })).toEqual({ ...alerts, all: true });
    // Out of the log, the dashboard as it was.
    expect(back({ ...back(log), page: "dash", log: {} })).toEqual(dash);
  });

  it("names the domain and the log's address in the tab title", () => {
    const v = parseView("/dashboard/", "?range=30d&aud=bots");
    expect(tabTitle(null, v)).toBe("Trakk");
    expect(tabTitle("a.example.com", v)).toBe(`Trakk${SEP}a.example.com`);
    const log = { ...v, page: "log" as const };
    expect(tabTitle("a.example.com", { ...log, log: { path: "/x" } })).toBe(
      `Trakk${SEP}a.example.com${SEP}Log`,
    );
    expect(
      tabTitle("a.example.com", { ...log, log: { addr: "192.0.2.1" } }),
    ).toBe(`Trakk${SEP}a.example.com${SEP}192.0.2.1`);
    for (const asn of ["64500", "AS64500"])
      expect(tabTitle("a.example.com", { ...log, log: { asn } })).toBe(
        `Trakk${SEP}a.example.com${SEP}AS64500`,
      );
    const alerts = { ...v, page: "alerts" as const };
    expect(tabTitle("a.example.com", alerts)).toBe(
      `Trakk${SEP}a.example.com${SEP}Alerts`,
    );
    expect(tabTitle("a.example.com", { ...alerts, all: true })).toBe(
      `Trakk${SEP}Alerts`,
    );
  });

  it("finds the mount", () => {
    expect(basePath("/dashboard/")).toBe("/dashboard/");
    expect(basePath("/dashboard/log")).toBe("/dashboard/");
    expect(basePath("/dashboard")).toBe("/dashboard/");
    expect(basePath("/")).toBe("/");
    expect(basePath("/log")).toBe("/");
    expect(basePath("/dashboard/alerts")).toBe("/dashboard/");
    expect(basePath("/dashboard/alerts/")).toBe("/dashboard/");
    expect(basePath("/alerts")).toBe("/");
  });
});

describe("api client", () => {
  it("sends the scope and the viewer's zone on every dashboard query", async () => {
    const calls: string[] = [];
    const fx = fixtureFetch();
    setFetcher((u, i) => {
      calls.push(u);
      return fx(u, i);
    });
    const o = await api.overview(scope);
    expect(o.tiles.map((t) => t.key)).toEqual([
      "visitors",
      "views",
      "events",
      "requests",
      "auto_share",
      "errors",
    ]);
    await api.list(scope, { dim: "pages", within: "/AS:n", q: "" });
    await api.visitors(scope, { sort: "requests", class: "unknown" });
    await api.visitor(scope, "198.51.100.7");
    await api.log(scope, { addr: "192.0.2.120" }, "1795353582000.48211");
    const u = new URL(calls[0], "http://x");
    expect(u.pathname).toBe("/api/dash/overview");
    expect(Object.fromEntries(u.searchParams)).toMatchObject({
      site: "lens.example.com",
      range: "7d",
      aud: "all",
      cmp: "prev",
      tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
    });
    expect(calls[1]).toContain(
      "dim=pages&sort=volume&limit=6&within=%2FAS%3An",
    );
    expect(calls[1]).not.toContain("q=");
    expect(calls[2]).toContain("sort=requests&class=unknown&limit=7");
    expect(calls[3]).toContain("key=198.51.100.7");
    expect(calls[4]).toContain("addr=192.0.2.120&cursor=1795353582000.48211");
  });

  it("asks for the alerts of a site, or of all without one", async () => {
    const calls: string[] = [];
    const fx = fixtureFetch();
    setFetcher((u, i) => {
      calls.push(u);
      return fx(u, i);
    });
    const all = await api.alerts({});
    expect(calls[0]).toBe("/api/dash/alerts");
    expect(all.alerts).toHaveLength(50);
    expect(all.alerts.some((a) => a.site === "")).toBe(true);
    await api.alerts({ site: "", before: all.next!, limit: 200 });
    expect(calls[1]).toBe(`/api/dash/alerts?before=${all.next}&limit=200`);
    const one = await api.alerts({ site: "api.example.com" });
    expect(calls[2]).toBe("/api/dash/alerts?site=api.example.com");
    expect(one.alerts.every((a) => a.site === "api.example.com")).toBe(true);
    expect(one.next).toBeNull();
    await expect(api.alerts({ before: "x" })).rejects.toMatchObject({
      status: 400,
    });
  });

  it("raises the backend's detail as an ApiError", async () => {
    setFetcher(fixtureFetch({ fail: ["/api/dash/overview"] }));
    await expect(api.overview(scope)).rejects.toEqual(
      new ApiError(500, "fixture failure"),
    );
    setFetcher(
      async () =>
        new Response("<html>", { status: 502, statusText: "Bad Gateway" }),
    );
    await expect(api.sites()).rejects.toMatchObject({
      status: 502,
      message: "Bad Gateway",
    });
    // A panic's 500 comes from kit's Recover as {"error"}.
    setFetcher(
      async () =>
        new Response(JSON.stringify({ error: "internal server error" }), {
          status: 500,
        }),
    );
    await expect(api.sites()).rejects.toMatchObject({
      status: 500,
      message: "internal server error",
    });
  });

  it("tells the app when the session is gone", async () => {
    const seen = vi.fn();
    setAuthListener(seen);
    setFetcher(fixtureFetch({ me: 403 }));
    await expect(api.domain(scope)).rejects.toBeInstanceOf(ApiError);
    expect(seen).toHaveBeenCalledWith(403);
    await expect(api.me()).rejects.toMatchObject({ status: 403 });
    expect(seen).toHaveBeenCalledTimes(1);
  });

  it("serves the fixtures the contract describes", async () => {
    setFetcher(fixtureFetch());
    const { sites } = await api.sites();
    expect(sites).toEqual(SITES);
    const noFeed = { ...scope, site: "console.example.com" };
    expect((await api.overview(noFeed)).tiles.some((t) => t.server)).toBe(
      false,
    );
    await expect(api.classes(noFeed)).rejects.toMatchObject({ status: 404 });
    const v = await api.visitors(noFeed, { sort: "views" });
    expect(v.rows.every((r) => r.requests === null)).toBe(true);
    const off = await api.overview({ ...scope, cmp: "off" });
    expect(off.tiles[0].change).toBeNull();
    expect((await api.overview({ ...scope, aud: "bots" })).tiles[0].label).toBe(
      "Addresses",
    );
    const l = await api.list(scope, { dim: "pages", limit: 3, sort: "change" });
    expect(l.rows).toHaveLength(3);
    expect(l.other?.count).toBe(5);
    expect(await api.status(scope)).toMatchObject({ rows: expect.any(Array) });
    expect((await api.clients(scope)).browsers.length).toBeLessThanOrEqual(5);
    expect((await api.noteworthy(scope)).items.length).toBeLessThanOrEqual(5);
    const d = await api.domain(noFeed);
    expect(d.sources.server).toBeNull();
    // The backend's page and cursor.
    const page = await api.log(scope, {}, "");
    expect(page.rows).toHaveLength(100);
    expect(page.next).toMatch(/^\d+\.\d+$/);
    // Another site's sources: the script alone, a feed alone.
    const only = (site: string) => api.log({ ...scope, site }, {}, "");
    expect(
      (await only("console.example.com")).rows.every(
        (r) => r.source === "browser",
      ),
    ).toBe(true);
    expect(
      (await only("api.example.com")).rows.every((r) => r.source === "server"),
    ).toBe(true);
    await expect(
      api.log(scope, { addr: "2001:db8:91::/48" }, ""),
    ).rejects.toMatchObject({ status: 400 });
    expect((await api.logout()).status).toBe(204);
  });
});

describe("shell", () => {
  it("links its own small icon, never the host's /favicon.ico", () => {
    // The build makes the public file's path relative, and the server
    // serves it under /dashboard/.
    expect(shell).toContain(
      '<link rel="icon" type="image/svg+xml" href="/icon.svg" />',
    );
    expect(icon).toMatch(/^<!--[^]*-->\s*<svg [^]*<\/svg>\s*$/);
    // The column chart's strokes.
    expect(icon).toContain('<path d="M13 17V5"/>');
    expect(icon.length).toBeLessThan(4096);
  });
});

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
  num,
  pct,
  seconds,
  value,
  when,
} from "./format";
import {
  basePath,
  parseView,
  storeAudience,
  storedAudience,
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

describe("state", () => {
  it("reads the view from the URL with defaults", () => {
    expect(parseView("/dashboard/", "")).toEqual({
      page: "dash",
      site: null,
      range: "7d",
      aud: "all",
      cmp: "prev",
      log: {},
    });
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
      range: "7d",
      aud: "all",
      cmp: "prev",
    });
  });

  it("remembers the audience per browser", () => {
    storeAudience("humans");
    expect(storedAudience()).toBe("humans");
    expect(parseView("/", "").aud).toBe("humans");
    expect(parseView("/", "?aud=bots").aud).toBe("bots");
  });

  it("writes the URL back, defaults left out", () => {
    const v = parseView("/dashboard/", "");
    expect(viewHref("/dashboard/", v)).toBe("/dashboard/?aud=all");
    expect(
      viewHref("/dashboard/", {
        ...v,
        site: "a.example.com",
        range: "30d",
        aud: "bots",
        cmp: "off",
      }),
    ).toBe("/dashboard/?site=a.example.com&range=30d&aud=bots&cmp=off");
    // The log keeps domain and range; audience and comparison drop.
    expect(
      viewHref("/dashboard/", {
        ...v,
        page: "log",
        aud: "bots",
        log: { addr: "2001:db8::/64", path: "/AS:n" },
      }),
    ).toBe("/dashboard/log?addr=2001%3Adb8%3A%3A%2F64&path=%2FAS%3An");
  });

  it("finds the mount", () => {
    expect(basePath("/dashboard/")).toBe("/dashboard/");
    expect(basePath("/dashboard/log")).toBe("/dashboard/");
    expect(basePath("/dashboard")).toBe("/dashboard/");
    expect(basePath("/")).toBe("/");
    expect(basePath("/log")).toBe("/");
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
    await api.log(scope, { addr: "192.0.2.120" }, "25");
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
    expect(calls[4]).toContain("addr=192.0.2.120&cursor=25");
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
    expect((await api.log(scope, {}, "")).rows).toHaveLength(25);
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
    expect(icon).toMatch(/^<svg [^]*<\/svg>\s*$/);
    expect(icon.length).toBeLessThan(600);
  });
});

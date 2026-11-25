import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { type Watch, type WatchHost, setFetcher } from "./api";
import App from "./App";
import { MainChart, StackChart, scaleLines } from "./charts";
import { type FixtureOptions, TRAIL_NEXT, fixtureFetch } from "./fixtures";
import { SEP, pct } from "./format";
import { VitalHint } from "./ui";

let calls: string[] = [];

function start(url = "/dashboard/", opt: FixtureOptions = {}) {
  window.history.replaceState(null, "", url);
  calls = [];
  const fx = fixtureFetch(opt);
  setFetcher((u, i) => {
    calls.push(u);
    return fx(u, i);
  });
  return render(<App />);
}

// Answers to requests matching hold.match wait until released.
const hold = {
  match: null as ((u: string) => boolean) | null,
  waiting: [] as { u: string; go: () => void }[],
  // Releases the held answers whose URL matches, in the order given.
  release(pick: (u: string) => boolean = () => true) {
    const go = this.waiting.filter((w) => pick(w.u));
    this.waiting = this.waiting.filter((w) => !pick(w.u));
    return act(async () => {
      for (const w of go) w.go();
    });
  },
};

function startHeld(url: string) {
  window.history.replaceState(null, "", url);
  calls = [];
  hold.match = null;
  hold.waiting = [];
  const fx = fixtureFetch();
  setFetcher((u, i) => {
    calls.push(u);
    if (!hold.match?.(u)) return fx(u, i);
    return new Promise<Response>((res, rej) =>
      hold.waiting.push({ u, go: () => fx(u, i).then(res, rej) }),
    );
  });
  return render(<App />);
}

// A main chart's props, but for its figures.
const CHART = {
  axis: { ticks: [], weekends: [] },
  meanLabel: "6 h",
  open: false,
  title: "T",
  prevLabel: "before",
};

const panel = (name: string) => screen.getByRole("region", { name });
// Text as rendered: the default normalizer folds an en space into a plain
// one, this one collapses only ASCII whitespace.
const asIs = {
  normalizer: (s: string) =>
    s.replace(/[ \t\r\n]+/g, " ").replace(/^ +| +$/g, ""),
};
const lastCall = (path: string) =>
  new URL(calls.filter((c) => c.startsWith(path)).at(-1)!, "http://x")
    .searchParams;

beforeEach(() => localStorage.clear());
afterEach(() => setFetcher(null));

describe("sign-in", () => {
  const cover = () =>
    document.querySelector('img[src="/dashboard/login-cover.webp"]');
  const tokenBox = () => screen.getByLabelText("Admin token");

  it("links to the login on 401 and never redirects itself", async () => {
    start("/dashboard/", { me: 401 });
    const link = await screen.findByRole("link", { name: "Sign in" });
    expect(link).toHaveAttribute("href", "/api/auth/login");
    expect(calls).toEqual(["/api/me", "/api/auth/methods"]);
    // The token below it, folded away.
    const fold = screen.getByText("Use the admin token instead");
    expect(fold.closest("details")).not.toHaveAttribute("open");
    expect(tokenBox()).toHaveAttribute("type", "password");
    expect(tokenBox()).toHaveAttribute("autocomplete", "current-password");
    expect(tokenBox()).not.toHaveFocus();
    // The art behind the card, decoration only.
    expect(cover()).toHaveAttribute("alt", "");
    // The icon's chip beside the wordmark.
    expect(
      screen
        .getByRole("heading", { name: "Trakk" })
        .querySelector('img[src="/dashboard/icon.svg"]'),
    ).not.toBeNull();
  });

  it("offers single sign-on alone, the token form alone, or says neither is set up", async () => {
    const { unmount } = start("/dashboard/", {
      me: 401,
      methods: { oidc: true, token: false },
    });
    await screen.findByRole("link", { name: "Sign in" });
    expect(screen.queryByLabelText("Admin token")).toBeNull();
    unmount();

    const second = start("/dashboard/", {
      me: 401,
      methods: { oidc: false, token: true },
    });
    await screen.findByLabelText("Admin token");
    expect(screen.queryByRole("link", { name: "Sign in" })).toBeNull();
    expect(screen.queryByText("Use the admin token instead")).toBeNull();
    await waitFor(() => expect(tokenBox()).toHaveFocus());
    const form = screen.getByRole("form", { name: "Admin token sign-in" });
    expect(form).toHaveAttribute("method", "post");
    expect(form).toHaveAttribute("action", "/api/auth/token");
    expect(form).toHaveTextContent("BACKEND_ADMIN_TOKEN");
    second.unmount();

    start("/dashboard/", { me: 401, methods: { oidc: false, token: false } });
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Sign-in is not configured",
    );
  });

  it("signs in with the admin token and loads the dashboard", async () => {
    start("/dashboard/", {
      me: 401,
      methods: { oidc: false, token: true },
      token: 204,
    });
    fireEvent.change(await screen.findByLabelText("Admin token"), {
      target: { value: "fixture-token" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Log in" }));
    expect(await screen.findByText(`dev${SEP}`, asIs)).toBeInTheDocument();
    expect(calls.filter((c) => c === "/api/me")).toHaveLength(2);
    // The signed-in dashboard never fetches the art.
    expect(cover()).toBeNull();
    // Nor shows the chip: the header keeps the plain wordmark.
    expect(document.querySelector('img[src$="icon.svg"]')).toBeNull();
  });

  it("says why a token was refused, never echoing it", async () => {
    for (const [status, text] of [
      [400, "could not read the sign-in form"],
      [401, "That is not the admin token."],
      [403, "refused a sign-in sent from another site"],
      [404, "not enabled on this server"],
      [413, "That value is too long to be the admin token."],
      [429, "Too many attempts. Wait 30 s and try again."],
      [500, "The server could not process the sign-in. Try again."],
    ] as const) {
      const { unmount } = start("/dashboard/", {
        me: 401,
        methods: { oidc: false, token: true },
        token: status,
      });
      fireEvent.change(await screen.findByLabelText("Admin token"), {
        target: { value: "s3cret-value" },
      });
      fireEvent.click(screen.getByRole("button", { name: "Log in" }));
      expect(await screen.findByRole("alert")).toHaveTextContent(text);
      expect(tokenBox()).toHaveValue("");
      // A refusal is no lost session: the card stays, nothing is retried
      // (a 403 here is the CSRF guard's, not the session's).
      expect(calls.filter((c) => c === "/api/me")).toHaveLength(1);
      expect(calls.filter((c) => c === "/api/auth/token")).toHaveLength(1);
      expect(document.body.textContent).not.toContain("s3cret-value");
      expect(localStorage.length).toBe(0);
      unmount();
    }
    // A network failure reads as one, not as a refused token.
    window.history.replaceState(null, "", "/dashboard/");
    const fx = fixtureFetch({ me: 401, methods: { oidc: true, token: true } });
    setFetcher((u, i) =>
      u === "/api/auth/token"
        ? Promise.reject(new TypeError("down"))
        : fx(u, i),
    );
    render(<App />);
    fireEvent.click(await screen.findByText("Use the admin token instead"));
    fireEvent.change(tokenBox(), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Log in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Could not reach the server.",
    );
  });

  it("says so when the browser did not keep an accepted session", async () => {
    start("/dashboard/", {
      me: 401,
      methods: { oidc: false, token: true },
      token: 204,
      keep: false,
    });
    fireEvent.change(await screen.findByLabelText("Admin token"), {
      target: { value: "fixture-token" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Log in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "did not keep the session",
    );
    // Asked once more, then the card: no loop.
    await new Promise((r) => setTimeout(r, 50));
    expect(calls.filter((c) => c === "/api/me")).toHaveLength(2);
    expect(calls.filter((c) => c === "/api/auth/token")).toHaveLength(1);
    expect(screen.getByLabelText("Admin token")).toBeInTheDocument();
  });

  it("says so on 403", async () => {
    start("/dashboard/", { me: 403 });
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "not in the group",
    );
    expect(cover()).not.toBeNull();
    expect(calls).toEqual(["/api/me"]);
  });

  it("reports an unreachable server", async () => {
    window.history.replaceState(null, "", "/dashboard/");
    setFetcher(async () => {
      throw new Error("offline");
    });
    render(<App />);
    expect(await screen.findByRole("alert")).toHaveTextContent("offline");
  });

  it("swaps to sign-in when a dashboard call says the session is gone", async () => {
    window.history.replaceState(null, "", "/dashboard/");
    const fx = fixtureFetch();
    setFetcher((u, i) =>
      u.startsWith("/api/dash/overview")
        ? Promise.resolve(new Response('{"detail":"expired"}', { status: 401 }))
        : fx(u, i),
    );
    render(<App />);
    expect(
      await screen.findByRole("link", { name: "Sign in" }),
    ).toBeInTheDocument();
    // The dashboard had named its domain; the sign-in card is the plain name.
    expect(document.title).toBe("Trakk");
  });
});

describe("the last domain and each domain's range", () => {
  const remember = (site: string, range: string) => {
    localStorage.setItem("app.site", site);
    localStorage.setItem(`app.range.${site}`, range);
  };
  const query = () => new URLSearchParams(window.location.search);
  const at = () => [query().get("site"), query().get("range")];
  // Every overview asked for, in order: the first must already be right.
  const asked = () =>
    calls
      .filter((c) => c.startsWith("/api/dash/overview"))
      .map((c) => {
        const q = new URL(c, "http://x").searchParams;
        return [q.get("site"), q.get("range")];
      });
  const ready = () => screen.findByRole("combobox", { name: "Domain" });
  const pick = (site: string) =>
    fireEvent.change(screen.getByRole("combobox", { name: "Domain" }), {
      target: { value: site },
    });

  it("open with a bare URL, which then names them", async () => {
    remember("console.example.com", "30d");
    const steps = window.history.length;
    start("/dashboard/#origin");
    expect(await ready()).toHaveValue("console.example.com");
    await waitFor(() =>
      expect(asked()).toEqual([["console.example.com", "30d"]]),
    );
    expect(at()).toEqual(["console.example.com", "30d"]);
    // Replaced, not pushed, the fragment kept.
    expect(window.history.length).toBe(steps);
    expect(window.location.hash).toBe("#origin");
  });

  it("start with the first domain and 7d, the range in the URL", async () => {
    start();
    expect(await ready()).toHaveValue("lens.example.com");
    expect(at()).toEqual(["lens.example.com", "7d"]);
    expect(localStorage.getItem("app.site")).toBe("lens.example.com");
    expect(localStorage.getItem("app.range.lens.example.com")).toBe("7d");
  });

  it("give way to the URL, whose view becomes the last", async () => {
    remember("console.example.com", "30d");
    start("/dashboard/?site=lens.example.com&range=90d");
    expect(await ready()).toHaveValue("lens.example.com");
    await waitFor(() => expect(asked()).toEqual([["lens.example.com", "90d"]]));
    expect(localStorage.getItem("app.site")).toBe("lens.example.com");
    expect(localStorage.getItem("app.range.lens.example.com")).toBe("90d");
    expect(localStorage.getItem("app.range.console.example.com")).toBe("30d");
  });

  it("follow a switch in the menu, each domain with its own", async () => {
    remember("lens.example.com", "30d");
    start("/dashboard/?aud=bots&cmp=off");
    await ready();
    pick("console.example.com");
    await waitFor(() => expect(at()).toEqual(["console.example.com", "7d"]));
    // Audience and comparison stay as they were.
    expect([query().get("aud"), query().get("cmp")]).toEqual(["bots", "off"]);
    fireEvent.click(screen.getByRole("button", { name: "24h" }));
    await waitFor(() => expect(at()).toEqual(["console.example.com", "24h"]));
    pick("lens.example.com");
    await waitFor(() => expect(at()).toEqual(["lens.example.com", "30d"]));
    pick("console.example.com");
    await waitFor(() => expect(at()).toEqual(["console.example.com", "24h"]));
    expect(localStorage.getItem("app.site")).toBe("console.example.com");
    expect(localStorage.getItem("app.range.lens.example.com")).toBe("30d");
    // No domain was ever asked for with another's range.
    expect(asked()).not.toContainEqual(["console.example.com", "30d"]);
    expect(asked()).not.toContainEqual(["lens.example.com", "24h"]);
    expect(asked()).not.toContainEqual(["lens.example.com", "7d"]);
  });

  it("fall back to the first domain when the last is gone", async () => {
    remember("gone.example.com", "30d");
    start();
    expect(await ready()).toHaveValue("lens.example.com");
    await waitFor(() => expect(asked()).toEqual([["lens.example.com", "7d"]]));
    expect(localStorage.getItem("app.site")).toBe("lens.example.com");
  });

  it("stay put in this tab when another one moves on", async () => {
    start();
    await ready();
    remember("console.example.com", "12mo");
    fireEvent.click(screen.getByRole("button", { name: "30d" }));
    await waitFor(() => expect(at()).toEqual(["lens.example.com", "30d"]));
  });

  it("fill a gap on a step back without sending it forward again", async () => {
    start();
    await ready();
    fireEvent.click(screen.getByRole("button", { name: "30d" }));
    expect(at()).toEqual(["lens.example.com", "30d"]);
    act(() => {
      window.history.back();
    });
    await waitFor(() => expect(at()).toEqual(["lens.example.com", "7d"]));
    // A URL with the gap itself, as a step back onto it.
    remember("console.example.com", "90d");
    act(() => {
      window.history.replaceState(null, "", "/dashboard/");
    });
    const steps = window.history.length;
    act(() => {
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    await waitFor(() => expect(at()).toEqual(["console.example.com", "90d"]));
    expect(window.history.length).toBe(steps);
  });

  it("open the log too", async () => {
    remember("console.example.com", "24h");
    start("/dashboard/log?kind=page_view");
    await screen.findByRole("region", { name: "Log" });
    expect(at()).toEqual(["console.example.com", "24h"]);
    expect(query().get("kind")).toBe("page_view");
    await waitFor(() =>
      expect(lastCall("/api/dash/log").get("range")).toBe("24h"),
    );
    expect(lastCall("/api/dash/log").get("site")).toBe("console.example.com");
  });
});

describe("dashboard on a domain with a server feed", () => {
  it("shows every section in order with the sources labelled", async () => {
    start();
    expect(
      await screen.findByText("Good week for people, busy one for scripts."),
    ).toHaveClass("gen");
    const heads = screen
      .getAllByRole("heading", { level: 2 })
      .map((h) => h.textContent);
    expect(heads).toEqual([
      "Overview",
      "Traffic",
      "Visitors",
      "Content",
      "Origin and clients",
      "This domain",
    ]);
    expect(screen.getAllByText("server").length).toBeGreaterThan(2);
    // Tiles in the contract's order, the server ones included.
    const tiles = within(panel("Overview"))
      .getAllByRole("button", { pressed: false })
      .concat(
        within(panel("Overview")).getAllByRole("button", { pressed: true }),
      );
    expect(tiles).toHaveLength(6);
    expect(document.querySelector(".topbar")?.textContent).toContain(
      `7 humans online${SEP}3 automated`,
    );
    // A narrow window says "bots" for "automated", shown in place of it.
    const bar = document.querySelector<HTMLElement>(".topbar")!;
    expect(within(bar).getByText("automated")).toHaveClass(
      "max-[900px]:hidden",
    );
    expect(within(bar).getByText("bots")).toHaveClass("min-[900px]:hidden");
    expect(await screen.findByText("Who is asking")).toBeInTheDocument();
    await screen.findByText("Requests by class");
    // Every query carries the scope.
    const q = lastCall("/api/dash/overview");
    expect(q.get("site")).toBe("lens.example.com");
    expect(q.get("range")).toBe("7d");
  });

  it("switches the main chart with a tile", async () => {
    start();
    const ov = await screen.findByRole("region", { name: "Overview" });
    await within(ov).findByText(`Unique visitors per hour${SEP}all`, asIs);
    fireEvent.click(within(ov).getByRole("button", { name: /Page views/ }));
    expect(
      within(ov).getByText(`Page views per hour${SEP}all`, asIs),
    ).toBeInTheDocument();
    const hit = within(ov).getByTestId("chart-hit");
    fireEvent.pointerMove(hit, { clientX: 500, clientY: 100 });
    const tip = screen.getByRole("tooltip");
    expect(tip.querySelector("h4")).toHaveTextContent(
      /^\w{3} \d\d\.11\. \d\d:00–\d\d:00$/,
    );
    expect(
      [...tip.querySelectorAll(".tr span")].map((s) => s.textContent),
    ).toEqual(["Page views", "6 h average", "prior 7 d average"]);
    // The mean against the prior one, as the backend computed it.
    expect(tip.querySelector(".tr:last-child b")!.textContent).toMatch(
      new RegExp(`^[\\d.]+${SEP}[↑↓→] [\\d.]+ %$`),
    );
    fireEvent.pointerLeave(hit);
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("runs every range to now: 24h ends at the minute, nothing filling", async () => {
    start();
    const ov = await screen.findByRole("region", { name: "Overview" });
    await within(ov).findByText(`Unique visitors per hour${SEP}all`, asIs);
    expect(screen.queryByText(/excluded/)).toBeNull();
    expect(ov.querySelector('[fill^="url(#hatch"]')).not.toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "24h" }));
    await within(ov).findByText(`Unique visitors per 10 min${SEP}all`, asIs);
    expect(screen.queryByText(/excluded/)).toBeNull();
    expect(ov.querySelector('[fill^="url(#hatch"]')).toBeNull();
    // The last point and the last hour.
    fireEvent.pointerMove(within(ov).getByTestId("chart-hit"), {
      clientX: 5000,
      clientY: 100,
    });
    expect(screen.getByRole("tooltip").textContent).toMatch(/^14:10–14:20/);
    expect(screen.getByRole("tooltip")).not.toHaveTextContent("still filling");
    fireEvent.pointerLeave(within(ov).getByTestId("chart-hit"));
    const bars = await screen.findAllByTestId("stack-hit");
    fireEvent.mouseMove(bars[bars.length - 1], { clientX: 10, clientY: 10 });
    await waitFor(() =>
      expect(screen.getByRole("tooltip").textContent).toMatch(/^hour to 14:20/),
    );
  });

  it("draws no comparison with the comparison off", async () => {
    start("/dashboard/?cmp=off");
    const ov = await screen.findByRole("region", { name: "Overview" });
    await within(ov).findByText(`Unique visitors per hour${SEP}all`, asIs);
    // The backend sends an empty previous series, not none.
    expect(lastCall("/api/dash/overview").get("cmp")).toBe("off");
    fireEvent.pointerMove(within(ov).getByTestId("chart-hit"), {
      clientX: 500,
      clientY: 100,
    });
    const tip = screen.getByRole("tooltip");
    expect(tip).toHaveTextContent("Unique visitors");
    expect(tip).not.toHaveTextContent(/prior/);
    // The slot and its mean, nothing else.
    expect(tip.querySelectorAll(".tr")).toHaveLength(2);
    expect(ov.querySelector('[stroke="var(--color-reference)"]')).toBeNull();
    // No tile has a change, so none keeps a line for one.
    expect(ov.querySelector(".tile-delta")).toBeNull();
  });

  it("asks for the briefing again only when the domain or range changes", async () => {
    start("/dashboard/");
    await screen.findByText(/Good week for people/);
    const briefings = () =>
      calls.filter((c) => c.startsWith("/api/dash/noteworthy")).length;
    expect(briefings()).toBe(1);
    fireEvent.click(screen.getByRole("button", { name: "vs last year" }));
    await waitFor(() =>
      expect(lastCall("/api/dash/overview").get("cmp")).toBe("yoy"),
    );
    fireEvent.click(screen.getByRole("button", { name: "Bots" }));
    await waitFor(() =>
      expect(lastCall("/api/dash/overview").get("aud")).toBe("bots"),
    );
    expect(briefings()).toBe(1);
    fireEvent.click(screen.getByRole("button", { name: "30d" }));
    await waitFor(() => expect(briefings()).toBe(2));
  });

  it("keeps range, audience and comparison in the URL", async () => {
    start();
    await screen.findByText(`Unique visitors per hour${SEP}all`, asIs);
    fireEvent.click(screen.getByRole("button", { name: "30d" }));
    expect(window.location.search).toContain("range=30d");
    await waitFor(() =>
      expect(lastCall("/api/dash/overview").get("range")).toBe("30d"),
    );
    fireEvent.click(screen.getByRole("button", { name: "Bots" }));
    expect(window.location.search).toContain("aud=bots");
    expect(localStorage.getItem("app.aud")).toBe("bots");
    expect(
      await screen.findByText(`Addresses per 3 hours${SEP}bots`, asIs),
    ).toBeInTheDocument();
    // Traffic and the errors show every class whatever the switch says.
    await waitFor(() =>
      expect(lastCall("/api/dash/visitors").get("aud")).toBe("bots"),
    );
    const traffic = calls.filter(
      (c) =>
        c.startsWith("/api/dash/status") ||
        /dim=(probed|refused|errors)/.test(c),
    );
    expect(traffic.length).toBeGreaterThan(0);
    expect(traffic.every((c) => c.includes("aud=all"))).toBe(true);
    // The class filter of the visitor table is only there under All.
    expect(screen.queryByRole("group", { name: "Class" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "off" }));
    expect(window.location.search).toContain("cmp=off");
    await waitFor(() =>
      expect(lastCall("/api/dash/overview").get("cmp")).toBe("off"),
    );
    // No legend entry for a comparison that is off.
    await waitFor(() =>
      expect(within(panel("Overview")).queryByText(/^prior 7 d$/)).toBeNull(),
    );
    await waitFor(() =>
      expect(
        screen.queryAllByRole("button", { name: "By change" }),
      ).toHaveLength(0),
    );
    expect(screen.getByText(/no comparison/)).toBeInTheDocument();
    // Back restores the previous view: the comparison on again, range and
    // audience as they were.
    act(() => {
      window.history.back();
    });
    await waitFor(() =>
      expect(window.location.search).not.toContain("cmp=off"),
    );
    const pressed = (group: string) =>
      within(screen.getByRole("group", { name: group }))
        .getAllByRole("button", { pressed: true })
        .map((b) => b.textContent);
    await waitFor(() =>
      expect(pressed("Compare")).toEqual(["vs previous period"]),
    );
    expect(pressed("Range")).toEqual(["30d"]);
    expect(pressed("Audience")).toEqual(["Bots"]);
    expect(screen.queryByText(/no comparison/)).toBeNull();
    expect(
      await screen.findAllByRole("button", { name: "By change" }),
    ).not.toHaveLength(0);
    await waitFor(() =>
      expect(lastCall("/api/dash/overview").get("cmp")).toBe("prev"),
    );
  });

  it("jumps from a finding to its panel or into the log", async () => {
    start();
    const nw = await screen.findByRole("region", { name: "Noteworthy" });
    const rows = await within(nw).findAllByRole("button");
    expect(rows).toHaveLength(5);
    expect(within(nw).getByText("1 more")).toBeInTheDocument();
    // The computed text where the model wrote none.
    expect(within(nw).getByText("AS64510 Example Cloud")).not.toHaveClass(
      "gen",
    );
    // A line over findings that add up shows their sum, one over findings
    // that do not how many it covers; the detail names each.
    expect(rows[2]).toHaveAccessibleName(
      "283 probes of: Scanners look for secrets and logins",
    );
    expect(rows[3]).toHaveAccessibleName(
      "2 findings: Hacker News brought a wave of new people",
    );
    expect(rows[3].querySelector("b")).toHaveTextContent("2");
    fireEvent.mouseMove(rows[3], { clientX: 10, clientY: 10 });
    expect(screen.getByRole("tooltip")).toHaveTextContent(
      "New referrer: news.ycombinator.com, and 1 related",
    );
    expect(screen.getByRole("tooltip")).toHaveTextContent(
      "749 visitors from New referrer",
    );
    fireEvent.mouseLeave(rows[3]);
    fireEvent.mouseMove(rows[4], { clientX: 10, clientY: 10 });
    expect(screen.getByRole("tooltip")).toHaveTextContent("2340 requests from");
    fireEvent.mouseLeave(rows[4]);
    fireEvent.mouseMove(rows[1], { clientX: 10, clientY: 10 });
    expect(screen.getByRole("tooltip")).toHaveTextContent(
      "88.4 % of all requests",
    );
    fireEvent.mouseLeave(rows[1]);
    fireEvent.click(rows[1]);
    const detail = await screen.findByRole("region", { name: "Visitor" });
    expect(
      await within(detail).findByText("Scraping attempt"),
    ).toBeInTheDocument();
    // A network spike opens the log on that network's requests.
    fireEvent.click(rows[4]);
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    const q = new URLSearchParams(window.location.search);
    expect([q.get("asn"), q.get("kind")]).toEqual(["64510", "request"]);
    const log = await screen.findByRole("region", { name: "Log" });
    expect(within(log).getByText(/Network AS64510/)).toBeInTheDocument();
    await waitFor(() =>
      expect(lastCall("/api/dash/log").get("asn")).toBe("64510"),
    );
  });

  it("lists visitors with sorts, filter, search, show all and the detail", async () => {
    start();
    const v = await screen.findByRole("region", {
      name: "Visitors by address",
    });
    await within(v).findByText("198.51.100.7");
    expect(
      within(v).getByText(`AS64510 Example Cloud${SEP}Hosting`, asIs),
    ).toBeInTheDocument();
    expect(v.textContent).toContain(
      `Mostly refused${SEP}API only, no page loaded${SEP}Rapid requests${SEP}Hosting network`,
    );
    expect(within(v).getByText(/Top 7 of 17 addresses/)).toBeInTheDocument();
    // A refused share of 20 % or more is red.
    expect(within(v).getByText(/^2['’]338$/)).toHaveClass("bad");
    fireEvent.click(within(v).getByRole("button", { name: "Most refused" }));
    await waitFor(() =>
      expect(lastCall("/api/dash/visitors").get("sort")).toBe("refused"),
    );
    fireEvent.click(
      within(v).getByRole("button", { name: "Unknown automated" }),
    );
    await waitFor(() =>
      expect(lastCall("/api/dash/visitors").get("class")).toBe("unknown"),
    );
    await waitFor(() =>
      expect(within(v).queryByText("203.0.113.42")).toBeNull(),
    );
    fireEvent.click(within(v).getByRole("button", { name: "All" }));
    fireEvent.change(within(v).getByRole("textbox"), {
      target: { value: "googlebot" },
    });
    await waitFor(
      () => expect(lastCall("/api/dash/visitors").get("q")).toBe("googlebot"),
      { timeout: 2000 },
    );
    await waitFor(() =>
      expect(within(v).queryByText("198.51.100.7")).toBeNull(),
    );
    fireEvent.change(within(v).getByRole("textbox"), {
      target: { value: "nothing-matches" },
    });
    expect(
      await within(v).findByText(
        /No address, network or client matches/,
        {},
        { timeout: 2000 },
      ),
    ).toBeInTheDocument();
    fireEvent.change(within(v).getByRole("textbox"), { target: { value: "" } });
    await within(v).findByText("198.51.100.7", {}, { timeout: 2000 });
    fireEvent.click(within(v).getByRole("button", { name: "Most requests" }));
    // Paging waits for the new order's answer.
    expect(
      within(v).getByRole("button", { name: "Show all →" }),
    ).toBeDisabled();
    await waitFor(() => expect(v).not.toHaveAttribute("aria-busy"));
    fireEvent.click(within(v).getByRole("button", { name: "Show all →" }));
    await waitFor(() =>
      expect(lastCall("/api/dash/visitors").get("limit")).toBe("50"),
    );
    await within(v).findByText("192.0.2.18");
    fireEvent.click(within(v).getByRole("button", { name: "Show top 7" }));
    await waitFor(() => expect(within(v).queryByText("192.0.2.18")).toBeNull());
    // A row fills the detail below.
    fireEvent.click(within(v).getByText("203.0.113.42"));
    const d = screen.getByRole("region", { name: "Visitor" });
    await within(d).findByText("Person");
    // What it does, as the table says it: the model's sentence, marked.
    const does = within(d).getByText(/Looked up one AS after a search/);
    expect(does).toHaveClass("gen");
    expect(does).toHaveAttribute("title", "Written by the model");
    expect(
      within(d).getByRole("img", { name: "Activity by hour" }).children,
    ).toHaveLength(24);
    // A run the page bound splits: shown as going on, not as complete.
    const run = () =>
      within(d)
        .getByText(new RegExp(`^view /:text${SEP}`), asIs)
        .closest("li") as HTMLElement;
    expect(run()).toHaveTextContent("page_view × 3+");
    expect(run()).toHaveTextContent("every 10 s, continues earlier");
    // A web vital's abbreviation explained behind an (i).
    const hint = within(d).getByRole("button", { name: "What is CLS?" });
    const pop = document.getElementById(hint.getAttribute("popovertarget")!)!;
    expect(pop).toHaveAttribute("popover", "auto");
    within(pop).getByText(`CLS${SEP}Cumulative Layout Shift`, asIs);
    expect(pop).toHaveTextContent("good up to 0.1, poor over 0.25");
    fireEvent.click(within(d).getByRole("button", { name: "Load earlier →" }));
    await waitFor(() =>
      expect(lastCall("/api/dash/visitor").get("cursor")).toBe(TRAIL_NEXT),
    );
    // Its older part joins it: one run, both parts' intervals.
    await waitFor(() =>
      expect(d.querySelectorAll("ol.trail > li")).toHaveLength(6),
    );
    expect(within(d).getAllByText(/^view \/:text/)).toHaveLength(1);
    expect(run()).toHaveTextContent("page_view × 5");
    expect(run()).toHaveTextContent("every 10 s to 20 s");
    expect(run()).not.toHaveTextContent("continues");
    expect(
      within(d).queryByRole("button", { name: "Load earlier →" }),
    ).toBeNull();
    fireEvent.click(
      within(d).getByRole("button", { name: /Every event of this address/ }),
    );
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    expect(new URLSearchParams(window.location.search).get("addr")).toBe(
      "203.0.113.42",
    );
  });

  it("pages through the visitors", async () => {
    start();
    const v = await screen.findByRole("region", {
      name: "Visitors by address",
    });
    await within(v).findByText("198.51.100.7");
    fireEvent.click(within(v).getByRole("button", { name: "Show all →" }));
    await within(v).findByText(/17 of 17 addresses/);
    expect(within(v).queryByRole("button", { name: "Load more →" })).toBeNull();
  });

  it("filters the API clients, a class of their own", async () => {
    start();
    const v = await screen.findByRole("region", {
      name: "Visitors by address",
    });
    await within(v).findByText("198.51.100.7");
    fireEvent.click(
      await within(v).findByRole("button", { name: "API clients" }),
    );
    await waitFor(() =>
      expect(lastCall("/api/dash/visitors").get("class")).toBe("api"),
    );
    const row = (await within(v).findByText("198.51.100.30")).closest("tr")!;
    expect(within(row).getByText("API client")).toBeInTheDocument();
    expect(within(row).getByText("Signed-in API client")).toBeInTheDocument();
    await waitFor(() =>
      expect(within(v).queryByText("198.51.100.7")).toBeNull(),
    );
  });

  it("marks the model's client label and keeps the raw client", async () => {
    start();
    const v = await screen.findByRole("region", {
      name: "Visitors by address",
    });
    const label = await within(v).findByText("Python scraping script");
    expect(label).toHaveClass("gen");
    expect(label.closest("td")).toHaveAttribute(
      "title",
      "python-requests/2.32.3",
    );
    // The detail names the client as the table does.
    fireEvent.click(label);
    const d = await screen.findByRole("region", { name: "Visitor" });
    const detail = await within(d).findByText("Python scraping script");
    expect(detail.closest("dd")).toHaveAttribute(
      "title",
      "python-requests/2.32.3",
    );
  });

  it("shows an address without events in the range as empty, not failed", async () => {
    window.history.replaceState(null, "", "/dashboard/");
    const fx = fixtureFetch();
    setFetcher((u, i) =>
      u.startsWith("/api/dash/visitor?")
        ? Promise.resolve(
            new Response(
              JSON.stringify({
                detail: "no events from this address in the range",
              }),
              { status: 404 },
            ),
          )
        : fx(u, i),
    );
    render(<App />);
    const d = await screen.findByRole("region", { name: "Visitor" });
    expect(
      await within(d).findByText("No events from this address in this range."),
    ).toBeInTheDocument();
    expect(within(d).queryByRole("alert")).toBeNull();
  });

  it("shows a reading's related address and the computed fallback", async () => {
    start();
    const d = await screen.findByRole("region", { name: "Visitor" });
    expect(await within(d).findByText(/Same class in this \/24/)).toBeTruthy();
    fireEvent.click(
      await within(d).findByRole("button", { name: "198.51.100.201" }),
    );
    await waitFor(() =>
      expect(
        within(d).getByText("198.51.100.201", { selector: "div" }),
      ).toBeInTheDocument(),
    );
    const v = screen.getByRole("region", { name: "Visitors by address" });
    fireEvent.click(within(v).getByText("203.0.113.9"));
    expect(
      await within(d).findByText(/No reading for this address yet/),
    ).toBeInTheDocument();
    // Without the model's sentence, the computed one, unmarked.
    fireEvent.click(within(v).getByText("192.0.2.66"));
    const does = await within(d).findByText(
      "Known bot, mostly view /AS:n (1520×)",
    );
    expect(does).not.toHaveClass("gen");
  });

  it("runs the top-N lists: order, show all, search, expand, log", async () => {
    start();
    const pages = await screen.findByRole("region", { name: "Pages" });
    await within(pages).findByText("/AS:n");
    expect(within(pages).getByText(`Other${SEP}2`, asIs)).toBeInTheDocument();
    // Entry is the share of visits that began on the page.
    expect(within(pages).getByText("6.6 %")).toBeInTheDocument();
    fireEvent.click(within(pages).getByRole("button", { name: "By change" }));
    await waitFor(() =>
      expect(calls.some((c) => c.includes("dim=pages&sort=change"))).toBe(true),
    );
    fireEvent.click(within(pages).getByRole("button", { name: "Entry" }));
    await waitFor(() =>
      expect(calls.some((c) => c.includes("dim=entry"))).toBe(true),
    );
    fireEvent.click(within(pages).getByRole("button", { name: "Top" }));
    // Rows are keyed by position (so nothing re-animates): wait until the
    // Top answer is drawn, or the caret found may already belong to
    // another row.
    await waitFor(() =>
      expect(within(pages).getAllByRole("row")[2]).toHaveTextContent("/AS:n"),
    );
    fireEvent.click(within(pages).getByRole("button", { name: "Open /AS:n" }));
    expect(await within(pages).findByText("/AS15169")).toBeInTheDocument();
    expect(within(pages).getByText("and 1 more")).toBeInTheDocument();
    fireEvent.click(within(pages).getByRole("button", { name: "Close /AS:n" }));
    expect(within(pages).queryByText("/AS15169")).toBeNull();

    fireEvent.click(within(pages).getByRole("button", { name: /Show all 8/ }));
    const dlg = screen.getByRole("dialog");
    await within(dlg).findByText("/privacy");
    fireEvent.change(within(dlg).getByRole("textbox"), {
      target: { value: "zzz" },
    });
    expect(
      await within(dlg).findByText(/Nothing matches/, {}, { timeout: 2000 }),
    ).toBeInTheDocument();
    expect(lastCall("/api/dash/list").get("limit")).toBe("500");
    fireEvent.click(within(dlg).getByRole("button", { name: "By volume" }));
    fireEvent.keyDown(window, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();

    // Bars: referrers open the full table too, as a generic table.
    const refs = screen.getByRole("region", { name: "Referrers" });
    fireEvent.click(
      await within(refs).findByRole("button", { name: /Show all/ }),
    );
    const d2 = screen.getByRole("dialog");
    await within(d2).findByText("reddit.com");
    fireEvent.click(within(d2).getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).toBeNull();

    // A probed path opens the log filtered to it, by keyboard too.
    const probed = screen.getByRole("region", { name: "Probed paths" });
    const row = (await within(probed).findAllByRole("button")).find((b) =>
      b.textContent?.includes("/.env"),
    )!;
    fireEvent.keyDown(row, { key: "Enter" });
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    // The method and template, as the log's path filter matches them.
    expect(new URLSearchParams(window.location.search).get("path")).toBe(
      "GET /.env",
    );
  });

  it("lets the keyboard reach every clickable row", async () => {
    start();
    const v = await screen.findByRole("region", {
      name: "Visitors by address",
    });
    const vrow = await within(v).findByRole("button", {
      name: /^203\.0\.113\.42/,
    });
    vrow.focus();
    expect(vrow).toHaveFocus();
    fireEvent.click(vrow);
    const d = screen.getByRole("region", { name: "Visitor" });
    expect(await within(d).findByText("Person")).toBeInTheDocument();
    expect(vrow).toHaveAttribute("aria-pressed", "true");

    const pages = screen.getByRole("region", { name: "Pages" });
    await within(pages).findByText("/AS:n");
    // The table keeps its rows; the first cell is the row's button.
    const row = within(pages).getByText("/AS:n").closest("tr")!;
    expect(within(pages).getAllByRole("row")).toContain(row);
    expect(within(row).getByRole("button", { name: "/AS:n" })).toBeTruthy();
    fireEvent.click(within(pages).getByRole("button", { name: "Open /AS:n" }));
    // The caret opens the row, it does not follow it.
    expect(window.location.pathname).toBe("/dashboard/");
    fireEvent.click(
      await within(pages).findByRole("button", { name: "/AS15169" }),
    );
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    expect(new URLSearchParams(window.location.search).get("path")).toBe(
      "/AS15169",
    );
  });

  it("opens the log from a page and from an event", async () => {
    start();
    const ev = await screen.findByRole("region", { name: "Events" });
    fireEvent.click(await within(ev).findByRole("button", { name: "Custom" }));
    await waitFor(() =>
      expect(lastCall("/api/dash/list").get("sub")).toBe("custom"),
    );
    expect(document.title).toBe(`Trakk${SEP}lens.example.com`);
    fireEvent.click(await within(ev).findByText("share_link"));
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    expect(document.title).toBe(`Trakk${SEP}lens.example.com${SEP}Log`);
    expect(new URLSearchParams(window.location.search).get("path")).toBe(
      "share_link",
    );
  });

  it("opens the log for an automatic event by its kind", async () => {
    start();
    const ev = await screen.findByRole("region", { name: "Events" });
    fireEvent.click(await within(ev).findByText("download"));
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    const q = new URLSearchParams(window.location.search);
    expect(q.get("kind")).toBe("download");
    expect(q.get("path")).toBeNull();
  });

  it("draws the traffic charts with tooltips", async () => {
    start();
    const t = await screen.findByRole("region", { name: "Requests by class" });
    const bars = await within(t).findAllByTestId("stack-hit");
    // Six hours each, up to the one now is in.
    expect(bars).toHaveLength(27);
    fireEvent.mouseMove(bars[26], { clientX: 900, clientY: 100 });
    expect(screen.getByRole("tooltip").textContent).toMatch(
      new RegExp(`^Sun 22.11. 12:00–18:00${SEP}still filling`),
    );
    fireEvent.mouseLeave(bars[26]);
    const who = screen.getByRole("region", { name: "Who is asking" });
    const parts = within(who).getAllByTestId("strip-part");
    fireEvent.mouseMove(parts[0], { clientX: 10, clientY: 10 });
    expect(screen.getByRole("tooltip")).toHaveTextContent("Humans");
    fireEvent.mouseLeave(parts[0]);
    const refused = screen.getByRole("region", {
      name: "Refused requests by address",
    });
    expect(await within(refused).findByText("5xx")).toBeInTheDocument();
    fireEvent.click(await within(refused).findByText("198.51.100.201"));
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
  });

  it("draws the map and the clients", async () => {
    start();
    const map = await screen.findByRole("region", {
      name: "Where visitors come from",
    });
    const svg = await within(map).findByRole(
      "img",
      { name: "Visitors by country" },
      { timeout: 3000 },
    );
    const ch = svg.querySelector('[data-cc="CH"]')!;
    expect(ch.getAttribute("fill")).toBe("var(--color-seq-4)");
    expect(svg.querySelector('[data-cc="AQ"]')).toBeNull();
    fireEvent.mouseMove(ch, { clientX: 10, clientY: 10 });
    expect(screen.getByRole("tooltip")).toHaveTextContent("Switzerland");
    fireEvent.mouseLeave(ch);
    const fr = svg.querySelector('[data-cc="ZA"]')!;
    fireEvent.mouseMove(fr, { clientX: 10, clientY: 10 });
    expect(screen.getByRole("tooltip")).toHaveTextContent("none");
    expect(within(map).getByText("+ 10 more")).toBeInTheDocument();
    const cl = screen.getByRole("region", { name: "Clients" });
    expect(
      await within(cl).findByText("Operating systems"),
    ).toBeInTheDocument();
  });

  it("takes the map's legend and its remainder from the backend", async () => {
    window.history.replaceState(null, "", "/dashboard/");
    const fx = fixtureFetch();
    const legend = {
      rows: ["CH", "DE", "US"].map((cc, i) => ({
        key: cc,
        label: cc,
        sub: "",
        gen: null,
        value: 30 - i,
        second: 0,
        share: 10,
        change: null,
        verdict: "flat",
        expandable: false,
      })),
      other: { count: 42, value: 100 },
      total: 99,
      server: false,
    };
    setFetcher((u, i) => {
      const q = new URL(u, "http://x").searchParams;
      return q.get("dim") === "countries" && q.get("limit") === "5"
        ? Promise.resolve(new Response(JSON.stringify(legend)))
        : fx(u, i);
    });
    render(<App />);
    const map = await screen.findByRole("region", {
      name: "Where visitors come from",
    });
    expect(await within(map).findByText("+ 42 more")).toBeInTheDocument();
    const shown = map.querySelectorAll(".legend.mt-2\\.5 > span b");
    expect([...shown].map((b) => b.textContent)).toEqual(["30", "29", "28"]);
  });

  it("shows the domain footer", async () => {
    start();
    const d = await screen.findByRole("region", { name: "This domain" });
    expect(await within(d).findByText("Rate-limited")).toBeInTheDocument();
    // The collector-wide figure: unknown domains and wrong feed keys.
    expect(
      within(d).getByText(`Unknown site or wrong key${SEP}all sites`, asIs),
    ).toBeInTheDocument();
    expect(within(d).getByText("412 MB")).toBeInTheDocument();
    // Raw events and the figures, which reach further back.
    expect(
      within(d).getByText(
        new RegExp(
          `^1['’]284['’]551 stored${SEP}since 3\\.9\\.2026${SEP}kept 90 days$`,
        ),
        asIs,
      ),
    ).toBeInTheDocument();
    expect(within(d).getByText("Figures").nextSibling).toHaveTextContent(
      "since 20.10.2025",
    );
    expect(within(d).queryByText("First event")).toBeNull();
    // The per-address sums expire with raw.
    expect(within(d).getByText("Addresses").nextSibling).toHaveTextContent(
      "kept 25 months",
    );
    expect(within(d).getByText("41 of 200 calls today")).toBeInTheDocument();
    // No purge pending: nothing about older raw.
    expect(d.textContent).not.toMatch(/is deleted from/);
  });

  it("says from when older raw is deleted during the purge's grace", async () => {
    window.history.replaceState(null, "", "/dashboard/");
    const fx = fixtureFetch();
    setFetcher(async (u, i) => {
      const res = await fx(u, i);
      if (!u.startsWith("/api/dash/domain?")) return res;
      const d = await res.json();
      // An upgraded install: raw still reaches back two years.
      return new Response(
        JSON.stringify({
          ...d,
          storage: {
            ...d.storage,
            oldest: "2024-10-30T00:00:00Z",
            purge_from: "2026-11-29T13:00:00Z",
          },
        }),
      );
    });
    render(<App />);
    const d = await screen.findByRole("region", { name: "This domain" });
    expect(
      await within(d).findByText(
        new RegExp(`since 30\\.10\\.2024${SEP}kept 90 days$`),
        asIs,
      ),
    ).toBeInTheDocument();
    expect(
      within(d).getByText("raw older than 90 days is deleted from 29.11.2026"),
    ).toBeInTheDocument();
  });

  it("shows no figures row for a domain without data", async () => {
    window.history.replaceState(null, "", "/dashboard/");
    const fx = fixtureFetch();
    setFetcher(async (u, i) => {
      const res = await fx(u, i);
      if (!u.startsWith("/api/dash/domain?")) return res;
      const d = await res.json();
      return new Response(
        JSON.stringify({
          ...d,
          sources: { ...d.sources, first: null },
          storage: {
            ...d.storage,
            events: 0,
            oldest: null,
            figures_since: null,
          },
        }),
      );
    });
    render(<App />);
    const d = await screen.findByRole("region", { name: "This domain" });
    expect(
      await within(d).findByText("Database, all sites"),
    ).toBeInTheDocument();
    expect(within(d).queryByText("Figures")).toBeNull();
    expect(d.textContent).not.toMatch(/since –/);
  });

  it("shows a failing panel's error and keeps the rest", async () => {
    start("/dashboard/", { fail: ["/api/dash/list", "/api/dash/domain"] });
    const pages = await screen.findByRole("region", { name: "Pages" });
    expect(await within(pages).findByRole("alert")).toHaveTextContent(
      "fixture failure",
    );
    expect(
      await screen.findByText("Good week for people, busy one for scripts."),
    ).toBeInTheDocument();
  });

  it("signs out and reloads", async () => {
    const reload = vi.fn();
    vi.stubGlobal("location", {
      ...window.location,
      reload,
      pathname: "/dashboard/",
      search: "",
    });
    start();
    fireEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(reload).toHaveBeenCalled());
    expect(calls).toContain("/api/auth/logout");
    vi.unstubAllGlobals();
  });
});

// The fixtures' raw events reach back to 3.9.2026: 90 d and 12 mo start
// before, 7 d and 30 d after.
describe("where raw events end", () => {
  const noted = /since 3\.9\.2026|back to 3\.9\.2026/;

  it("says nothing when raw covers the range", async () => {
    start("/dashboard/?range=30d");
    const d = await screen.findByRole("region", { name: "Visitor" });
    await within(d).findByText("Scraping attempt");
    const pages = await screen.findByRole("region", { name: "Pages" });
    fireEvent.click(
      await within(pages).findByRole("button", { name: "Open /AS:n" }),
    );
    await within(pages).findByText("/AS15169");
    const v = panel("Visitors by address");
    await within(v).findByText(/Top 7 of 17 addresses/);
    // The range's start is in (the overview's live count shows).
    await screen.findByText(/humans/);
    for (const p of [d, pages, v]) expect(p.textContent).not.toMatch(noted);
  });

  it("says where the trail, the hours, the reading and the table's sentences begin", async () => {
    start("/dashboard/?range=12mo");
    const d = await screen.findByRole("region", { name: "Visitor" });
    await within(d).findByText("Scraping attempt");
    // A trail without an earlier page ends with the note.
    expect(
      await within(d).findByText(
        "The trail ends here: raw events reach back to 3.9.2026.",
      ),
    ).toBeInTheDocument();
    const heads = [...d.querySelectorAll(".eyebrow")].map((e) => e.textContent);
    expect(heads).toContain(`Reading${SEP}since 3.9.2026`);
    expect(heads).toContain(
      `Activity by hour${SEP}00 to 23${SEP}since 3.9.2026`,
    );
    // A narrow column breaks before the dot: it goes with "since …".
    const tails = [...d.querySelectorAll(".eyebrow .whitespace-nowrap")];
    expect(tails.map((e) => e.textContent)).toEqual([
      `${SEP.trimStart()}since 3.9.2026`,
      `${SEP.trimStart()}since 3.9.2026`,
    ]);
    // What it does, under its sentence.
    expect(
      within(d).getByText(/^Walks the IP lookup API/).parentElement,
    ).toHaveTextContent(/scraping attempt\.since 3\.9\.2026$/);
    const v = panel("Visitors by address");
    expect(
      within(v).getByText(
        new RegExp(
          `summed over the whole range${SEP}what it does since 3\\.9\\.2026$`,
        ),
        asIs,
      ),
    ).toBeInTheDocument();
    // A trail with earlier pages: the note once the last is in.
    fireEvent.click(within(v).getByText("203.0.113.42"));
    await within(d).findByText("Person");
    expect(within(d).queryByText(/The trail ends here/)).toBeNull();
    fireEvent.click(within(d).getByRole("button", { name: "Load earlier →" }));
    expect(
      await within(d).findByText(/The trail ends here/),
    ).toBeInTheDocument();
    // A /64's addresses are counted from raw too.
    fireEvent.click(within(v).getByText("2001:db8:4f2:1a00::/64"));
    const n = await within(d).findByText(/in this \/64/);
    expect(n.textContent).toBe(`3 in this /64${SEP}since 3.9.2026`);
    expect(n.querySelector(".whitespace-nowrap")?.textContent).toBe(
      `${SEP.trimStart()}since 3.9.2026`,
    );
  });

  it("follows a range switched in place", async () => {
    start("/dashboard/?range=12mo");
    await screen.findByRole("region", { name: "Visitor" });
    const v = panel("Visitors by address");
    const said = /what it does since 3\.9\.2026$/;
    expect(await within(v).findByText(said)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "30d" }));
    await waitFor(() => expect(v.textContent).not.toMatch(noted));
    await within(v).findByText(/Top 7 of 17 addresses/);
    fireEvent.click(screen.getByRole("button", { name: "12mo" }));
    expect(await within(v).findByText(said)).toBeInTheDocument();
  });

  it("says a visitor's raw events are gone, not that it has none", async () => {
    window.history.replaceState(null, "", "/dashboard/?range=12mo");
    const fx = fixtureFetch();
    setFetcher(async (u, i) => {
      const res = await fx(u, i);
      if (!u.startsWith("/api/dash/visitor?")) return res;
      const v = await res.json();
      return new Response(
        JSON.stringify({ ...v, trail: [], trail_next: "", reading: null }),
      );
    });
    render(<App />);
    const d = await screen.findByRole("region", { name: "Visitor" });
    expect(
      await within(d).findByText(
        "No events since 3.9.2026: raw events reach back no further.",
      ),
    ).toBeInTheDocument();
    expect(within(d).queryByText("No events in this range.")).toBeNull();
  });

  it("says where a template's paths begin", async () => {
    start("/dashboard/?range=12mo");
    const pages = await screen.findByRole("region", { name: "Pages" });
    fireEvent.click(
      await within(pages).findByRole("button", { name: "Open /AS:n" }),
    );
    expect(
      await within(pages).findByText(
        `and 1 more${SEP}paths since 3.9.2026`,
        asIs,
      ),
    ).toBeInTheDocument();
  });

  it("says where the log begins", async () => {
    const first = start("/dashboard/log?site=lens.example.com&range=90d");
    const log = await screen.findByRole("region", { name: "Log" });
    expect(
      await within(log).findByText(
        `100 of 250 events in this range${SEP}the log reaches back to 3.9.2026`,
        asIs,
      ),
    ).toBeInTheDocument();
    first.unmount();
    start("/dashboard/log?site=lens.example.com&range=30d");
    const log30 = await screen.findByRole("region", { name: "Log" });
    await within(log30).findByText("100 of 250 events in this range", asIs);
    // The range's start is in (the overview's live count shows).
    await screen.findByText(/humans/);
    expect(log30.textContent).not.toMatch(noted);
  });
});

describe("while an answer is on its way", () => {
  it("never shows another domain's data under the new one's name", async () => {
    startHeld("/dashboard/?site=console.example.com");
    await screen.findByText("2 findings against the prior 7 d");
    const v = panel("Visitors by address");
    await waitFor(() =>
      expect(within(v).queryAllByRole("row").length).toBeGreaterThan(1),
    );
    expect(document.querySelector(".topbar")).toHaveTextContent("online");
    hold.match = (u) => u.includes("site=lens.example.com");
    fireEvent.change(screen.getByRole("combobox", { name: "Domain" }), {
      target: { value: "lens.example.com" },
    });
    expect(screen.queryByText("2 findings against the prior 7 d")).toBeNull();
    expect(document.querySelector(".topbar")).not.toHaveTextContent("online");
    expect(screen.queryByText(/one point per/)).toBeNull();
    expect(
      within(panel("Visitors by address")).queryAllByRole("row"),
    ).toHaveLength(0);
    expect(
      within(panel("Overview")).getByLabelText("Loading the overview"),
    ).toHaveAttribute("aria-busy", "true");
    for (const name of ["Pages", "Clients", "This domain"])
      expect(within(panel(name)).queryByRole("table")).toBeNull();
    await hold.release();
    expect(
      await screen.findByText("Good week for people, busy one for scripts."),
    ).toBeInTheDocument();
  });

  it("keeps the same domain's answer dimmed, without paging from it", async () => {
    startHeld("/dashboard/");
    const ov = await screen.findByRole("region", { name: "Overview" });
    await within(ov).findByText(`Unique visitors per hour${SEP}all`, asIs);
    const v = panel("Visitors by address");
    await within(v).findByText("198.51.100.7");
    const mainChart = () => within(ov).getByTestId("chart-hit").closest("svg");
    const chart = mainChart();
    expect(chart).not.toBeNull();
    hold.match = (u) => u.includes("range=30d");
    fireEvent.click(screen.getByRole("button", { name: "30d" }));
    expect(ov).toHaveAttribute("aria-busy", "true");
    expect(v).toHaveAttribute("aria-busy", "true");
    expect(within(v).getByText("198.51.100.7")).toBeInTheDocument();
    expect(
      within(v).getByRole("button", { name: "Show all →" }),
    ).toBeDisabled();
    expect(panel("Pages")).toHaveAttribute("aria-busy", "true");
    await hold.release();
    await waitFor(() => expect(ov).not.toHaveAttribute("aria-busy"));
    expect(v).not.toHaveAttribute("aria-busy");
    // The same chart, redrawn in place: its entrance does not run again.
    expect(mainChart()).toBe(chart);
  });

  it("never pages the log with another filter's cursor", async () => {
    startHeld("/dashboard/log?site=lens.example.com");
    const log = await screen.findByRole("region", { name: "Log" });
    await within(log).findByText("100 of 250 events in this range");
    fireEvent.click(
      within(log).getByRole("button", { name: "Load earlier →" }),
    );
    await within(log).findByText("200 of 250 events in this range");
    hold.match = (u) => u.startsWith("/api/dash/log");
    fireEvent.change(within(log).getByRole("combobox", { name: "Kind" }), {
      target: { value: "request" },
    });
    expect(log).toHaveAttribute("aria-busy", "true");
    const earlier = within(log).getByRole("button", { name: /Load earlier/ });
    expect(earlier).toBeDisabled();
    fireEvent.click(earlier);
    await hold.release();
    await waitFor(() => expect(log).not.toHaveAttribute("aria-busy"));
    expect(
      calls.filter((c) => c.includes("kind=request") && c.includes("cursor=")),
    ).toEqual([]);
  });

  it("drops a page that arrives for a query already left", async () => {
    startHeld("/dashboard/log?site=lens.example.com");
    const log = await screen.findByRole("region", { name: "Log" });
    await within(log).findByText("100 of 250 events in this range");
    hold.match = (u) => u.startsWith("/api/dash/log");
    // The 7 d query's second page is asked for, then the range changes.
    fireEvent.click(
      within(log).getByRole("button", { name: "Load earlier →" }),
    );
    fireEvent.click(screen.getByRole("button", { name: "30d" }));
    const old = (u: string) => u.includes("range=7d");
    await hold.release((u) => !old(u));
    await within(log).findByText("100 of 250 events in this range");
    expect(log).not.toHaveAttribute("aria-busy");
    // Its own paging is not stuck behind the old request.
    const earlier = within(log).getByRole("button", { name: "Load earlier →" });
    expect(earlier).toBeEnabled();
    fireEvent.click(earlier);
    await hold.release((u) => !old(u));
    await within(log).findByText("200 of 250 events in this range");
    // The old query's page lands last and changes nothing.
    await hold.release();
    expect(
      within(log).getByText("200 of 250 events in this range"),
    ).toBeInTheDocument();
    expect(within(log).getAllByRole("row")).toHaveLength(201);
  });

  it("never pages a visitor's trail with another visitor's cursor", async () => {
    startHeld("/dashboard/");
    const v = await screen.findByRole("region", {
      name: "Visitors by address",
    });
    fireEvent.click(await within(v).findByText("203.0.113.42"));
    const d = panel("Visitor");
    await within(d).findByText("Person");
    expect(
      within(d).getByRole("button", { name: "Load earlier →" }),
    ).toBeEnabled();
    hold.match = (u) => u.startsWith("/api/dash/visitor?");
    fireEvent.click(within(v).getByText("198.51.100.7"));
    expect(d).toHaveAttribute("aria-busy", "true");
    const earlier = within(d).getByRole("button", { name: "Load earlier →" });
    expect(earlier).toBeDisabled();
    fireEvent.click(earlier);
    await hold.release();
    expect(
      calls.filter(
        (c) =>
          c.startsWith("/api/dash/visitor?") &&
          c.includes("198.51.100.7") &&
          c.includes("cursor="),
      ),
    ).toEqual([]);
  });
});

describe("script errors", () => {
  it("opens a script error's detail, a server error's log", async () => {
    start("/dashboard/?site=lens.example.com");
    const errors = await screen.findByRole("region", { name: "Errors" });
    fireEvent.click(await within(errors).findByText(/^TypeError/));
    const d = await screen.findByRole("region", { name: "Error" });
    expect(lastCall("/api/dash/error").get("key")).toBe(
      'TypeError: cannot read "prefixes"',
    );
    // Every class, as the list above it counts.
    expect(lastCall("/api/dash/error").get("aud")).toBe("all");
    expect(
      await within(d).findByText("/assets/index-C4v1rT2s.js:1:48113"),
    ).toBeInTheDocument();
    expect(within(d).getByText("The site's code")).toBeInTheDocument();
    // Every origin is explained, one whose places are all among the cut ones
    // too, in the backend's order.
    expect(
      within(d)
        .getAllByRole("term")
        .map((x) => x.textContent),
    ).toEqual([
      "The site's code",
      "Another host's script",
      "Not the site's code",
    ]);
    expect(within(d).getByText(/1.1 % of 1.310/)).toBeInTheDocument();
    expect(within(d).getByText("2 more")).toBeInTheDocument();
    expect(
      within(within(d).getByRole("list", { name: "Stack" })).getAllByRole(
        "listitem",
      ),
    ).toHaveLength(2);
    // A visitor's trail says what came before.
    fireEvent.click(within(d).getByRole("button", { name: "203.0.113.42" }));
    await screen.findByRole("region", { name: "Visitor" });
    expect(lastCall("/api/dash/visitor").get("key")).toBe("203.0.113.42");
    fireEvent.click(within(d).getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("region", { name: "Error" })).toBeNull();
    fireEvent.click(within(errors).getByText("GET /api/v1/search"));
    expect(window.location.pathname).toBe("/dashboard/log");
  });

  it("leads from the detail to its events", async () => {
    start("/dashboard/?site=console.example.com");
    const errors = await screen.findByRole("region", { name: "Errors" });
    fireEvent.click(await within(errors).findByText(/^Unhandled rejection/));
    const d = await screen.findByRole("region", { name: "Error" });
    fireEvent.click(
      await within(d).findByRole("button", {
        name: "Every occurrence in the log →",
      }),
    );
    const name = "Unhandled rejection: AbortError";
    const log = await screen.findByRole("region", { name: "Log" });
    // Exactly this message, as the detail counts it: no substring.
    for (const q of [
      new URLSearchParams(window.location.search),
      lastCall("/api/dash/log"),
    ]) {
      expect(q.get("kind")).toBe("js_error");
      expect(q.get("name")).toBe(name);
      expect(q.get("path")).toBeNull();
    }
    expect(within(log).getByText(`Name ${name}`)).toBeInTheDocument();
    fireEvent.click(
      within(log).getByRole("button", { name: "Clear the name filter" }),
    );
    await waitFor(() =>
      expect(lastCall("/api/dash/log").get("name")).toBeNull(),
    );
    expect(lastCall("/api/dash/log").get("kind")).toBe("js_error");
  });

  // One stored without a message: no name to ask the detail for.
  it("opens the log for a script error without a message", async () => {
    start("/dashboard/?site=lens.example.com");
    const fx = fixtureFetch();
    setFetcher(async (u, i) => {
      calls.push(u);
      const r = await fx(u, i);
      if (!u.startsWith("/api/dash/list?") || !u.includes("dim=errors"))
        return r;
      const l = await r.json();
      l.rows[0] = { ...l.rows[0], key: "", label: "" };
      return new Response(JSON.stringify(l));
    });
    const errors = await screen.findByRole("region", { name: "Errors" });
    fireEvent.click((await within(errors).findAllByText("js"))[0]);
    const log = await screen.findByRole("region", { name: "Log" });
    expect(window.location.pathname).toBe("/dashboard/log");
    expect(within(log).getByRole("combobox", { name: "Kind" })).toHaveValue(
      "js_error",
    );
    const q = lastCall("/api/dash/log");
    expect(q.get("kind")).toBe("js_error");
    expect(q.get("name")).toBeNull();
    expect(calls.some((c) => c.startsWith("/api/dash/error"))).toBe(false);
  });

  // An origin says what it is, with the place beside it.
  it("names code without a web address", async () => {
    start("/dashboard/?site=lens.example.com");
    const fx = fixtureFetch();
    setFetcher(async (u, i) => {
      const r = await fx(u, i);
      if (!u.startsWith("/api/dash/error?")) return r;
      const d = await r.json();
      d.origins = ["named"];
      d.places.rows[0].where = {
        origin: "named",
        host: "",
        place: "app.js:3:7",
      };
      return new Response(JSON.stringify(d));
    });
    const errors = await screen.findByRole("region", { name: "Errors" });
    fireEvent.click(await within(errors).findByText(/^TypeError/));
    const d = await screen.findByRole("region", { name: "Error" });
    expect(await within(d).findByText("app.js:3:7")).toBeInTheDocument();
    expect(within(d).getByRole("term")).toHaveTextContent(
      "Code without a web address",
    );
  });

  it("opens the detail from the full list, which closes", async () => {
    start("/dashboard/?site=lens.example.com");
    const fx = fixtureFetch();
    // More errors than the panel lists.
    setFetcher(async (u, i) => {
      const r = await fx(u, i);
      if (!u.startsWith("/api/dash/list?") || !u.includes("dim=errors"))
        return r;
      return new Response(JSON.stringify({ ...(await r.json()), total: 9 }));
    });
    const errors = await screen.findByRole("region", { name: "Errors" });
    fireEvent.click(
      await within(errors).findByRole("button", { name: "Show all 9 →" }),
    );
    const all = await screen.findByRole("dialog", { name: "Errors" });
    fireEvent.click(await within(all).findByText(/^Unhandled rejection/));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(
      await screen.findByRole("region", { name: "Error" }),
    ).toHaveTextContent("Unhandled rejection: AbortError");
  });

  it("says in the log where an error came from", async () => {
    start("/dashboard/log?site=lens.example.com&kind=js_error");
    const log = await screen.findByRole("region", { name: "Log" });
    const line = (
      await within(log).findAllByText(/^Details withheld.*on \/AS13335$/)
    )[0];
    expect(line).toHaveAttribute("title", expect.stringContaining("hides"));
  });
});

describe("dashboard on a domain without a server feed", () => {
  it("drops everything only the feed can know", async () => {
    start("/dashboard/?site=console.example.com");
    expect(
      await screen.findByText("2 findings against the prior 7 d"),
    ).toBeInTheDocument();
    const heads = screen
      .getAllByRole("heading", { level: 2 })
      .map((h) => h.textContent);
    expect(heads).not.toContain("Traffic");
    expect(screen.queryByText("Who is asking")).toBeNull();
    // Errors sit beside Noteworthy, JS errors only.
    const errors = screen.getByRole("region", { name: "Errors" });
    expect(
      await within(errors).findByText("JavaScript errors"),
    ).toBeInTheDocument();
    await within(errors).findByText(/Unhandled rejection/);
    expect(within(errors).queryByText("500 Internal Server Error")).toBeNull();
    // No source labels with one source, no server tiles or columns.
    expect(screen.queryByText("server")).toBeNull();
    const ov = screen.getByRole("region", { name: "Overview" });
    expect(within(ov).queryByText("Requests")).toBeNull();
    const v = screen.getByRole("region", { name: "Visitors by address" });
    expect(
      within(v).getByRole("button", { name: "Most views" }),
    ).toBeInTheDocument();
    expect(within(v).queryByText("Refused")).toBeNull();
    expect(screen.queryByRole("link", { name: "Traffic" })).toBeNull();
    const d = screen.getByRole("region", { name: "This domain" });
    expect(await within(d).findByText(/not connected/)).toBeInTheDocument();
    await within(v).findAllByRole("row");
    // Nothing only the feed can answer was asked for.
    const serverOnly = calls.filter(
      (c) =>
        c.startsWith("/api/dash/classes") ||
        c.startsWith("/api/dash/status") ||
        /[?&]dim=(probed|refused)\b/.test(c),
    );
    expect(serverOnly).toEqual([]);
    expect(calls.some((c) => c.startsWith("/api/dash/list"))).toBe(true);
    fireEvent.change(screen.getByRole("combobox", { name: "Domain" }), {
      target: { value: "lens.example.com" },
    });
    expect(window.location.search).toContain("site=lens.example.com");
    expect(await screen.findByText("Who is asking")).toBeInTheDocument();
  });

  it("shows empty states for a domain without data", async () => {
    window.history.replaceState(null, "", "/dashboard/?site=blog.example.org");
    const fx = fixtureFetch();
    const empty = {
      rows: [],
      other: { count: 0, value: 0 },
      total: 0,
      server: false,
    };
    setFetcher((u, i) =>
      u.startsWith("/api/dash/list")
        ? Promise.resolve(new Response(JSON.stringify(empty)))
        : u.startsWith("/api/dash/visitors")
          ? Promise.resolve(
              new Response(JSON.stringify({ rows: [], total: 0, next: "" })),
            )
          : fx(u, i),
    );
    render(<App />);
    expect(
      await screen.findByText("No page views in this range yet."),
    ).toBeInTheDocument();
    expect(
      screen.getByText("No visitors in this range yet."),
    ).toBeInTheDocument();
    expect(
      screen.getByText("No visitors with a known country in this range."),
    ).toBeInTheDocument();
    // Its feed is configured but has not delivered: no server panels yet,
    // and the footer says the feed is waiting, not missing.
    expect(screen.queryByText("Who is asking")).toBeNull();
    const footer = screen.getByRole("region", { name: "This domain" });
    expect(
      await within(footer).findByText(/configured, no events yet/),
    ).toBeInTheDocument();
    expect(within(footer).queryByText(/not connected/)).toBeNull();
  });
});

describe("log page", () => {
  it("filters, pages back and keeps domain and range", async () => {
    start("/dashboard/log?site=lens.example.com&range=30d");
    const log = await screen.findByRole("region", { name: "Log" });
    await within(log).findByText("100 of 250 events in this range");
    expect(
      within(log).getAllByRole("button", { name: "What is LCP?" }).length,
    ).toBeGreaterThan(0);
    expect(screen.queryByRole("group", { name: "Audience" })).toBeNull();
    expect(screen.queryByRole("group", { name: "Compare" })).toBeNull();
    expect(lastCall("/api/dash/log").get("range")).toBe("30d");
    fireEvent.click(
      within(log).getByRole("button", { name: "Load earlier →" }),
    );
    await within(log).findByText("200 of 250 events in this range");
    fireEvent.change(within(log).getByRole("combobox", { name: "Kind" }), {
      target: { value: "request" },
    });
    await waitFor(() =>
      expect(lastCall("/api/dash/log").get("kind")).toBe("request"),
    );
    expect(window.location.search).toContain("kind=request");
    fireEvent.change(within(log).getByRole("combobox", { name: "Status" }), {
      target: { value: "4xx" },
    });
    await within(log).findByText(/of 36 events/);
    fireEvent.change(within(log).getByRole("combobox", { name: "Class" }), {
      target: { value: "unknown" },
    });
    fireEvent.change(
      within(log).getByRole("textbox", { name: "Path or event name" }),
      {
        target: { value: "/api" },
      },
    );
    await waitFor(
      () => expect(lastCall("/api/dash/log").get("path")).toBe("/api"),
      { timeout: 2000 },
    );
    fireEvent.click(within(log).getByRole("button", { name: "Clear filters" }));
    await within(log).findByText("100 of 250 events in this range");
    fireEvent.click(
      within(log).getAllByRole("button", { name: "192.0.2.120" })[0],
    );
    await waitFor(() =>
      expect(lastCall("/api/dash/log").get("addr")).toBe("192.0.2.120"),
    );
    expect(
      within(log).getByRole("textbox", { name: "Address or /64" }),
    ).toHaveValue("192.0.2.120");
    fireEvent.change(
      within(log).getByRole("textbox", { name: "Address or /64" }),
      {
        target: { value: "203.0.113.99" },
      },
    );
    expect(
      await within(log).findByText(
        "No event in this range matches the filter.",
        {},
        { timeout: 2000 },
      ),
    ).toBeInTheDocument();
  });

  it("shows a network filter set by a link and drops it on its own", async () => {
    const first = start(
      "/dashboard/log?site=lens.example.com&asn=AS64510&kind=request",
    );
    const log = await screen.findByRole("region", { name: "Log" });
    await within(log).findByText("36 of 36 events in this range");
    expect(lastCall("/api/dash/log").get("asn")).toBe("AS64510");
    expect(within(log).getByText(/Network AS64510/)).toBeInTheDocument();
    expect(
      within(log).getAllByRole("button", { name: "198.51.100.7" }),
    ).toHaveLength(36);
    fireEvent.click(
      within(log).getByRole("button", { name: "Clear the network filter" }),
    );
    await within(log).findByText("72 of 72 events in this range");
    expect(lastCall("/api/dash/log").get("asn")).toBeNull();
    expect(new URLSearchParams(window.location.search).get("kind")).toBe(
      "request",
    );
    expect(within(log).queryByText(/Network AS/)).toBeNull();
    first.unmount();
    // A malformed one is the backend's 400, shown as such.
    start("/dashboard/log?site=lens.example.com&asn=cloud");
    const bad = await screen.findByRole("region", { name: "Log" });
    expect(await within(bad).findByRole("alert")).toHaveTextContent(
      "bad log filter",
    );
  });

  it("goes back to a dashboard section from the menu", async () => {
    start("/dashboard/log?site=lens.example.com");
    await screen.findByRole("region", { name: "Log" });
    fireEvent.click(screen.getByRole("link", { name: "Visitors" }));
    await waitFor(() => expect(window.location.pathname).toBe("/dashboard/"));
    expect(
      await screen.findByRole("region", { name: "Visitors by address" }),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("link", { name: "Log" }));
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    fireEvent.click(screen.getByRole("link", { name: "Domain" }));
    await waitFor(() => expect(window.location.pathname).toBe("/dashboard/"));
  });

  describe("text filters typed within the pause", () => {
    const pause = () => new Promise((r) => setTimeout(r, 500));
    const box = (log: HTMLElement, name: string) =>
      within(log).getByRole("textbox", { name });

    it("keeps each of them", async () => {
      start("/dashboard/log?site=lens.example.com");
      const log = await screen.findByRole("region", { name: "Log" });
      await within(log).findByText("100 of 250 events in this range");
      fireEvent.change(box(log, "Address or /64"), {
        target: { value: "192.0.2.120" },
      });
      fireEvent.change(box(log, "Path or event name"), {
        target: { value: "/api" },
      });
      // However long the first filter's render takes.
      await waitFor(
        () => {
          const q = lastCall("/api/dash/log");
          expect([q.get("addr"), q.get("path")]).toEqual([
            "192.0.2.120",
            "/api",
          ]);
        },
        { timeout: 4000 },
      );
      const url = new URLSearchParams(window.location.search);
      expect([url.get("addr"), url.get("path")]).toEqual([
        "192.0.2.120",
        "/api",
      ]);
    });

    it("keep what was typed, sending it trimmed", async () => {
      start("/dashboard/log?site=lens.example.com");
      const log = await screen.findByRole("region", { name: "Log" });
      await within(log).findByText(/events in this range/);
      fireEvent.change(box(log, "Path or event name"), {
        target: { value: "sign " },
      });
      await waitFor(
        () => expect(lastCall("/api/dash/log").get("path")).toBe("sign"),
        { timeout: 4000 },
      );
      // The space just typed stays in the field.
      expect(box(log, "Path or event name")).toHaveValue("sign ");
      fireEvent.change(box(log, "Path or event name"), {
        target: { value: "sign up" },
      });
      await waitFor(
        () => expect(lastCall("/api/dash/log").get("path")).toBe("sign up"),
        { timeout: 4000 },
      );
      expect(box(log, "Path or event name")).toHaveValue("sign up");
    });

    it("are dropped when the filters are cleared", async () => {
      start("/dashboard/log?site=lens.example.com&path=/x");
      const log = await screen.findByRole("region", { name: "Log" });
      await within(log).findByText(/events in this range/);
      fireEvent.change(box(log, "Address or /64"), {
        target: { value: "203.0.113.99" },
      });
      fireEvent.click(
        within(log).getByRole("button", { name: "Clear filters" }),
      );
      await pause();
      expect(window.location.search).not.toMatch(/addr|path/);
      expect(lastCall("/api/dash/log").get("addr")).toBeNull();
      expect(box(log, "Address or /64")).toHaveValue("");
    });

    it("never pull the visitor back into the log", async () => {
      start("/dashboard/log?site=lens.example.com");
      const log = await screen.findByRole("region", { name: "Log" });
      await within(log).findByText("100 of 250 events in this range");
      fireEvent.change(box(log, "Path or event name"), {
        target: { value: "/api" },
      });
      fireEvent.click(screen.getByRole("link", { name: "Visitors" }));
      await pause();
      expect(window.location.pathname).toBe("/dashboard/");
      expect(screen.queryByRole("region", { name: "Log" })).toBeNull();
    });
  });

  it("shows the source column only on a site with both sources", async () => {
    for (const [site, column, kind] of [
      ["lens.example.com", true, "request"],
      ["api.example.com", false, "request"],
      ["blog.example.org", false, null],
    ] as const) {
      const { unmount } = start(`/dashboard/log?site=${site}`);
      const log = await screen.findByRole("region", { name: "Log" });
      await within(log).findAllByText(/events in this range/);
      if (kind) await within(log).findAllByText(kind);
      expect(within(log).queryByText("Source") != null).toBe(column);
      unmount();
    }
    // The footer says which source never delivered.
    start("/dashboard/?site=api.example.com");
    const footer = await screen.findByRole("region", { name: "This domain" });
    expect(
      await within(footer).findByText(/no events yet/),
    ).toBeInTheDocument();
  });

  it("hides server kinds and the source column without a feed", async () => {
    start("/dashboard/log?site=console.example.com");
    const log = await screen.findByRole("region", { name: "Log" });
    await within(log).findAllByText("page_view");
    expect(within(log).queryByRole("combobox", { name: "Status" })).toBeNull();
    expect(within(log).queryByText("Source")).toBeNull();
    expect(within(log).queryByRole("option", { name: "request" })).toBeNull();
  });
});

describe("alerts page", () => {
  const alerts = () => screen.findByRole("region", { name: "Alerts" });
  const rows = (p: HTMLElement) => within(p).getAllByRole("row").slice(1);
  const rowOf = (p: HTMLElement, text: string) =>
    within(p).getByText(text).closest("tr")!;

  it("opens from the menu, on the domain, and goes back", async () => {
    start("/dashboard/?site=lens.example.com&range=30d");
    await screen.findByRole("region", { name: "Overview" });
    fireEvent.click(screen.getByRole("link", { name: "Alerts" }));
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/alerts"),
    );
    expect(window.location.search).toBe("?site=lens.example.com&range=30d");
    const p = await alerts();
    await within(p).findByText("50 alerts shown");
    expect(screen.getByRole("link", { name: "Alerts" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(document.title).toBe(`Trakk${SEP}lens.example.com${SEP}Alerts`);
    // No range, audience or comparison: the history spans none.
    for (const g of ["Range", "Audience", "Compare"])
      expect(screen.queryByRole("group", { name: g })).toBeNull();
    const q = lastCall("/api/dash/alerts");
    expect(Object.fromEntries(q)).toEqual({ site: "lens.example.com" });
    // The domain's own: no site column, no instance alert.
    expect(within(p).queryByText("Site")).toBeNull();
    expect(within(p).queryByText("Instance")).toBeNull();
    fireEvent.click(screen.getByRole("link", { name: "Log" }));
    await screen.findByRole("region", { name: "Log" });
    act(() => {
      window.history.back();
    });
    await alerts();
    expect(window.location.pathname).toBe("/dashboard/alerts");
    fireEvent.click(screen.getByRole("link", { name: "Visitors" }));
    await waitFor(() => expect(window.location.pathname).toBe("/dashboard/"));
    expect(new URLSearchParams(window.location.search).get("range")).toBe(
      "30d",
    );
    expect(screen.queryByRole("region", { name: "Alerts" })).toBeNull();
  });

  it("opens every site's and the instance's from a link, and the domain's again", async () => {
    start("/dashboard/alerts?site=lens.example.com&all=1");
    const p = await alerts();
    await within(p).findByText("50 alerts shown");
    expect(lastCall("/api/dash/alerts").get("site")).toBeNull();
    expect(document.title).toBe(`Trakk${SEP}Alerts`);
    expect(within(p).getByText("Site")).toBeInTheDocument();
    expect(within(p).getAllByText("Instance").length).toBeGreaterThan(0);
    expect(within(p).getByText("console.example.com")).toBeInTheDocument();
    // Every kind by its label, an unknown one by its id.
    for (const k of [
      "New error",
      "Address spike",
      "Unknown site or wrong key",
      "Failed calls level",
      "Network spike",
      "New probed path",
      "Source silent",
      "Background job failed",
      "New referrer",
      "Visitors trend",
      "Model budget",
      "Database size cap",
      "New endpoint",
      "New failure reason",
      "Calls trend",
      "Failed calls trend",
      "Unknown automated trend",
      "Events refused",
      "Events dropped",
      "95th percentile level",
      "Requests level",
      "Visitors level",
      "country.new",
    ])
      expect(within(p).getAllByText(k).length).toBeGreaterThan(0);
    expect(within(p).getByText("country.new")).toHaveAttribute(
      "title",
      "country.new",
    );
    // Severity, state and delivery.
    for (const w of ["high", "normal", "low", "open", "cleared", "one-off"])
      expect(within(p).getAllByText(w).length).toBeGreaterThan(0);
    for (const d of ["sent", "partly sent", "failed", "pending", "not sent"])
      expect(within(p).getAllByText(d).length).toBeGreaterThan(0);
    // Each channel with how it went, a failed one marked.
    const partial = rowOf(p, 'New error: TypeError: cannot read "prefixes"');
    expect(partial.textContent).toContain(
      `ops ok${SEP}mail ok${SEP}pager failed`,
    );
    expect(within(partial).getByText("pager")).toHaveClass("text-danger-ink");
    expect(partial).toHaveTextContent("Safari on /AS:n pages");
    // A figure to its tenth, its unit beside it.
    const failed = rowOf(p, "Failed calls over 5 % in the last hour");
    expect(failed).toHaveTextContent("11.1%");
    // Plain text, never markup.
    expect(partial.querySelector("b, a, img")).toBeNull();
    fireEvent.click(
      within(within(p).getByRole("group", { name: "Alerts of" })).getByRole(
        "button",
        { name: "lens.example.com" },
      ),
    );
    await waitFor(() =>
      expect(lastCall("/api/dash/alerts").get("site")).toBe("lens.example.com"),
    );
    expect(window.location.search).toBe("?site=lens.example.com&range=7d");
    await waitFor(() => expect(within(p).queryByText("Instance")).toBeNull());
    fireEvent.click(within(p).getByRole("button", { name: "All sites" }));
    await waitFor(() => expect(window.location.search).toContain("all=1"));
  });

  it("follows the domain picked", async () => {
    start("/dashboard/alerts?site=lens.example.com");
    const p = await alerts();
    await within(p).findByText("50 alerts shown");
    fireEvent.change(screen.getByRole("combobox", { name: "Domain" }), {
      target: { value: "api.example.com" },
    });
    await within(p).findByText("6 alerts shown");
    expect(lastCall("/api/dash/alerts").get("site")).toBe("api.example.com");
    expect(window.location.pathname).toBe("/dashboard/alerts");
    expect(
      within(p).queryByRole("button", { name: /Load earlier/ }),
    ).toBeNull();
  });

  it("pages back to the first alert", async () => {
    start("/dashboard/alerts?site=lens.example.com");
    const p = await alerts();
    await within(p).findByText("50 alerts shown");
    fireEvent.click(within(p).getByRole("button", { name: "Load earlier →" }));
    await within(p).findByText("58 alerts shown");
    expect(lastCall("/api/dash/alerts").get("before")).toBe("350");
    // The last page: nothing further, no page twice.
    expect(
      within(p).queryByRole("button", { name: /Load earlier/ }),
    ).toBeNull();
    expect(rows(p)).toHaveLength(58);
  });

  it("jumps from a row where it happened, on its own domain", async () => {
    const first = start("/dashboard/alerts?site=lens.example.com&all=1");
    let p = await alerts();
    await within(p).findByText("50 alerts shown");
    // No target, no jump.
    const job = rowOf(p, "The daily aggregate build failed");
    expect(job).not.toHaveClass("click");
    expect(within(job).queryByRole("button")).toBeNull();
    // Another domain's: the log there, with that domain's range.
    localStorage.setItem("app.range.api.example.com", "90d");
    fireEvent.click(rowOf(p, "New failure reason: invalid_params"));
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    const q = new URLSearchParams(window.location.search);
    expect([q.get("site"), q.get("range"), q.get("failure")]).toEqual([
      "api.example.com",
      "90d",
      "invalid_params",
    ]);
    await waitFor(() =>
      expect(lastCall("/api/dash/log").get("site")).toBe("api.example.com"),
    );
    first.unmount();

    // The domain's own: a network into the log, an address into its detail.
    start("/dashboard/alerts?site=lens.example.com");
    p = await alerts();
    await within(p).findByText("50 alerts shown");
    fireEvent.click(rowOf(p, "AS64510 Example Cloud"));
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    expect(new URLSearchParams(window.location.search).get("asn")).toBe(
      "64510",
    );
    fireEvent.click(screen.getByRole("link", { name: "Alerts" }));
    p = await alerts();
    await within(p).findByText("50 alerts shown");
    // The keyboard's way in: the row's first cell.
    const spike = rowOf(p, "198.51.100.7 (AS64510 Example Cloud)");
    fireEvent.click(within(spike).getByRole("button"));
    const detail = await screen.findByRole("region", { name: "Visitor" });
    expect(
      await within(detail).findByText("Scraping attempt"),
    ).toBeInTheDocument();
    expect(window.location.pathname).toBe("/dashboard/");
  });

  it("jumps from another domain's row to its panel and visitor", async () => {
    start("/dashboard/alerts?site=console.example.com&all=1");
    const p = await alerts();
    await within(p).findByText("50 alerts shown");
    fireEvent.click(rowOf(p, "198.51.100.7 (AS64510 Example Cloud)"));
    await waitFor(() => expect(window.location.pathname).toBe("/dashboard/"));
    expect(new URLSearchParams(window.location.search).get("site")).toBe(
      "lens.example.com",
    );
    const detail = await screen.findByRole("region", { name: "Visitor" });
    expect(
      await within(detail).findByText("Scraping attempt"),
    ).toBeInTheDocument();
  });

  it("says when there are none, and when they failed to load", async () => {
    const first = start("/dashboard/alerts?site=blog.example.org");
    const p = await alerts();
    expect(
      await within(p).findByText(
        "Nothing has raised an alert on this domain yet.",
      ),
    ).toBeInTheDocument();
    expect(within(p).queryByRole("table")).toBeNull();
    expect(
      within(p).queryByRole("button", { name: /Load earlier/ }),
    ).toBeNull();
    first.unmount();
    start("/dashboard/alerts?site=lens.example.com", {
      fail: ["/api/dash/alerts"],
    });
    expect(await within(await alerts()).findByRole("alert")).toHaveTextContent(
      "fixture failure",
    );
  });
});

describe("the footer's notifications", () => {
  const block = async (opt: FixtureOptions = {}) => {
    start("/dashboard/?site=lens.example.com", opt);
    const d = await screen.findByRole("region", { name: "This domain" });
    await within(d).findByText("Rate limit");
    return within(d).getByRole("region", { name: "Notifications" });
  };

  it("shows faults, rules and each channel, warning only of a failing one", async () => {
    const n = await block();
    expect(within(n).getByText("Alerts").nextSibling).toHaveTextContent(/^on$/);
    expect(
      within(n).getByText(
        'rule "api-latency": unknown channel "sms", the rule is off',
      ),
    ).toHaveClass("wrap-anywhere");
    expect(
      within(n)
        .getByText(/unknown channel/)
        .closest("ul"),
    ).toHaveClass("text-ochre");
    expect(
      within(n).getByText(`9 active${SEP}1 off`, asIs),
    ).toBeInTheDocument();
    for (const c of ["ops", "mail", "pager"])
      expect(within(n).getByText(c)).toBeInTheDocument();
    expect(
      within(n).getByText(`ntfy${SEP}every alert`, asIs),
    ).toBeInTheDocument();
    expect(
      within(n).getByText(`email${SEP}normal and high`, asIs),
    ).toBeInTheDocument();
    expect(
      within(n).getByText(`pushover${SEP}high only`, asIs),
    ).toBeInTheDocument();
    // The pager's error came after its last delivery, the mail's before.
    expect(within(n).getByText(/invalid application token/)).toHaveClass(
      "text-ochre",
    );
    expect(within(n).getByText(/SMTP 421/)).toHaveClass("text-muted");
    expect(within(n).getAllByText(/^last delivered /)).toHaveLength(3);
    expect(n.textContent).not.toMatch(/https?:|\/\//);
  });

  it("says it is switched off", async () => {
    const n = await block({ notify: "off" });
    expect(n).toHaveTextContent("switched off");
    expect(within(n).getByText("pager")).toBeInTheDocument();
  });

  it("says it is not configured, also by a backend without alerts", async () => {
    for (const notify of ["unconfigured", "missing"] as const) {
      const n = await block({ notify });
      expect(n).toHaveTextContent("not configured");
      expect(within(n).queryByText("Rules")).toBeNull();
      expect(within(n).queryByText("Channels")).toBeNull();
      cleanup();
    }
  });

  it("lists faults of a configuration it could not take", async () => {
    window.history.replaceState(null, "", "/dashboard/?site=lens.example.com");
    const fx = fixtureFetch();
    setFetcher(async (u, i) => {
      const res = await fx(u, i);
      if (!u.startsWith("/api/dash/domain?")) return res;
      const d = await res.json();
      return new Response(
        JSON.stringify({
          ...d,
          notify: {
            configured: false,
            enabled: false,
            faults: ['line 4: unknown service "sms"'],
            rules: { active: 0, off: 0 },
            // A Go nil slice.
            channels: null,
          },
        }),
      );
    });
    render(<App />);
    const d = await screen.findByRole("region", { name: "This domain" });
    const n = await within(d).findByRole("region", { name: "Notifications" });
    expect(n).toHaveTextContent("not configured");
    expect(
      within(n)
        .getByText(/unknown service/)
        .closest("ul"),
    ).toHaveClass("text-ochre");
  });

  it("says when no channel is set up", async () => {
    const n = await block({ notify: "bare" });
    expect(within(n).getByText("Channels").nextSibling).toHaveTextContent(
      "none",
    );
    expect(within(n).queryByText("Config faults")).toBeNull();
    expect(
      within(n).getByText(`3 active${SEP}0 off`, asIs),
    ).toBeInTheDocument();
  });
});

describe("an API", () => {
  it("shows calls instead of visits, and nothing only a browser reports", async () => {
    start("/dashboard/?site=api.example.com&aud=humans");
    expect(
      await screen.findByRole("button", { name: /^Calls/ }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(
      screen.queryByRole("button", { name: /^Unique visitors/ }),
    ).toBeNull();
    for (const name of [
      "Endpoints",
      "Accounts",
      "Failures",
      "Clients",
      "Networks",
    ])
      expect(panel(name)).toBeInTheDocument();
    for (const name of ["Pages", "Events", "Referrers"])
      expect(screen.queryByRole("region", { name })).toBeNull();
    expect(screen.getByRole("link", { name: "API" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Content" })).toBeNull();
    const row = (
      await within(panel("Endpoints")).findByText("POST /mcp search")
    ).closest("tr")!;
    expect(row).toHaveTextContent("210 ms");
    expect(row).toHaveTextContent("1'480 ms");
    expect(row).toHaveTextContent("11");
    // Its callers are programs: no audience to pick, the remembered one
    // ignored, and where they are counts addresses.
    expect(screen.queryByRole("group", { name: "Audience" })).toBeNull();
    expect(lastCall("/api/dash/overview").get("aud")).toBe("all");
    expect(panel("Where callers are")).toBeInTheDocument();
    expect(calls.find((c) => c.includes("dim=countries"))).toContain(
      "aud=bots",
    );
    expect(calls.find((c) => c.includes("dim=networks"))).toContain("aud=all");
    expect(
      await within(panel("Clients")).findByText("lens-cli/2.0.1"),
    ).toBeInTheDocument();
    expect(calls.some((c) => c.startsWith("/api/dash/clients"))).toBe(false);
    for (const dim of ["endpoints", "accounts", "failures", "agents"])
      expect(calls.find((c) => c.includes(`dim=${dim}`))).toContain("aud=all");
  });

  it.each([
    ["Endpoints", "POST /mcp whois", "endpoint", "POST /mcp whois"],
    ["Accounts", "a91k2m4p", "account", "a91k2m4p"],
    ["Failures", "HTTP 429", "failure", "HTTP 429"],
  ])("opens the log on a row of %s", async (title, text, key, value) => {
    start("/dashboard/?site=api.example.com");
    await screen.findByRole("button", { name: /^Calls/ });
    fireEvent.click(await within(panel(title)).findByText(text));
    const log = await screen.findByRole("region", { name: "Log" });
    expect(lastCall("/api/dash/log").get(key)).toBe(value);
    expect(new URLSearchParams(window.location.search).get(key)).toBe(value);
    // It can be seen and dropped on its own.
    fireEvent.click(
      within(log).getByRole("button", { name: `Clear the ${key} filter` }),
    );
    await waitFor(() => expect(lastCall("/api/dash/log").get(key)).toBeNull());
  });

  it("opens an account's calls, not its every request", async () => {
    start("/dashboard/?site=api.example.com");
    await screen.findByRole("button", { name: /^Calls/ });
    fireEvent.click(await within(panel("Accounts")).findByText("a91k2m4p"));
    const log = await screen.findByRole("region", { name: "Log" });
    expect(lastCall("/api/dash/log").get("calls")).toBe("1");
    fireEvent.click(
      within(log).getByRole("button", { name: "Clear the calls filter" }),
    );
    await waitFor(() =>
      expect(lastCall("/api/dash/log").get("calls")).toBeNull(),
    );
    expect(lastCall("/api/dash/log").get("account")).toBe("a91k2m4p");
  });

  // Answers with what the backend sends changed by edit.
  const startEdited = (
    url: string,
    path: string,
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    edit: (body: any) => void,
  ) => {
    window.history.replaceState(null, "", url);
    calls = [];
    const fx = fixtureFetch();
    setFetcher(async (u, i) => {
      calls.push(u);
      const r = await fx(u, i);
      if (!u.startsWith(path)) return r;
      const body = await r.json();
      edit(body);
      return new Response(JSON.stringify(body), { status: r.status });
    });
    return render(<App />);
  };

  it("is shown as one before its feed delivered", async () => {
    startEdited(
      "/dashboard/?site=api.example.com&aud=humans",
      "/api/dash/sites",
      (b) => {
        for (const s of b.sites) s.server = null;
      },
    );
    await screen.findByRole("button", { name: /^Calls/ });
    expect(panel("Endpoints")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Pages" })).toBeNull();
    expect(lastCall("/api/dash/overview").get("aud")).toBe("all");
  });

  it("says where the figures begin, and shows no figure as none", async () => {
    startEdited(
      "/dashboard/?site=api.example.com",
      "/api/dash/overview",
      (b) => {
        b.period.since = "2026-11-19T23:00:00Z";
        const t = b.tiles.find((x: { key: string }) => x.key === "p95");
        Object.assign(t, { value: null, prev: null, change: null });
        t.series = t.series.map((v: number, i: number) => (i < 3 ? null : v));
      },
    );
    const tile = await screen.findByRole("button", {
      name: /^95th percentile/,
    });
    expect(tile).toHaveTextContent("–");
    expect(tile).not.toHaveTextContent("ms");
    // Its change's line is kept empty: level with its neighbours.
    expect(tile.querySelector(".tile-delta")).toBeEmptyDOMElement();
    fireEvent.click(tile);
    expect(screen.getByText(/figures since/)).toHaveTextContent("2026");
    // The largest on the chart has the unit, and its time.
    expect(screen.getByTestId("peak").textContent).toMatch(
      new RegExp(`ms${SEP}\\w{3} \\d\\d\\.11\\.`),
    );
  });

  it("draws a quiet tile on the low frame, the others on theirs", async () => {
    startEdited(
      "/dashboard/?site=api.example.com&range=24h",
      "/api/dash/overview",
      (b) => {
        b.period.low = {
          slot: "30 min",
          mean: "3 h",
          labels: ["13:50–14:20", "14:20–14:50"],
          ticks: [],
          weekends: [],
          open: false,
          first: null,
        };
        const t = b.tiles.find((x: { key: string }) => x.key === "accounts");
        Object.assign(t, {
          low: true,
          series: [2, 3],
          mean: [2, 2.5],
          prev_mean: [1, 1],
          delta: [100, 150],
        });
      },
    );
    const ov = await screen.findByRole("region", { name: "Overview" });
    await within(ov).findByText(`Calls per 10 min${SEP}all`, asIs);
    // Two grids: the bar names neither, the chart's heading its own.
    expect(screen.queryByText(/one point per/)).toBeNull();
    expect(screen.getByText(/compared with the/)).toBeInTheDocument();
    const hit = () => within(ov).getByTestId("chart-hit");
    fireEvent.pointerMove(hit(), { clientX: 5000, clientY: 100 });
    expect(screen.getByRole("tooltip").textContent).toMatch(/^14:10–14:20/);
    fireEvent.pointerLeave(hit());
    fireEvent.click(within(ov).getByRole("button", { name: /^Accounts/ }));
    expect(
      within(ov).getByText(`Accounts per 30 min${SEP}all`, asIs),
    ).toBeInTheDocument();
    expect(within(ov).getByText("3 h average")).toBeInTheDocument();
    fireEvent.pointerMove(hit(), { clientX: 5000, clientY: 100 });
    expect(screen.getByRole("tooltip").textContent).toMatch(
      /^14:20–14:50Accounts33 h average2\.5/,
    );
  });

  it("draws a series broken where it has no figure", () => {
    const { container } = render(
      <MainChart
        {...CHART}
        labels={["a", "b", "c", "d", "e"]}
        cur={[null, 4, null, 6, 8]}
        mean={[null, 4, null, 5, 7]}
        prev={[1, null, null, 2, 3]}
        kind="sum"
        unit="ms"
      />,
    );
    for (const id of ["slots", "mean"])
      expect(screen.getByTestId(id).getAttribute("d")).toMatch(
        /^M\S+ \S+h0M\S+ \S+L\S+ \S+$/,
      );
    expect(container.innerHTML).not.toMatch(/NaN|null/);
    // Nothing at all.
    for (const kind of ["sum", "rate"] as const) {
      const none = render(
        <MainChart
          {...CHART}
          labels={["a", "b"]}
          cur={[null, null]}
          mean={[null, null]}
          prev={null}
          kind={kind}
        />,
      );
      expect(none.container).toHaveTextContent("No data in this range yet.");
    }
    // The previous period's line stands without this one's.
    const before = render(
      <MainChart
        {...CHART}
        labels={["a", "b"]}
        cur={[null, null]}
        mean={[null, null]}
        prev={[3, 4]}
        kind="rate"
      />,
    );
    expect(within(before.container).getByTestId("prev")).toBeInTheDocument();
    expect(before.container.innerHTML).not.toMatch(/NaN|Infinity|null/);
  });

  it("shows a request's account and duration in the log", async () => {
    start("/dashboard/log?site=api.example.com");
    const log = await screen.findByRole("region", { name: "Log" });
    const acc = await within(log).findAllByRole("button", { name: "a91k2m4p" });
    expect(acc[0].closest("tr")).toHaveTextContent(/\d ms/);
    fireEvent.click(acc[0]);
    await waitFor(() =>
      expect(lastCall("/api/dash/log").get("account")).toBe("a91k2m4p"),
    );
  });

  it("gives a website with a feed its endpoints and failures, one without none", async () => {
    const { unmount } = start("/dashboard/?site=lens.example.com&aud=humans");
    await screen.findByRole("region", { name: "Endpoints" });
    expect(panel("Pages")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Accounts" })).toBeNull();
    const reason = await within(panel("Failures")).findByText("invalid_params");
    // Whoever fails, though the page's audience is people, and it says so.
    expect(calls.find((c) => c.includes("dim=failures"))).toContain("aud=all");
    expect(panel("Failures")).toHaveTextContent("every class of client");
    fireEvent.click(reason);
    await screen.findByRole("region", { name: "Log" });
    expect(lastCall("/api/dash/log").get("failure")).toBe("invalid_params");
    unmount();
    start("/dashboard/?site=console.example.com");
    await screen.findByRole("region", { name: "Pages" });
    for (const name of ["Endpoints", "Failures"])
      expect(screen.queryByRole("region", { name })).toBeNull();
    expect(calls.some((c) => /dim=(endpoints|failures)/.test(c))).toBe(false);
  });
});

describe("the class of API clients", () => {
  // The sites' answer as the backend sends it, the API clients' row changed.
  const startWith = (
    url: string,
    // null: the classes fail to load.
    api: { requests: number; change: { pct: number } | null } | null,
    // The visitors' table has an API client.
    rows = false,
  ) => {
    window.history.replaceState(null, "", url);
    calls = [];
    const fx = fixtureFetch();
    setFetcher(async (u, i) => {
      calls.push(u);
      const r = await fx(u, i);
      if (u.startsWith("/api/dash/visitors")) {
        const body = await r.json();
        body.rows = body.rows.filter(
          (v: { class: string }) => v.class !== "api",
        );
        if (rows) body.rows[0].class = "api";
        return new Response(JSON.stringify(body), { status: r.status });
      }
      if (!u.startsWith("/api/dash/classes")) return r;
      if (!api)
        return new Response(JSON.stringify({ detail: "down" }), {
          status: 500,
        });
      const body = await r.json();
      body.series.api = body.series.api.map(() => 0);
      for (const row of body.rows)
        if (row.class === "api")
          Object.assign(row, { share: 0, addresses: 0, ...api });
      return new Response(JSON.stringify(body), { status: r.status });
    });
    return render(<App />);
  };
  const named = (region: string) =>
    within(panel(region)).queryAllByText("API clients").length > 0;
  const loaded = async () => {
    const who = await screen.findByRole("region", { name: "Who is asking" });
    await within(who).findAllByText("Uptime probes");
  };

  it("is left out where there are none and were none", async () => {
    startWith("/dashboard/?site=lens.example.com&aud=all", {
      requests: 0,
      change: null,
    });
    await loaded();
    for (const r of [
      "Who is asking",
      "Requests by class",
      "Visitors by address",
    ])
      expect(named(r)).toBe(false);
    const probes = screen.getAllByText("Uptime probes").length;
    fireEvent.mouseMove(screen.getAllByTestId("stack-hit")[0]);
    expect(screen.getAllByText("Uptime probes")).toHaveLength(probes + 1);
    expect(screen.queryByText("API clients")).toBeNull();
    expect(screen.queryByText(/signed-in API clients/)).toBeNull();
    // The others say 0 when they have none.
    expect(
      within(panel("Who is asking")).getAllByText("Known bots").length,
    ).toBeGreaterThan(0);
  });

  it.each([
    ["some", "lens.example.com", { requests: 4, change: null }],
    [
      "a fall to none",
      "lens.example.com",
      { requests: 0, change: { pct: -100 } },
    ],
    ["an API", "api.example.com", { requests: 0, change: null }],
  ] as const)("is shown for %s", async (_, site, row) => {
    startWith(`/dashboard/?site=${site}&aud=all`, row);
    await loaded();
    for (const r of [
      "Who is asking",
      "Requests by class",
      "Visitors by address",
    ])
      expect(named(r)).toBe(true);
    expect(screen.getByText(/signed-in API clients/)).toBeInTheDocument();
  });

  it("stays a tab while picked, and is none without a feed", async () => {
    const tab = () =>
      within(panel("Visitors by address")).queryByRole("button", {
        name: "API clients",
      });
    const site = (s: string) =>
      fireEvent.change(screen.getByRole("combobox", { name: "Domain" }), {
        target: { value: s },
      });
    start("/dashboard/?site=console.example.com&aud=all");
    await screen.findByRole("region", { name: "Visitors by address" });
    expect(tab()).toBeNull();
    site("lens.example.com");
    await waitFor(() => expect(tab()).not.toBeNull());
    fireEvent.click(tab()!);
    site("console.example.com");
    await waitFor(() =>
      expect(
        screen.queryByRole("region", { name: "Who is asking" }),
      ).toBeNull(),
    );
    expect(tab()).toHaveAttribute("aria-pressed", "true");
  });

  it("stays a tab where the table has some, or the figures failed", async () => {
    const url = "/dashboard/?site=lens.example.com&aud=all";
    const tab = { name: "API clients" };
    const { unmount } = startWith(url, { requests: 0, change: null }, true);
    await loaded();
    expect(named("Who is asking")).toBe(false);
    await within(panel("Visitors by address")).findByRole("button", tab);
    unmount();
    startWith(url, null);
    const v = await screen.findByRole("region", {
      name: "Visitors by address",
    });
    await within(v).findByRole("button", tab);
  });

  it("is always offered by the log's filter", async () => {
    startWith("/dashboard/log?site=lens.example.com", {
      requests: 0,
      change: null,
    });
    const log = await screen.findByRole("region", { name: "Log" });
    expect(
      within(log).getByRole("option", { name: "API clients" }),
    ).toBeInTheDocument();
  });
});

describe("vital hint", () => {
  it("explains only a line the backend wrote for a vital", () => {
    const hint = (kind: string, what: string) => {
      const { container, unmount } = render(
        <VitalHint kind={kind} what={what} />,
      );
      const has = container.querySelector("button") !== null;
      unmount();
      return has;
    };
    expect(hint("vital", "vital LCP")).toBe(true);
    expect(hint("vital", "vital LCP 2.1 s")).toBe(true);
    expect(hint("vital", "vital INP 184 ms")).toBe(true);
    expect(hint("vital", "vital CLS 0.004")).toBe(true);
    expect(hint("vital", "vital LCP fake")).toBe(false);
    expect(hint("vital", "vital CLS 9 hacked")).toBe(false);
    expect(hint("vital", "vital constructor")).toBe(false);
    expect(hint("custom", "event LCP")).toBe(false);
  });
});

describe("separators", () => {
  // A separating dot with a plain space beside it, in the rendered text or in
  // a title or label that mirrors it: SEP's en spaces are the only spacing.
  const plainDots = () => {
    const texts = [document.body.textContent ?? ""];
    for (const el of document.querySelectorAll("[title], [aria-label]")) {
      texts.push(el.getAttribute("title") ?? "");
      texts.push(el.getAttribute("aria-label") ?? "");
    }
    return texts.flatMap((t) => t.match(/.{0,24}(?: ·|· ).{0,24}/g) ?? []);
  };

  it("spaces every dot with en spaces on every screen", async () => {
    const body = () => document.body.textContent ?? "";
    // The dashboard with a feed, no model and full lists cut at their
    // limit: every section and a visitor's detail.
    const first = start("/dashboard/", { beyond: 600, noModel: true });
    const footer = await screen.findByRole("region", { name: "This domain" });
    await within(footer).findByText("Rate limit");
    expect(footer.textContent).toContain(
      `no model configured${SEP}computed texts only`,
    );
    const pages = await screen.findByRole("region", { name: "Pages" });
    await within(pages).findByText("/AS:n");
    const v = panel("Visitors by address");
    fireEvent.click(await within(v).findByText("203.0.113.42"));
    const d = screen.getByRole("region", { name: "Visitor" });
    await within(d).findByText("Person");
    expect(body()).toContain(`Chrome${SEP}macOS`);
    expect(d.textContent).toMatch(new RegExp(`AS\\d+ [^·]+${SEP}ISP`));
    expect(d.textContent).toContain(
      `Script beacon${SEP}Engaged${SEP}Assets loaded${SEP}Also unknown`,
    );
    expect(plainDots()).toEqual([]);

    // Each chart's tooltip on its last point, still filling.
    const ov = panel("Overview");
    const hit = within(ov).getByTestId("chart-hit");
    fireEvent.pointerMove(hit, { clientX: 1e6, clientY: 100 });
    expect(screen.getByRole("tooltip").textContent).toContain(
      `${SEP}still filling`,
    );
    expect(plainDots()).toEqual([]);
    fireEvent.pointerLeave(hit);
    const t = screen.getByRole("region", { name: "Requests by class" });
    const bars = await within(t).findAllByTestId("stack-hit");
    fireEvent.mouseMove(bars[26], { clientX: 900, clientY: 100 });
    expect(screen.getByRole("tooltip").textContent).toContain(
      `${SEP}still filling`,
    );
    expect(plainDots()).toEqual([]);
    fireEvent.mouseLeave(bars[26]);

    // A full list cut at its limit.
    fireEvent.click(within(pages).getByRole("button", { name: /Show all 8/ }));
    const dlg = screen.getByRole("dialog");
    await within(dlg).findByText("/privacy");
    expect(dlg.textContent).toContain(`${SEP}the first 500 are shown`);
    expect(plainDots()).toEqual([]);
    fireEvent.keyDown(window, { key: "Escape" });

    // The filter bar without a comparison.
    fireEvent.click(screen.getByRole("button", { name: "off" }));
    await waitFor(() => expect(body()).toContain(`${SEP}no comparison`));
    expect(plainDots()).toEqual([]);
    first.unmount();

    // A domain whose script and feed never delivered, and one without a
    // feed: the footer's other source texts.
    const second = start("/dashboard/?site=blog.example.org");
    await screen.findByText(/configured, no events yet/);
    expect(body()).toContain(`no events yet${SEP}load the script`);
    expect(plainDots()).toEqual([]);
    second.unmount();
    const third = start("/dashboard/?site=console.example.com");
    await screen.findByText(/not connected/);
    expect(plainDots()).toEqual([]);
    third.unmount();

    // The log.
    const fourth = start("/dashboard/log?site=lens.example.com&range=30d");
    const log = await screen.findByRole("region", { name: "Log" });
    await within(log).findByText("100 of 250 events in this range");
    expect(log.textContent).toContain(SEP);
    expect(plainDots()).toEqual([]);
    fourth.unmount();

    // The alerts, every site's and the instance's.
    const fifth = start("/dashboard/alerts?site=lens.example.com&all=1");
    const al = await screen.findByRole("region", { name: "Alerts" });
    await within(al).findByText("50 alerts shown");
    expect(al.textContent).toContain(SEP);
    expect(plainDots()).toEqual([]);
    fifth.unmount();

    // The sign-in card.
    start("/dashboard/", { me: 401 });
    await screen.findByRole("link", { name: "Sign in" });
    expect(plainDots()).toEqual([]);
  });

  it("finds a client typed with plain spaces", async () => {
    start();
    const v = await screen.findByRole("region", {
      name: "Visitors by address",
    });
    await within(v).findByText("198.51.100.7");
    fireEvent.change(within(v).getByRole("textbox"), {
      target: { value: "chrome · macos" },
    });
    await waitFor(
      () => expect(within(v).queryByText("198.51.100.7")).toBeNull(),
      { timeout: 2000 },
    );
    expect(within(v).getByText("203.0.113.42")).toBeInTheDocument();
  });
});

describe("main chart", () => {
  it("labels a count's axis with whole numbers only", () => {
    const axis = (cur: number[], kind: "sum" | "rate") => {
      const { container, unmount } = render(
        <MainChart
          {...CHART}
          labels={cur.map((_, i) => `${i}`)}
          cur={cur}
          mean={cur}
          prev={null}
          kind={kind}
        />,
      );
      const texts = [...container.querySelectorAll("text[text-anchor=end]")];
      const out = texts.map((t) => t.textContent);
      unmount();
      return out;
    };
    // An empty range: 0 and 1, not a rounded 0.5 printed as a second 1.
    expect(axis([0, 0, 0], "sum")).toEqual(["0", "1"]);
    expect(axis([0, 4, 3], "sum")).toEqual(["0", "1", "2", "3", "4"]);
    expect(axis([0, 5, 3], "sum")).toEqual(["0", "2", "4", "6"]);
    // As close above the largest as round steps allow, not at 20'000.
    expect(axis([0, 11432], "sum")).toHaveLength(5);
    expect(scaleLines(11432, "sum")).toEqual([0, 3000, 6000, 9000, 12000]);
    expect(scaleLines(16, "sum")).toEqual([0, 4, 8, 12, 16]);
    // A share is never past all of it.
    expect(scaleLines(97, "rate")).toEqual([0, 25, 50, 75, 100]);
    expect(scaleLines(0.4, "rate")).toEqual([0, 0.1, 0.2, 0.3, 0.4]);
    // Every line of a share prints as its own label, to the tenth.
    for (const v of [0.004, 0.05, 0.3, 1, 2.3, 5, 9.7, 12, 33, 61, 100]) {
      const printed = scaleLines(v, "rate").map((x) => pct(x));
      expect(new Set(printed).size).toBe(printed.length);
      expect(scaleLines(v, "rate").at(-1)).toBeGreaterThanOrEqual(v);
      for (const x of scaleLines(v, "rate"))
        expect(Math.abs(x * 10 - Math.round(x * 10))).toBeLessThan(1e-9);
    }
    // The requests by class, stacked: the same for its counts.
    const zero = [0, 0];
    const { container } = render(
      <StackChart
        labels={["a", "b"]}
        series={{
          human: zero,
          api: zero,
          bot: zero,
          monitor: zero,
          unknown: zero,
        }}
        axis={CHART.axis}
        open={false}
        partial={[true, false]}
      />,
    );
    expect(
      [...container.querySelectorAll("text[text-anchor=end]")].map(
        (t) => t.textContent,
      ),
    ).toEqual(["0", "1"]);
    // A week the range cuts is no week that is filling.
    fireEvent.mouseMove(screen.getAllByTestId("stack-hit")[0]);
    expect(screen.getByRole("tooltip")).toHaveTextContent("part of a week");
  });

  it("says where the figures begin, and what is the weekend", () => {
    const { container } = render(
      <MainChart
        {...CHART}
        labels={["a", "b", "c", "d"]}
        cur={[null, null, 7, 3]}
        mean={[null, null, 7, 5]}
        prev={[]}
        kind="sum"
        first={2}
        axis={{
          ticks: [
            { at: 1, label: "Mon", line: true },
            { at: 2, label: "", line: true },
          ],
          weekends: [[0, 1]],
        }}
      />,
    );
    expect(screen.getByTestId("no-data")).toHaveTextContent("no data yet");
    expect(screen.getAllByTestId("weekend")).toHaveLength(1);
    expect(container.querySelectorAll("[stroke-dasharray='2 4']")).toHaveLength(
      2,
    );
    expect(screen.getByText("Mon")).toBeInTheDocument();
    expect(screen.getByTestId("peak").textContent).toBe(`7${SEP}c`);
    expect(screen.queryByTestId("prev")).toBeNull();
  });

  it("draws a count in bars, a share and a measure as a line", () => {
    const draw = (kind: "sum" | "rate", unit = "", open = false) => {
      const { container, unmount } = render(
        <MainChart
          {...CHART}
          labels={["a", "b", "c", "d"]}
          cur={[2, 0, null, 3]}
          mean={[1, 1, 1, 2]}
          prev={null}
          kind={kind}
          unit={unit}
          open={open}
        />,
      );
      const d = (id: string) =>
        container.querySelector(`[data-testid=${id}]`)?.getAttribute("d") ??
        null;
      const out = { slots: d("slots"), open: d("open-slot") };
      expect(container.innerHTML).not.toMatch(/NaN|Infinity|undefined/);
      unmount();
      return out;
    };
    // No bar of nothing, nor where there is no figure.
    expect(draw("sum").slots!.match(/z/g)).toHaveLength(2);
    expect(draw("sum").open).toBeNull();
    // The open slot apart, paler.
    const filling = draw("sum", "", true);
    expect(filling.slots!.match(/z/g)).toHaveLength(1);
    expect(filling.open!.match(/z/g)).toHaveLength(1);
    for (const [kind, unit] of [
      ["rate", "%"],
      ["sum", "ms"],
    ] as const) {
      expect(draw(kind, unit).slots).toMatch(/^M[^z]*$/);
      expect(draw(kind, unit, true).open).toBeNull();
    }
  });

  it("keeps a bar in its slot, however narrow", () => {
    const n = 2000;
    render(
      <MainChart
        {...CHART}
        labels={Array.from({ length: n }, (_, i) => `${i}`)}
        cur={Array.from({ length: n }, (_, i) => (i === 1 ? 0 : 5))}
        mean={Array.from({ length: n }, () => 5)}
        prev={null}
        kind="sum"
      />,
    );
    const xs = screen
      .getByTestId("slots")
      .getAttribute("d")!
      .split("z")
      .slice(0, 2)
      .map((b) => Number(b.match(/^M([\d.]+)/)![1]));
    const w = screen
      .getByTestId("slots")
      .getAttribute("d")!
      .match(/q\S+ \S+ ([\d.]+) \S+h(-?[\d.]+)q/)!;
    // Between the first and the third, the empty second.
    expect(xs[1] - xs[0]).toBeGreaterThan(Number(w[1]) * 2 + Number(w[2]));
  });

  it("lights the bar under the pointer", () => {
    render(
      <MainChart
        {...CHART}
        labels={["a", "b"]}
        cur={[2, 3]}
        mean={[2, 3]}
        prev={null}
        kind="sum"
      />,
    );
    fireEvent.pointerMove(screen.getByTestId("chart-hit"), { clientX: 5000 });
    expect(screen.getByTestId("hovered").getAttribute("d")).toMatch(/z$/);
  });

  it("names no largest share", () => {
    render(
      <MainChart
        {...CHART}
        labels={["a", "b"]}
        cur={[100, 3]}
        mean={[100, 4]}
        prev={[2, 3]}
        kind="rate"
      />,
    );
    expect(screen.queryByTestId("peak")).toBeNull();
  });

  it("prints labels apart", () => {
    const { container } = render(
      <MainChart
        {...CHART}
        labels={Array.from({ length: 30 }, (_, i) => `${i}`)}
        cur={Array.from({ length: 30 }, () => 1)}
        mean={Array.from({ length: 30 }, () => 1)}
        prev={null}
        kind="sum"
        axis={{
          ticks: Array.from({ length: 30 }, (_, i) => ({
            at: i,
            label: `2${i % 10}.11.`,
            line: false,
          })),
          weekends: [],
        }}
      />,
    );
    const xs = [...container.querySelectorAll("text[text-anchor=middle]")]
      .filter((t) => t.textContent!.endsWith(".11."))
      .map((t) => Number(t.getAttribute("x")));
    expect(xs.length).toBeGreaterThan(5);
    for (let i = 1; i < xs.length; i++)
      expect(xs[i] - xs[i - 1]).toBeGreaterThan(6 * 6.4);
  });
});

describe("the watch", () => {
  // The fixtures' watch answer with patch over it.
  function startWatch(
    url: string,
    patch: Partial<Watch>,
    opt: FixtureOptions = {},
  ) {
    window.history.replaceState(null, "", url);
    calls = [];
    const fx = fixtureFetch(opt);
    setFetcher(async (u, i) => {
      calls.push(u);
      const res = await fx(u, i);
      if (!u.startsWith("/api/dash/watch")) return res;
      return new Response(JSON.stringify({ ...(await res.json()), ...patch }));
    });
    return render(<App />);
  }
  const rowOf = (region: HTMLElement, text: string) =>
    within(region).getByText(text).closest("tr")!;
  const host = (h: string, isNew = false): WatchHost => ({
    host: h,
    first_seen: "2026-11-01T09:00:00Z",
    last_seen: "2026-11-22T13:00:00Z",
    addrs: 5,
    new: isNew,
    provisional: false,
  });

  it("lists answered probes beside the probed paths and opens their requests", async () => {
    start();
    const a = await screen.findByRole("region", { name: "Answered probes" });
    await within(a).findByText("GET /wp-content/plugins/hello/up.php");
    // Newest first, what happened in words, the backend's lines under it.
    const rows = within(a).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.querySelector(".font-mono")?.textContent)).toEqual(
      [
        "POST /xmlrpc.php",
        "GET /wp-content/plugins/hello/up.php",
        "GET /.git/config",
      ],
    );
    expect(rows[0]).toHaveTextContent("new target");
    expect(within(rows[0]).getByText("provisional")).toBeInTheDocument();
    expect(rows[1]).toHaveTextContent("refused path now answered");
    expect(rows[1]).toHaveTextContent(
      "Answered 18.4 kB, refused 512 B or 4.5 kB",
    );
    expect(within(rows[1]).queryByText("provisional")).toBeNull();
    expect(rows[2]).toHaveTextContent("a scanner got an answer");
    // Its neighbour in the same row of cards.
    expect(a.previousElementSibling).toHaveAttribute(
      "aria-label",
      "Probed paths",
    );
    // Counted over the site's whole time, not the range, once a day a key
    // was left out.
    expect(a).toHaveTextContent(
      "Past the watch's limits, a path or script host was left out 37 times (at most once a day each).",
    );
    // One answer for both cards, whatever the audience.
    const asked = calls.filter((c) => c.startsWith("/api/dash/watch"));
    expect(asked).toHaveLength(1);
    expect(new URL(asked[0], "http://x").searchParams.get("aud")).toBe("all");
    // A scanner's row opens its address.
    fireEvent.click(rows[2]);
    const d = await screen.findByRole("region", { name: "Visitor" });
    expect(
      await within(d).findByText(/Headless Chrome stepping/),
    ).toBeInTheDocument();
    // A path's its requests.
    fireEvent.click(rows[1]);
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    const q = new URLSearchParams(window.location.search);
    expect([q.get("path"), q.get("kind")]).toEqual([
      "GET /wp-content/plugins/hello/up.php",
      "request",
    ]);
  });

  it("opens nothing once a probe's requests are gone", async () => {
    start("/dashboard/?range=90d");
    const a = await screen.findByRole("region", { name: "Answered probes" });
    await within(a).findByText("GET /cgi-bin/status");
    const row = rowOf(a, "GET /cgi-bin/status");
    expect(row).toHaveTextContent("requests no longer kept");
    expect(row).not.toHaveClass("click");
    expect(within(row).queryByRole("button")).toBeNull();
  });

  it("lists the script hosts, new ones first and marked", async () => {
    start();
    const s = await screen.findByRole("region", { name: "Script hosts" });
    await within(s).findByText("cdn.evil.example");
    const rows = within(s).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(6);
    expect(within(rows[0]).getByText("new")).toBeInTheDocument();
    expect(within(rows[1]).getByText("provisional")).toBeInTheDocument();
    expect(within(rows[2]).queryByText("new")).toBeNull();
    // A host is no link anywhere.
    expect(rows[0]).not.toHaveClass("click");
    // Addresses, not visitors (a visitor id is the browser's word), and a
    // day's, said under the table.
    expect(within(s).getAllByRole("columnheader")[3]).toHaveTextContent(
      "Addresses",
    );
    expect(rows[0]).toHaveTextContent("4");
    expect(s).toHaveTextContent(
      "Addresses: the most of one day that reported the host.",
    );
    fireEvent.click(within(s).getByRole("button", { name: "Show all 9 →" }));
    expect(within(s).getAllByRole("row")).toHaveLength(10);
  });

  it("jumps from its findings to the log, the visitor or the marked row", async () => {
    start("/dashboard/?range=90d", { nw: "watch" });
    const nw = await screen.findByRole("region", { name: "Noteworthy" });
    const lines = await within(nw).findAllByRole("button");
    expect(lines.map((b) => b.getAttribute("aria-label"))).toEqual([
      "1 address: Refused path now answered: GET /wp-content/plugins/hello/up.php",
      "1 address: A scanner got an answer: GET /.git/config",
      "4 addresses loaded: New script host: cdn.evil.example",
      "2 addresses: Refused path now answered: GET /cgi-bin/status (its requests are no longer kept)",
      "3 addresses: New target answered: POST /xmlrpc.php",
    ]);
    // A new script host: its row in the card, marked.
    const s = await screen.findByRole("region", { name: "Script hosts" });
    await within(s).findByText("cdn.evil.example");
    expect(rowOf(s, "cdn.evil.example")).not.toHaveClass("sel");
    fireEvent.click(lines[2]);
    expect(rowOf(s, "cdn.evil.example")).toHaveClass("sel");
    expect(rowOf(s, "cdn.evil.example")).toHaveAttribute(
      "aria-selected",
      "true",
    );
    // A path whose requests are gone: its row in the answered probes.
    const a = screen.getByRole("region", { name: "Answered probes" });
    await within(a).findByText("GET /cgi-bin/status");
    fireEvent.click(lines[3]);
    expect(rowOf(a, "GET /cgi-bin/status")).toHaveClass("sel");
    expect(rowOf(s, "cdn.evil.example")).not.toHaveClass("sel");
    // A scanner's hit: its address.
    fireEvent.click(lines[1]);
    const d = await screen.findByRole("region", { name: "Visitor" });
    expect(
      await within(d).findByText(/Headless Chrome stepping/),
    ).toBeInTheDocument();
    // A flipped path: its requests.
    fireEvent.click(lines[0]);
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    expect(new URLSearchParams(window.location.search).get("path")).toBe(
      "GET /wp-content/plugins/hello/up.php",
    );
  });

  it("shows the row a jump names past the first ones", async () => {
    const others = Array.from({ length: 8 }, (_, i) =>
      host(`h${i}.example.net`),
    );
    startWatch(
      "/dashboard/",
      { hosts: [...others, host("cdn.evil.example", true)] },
      { nw: "watch" },
    );
    const s = await screen.findByRole("region", { name: "Script hosts" });
    await within(s).findByText("h0.example.net");
    expect(within(s).queryByText("cdn.evil.example")).toBeNull();
    const nw = screen.getByRole("region", { name: "Noteworthy" });
    fireEvent.click(
      await within(nw).findByRole("button", { name: /New script host/ }),
    );
    expect(rowOf(s, "cdn.evil.example")).toHaveClass("sel");
    expect(within(s).queryByRole("button", { name: /Show all/ })).toBeNull();
  });

  it("says when it is still reading, learning or has nothing", async () => {
    // Still reading: both cards say so.
    const first = startWatch("/dashboard/", {
      ready: false,
      paths: [],
      hosts: [],
    });
    const a = await screen.findByRole("region", { name: "Answered probes" });
    expect(
      await within(a).findByText(/Still reading the kept requests/),
    ).toBeInTheDocument();
    expect(
      within(panel("Script hosts")).getByText(
        /Still reading the kept requests/,
      ),
    ).toBeInTheDocument();
    first.unmount();
    // Learning: until when.
    const second = startWatch("/dashboard/", {
      paths: [],
      hosts: [],
      learning_until: "2099-01-02T00:00:00Z",
      scripts_learning_until: null,
    });
    const b = await screen.findByRole("region", { name: "Answered probes" });
    expect(
      await within(b).findByText(
        "Learning which paths this site refuses until 2.1.2099: nothing is judged before.",
      ),
    ).toBeInTheDocument();
    expect(
      within(panel("Script hosts")).getByText(
        /No page has reported its script hosts yet/,
      ),
    ).toBeInTheDocument();
    second.unmount();
    // Learned, nothing in the range.
    const third = startWatch("/dashboard/", { paths: [], hosts: [], cut: 0 });
    const c = await screen.findByRole("region", { name: "Answered probes" });
    expect(
      await within(c).findByText("No probe got an answer in this range."),
    ).toBeInTheDocument();
    expect(
      within(panel("Script hosts")).getByText(
        "No page loaded scripts from another host in this range.",
      ),
    ).toBeInTheDocument();
    expect(c).not.toHaveTextContent("left out");
    third.unmount();
    // Learning the hosts: they show, none new yet.
    startWatch("/dashboard/", {
      hosts: [host("fonts.example.net")],
      scripts_learning_until: "2099-01-02T00:00:00Z",
    });
    const s = await screen.findByRole("region", { name: "Script hosts" });
    await within(s).findByText("fonts.example.net");
    expect(s).toHaveTextContent(
      "Learning the hosts until 2.1.2099: none is new before.",
    );
  });

  it("shows a failed answer in both cards", async () => {
    start("/dashboard/", { fail: ["/api/dash/watch"] });
    const a = await screen.findByRole("region", { name: "Answered probes" });
    expect(await within(a).findByRole("alert")).toHaveTextContent(
      "fixture failure",
    );
    expect(
      within(panel("Script hosts")).getByRole("alert"),
    ).toBeInTheDocument();
  });

  it("names a day the site answered unknown paths in the domain footer", async () => {
    start();
    const footer = await screen.findByRole("region", { name: "This domain" });
    await within(footer).findByText("Rate limit");
    await waitFor(() =>
      expect(footer.textContent).toContain(
        `answers unknown paths 2xx, last on 18.11.2026 (UTC)${SEP}answered probes are not judged on such a day`,
      ),
    );
  });

  it("has no answered probes without a feed, the script hosts still", async () => {
    const first = start("/dashboard/?site=console.example.com", {
      nw: "watch",
    });
    const s = await screen.findByRole("region", { name: "Script hosts" });
    await within(s).findByText("cdn.evil.example");
    expect(
      screen.queryByRole("region", { name: "Answered probes" }),
    ).toBeNull();
    const nw = screen.getByRole("region", { name: "Noteworthy" });
    expect(
      await within(nw).findByText("New script host: cdn.evil.example"),
    ).toBeInTheDocument();
    expect(within(nw).queryByText(/Refused path/)).toBeNull();
    first.unmount();
    // An answer that says no feed: no card either.
    startWatch("/dashboard/?site=lens.example.com", { feed: false });
    const s2 = await screen.findByRole("region", { name: "Script hosts" });
    await within(s2).findByText("cdn.evil.example");
    await screen.findByRole("region", { name: "Probed paths" });
    expect(
      screen.queryByRole("region", { name: "Answered probes" }),
    ).toBeNull();
  });

  it("shows an API its answered probes, no script hosts", async () => {
    start("/dashboard/?site=api.example.com");
    await screen.findByRole("region", { name: "Answered probes" });
    expect(screen.queryByRole("region", { name: "Script hosts" })).toBeNull();
  });

  it("shows an API the script hosts its pages report, where a finding jumps", async () => {
    startWatch(
      "/dashboard/?site=api.example.com",
      { hosts: [host("cdn.evil.example", true)] },
      { nw: "watch" },
    );
    const s = await screen.findByRole("region", { name: "Script hosts" });
    await within(s).findByText("cdn.evil.example");
    const nw = screen.getByRole("region", { name: "Noteworthy" });
    fireEvent.click(
      await within(nw).findByRole("button", { name: /New script host/ }),
    );
    expect(rowOf(s, "cdn.evil.example")).toHaveClass("sel");
  });
});

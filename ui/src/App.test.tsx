import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setFetcher } from "./api";
import App from "./App";
import { type FixtureOptions, fixtureFetch } from "./fixtures";

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

const panel = (name: string) => screen.getByRole("region", { name });
const lastCall = (path: string) =>
  new URL(calls.filter((c) => c.startsWith(path)).at(-1)!, "http://x")
    .searchParams;

beforeEach(() => localStorage.clear());
afterEach(() => setFetcher(null));

describe("sign-in", () => {
  it("links to the login on 401 and never redirects itself", async () => {
    start("/dashboard/", { me: 401 });
    const link = await screen.findByRole("link", { name: "Sign in" });
    expect(link).toHaveAttribute("href", "/api/auth/login");
    expect(calls).toEqual(["/api/me"]);
  });

  it("says so on 403", async () => {
    start("/dashboard/", { me: 403 });
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "not in the group",
    );
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
    expect(document.querySelector(".topbar")).toHaveTextContent(
      "7 humans online · 3 automated",
    );
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
    await within(ov).findByText("Visitors per day · all");
    fireEvent.click(within(ov).getByRole("button", { name: /Page views/ }));
    expect(
      within(ov).getByText("Page views per day · all"),
    ).toBeInTheDocument();
    const hit = within(ov).getByTestId("chart-hit");
    fireEvent.mouseMove(hit, { clientX: 500, clientY: 100 });
    const tip = screen.getByRole("tooltip");
    expect(tip).toHaveTextContent("Page views");
    expect(tip).toHaveTextContent("Median");
    fireEvent.mouseLeave(hit);
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("draws no comparison with the comparison off", async () => {
    start("/dashboard/?cmp=off");
    const ov = await screen.findByRole("region", { name: "Overview" });
    await within(ov).findByText("Visitors per day · all");
    // The backend sends an empty previous series, not none.
    expect(lastCall("/api/dash/overview").get("cmp")).toBe("off");
    fireEvent.mouseMove(within(ov).getByTestId("chart-hit"), {
      clientX: 500,
      clientY: 100,
    });
    const tip = screen.getByRole("tooltip");
    expect(tip).toHaveTextContent("Visitors");
    expect(tip).not.toHaveTextContent(/prior/);
    // This period and the median, nothing else.
    expect(tip.querySelectorAll(".tr")).toHaveLength(2);
    expect(ov.querySelector('[stroke="var(--color-reference)"]')).toBeNull();
  });

  it("keeps range, audience and comparison in the URL", async () => {
    start();
    await screen.findByText("Visitors per day · all");
    fireEvent.click(screen.getByRole("button", { name: "30d" }));
    expect(window.location.search).toContain("range=30d");
    await waitFor(() =>
      expect(lastCall("/api/dash/overview").get("range")).toBe("30d"),
    );
    fireEvent.click(screen.getByRole("button", { name: "Bots" }));
    expect(window.location.search).toContain("aud=bots");
    expect(localStorage.getItem("nui.aud")).toBe("bots");
    expect(
      await screen.findByText("Addresses per day · bots"),
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
    // Back restores the previous view.
    act(() => {
      window.history.back();
    });
    await waitFor(() =>
      expect(window.location.search).not.toContain("cmp=off"),
    );
  });

  it("jumps from a finding to its panel or into the log", async () => {
    start();
    const nw = await screen.findByRole("region", { name: "Noteworthy" });
    const rows = await within(nw).findAllByRole("button");
    expect(rows.length).toBeLessThanOrEqual(5);
    expect(
      within(nw).getByText("New referrer news.ycombinator.com"),
    ).not.toHaveClass("gen");
    fireEvent.mouseMove(rows[0], { clientX: 10, clientY: 10 });
    expect(screen.getByRole("tooltip")).toHaveTextContent("Safari only");
    fireEvent.mouseLeave(rows[0]);
    fireEvent.click(rows[1]);
    const detail = await screen.findByRole("region", { name: "Visitor" });
    expect(
      await within(detail).findByText("Scraping attempt"),
    ).toBeInTheDocument();
    fireEvent.click(rows[4]);
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    expect(window.location.search).toContain("addr=2001");
  });

  it("lists visitors with sorts, filter, search, show all and the detail", async () => {
    start();
    const v = await screen.findByRole("region", {
      name: "Visitors by address",
    });
    await within(v).findByText("198.51.100.7");
    expect(within(v).getByText(/Top 7 of 16 addresses/)).toBeInTheDocument();
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
    await within(d).findByText("Ordinary use");
    expect(
      within(d).getByRole("img", { name: "Activity by hour" }).children,
    ).toHaveLength(24);
    fireEvent.click(within(d).getByRole("button", { name: "Load earlier →" }));
    await waitFor(() =>
      expect(lastCall("/api/dash/visitor").get("cursor")).toBe("t5"),
    );
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
    await within(v).findByText(/16 of 16 addresses/);
    expect(within(v).queryByRole("button", { name: "Load more →" })).toBeNull();
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
      "python-requests/2.32",
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
  });

  it("runs the top-N lists: order, show all, search, expand, log", async () => {
    start();
    const pages = await screen.findByRole("region", { name: "Pages" });
    await within(pages).findByText("/AS:n");
    expect(within(pages).getByText("Other · 2")).toBeInTheDocument();
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
    expect(new URLSearchParams(window.location.search).get("path")).toBe(
      "GET /.env → 404",
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
    expect(await within(d).findByText("Ordinary use")).toBeInTheDocument();
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
    fireEvent.click(await within(ev).findByText("share_link"));
    await waitFor(() =>
      expect(window.location.pathname).toBe("/dashboard/log"),
    );
    expect(new URLSearchParams(window.location.search).get("path")).toBe(
      "share_link",
    );
  });

  it("draws the traffic charts with tooltips", async () => {
    start();
    const t = await screen.findByRole("region", { name: "Requests by class" });
    const bars = await within(t).findAllByTestId("stack-hit");
    expect(bars).toHaveLength(7);
    fireEvent.mouseMove(bars[6], { clientX: 900, clientY: 100 });
    expect(screen.getByRole("tooltip")).toHaveTextContent("still filling");
    fireEvent.mouseLeave(bars[6]);
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

  it("shows the domain footer", async () => {
    start();
    const d = await screen.findByRole("region", { name: "This domain" });
    expect(await within(d).findByText("Rate-limited")).toBeInTheDocument();
    expect(within(d).getByText("412 MB")).toBeInTheDocument();
    expect(
      within(d).getByText("25 months, raw, including addresses"),
    ).toBeInTheDocument();
    expect(within(d).getByText("41 of 200 calls today")).toBeInTheDocument();
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
    await within(ov).findByText("Visitors per day · all");
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
    await within(log).findByText("25 of 70 events in this range");
    fireEvent.click(
      within(log).getByRole("button", { name: "Load earlier →" }),
    );
    await within(log).findByText("50 of 70 events in this range");
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
    await within(log).findByText("25 of 70 events in this range");
    hold.match = (u) => u.startsWith("/api/dash/log");
    // The 7 d query's second page is asked for, then the range changes.
    fireEvent.click(
      within(log).getByRole("button", { name: "Load earlier →" }),
    );
    fireEvent.click(screen.getByRole("button", { name: "30d" }));
    const old = (u: string) => u.includes("range=7d");
    await hold.release((u) => !old(u));
    await within(log).findByText("25 of 70 events in this range");
    expect(log).not.toHaveAttribute("aria-busy");
    // Its own paging is not stuck behind the old request.
    const earlier = within(log).getByRole("button", { name: "Load earlier →" });
    expect(earlier).toBeEnabled();
    fireEvent.click(earlier);
    await hold.release((u) => !old(u));
    await within(log).findByText("50 of 70 events in this range");
    // The old query's page lands last and changes nothing.
    await hold.release();
    expect(
      within(log).getByText("50 of 70 events in this range"),
    ).toBeInTheDocument();
    expect(within(log).getAllByRole("row")).toHaveLength(51);
  });

  it("never pages a visitor's trail with another visitor's cursor", async () => {
    startHeld("/dashboard/");
    const v = await screen.findByRole("region", {
      name: "Visitors by address",
    });
    fireEvent.click(await within(v).findByText("203.0.113.42"));
    const d = panel("Visitor");
    await within(d).findByText("Ordinary use");
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
    // The domain switch keeps the other filters.
    fireEvent.change(screen.getByRole("combobox", { name: "Domain" }), {
      target: { value: "lens.example.com" },
    });
    expect(window.location.search).toContain("site=lens.example.com");
    expect(await screen.findByText("Who is asking")).toBeInTheDocument();
  });

  it("shows empty states for a domain without data", async () => {
    window.history.replaceState(null, "", "/dashboard/?site=blog.example.org");
    const fx = fixtureFetch();
    const empty = { rows: [], other: null, total: 0, server: false };
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
  });
});

describe("log page", () => {
  it("filters, pages back and keeps domain and range", async () => {
    start("/dashboard/log?site=lens.example.com&range=30d");
    const log = await screen.findByRole("region", { name: "Log" });
    await within(log).findByText("25 of 70 events in this range");
    expect(screen.queryByRole("group", { name: "Audience" })).toBeNull();
    expect(screen.queryByRole("group", { name: "Compare" })).toBeNull();
    expect(lastCall("/api/dash/log").get("range")).toBe("30d");
    fireEvent.click(
      within(log).getByRole("button", { name: "Load earlier →" }),
    );
    await within(log).findByText("50 of 70 events in this range");
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
    await within(log).findByText(/of 10 events/);
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
    await within(log).findByText("25 of 70 events in this range");
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
      await within(log).findByText("25 of 70 events in this range");
      fireEvent.change(box(log, "Address or /64"), {
        target: { value: "192.0.2.120" },
      });
      fireEvent.change(box(log, "Path or event name"), {
        target: { value: "/api" },
      });
      await pause();
      const q = lastCall("/api/dash/log");
      expect([q.get("addr"), q.get("path")]).toEqual(["192.0.2.120", "/api"]);
      const url = new URLSearchParams(window.location.search);
      expect([url.get("addr"), url.get("path")]).toEqual([
        "192.0.2.120",
        "/api",
      ]);
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
      await within(log).findByText("25 of 70 events in this range");
      fireEvent.change(box(log, "Path or event name"), {
        target: { value: "/api" },
      });
      fireEvent.click(screen.getByRole("link", { name: "Visitors" }));
      await pause();
      expect(window.location.pathname).toBe("/dashboard/");
      expect(screen.queryByRole("region", { name: "Log" })).toBeNull();
    });
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

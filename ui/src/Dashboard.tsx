import { useCallback, useEffect, useMemo, useState } from "react";
import {
  api,
  type Audience,
  type Compare,
  type LogFilter,
  type Me,
  type Classes,
  type Overview,
  type Panel as PanelKey,
  type Range,
  type Scope,
  type SiteInfo,
} from "./api";
import { ApiSection } from "./Calls";
import { Content } from "./Content";
import { DomainFooter } from "./DomainFooter";
import { SEP, num } from "./format";
import { useApi } from "./hooks";
import { LogPage } from "./LogPage";
import { Noteworthy } from "./Noteworthy";
import { Origin } from "./Origin";
import { OverviewSection } from "./Overview";
import {
  AUDIENCES,
  PRODUCT,
  RANGES,
  basePath,
  parseView,
  storeAudience,
  tabTitle,
  viewHref,
  type View,
} from "./state";
import { ErrorsPanel, Traffic, WhoIsAsking } from "./Traffic";
import {
  DashContext,
  ErrorBox,
  Seg,
  Skeleton,
  showsApiClients,
  useDash,
  type Dash,
} from "./ui";
import { Visitors } from "./Visitors";

const AUD_LABEL: Record<Audience, string> = {
  all: "All",
  humans: "Humans",
  bots: "Bots",
};
const CMP_LABEL: Record<Compare, string> = {
  prev: "vs previous period",
  yoy: "vs last year",
  off: "off",
};

// Panel ids a finding or a menu entry jumps to.
const PANEL_ID: Record<Exclude<PanelKey, "log">, string> = {
  errors: "errors-panel",
  visitors: "visitors",
  referrers: "referrers-panel",
  probed: "probed-panel",
  events: "events-panel",
  endpoints: "endpoints-panel",
  failures: "failures-panel",
  domain: "domain",
};

// Scroll to an element, waiting a few frames for it after a page switch.
function scrollWhenThere(id: string, tries = 20) {
  const el = document.getElementById(id);
  if (el) el.scrollIntoView?.({ behavior: "smooth", block: "start" });
  else if (tries > 0) setTimeout(() => scrollWhenThere(id, tries - 1), 50);
}

export default function Dashboard({ me }: { me: Me }) {
  const base = useMemo(() => basePath(window.location.pathname), []);
  const [view, setView] = useState<View>(() =>
    parseView(window.location.pathname, window.location.search),
  );
  const [picked, setPicked] = useState<{ site: string; key: string } | null>(
    null,
  );

  useEffect(() => {
    const f = () =>
      setView(parseView(window.location.pathname, window.location.search));
    window.addEventListener("popstate", f);
    return () => window.removeEventListener("popstate", f);
  }, []);

  const go = useCallback(
    (v: View) => {
      window.history.pushState(null, "", viewHref(base, v));
      setView(v);
    },
    [base],
  );

  // The log's filter, changed in place: applied to the view as it is when
  // the change lands (two text filters can fire before a render), and
  // never bringing the log back once the visitor has left it. The URL
  // follows below.
  const filterLog = useCallback((change: (f: LogFilter) => LogFilter) => {
    setView((cur) =>
      cur.page === "log" ? { ...cur, log: change(cur.log) } : cur,
    );
  }, []);
  useEffect(() => {
    if (view.page !== "log") return;
    const href = viewHref(base, view);
    if (href !== window.location.pathname + window.location.search)
      window.history.replaceState(null, "", href);
  }, [base, view]);

  const sites = useApi("sites", (s) => api.sites(s));
  const list = sites.data?.sites ?? null;
  const site: SiteInfo | null =
    list?.find((s) => s.site === view.site) ?? list?.[0] ?? null;

  const title = tabTitle(site?.site ?? null, view);
  useEffect(() => {
    document.title = title;
  }, [title]);
  // Signed out, the sign-in card shows under the plain name again.
  useEffect(
    () => () => {
      document.title = PRODUCT;
    },
    [],
  );

  const feed = site?.server != null;
  // Configured is enough: the backend answers for an API from then on.
  const isApi = site?.kind === "api" && site.feed;
  // An API's callers are programs: no audience to pick there, and the one
  // remembered from a website would empty it.
  const aud: Audience = isApi ? "all" : view.aud;
  const scope: Scope | null = site
    ? { site: site.site, range: view.range, aud, cmp: view.cmp }
    : null;
  const ov = useApi<Overview>(
    scope ? `overview|${JSON.stringify(scope)}` : null,
    (s) => api.overview(scope!, s),
    scope?.site,
  );
  // Who is asking and Traffic share one answer; it ignores the audience.
  const classes = useApi<Classes>(
    scope && site?.server && view.page === "dash"
      ? `classes|${scope.site}|${scope.range}|${scope.cmp}`
      : null,
    (s) => api.classes(scope!, s),
    scope?.site,
  );

  const openLog = useCallback(
    (f: LogFilter) => {
      go({ ...view, page: "log", log: f });
      window.scrollTo?.(0, 0);
    },
    [go, view],
  );

  const scrollToId = useCallback(
    (id: string) => {
      if (view.page !== "dash") go({ ...view, page: "dash" });
      scrollWhenThere(id);
    },
    [go, view],
  );

  const goPanel = useCallback(
    (p: PanelKey, filter: Record<string, string> = {}) => {
      if (p === "log") return openLog(filter);
      if (p === "visitors" && filter.key && site)
        setPicked({ site: site.site, key: filter.key });
      scrollToId(PANEL_ID[p]);
    },
    [openLog, scrollToId, site],
  );

  const set = (patch: Partial<View>) => go({ ...view, ...patch });

  const dash: Dash | null =
    site && scope
      ? {
          scope,
          site,
          feed,
          both: feed && site.browser != null,
          apiClients: showsApiClients(site, classes),
          openLog,
          goPanel,
          visitor: picked?.site === site.site ? picked.key : null,
          selectVisitor: (key) =>
            setPicked(key ? { site: site.site, key } : null),
        }
      : null;

  const nav: [string, string][] = [
    ["Overview", "overview"],
    ...(feed ? [["Traffic", "traffic"] as [string, string]] : []),
    ["Visitors", "visitors"],
    [isApi ? "API" : "Content", "content"],
    ["Origin", "origin"],
  ];

  return (
    <>
      <header className="topbar">
        <span className="wordmark">Trakk</span>
        <nav aria-label="Sections">
          {nav.map(([label, id]) => (
            <a
              key={id}
              href={`${viewHref(base, { ...view, page: "dash" })}#${id}`}
              onClick={(e) => {
                e.preventDefault();
                scrollToId(id);
              }}
            >
              {label}
            </a>
          ))}
          <a
            href={viewHref(base, { ...view, page: "log", log: {} })}
            aria-current={view.page === "log" ? "page" : undefined}
            onClick={(e) => {
              e.preventDefault();
              openLog({});
            }}
          >
            Log
          </a>
          <a
            href={`${viewHref(base, { ...view, page: "dash" })}#domain`}
            onClick={(e) => {
              e.preventDefault();
              scrollToId("domain");
            }}
          >
            Domain
          </a>
        </nav>
        <span className="flex-1" />
        {ov.data && (
          // Below 900 px "automated" says "bots", the audience switch's word
          // for the same classes: with it the bar needed 790 px and pushed
          // Sign out past a 768 px window (three-digit counts: up to 900 px).
          // Words, not a tooltip: an iPad has no hover.
          <span className="flex items-center text-[13px] whitespace-nowrap text-muted">
            <i className="live-dot mr-[7px]" />
            <span>
              <b className="font-medium text-ink tabular-nums">
                {num(ov.data.live.humans)}
              </b>{" "}
              humans<span className="max-[1099px]:hidden"> online</span>
              {SEP}
              <b className="font-medium text-ink tabular-nums">
                {num(ov.data.live.automated)}
              </b>{" "}
              <span className="max-[900px]:hidden">automated</span>
              <span className="min-[900px]:hidden">bots</span>
            </span>
          </span>
        )}
        <span className="text-[13px] whitespace-nowrap text-muted">
          <span className="max-[1099px]:hidden">
            {me.name}
            {SEP}
          </span>
          <button
            title={`Signed in as ${me.name}`}
            type="button"
            className="link !text-[13px] !text-muted hover:!text-ink"
            onClick={() => {
              api.logout().finally(() => window.location.reload());
            }}
          >
            Sign out
          </button>
        </span>
      </header>

      <main className="mx-auto max-w-[1400px] px-6 pt-[22px] pb-20">
        {sites.error && <ErrorBox error={sites.error} />}
        {!sites.error && !list && <Skeleton h={56} label="Loading domains" />}
        {list && list.length === 0 && (
          <div className="panel card text-[13.5px] text-muted">
            No domain is set up yet. Add one to Trakk's site list and load the
            script on it; its numbers appear here.
          </div>
        )}
        {dash && (
          <DashContext.Provider value={dash}>
            <FilterBar
              sites={list ?? []}
              view={view}
              overview={ov.data}
              busy={ov.loading}
              isApi={isApi}
              onSite={(s) => set({ site: s })}
              onRange={(r) => set({ range: r })}
              onAud={(a) => {
                storeAudience(a);
                set({ aud: a });
              }}
              onCmp={(c) => set({ cmp: c })}
            />
            {view.page === "log" ? (
              <LogPage filter={view.log} onFilter={filterLog} />
            ) : (
              <>
                <OverviewSection
                  data={ov.data}
                  error={ov.error}
                  busy={ov.loading}
                  aud={aud}
                  api={isApi}
                />
                {/* Side by side only from 1200 px: an iPad across (up to
                    1194 px) left Noteworthy's text ~250 px. */}
                <div className="mt-3 grid grid-cols-1 gap-3 min-[1200px]:grid-cols-2">
                  <Noteworthy />
                  {feed ? <WhoIsAsking classes={classes} /> : <ErrorsPanel />}
                </div>
                {feed && <Traffic classes={classes} />}
                <Visitors aud={aud} />
                {isApi ? <ApiSection /> : <Content />}
                <Origin aud={aud} api={isApi} />
                <DomainFooter />
              </>
            )}
          </DashContext.Provider>
        )}
      </main>
    </>
  );
}

function FilterBar({
  sites,
  view,
  overview,
  busy,
  isApi,
  onSite,
  onRange,
  onAud,
  onCmp,
}: {
  sites: SiteInfo[];
  view: View;
  overview: Overview | null;
  busy: boolean;
  isApi: boolean;
  onSite: (s: string) => void;
  onRange: (r: Range) => void;
  onAud: (a: Audience) => void;
  onCmp: (c: Compare) => void;
}) {
  const { site } = useDash();
  // Products in the order the backend lists their first domain.
  const products: [string, SiteInfo[]][] = [];
  for (const s of sites) {
    const g = products.find(([p]) => p === s.product);
    if (g) g[1].push(s);
    else products.push([s.product, [s]]);
  }
  const p = overview?.period;
  const dash = view.page === "dash";
  return (
    <div className="panel mb-4 flex flex-wrap items-center gap-3.5 px-4 py-3">
      <select
        name="site"
        aria-label="Domain"
        className="select"
        value={site.site}
        onChange={(e) => onSite(e.target.value)}
      >
        {products.map(([prod, ss]) => (
          <optgroup key={prod} label={prod}>
            {ss.map((s) => (
              <option key={s.site} value={s.site}>
                {s.site}
              </option>
            ))}
          </optgroup>
        ))}
      </select>
      <Seg
        label="Range"
        options={RANGES.map((r) => ({ key: r, label: r }))}
        value={view.range}
        onChange={onRange}
      />
      {dash && !isApi && (
        <Seg
          label="Audience"
          options={AUDIENCES.map((a) => ({ key: a, label: AUD_LABEL[a] }))}
          value={view.aud}
          onChange={onAud}
        />
      )}
      {p && dash && (
        <span
          className={`text-[12.5px] text-muted ${busy ? "busy" : ""}`}
          aria-busy={busy || undefined}
        >
          one point per <span className="text-ink-dim">{p.slot}</span>
          {SEP}
          {view.cmp === "off" ? (
            "no comparison"
          ) : (
            <>
              compared with the{" "}
              <span className="text-ink-dim">{p.prev_label}</span>
            </>
          )}
        </span>
      )}
      <span className="flex-1" />
      {dash && (
        <Seg
          label="Compare"
          options={(["prev", "yoy", "off"] as Compare[]).map((c) => ({
            key: c,
            label: CMP_LABEL[c],
          }))}
          value={view.cmp}
          onChange={onCmp}
        />
      )}
    </div>
  );
}

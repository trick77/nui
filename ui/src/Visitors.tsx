import { useState } from "react";
import {
  ApiError,
  api,
  type Audience,
  type TrailItem,
  type Visitor,
  type VisitorRow,
  type Visitors as VisitorsPage,
} from "./api";
import { Hours } from "./charts";
import {
  SEP,
  countryName,
  dateTime,
  joinSep,
  num,
  pct,
  seconds,
  when,
} from "./format";
import { useApi, useDebounced, useMore } from "./hooks";
import { scopeKey } from "./lists";
import {
  CardHead,
  ClassPill,
  Empty,
  ErrorBox,
  GenText,
  Panel,
  RowButton,
  SectionHead,
  Seg,
  Skeleton,
  Tabs,
  useDash,
} from "./ui";

type Sort = "requests" | "refused" | "events" | "views";

const CLASS_FILTER = [
  { key: "", label: "All" },
  { key: "human", label: "Humans" },
  { key: "bot", label: "Known bots" },
  { key: "unknown", label: "Unknown automated" },
];

const TOP = 7;
const PAGE = 50;

export function Visitors({ aud }: { aud: Audience }) {
  const { scope, feed, visitor, selectVisitor, openLog } = useDash();
  const sorts: { key: Sort; label: string }[] = feed
    ? [
        { key: "requests", label: "Most requests" },
        { key: "refused", label: "Most refused" },
        { key: "events", label: "Most events" },
      ]
    : [
        { key: "views", label: "Most views" },
        { key: "events", label: "Most events" },
      ];
  const [sortPick, setSort] = useState<Sort>("requests");
  const sort = sorts.some((s) => s.key === sortPick) ? sortPick : sorts[0].key;
  const [clsPick, setCls] = useState("");
  // The class filter narrows within the switch, and only under All.
  const cls = aud === "all" ? clsPick : "";
  const [q, setQ] = useState("");
  const dq = useDebounced(q.trim(), 300);
  const [all, setAll] = useState(false);
  const params = { sort, class: cls, q: dq, limit: all ? PAGE : TOP };
  const key = scopeKey("visitors", { scope, ...params });
  const page = useApi<VisitorsPage>(
    key,
    (s) => api.visitors(scope, params, s),
    scope.site,
  );
  const more = useMore<VisitorRow>(key);
  const rows = [...(page.data?.rows ?? []), ...more.items];
  // While the first page of a new query loads, the rows and the cursor
  // shown are the previous query's: no paging from them.
  const fresh = !page.loading;
  const next = more.next ?? page.data?.next ?? "";
  // A pick from a finding or a related address may lie outside the table.
  const selected = visitor ?? rows[0]?.key ?? null;
  // A column keeps the mark's margin only when a row shown has the mark.
  const hangClient = rows.some((v) => v.client_gen);
  const hangDoes = rows.some((v) => v.does.gen);

  return (
    <>
      <SectionHead
        id="visitors"
        title="Visitors"
        lead="Who is doing what. Click a row for the full trail."
        sources={["browser", "server"]}
      />
      <Panel label="Visitors by address" className="card" busy={!fresh}>
        <CardHead
          title="By address"
          right={
            aud === "all" ? (
              <Seg
                label="Class"
                options={CLASS_FILTER}
                value={clsPick}
                onChange={setCls}
              />
            ) : undefined
          }
        >
          <Tabs label="Sort" options={sorts} value={sort} onChange={setSort} />
        </CardHead>
        {page.error ? (
          <ErrorBox error={page.error} />
        ) : !page.data ? (
          <Skeleton h={300} label="Loading visitors" />
        ) : !rows.length ? (
          <Empty>
            {dq
              ? `No address, network or client matches “${dq}”.`
              : "No visitors in this range yet."}
          </Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="t">
              <thead>
                <tr>
                  <th>Address</th>
                  <th>Class</th>
                  <th />
                  <th>Client</th>
                  <th className="n">Visits</th>
                  <th className="n">Views</th>
                  {feed && <th className="n">Requests</th>}
                  {feed && <th className="n">Refused</th>}
                  <th>What it does</th>
                  <th className="n">Last</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((v, i) => (
                  <tr
                    key={i}
                    className={`click ${v.key === selected ? "sel" : ""}`}
                    aria-selected={v.key === selected}
                    onClick={() => selectVisitor(v.key)}
                  >
                    <td>
                      <RowButton pressed={v.key === selected}>
                        <span className="font-mono text-[12.5px]">
                          {v.display}
                        </span>
                        <span className="block text-[12px] text-muted">
                          {joinSep([v.network, v.network_kind])}
                        </span>
                      </RowButton>
                    </td>
                    <td>
                      <ClassPill cls={v.class} />
                    </td>
                    <td title={countryName(v.country)}>{v.country}</td>
                    <td title={v.client_gen ? v.client : undefined}>
                      <GenText
                        hang={hangClient}
                        gen={v.client_gen}
                        text={v.client}
                      />
                    </td>
                    <td className="n">{num(v.visits)}</td>
                    <td className="n">{num(v.views)}</td>
                    {feed && <td className="n">{num(v.requests)}</td>}
                    {feed && (
                      <td className="n">
                        {v.refused ? (
                          <>
                            <span
                              className={
                                (v.refused_share ?? 0) >= 20 ? "delta bad" : ""
                              }
                            >
                              {num(v.refused)}
                            </span>{" "}
                            <span className="text-[12px] text-muted">
                              {pct(v.refused_share)}
                            </span>
                          </>
                        ) : (
                          <span className="text-[12px] text-muted">0</span>
                        )}
                      </td>
                    )}
                    <td className="min-w-[220px]">
                      <GenText
                        hang={hangDoes}
                        gen={v.does.gen}
                        text={v.does.text}
                        className="text-[13px] text-ink-dim"
                      />
                      {v.signals.length > 0 && (
                        <div
                          className={`text-[12px] text-muted ${hangDoes ? "pl-4" : ""}`}
                        >
                          {joinSep(v.signals)}
                        </div>
                      )}
                    </td>
                    <td className="n font-mono text-[12px]">{when(v.last)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <div className="mt-2.5 flex flex-wrap items-center gap-4 border-t border-border-soft px-2.5 pt-2.5 text-[12.5px] text-muted">
          <span className="mr-auto">
            {page.data
              ? `${all ? "" : "Top "}${num(rows.length)} of ${num(page.data.total)} addresses in this range, each row summed over the whole range`
              : ""}
          </span>
          <input
            className="field w-[230px]"
            name="find"
            placeholder="Find address, network or client"
            aria-label="Find address, network or client"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
          {!all && page.data && page.data.total > rows.length && (
            <button
              type="button"
              className="link"
              disabled={!fresh}
              onClick={() => setAll(true)}
            >
              Show all →
            </button>
          )}
          {all && next && (
            <button
              type="button"
              className="link"
              disabled={more.busy || !fresh}
              onClick={() =>
                fresh &&
                more.load(() =>
                  api
                    .visitors(scope, { ...params, cursor: next })
                    .then((p) => ({ items: p.rows, next: p.next })),
                )
              }
            >
              {more.busy ? "Loading…" : "Load more →"}
            </button>
          )}
          {all && (
            <button
              type="button"
              className="link"
              onClick={() => setAll(false)}
            >
              Show top {TOP}
            </button>
          )}
        </div>
        {more.error && <ErrorBox error={more.error} />}
      </Panel>
      {selected && (
        <VisitorDetail
          key={`${scope.site}`}
          vkey={selected}
          onLog={() => openLog({ addr: selected })}
        />
      )}
    </>
  );
}

const KIND_COLOR: Record<string, string> = {
  page_view: "var(--color-s1)",
  outbound_click: "var(--color-s2)",
  engagement: "var(--color-s2)",
  download: "var(--color-s2)",
  session_start: "var(--color-ink-dim)",
  js_error: "var(--color-danger-ink)",
  resource_error: "var(--color-danger-ink)",
};

// A trail line as shown: a run a page boundary split is joined once both
// parts are loaded. Each part's interval is the median of its own gaps,
// which do not combine: a joined run shows them apart (intervals).
type TrailLine = TrailItem & { intervals: number[] };

// The pages' lines in order, newest first, each run that goes on into the
// next page joined with that page's first line.
function joinTrail(items: TrailItem[]): TrailLine[] {
  const out: TrailLine[] = [];
  for (const t of items) {
    const prev = out.at(-1);
    const own = t.count > 1 ? [t.interval_s] : [];
    if (prev?.continues && t.continued && prev.what === t.what) {
      out[out.length - 1] = {
        ...prev,
        count: prev.count + t.count,
        from: t.from,
        continues: t.continues,
        intervals: [...prev.intervals, ...own],
      };
    } else out.push({ ...t, intervals: own });
  }
  return out;
}

// A run's count: one still going on into a page not loaded is at least
// that.
function trailCount(t: TrailLine): string {
  if (t.continues) return ` × ${num(t.count)}+`;
  return t.count > 1 ? ` × ${num(t.count)}` : "";
}

function trailText(t: TrailLine): string {
  if (t.count <= 1 && !t.continues) return t.what;
  const span = `${SEP}${dateTime(t.from)} to ${dateTime(t.to)}`;
  // Parts of one run with different medians: the range between them.
  const lo = Math.min(...t.intervals);
  const hi = Math.max(...t.intervals);
  const every = !t.intervals.length
    ? ""
    : lo === hi
      ? `, every ${seconds(lo)}`
      : `, every ${seconds(lo)} to ${seconds(hi)}`;
  // Its older events are on the next page: the span so far.
  const more = t.continues ? ", continues earlier" : "";
  return `${t.what}${span}${every}${more}`;
}

function VisitorDetail({ vkey, onLog }: { vkey: string; onLog: () => void }) {
  const { scope, feed, selectVisitor } = useDash();
  const { site, range } = scope;
  const key = scopeKey("visitor", { site, range, vkey });
  const d = useApi<Visitor>(
    key,
    (s) => api.visitor(scope, vkey, undefined, s),
    site,
  );
  const more = useMore<TrailItem>(key);
  const v = d.data;
  const trail = joinTrail([...(v?.trail ?? []), ...more.items]);
  // The previous visitor's trail and cursor while this one loads.
  const fresh = !d.loading;
  const next = more.next ?? v?.trail_next ?? "";
  return (
    <Panel
      id="visitor-detail"
      label="Visitor"
      className="mt-3 grid grid-cols-1 min-[1000px]:grid-cols-[320px_1fr]"
      busy={!fresh}
    >
      {d.error instanceof ApiError && d.error.status === 404 ? (
        // A picked address with no events in this range (another range or
        // a finding chose it): nothing to show, not a failure.
        <div className="px-[18px] py-4 min-[1000px]:col-span-2">
          <div className="eyebrow">Visitor</div>
          <div className="mt-1 font-mono text-[15px]">{vkey}</div>
          <Empty>No events from this address in this range.</Empty>
        </div>
      ) : d.error ? (
        <div className="p-4 min-[1000px]:col-span-2">
          <ErrorBox error={d.error} />
        </div>
      ) : !v ? (
        <div className="p-4 min-[1000px]:col-span-2">
          <Skeleton h={220} label="Loading the visitor" />
        </div>
      ) : (
        <>
          <div className="border-border px-[18px] py-4 min-[1000px]:border-r">
            <div className="eyebrow">Visitor</div>
            <div className="mt-1 font-mono text-[15px]">{v.key}</div>
            <p className="mt-1.5 mb-0 text-[13px] text-ink-dim">
              <GenText
                hang={!!v.does.gen}
                gen={v.does.gen}
                text={v.does.text}
              />
            </p>
            <dl className="mt-2.5 mb-0 grid grid-cols-[auto_1fr] gap-x-3.5 gap-y-[5px] text-[13px]">
              <dt className="text-muted">Class</dt>
              <dd className="m-0">
                <ClassPill cls={v.class} />
              </dd>
              <dt className="text-muted">Network</dt>
              <dd className="m-0">{joinSep([v.network, v.network_kind])}</dd>
              <dt className="text-muted">Country</dt>
              <dd className="m-0">{countryName(v.country)}</dd>
              <dt className="text-muted">Client</dt>
              <dd className="m-0" title={v.client_gen ? v.client : undefined}>
                <GenText
                  hang={!!v.client_gen}
                  gen={v.client_gen}
                  text={v.client}
                />
              </dd>
              <dt className="text-muted">Signals</dt>
              <dd className="m-0">{joinSep(v.signals) || "–"}</dd>
              {feed && v.requests != null && (
                <>
                  <dt className="text-muted">Requests</dt>
                  <dd className="m-0">{num(v.requests)}</dd>
                </>
              )}
              {v.addresses > 1 && (
                <>
                  <dt className="text-muted">Addresses</dt>
                  <dd className="m-0">{num(v.addresses)} in this /64</dd>
                </>
              )}
            </dl>
            <div className="eyebrow mt-4">Reading</div>
            {v.reading ? (
              <>
                <div className="mt-1.5">
                  <span
                    className={`pill gen ${v.reading.suspicious ? "suspicious" : ""}`}
                  >
                    {v.reading.verdict}
                  </span>
                </div>
                {v.reading.evidence.length > 0 && (
                  <ul className="mt-2 mb-0 grid list-disc gap-[3px] pl-4 text-[13px] text-ink-dim marker:text-faint">
                    {v.reading.evidence.slice(0, 3).map((e, i) => (
                      <li key={i}>{e}</li>
                    ))}
                  </ul>
                )}
                {v.reading.related.length > 0 && (
                  <div className="mt-1.5 text-[12px] text-muted">
                    Same class in this {v.key.includes(":") ? "/48" : "/24"}:{" "}
                    {v.reading.related.map((r) => (
                      <button
                        key={r}
                        type="button"
                        className="link mr-2 font-mono !text-[12px]"
                        onClick={() => selectVisitor(r)}
                      >
                        {r}
                      </button>
                    ))}
                  </div>
                )}
                <div className="mt-1 text-[11.5px] text-faint">
                  based on {dateTime(v.reading.basis.from)} to{" "}
                  {dateTime(v.reading.basis.to)}
                </div>
              </>
            ) : (
              <p className="mt-1.5 mb-0 text-[13px] text-muted">
                No reading for this address yet; the signals above are what nui
                measured.
              </p>
            )}
            <div className="eyebrow mt-4">Activity by hour{SEP}00 to 23</div>
            <Hours levels={v.hours} />
          </div>
          <div className="px-[18px] py-4">
            <div className="flex flex-wrap items-baseline gap-3">
              <div className="eyebrow">Trail{SEP}newest first</div>
              <button type="button" className="link ml-auto" onClick={onLog}>
                Every event of this address in the log →
              </button>
            </div>
            {trail.length === 0 ? (
              <Empty>No events in this range.</Empty>
            ) : (
              <ol className="trail">
                {trail.map((t, i) => (
                  <li
                    key={i}
                    style={{
                      ["--kc" as string]:
                        KIND_COLOR[t.kind] ?? "var(--color-other)",
                    }}
                  >
                    <time className="font-mono text-[12px] text-muted">
                      {when(t.t)}
                    </time>
                    <span className="font-mono text-[11.5px] text-ink-dim">
                      {t.kind}
                      {trailCount(t)}
                    </span>
                    <span className="font-mono text-[12.5px] wrap-anywhere">
                      {trailText(t)}
                    </span>
                  </li>
                ))}
              </ol>
            )}
            {next && (
              <button
                type="button"
                className="link mt-3"
                disabled={more.busy || !fresh}
                onClick={() =>
                  fresh &&
                  more.load(() =>
                    api
                      .visitor(scope, vkey, next)
                      .then((p) => ({ items: p.trail, next: p.trail_next })),
                  )
                }
              >
                {more.busy ? "Loading…" : "Load earlier →"}
              </button>
            )}
            {more.error && <ErrorBox error={more.error} />}
          </div>
        </>
      )}
    </Panel>
  );
}

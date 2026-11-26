import { useEffect, useEffectEvent, useState } from "react";
import { api, type Log, type LogFilter, type LogRow } from "./api";
import { ErrorLine } from "./ErrorDetail";
import { joinSep, num, rawSince, when } from "./format";
import { useApi, useMore } from "./hooks";
import { scopeKey } from "./lists";
import {
  CardHead,
  ClassPill,
  Empty,
  ErrorBox,
  Panel,
  SectionHead,
  Skeleton,
  useDash,
  VitalHint,
} from "./ui";

const CLASS_OPTS: [string, string][] = [
  ["", "Class: any"],
  ["human", "Humans"],
  ["bot", "Known bots"],
  ["monitor", "Uptime probes"],
  ["unknown", "Unknown automated"],
  ["api", "API clients"],
];
const BROWSER_KINDS = [
  "page_view",
  "session_start",
  "engagement",
  "outbound_click",
  "download",
  "form",
  "search",
  "js_error",
  "resource_error",
  "vital",
  "custom",
];
const STATUS_OPTS: [string, string][] = [
  ["", "Status: any"],
  ["2xx", "2xx"],
  ["3xx", "3xx"],
  ["4xx", "4xx"],
  ["5xx", "5xx"],
];

// A text filter applied after a short pause, trimmed; the field keeps what
// was typed. It follows the URL when a click elsewhere sets another value.
// The pending change dies with the field or a newer value and reaches the
// latest onChange, so it applies to the filter as it is then.
function TextFilter({
  label,
  value,
  onChange,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
}) {
  const [draft, setDraft] = useState(value);
  const [seen, setSeen] = useState(value);
  if (value !== seen) {
    setSeen(value);
    // Its own trimmed text coming back is no new value.
    if (value !== draft.trim()) setDraft(value);
  }
  const apply = useEffectEvent((v: string) => onChange(v));
  useEffect(() => {
    const v = draft.trim();
    if (v === value) return;
    const t = setTimeout(() => apply(v), 300);
    return () => clearTimeout(t);
  }, [draft, value]);
  return (
    <input
      className="field w-[230px]"
      name={label}
      placeholder={label}
      aria-label={label}
      value={draft}
      onChange={(e) => setDraft(e.target.value)}
    />
  );
}

function Pick({
  label,
  value,
  opts,
  onChange,
}: {
  label: string;
  value: string;
  opts: [string, string][];
  onChange: (v: string) => void;
}) {
  return (
    <select
      className="select !py-[5px] !text-[13px]"
      name={label}
      aria-label={label}
      value={value}
      onChange={(e) => onChange(e.target.value)}
    >
      {opts.map(([v, l]) => (
        <option key={v} value={v}>
          {l}
        </option>
      ))}
    </select>
  );
}

// The exact filters a click elsewhere sets.
const EXACT = [
  "name",
  "endpoint",
  "account",
  "op",
  "failure",
  "service",
] as const;

// A filter set by a link, not typed: shown so it can be seen and dropped on
// its own.
function SetFilter({
  label,
  value,
  onClear,
}: {
  label: string;
  value: string;
  onClear: () => void;
}) {
  return (
    <span className="pill wrap-anywhere">
      <span>
        {label[0].toUpperCase() + label.slice(1)} {value}
      </span>
      <button
        type="button"
        className="link ml-1.5 !text-[13px]"
        aria-label={`Clear the ${label} filter`}
        onClick={onClear}
      >
        ×
      </button>
    </span>
  );
}

// Single events, newest first, filtered; domain and range from the filter
// bar, keyset pagination backwards.
export function LogPage({
  filter,
  onFilter,
}: {
  filter: LogFilter;
  // Changes apply to the filter as it is when they land, not as it was
  // rendered: two text filters can fire before a render.
  onFilter: (change: (f: LogFilter) => LogFilter) => void;
}) {
  const { scope, feed, both } = useDash();
  const { site, range } = scope;
  const key = scopeKey("log", { site, range, filter });
  const page = useApi<Log>(
    key,
    (s) => api.log(scope, filter, undefined, s),
    site,
  );
  const more = useMore<LogRow>(key);
  const rows = [...(page.data?.rows ?? []), ...more.items];
  // While a new filter's first page loads, the rows and the cursor shown
  // are the previous filter's: no paging from them.
  const fresh = !page.loading;
  const next = more.next ?? page.data?.next ?? "";
  // Raw events only: the range may start before the oldest.
  const since = rawSince(page.data?.detail_since);
  // Clearing starts the text fields afresh, dropping what is still pending.
  const [cleared, setCleared] = useState(0);
  const set = (k: keyof LogFilter, v: string) =>
    onFilter((cur) => {
      const f = { ...cur };
      if (v) f[k] = v;
      else delete f[k];
      return f;
    });
  const kinds: [string, string][] = [
    ["", "Kind: any"],
    ...(feed ? [["request", "request"] as [string, string]] : []),
    ...BROWSER_KINDS.map((k): [string, string] => [k, k]),
  ];
  return (
    <>
      <SectionHead
        id="log"
        title="Log"
        lead="Single events, newest first. A busy site fills this in seconds, so it is meant to be filtered."
        sources={["browser", "server"]}
      />
      <Panel label="Log" className="card" busy={!fresh}>
        <CardHead title="Events">
          <div className="flex flex-wrap items-center gap-2.5">
            <TextFilter
              key={`addr${cleared}`}
              label="Address or /64"
              value={filter.addr ?? ""}
              onChange={(v) => set("addr", v)}
            />
            {filter.asn && (
              <SetFilter
                label="network"
                value={`AS${filter.asn.replace(/^as/i, "")}`}
                onClear={() => set("asn", "")}
              />
            )}
            {filter.calls && (
              <SetFilter
                label="calls"
                value="only"
                onClear={() => set("calls", "")}
              />
            )}
            {EXACT.map(
              (k) =>
                filter[k] && (
                  <SetFilter
                    key={k}
                    label={k}
                    value={filter[k]}
                    onClear={() => set(k, "")}
                  />
                ),
            )}
            <TextFilter
              key={`path${cleared}`}
              label="Path or event name"
              value={filter.path ?? ""}
              onChange={(v) => set("path", v)}
            />
            <Pick
              label="Class"
              value={filter.class ?? ""}
              opts={CLASS_OPTS}
              onChange={(v) => set("class", v)}
            />
            <Pick
              label="Kind"
              value={filter.kind ?? ""}
              opts={kinds}
              onChange={(v) => set("kind", v)}
            />
            {feed && (
              <Pick
                label="Status"
                value={filter.status ?? ""}
                opts={STATUS_OPTS}
                onChange={(v) => set("status", v)}
              />
            )}
            {Object.keys(filter).length > 0 && (
              <button
                type="button"
                className="link"
                onClick={() => {
                  setCleared((n) => n + 1);
                  onFilter(() => ({}));
                }}
              >
                Clear filters
              </button>
            )}
          </div>
        </CardHead>
        {page.error ? (
          <ErrorBox error={page.error} />
        ) : !page.data ? (
          <Skeleton h={320} label="Loading the log" />
        ) : !rows.length ? (
          <Empty>
            {Object.keys(filter).length
              ? `No event ${since ? `since ${since}` : "in this range"} matches the filter.`
              : since
                ? `No events since ${since}.`
                : "No events in this range yet."}
          </Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="t">
              <thead>
                <tr>
                  <th>Time</th>
                  <th>Address</th>
                  <th>Class</th>
                  <th>Kind</th>
                  <th>What</th>
                  <th>Client</th>
                  {feed && <th>Account</th>}
                  {feed && <th className="n">Duration</th>}
                  {both && <th>Source</th>}
                </tr>
              </thead>
              <tbody>
                {rows.map((r, i) => (
                  <tr key={i}>
                    <td className="font-mono text-[12.5px] whitespace-nowrap">
                      {when(r.t)}
                    </td>
                    <td>
                      <button
                        type="button"
                        className="link font-mono !text-[12.5px] !text-ink"
                        title="Only this address"
                        onClick={() => set("addr", r.addr)}
                      >
                        {r.addr}
                      </button>
                    </td>
                    <td>
                      <ClassPill cls={r.class} />
                    </td>
                    <td className="font-mono text-[11.5px] text-ink-dim">
                      {r.kind}
                    </td>
                    <td className="min-w-[200px] font-mono text-[12.5px] wrap-anywhere">
                      {r.what}
                      <VitalHint kind={r.kind} what={r.what} />
                      {r.error && <ErrorLine where={r.error} page={r.page} />}
                    </td>
                    <td className="text-[12px] whitespace-nowrap text-muted">
                      {r.client}
                    </td>
                    {feed && (
                      <td>
                        {r.account && (
                          <button
                            type="button"
                            className="link font-mono !text-[12.5px] !text-ink"
                            title="Only this account"
                            onClick={() => set("account", r.account)}
                          >
                            {r.account}
                          </button>
                        )}
                      </td>
                    )}
                    {feed && (
                      <td className="n whitespace-nowrap">
                        {r.dur_ms != null && `${num(r.dur_ms)} ms`}
                      </td>
                    )}
                    {both && (
                      <td>
                        <span
                          className={`src ${r.source === "server" ? "srv" : ""}`}
                        >
                          {r.source}
                        </span>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <div className="mt-2.5 flex flex-wrap items-center gap-4 border-t border-border-soft px-2.5 pt-2.5 text-[12.5px] text-muted">
          <span className="mr-auto">
            {page.data
              ? joinSep([
                  `${num(rows.length)} of ${num(page.data.total)} events in this range`,
                  since && `the log reaches back to ${since}`,
                ])
              : ""}
          </span>
          {next && (
            <button
              type="button"
              className="link"
              disabled={more.busy || !fresh}
              onClick={() =>
                fresh &&
                more.load(() =>
                  api
                    .log(scope, filter, next, undefined, page.data?.from)
                    .then((p) => ({ items: p.rows, next: p.next })),
                )
              }
            >
              {more.busy ? "Loading…" : "Load earlier →"}
            </button>
          )}
        </div>
        {more.error && <ErrorBox error={more.error} />}
      </Panel>
    </>
  );
}

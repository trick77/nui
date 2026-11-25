import type { ReactNode } from "react";
import {
  api,
  type Alert,
  type AlertState,
  type Alerts,
  type DeliveryState,
  type Range,
  type Severity,
} from "./api";
import { SEP, num, when } from "./format";
import { useApi, useMore } from "./hooks";
import { scopeKey } from "./lists";
import {
  CardHead,
  Empty,
  ErrorBox,
  Panel,
  RowButton,
  SectionHead,
  Seg,
  Skeleton,
  useDash,
} from "./ui";

// What each kind of alert is about. Keys are the sender's: an unknown one,
// an inherited one (constructor) included, shows as its id.
const KIND: Record<string, string> = {
  "error.new": "New error",
  "failure.new": "New failure reason",
  "probe.new": "New probed path",
  "endpoint.new": "New endpoint",
  "referrer.new": "New referrer",
  "spike.addr": "Address spike",
  "spike.net": "Network spike",
  "trend.visitors": "Visitors trend",
  "trend.calls": "Calls trend",
  "trend.failed": "Failed calls trend",
  "trend.auto_share": "Unknown automated trend",
  "source.silent": "Source silent",
  "collector.refused": "Events refused",
  "collector.dropped": "Events dropped",
  "collector.unknown_domain": "Unknown site or wrong key",
  "job.failed": "Background job failed",
  "db.size_cap": "Database size cap",
  "llm.budget": "Model budget",
  "level.failed": "Failed calls level",
  "level.p95": "95th percentile level",
  "level.requests": "Requests level",
  "level.visitors": "Visitors level",
};

const kindLabel = (k: string) => (Object.hasOwn(KIND, k) ? KIND[k] : k);

const HOUR = 3_600_000;
const DAY = 24 * HOUR;

// The log's ranges, the smallest first, with how far back each surely
// reaches: a day range starts at a local midnight, so a day less than its
// days.
const LOG_REACH: [Range, number][] = [
  ["24h", 24 * HOUR],
  ["7d", 6 * DAY],
  ["30d", 29 * DAY],
  ["90d", 89 * DAY],
];

// The smallest log range that holds an alert's start, null when none does:
// the log would open empty there.
export function logRange(firstAt: string, now = Date.now()): Range | null {
  const age = now - Date.parse(firstAt);
  for (const [r, reach] of LOG_REACH) if (age <= reach) return r;
  return null;
}

// A label and its dot; a value the UI does not know shows as it came.
type Mark = [label: string, color: string | null];
const pick = <K extends string>(m: Record<K, Mark>, k: string): Mark =>
  Object.hasOwn(m, k) ? m[k as K] : [k, null];

const SEVERITY: Record<Severity, Mark> = {
  high: ["high", "var(--color-danger-ink)"],
  normal: ["normal", "var(--color-ochre)"],
  low: ["low", "var(--color-faint)"],
};
const STATE: Record<AlertState, Mark> = {
  open: ["open", "var(--color-ochre)"],
  cleared: ["cleared", "var(--color-online)"],
  once: ["one-off", null],
};
const DELIVERY: Record<DeliveryState, Mark> = {
  sent: ["sent", "var(--color-online)"],
  partial: ["partly sent", "var(--color-ochre)"],
  failed: ["failed", "var(--color-danger-ink)"],
  pending: ["pending", "var(--color-faint)"],
  none: ["not sent", null],
};

// A pill with its dot, or plain muted text for a mark without one.
function Tag({ mark }: { mark: Mark }) {
  const [label, color] = mark;
  if (!color) return <span className="text-[12.5px] text-muted">{label}</span>;
  return (
    <span className="pill">
      <i style={{ background: color }} />
      {label}
    </span>
  );
}

function Small({ children }: { children: ReactNode }) {
  return <span className="block text-[12px] text-muted">{children}</span>;
}

// A figure as sent: whole numbers whole, others to a tenth.
const figure = (f: number) => num(f, Number.isInteger(f) ? 0 : 1);

function Delivery({ d }: { d: Alert["delivery"] }) {
  // None may come as null.
  const channels = d.channels ?? [];
  return (
    <>
      <Tag mark={pick(DELIVERY, d.state)} />
      {d.at && <Small>{when(d.at)}</Small>}
      {channels.length > 0 && (
        <Small>
          {channels.map((c, i) => (
            <span key={c.name}>
              {i > 0 && SEP}
              <span className={c.ok ? "text-ink-dim" : "text-danger-ink"}>
                {c.name}
              </span>{" "}
              {c.ok ? "ok" : "failed"}
            </span>
          ))}
        </Small>
      )}
    </>
  );
}

// What the rules raised, newest first: the domain's, or every site's and
// the instance's own. Read-only; a row with a target leads where it
// happened, as a finding does.
export function AlertsPage({
  all,
  onAll,
  sites,
}: {
  all: boolean;
  onAll: (all: boolean) => void;
  // The listed domains: a row of another one leads there only if it is.
  sites: string[];
}) {
  const { site, goPanel } = useDash();
  const of = all ? "" : site.site;
  const key = scopeKey("alerts", { site: of });
  const page = useApi<Alerts>(key, (s) => api.alerts({ site: of }, s), of);
  const more = useMore<Alert>(key);
  const rows = [...(page.data?.alerts ?? []), ...more.items];
  // While another filter's first page loads, the rows and the cursor shown
  // are the previous one's: no paging from them.
  const fresh = !page.loading;
  const next = more.next ?? page.data?.next ?? "";
  // A row leads where it happened: the log over a range that holds it, a
  // panel as it is.
  const toLog = (a: Alert) => a.target?.panel === "log";
  const leads = (a: Alert) =>
    a.target != null &&
    (a.site === "" || sites.includes(a.site)) &&
    (!toLog(a) || logRange(a.first_at) != null);
  return (
    <>
      <SectionHead
        id="alerts"
        title="Alerts"
        lead="What the rules raised and where it was sent, newest first. The instance's own show under All sites."
      />
      <Panel label="Alerts" className="card" busy={!fresh}>
        <CardHead title="History">
          <Seg
            label="Alerts of"
            options={[
              { key: "site", label: site.site },
              { key: "all", label: "All sites" },
            ]}
            value={all ? "all" : "site"}
            onChange={(v) => onAll(v === "all")}
          />
        </CardHead>
        {page.error ? (
          <ErrorBox error={page.error} />
        ) : !page.data ? (
          <Skeleton h={320} label="Loading the alerts" />
        ) : !rows.length ? (
          <Empty>
            {all
              ? "Nothing has raised an alert yet."
              : "Nothing has raised an alert on this domain yet."}
          </Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="t">
              <thead>
                <tr>
                  <th>When</th>
                  {all && <th>Site</th>}
                  <th>Severity</th>
                  <th>Kind</th>
                  <th>What</th>
                  <th className="n">Figure</th>
                  <th>State</th>
                  <th>Delivery</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {rows.map((a) => {
                  const go = leads(a);
                  const time = (
                    <>
                      <span className="font-mono text-[12.5px] whitespace-nowrap">
                        {when(a.first_at)}
                      </span>
                      {a.last_at !== a.first_at && (
                        <Small>last {when(a.last_at)}</Small>
                      )}
                    </>
                  );
                  return (
                    <tr
                      key={a.id}
                      className={go ? "click" : ""}
                      onClick={
                        go
                          ? () =>
                              goPanel(
                                a.target!.panel,
                                a.target!.filter ?? {},
                                a.site || undefined,
                                toLog(a)
                                  ? (logRange(a.first_at) ?? undefined)
                                  : undefined,
                              )
                          : undefined
                      }
                    >
                      <td>{go ? <RowButton>{time}</RowButton> : time}</td>
                      {all && (
                        <td className="whitespace-nowrap">
                          {a.site || (
                            <span className="text-muted">Instance</span>
                          )}
                        </td>
                      )}
                      <td>
                        <Tag mark={pick(SEVERITY, a.severity)} />
                      </td>
                      <td
                        className="min-w-[110px] text-[12.5px] text-ink-dim"
                        title={a.kind}
                      >
                        {kindLabel(a.kind)}
                      </td>
                      <td className="min-w-[220px] wrap-anywhere">
                        {a.text}
                        {a.detail && <Small>{a.detail}</Small>}
                      </td>
                      <td className="n">
                        {a.figure != null && (
                          <>
                            {figure(a.figure)}
                            {a.unit && <Small>{a.unit}</Small>}
                          </>
                        )}
                      </td>
                      <td className="whitespace-nowrap">
                        <Tag mark={pick(STATE, a.state)} />
                        {a.state === "cleared" && a.cleared_at && (
                          <Small>{when(a.cleared_at)}</Small>
                        )}
                      </td>
                      <td className="whitespace-nowrap">
                        <Delivery d={a.delivery} />
                      </td>
                      <td className="text-faint">{go && "→"}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
        <div className="mt-2.5 flex flex-wrap items-center gap-4 border-t border-border-soft px-2.5 pt-2.5 text-[12.5px] text-muted">
          <span className="mr-auto">
            {page.data && rows.length > 0
              ? `${num(rows.length)} ${rows.length === 1 ? "alert" : "alerts"} shown`
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
                    .alerts({ site: of, before: next })
                    // null: the last page; useMore's null means none loaded.
                    .then((p) => ({ items: p.alerts, next: p.next ?? "" })),
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

import type { ReactNode } from "react";
import {
  api,
  type ClassKey,
  type ErrorBreakdown,
  type ErrorDetail as Detail,
  type ErrorOrigin,
  type ErrorWhere,
} from "./api";
import { SEP, dateTime, joinSep, num, pct, rawSince, when } from "./format";
import { useApi } from "./hooks";
import { scopeKey } from "./lists";
import {
  CLASSES,
  classColor,
  Empty,
  ErrorBox,
  Panel,
  Skeleton,
  useDash,
} from "./ui";

// What an error's origin says, short and in full.
const ORIGIN: Record<ErrorOrigin, [string, string]> = {
  withheld: [
    "Details withheld",
    "The browser hides this error's text, file and line, as it does when an error comes out of a script it treats as another site's. “Script error.” is the browser's stand-in, not a message of the page's code.",
  ],
  extension: [
    "Not the site's code",
    "Thrown by a browser extension, or by another script that is not the page's.",
  ],
  own: ["The site's code", "Thrown by a script of this domain."],
  other: [
    "Another host's script",
    "Thrown by a script this domain loads from another host.",
  ],
  unknown: [
    "No location",
    "The browser reported no file or line, as with a rejected promise.",
  ],
};

// Where an error happened, in a line.
export function whereText(w: ErrorWhere): string {
  return w.place || ORIGIN[w.origin][0];
}

// A log row's second line: where a script error came from, and on which
// page.
export function ErrorLine({
  where,
  page,
}: {
  where: ErrorWhere;
  page: string;
}) {
  return (
    <span
      className="mt-0.5 block text-[12px] text-muted"
      title={ORIGIN[where.origin][1]}
    >
      {ORIGIN[where.origin][0] + (page && `, on ${page}`)}
      {where.place && <span className="block">{where.place}</span>}
    </span>
  );
}

function Breakdown({
  title,
  of,
  label,
  className = "",
}: {
  title: string;
  of: ErrorBreakdown;
  label: (r: ErrorBreakdown["rows"][number]) => ReactNode;
  className?: string;
}) {
  return (
    <div className={`min-w-0 ${className}`}>
      <table className="t" aria-label={title}>
        <thead>
          <tr>
            <th>{title}</th>
            <th className="n">Count</th>
            <th className="n">Visitors</th>
          </tr>
        </thead>
        <tbody>
          {of.rows.map((r) => (
            <tr key={r.key}>
              <td className="wrap-anywhere">{label(r)}</td>
              <td className="n">{num(r.count)}</td>
              <td className="n">{num(r.visitors)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {of.more > 0 && (
        <p className="mx-2.5 mt-1 mb-0 text-[12px] text-muted">
          {num(of.more)} more
        </p>
      )}
    </div>
  );
}

const mono = (s: string) => (
  <span className="font-mono text-[12.5px]">{s}</span>
);

// One script error: whose it is, where it happens, whom it hits. Raw events
// only, so it may begin later than the range.
export function ErrorDetail({
  name,
  onClose,
}: {
  name: string;
  onClose: () => void;
}) {
  const { scope, openLog } = useDash();
  const d = useApi<Detail>(
    scopeKey("error", { scope, name }),
    (s) => api.error(scope, name, s),
    scope.site,
  );
  const since = rawSince(d.data?.detail_since);
  return (
    <Panel
      id="error-detail"
      label="Error"
      className="card mt-3"
      busy={d.loading}
    >
      <div className="flex flex-wrap items-baseline gap-3">
        <div className="eyebrow">Error</div>
        <button
          type="button"
          className="link ml-auto"
          onClick={() => openLog({ kind: "js_error", name })}
        >
          Every occurrence in the log →
        </button>
        <button type="button" className="link" onClick={onClose}>
          Close
        </button>
      </div>
      <div className="mt-1 font-mono text-[14px] wrap-anywhere">{name}</div>
      {d.error ? (
        <ErrorBox error={d.error} />
      ) : !d.data ? (
        <Skeleton h={220} label="Loading the error" />
      ) : d.data.count === 0 ? (
        <Empty>
          {since
            ? `No occurrence since ${since}: raw events reach back no further.`
            : "No occurrence in this range."}
        </Empty>
      ) : (
        <Body d={d.data} since={since} />
      )}
    </Panel>
  );
}

function Body({ d, since }: { d: Detail; since: string | null }) {
  const { goPanel } = useDash();
  return (
    <>
      <p className="mt-1.5 mb-0 text-[13px] text-ink-dim">
        {num(d.visitors)} {d.visitors === 1 ? "visitor" : "visitors"}
        {d.share != null && `, ${pct(d.share)} of ${num(d.viewers)}`}
        {SEP}
        {num(d.count)} recorded in {num(d.sessions)}{" "}
        {d.sessions === 1 ? "session" : "sessions"}
        {SEP}
        {d.first === d.last
          ? dateTime(d.last)
          : `${dateTime(d.first)} to ${dateTime(d.last)}`}
      </p>
      <p className="mt-1 mb-0 text-[12px] text-muted">
        {joinSep([
          "A page reports the same error at most 3 times, so the count can be low",
          since && `since ${since}`,
        ])}
      </p>
      <dl className="mt-3 mb-0 grid gap-2 text-[13px]">
        {d.origins.map((o) => (
          <div key={o}>
            <dt className="font-medium">{ORIGIN[o][0]}</dt>
            <dd className="m-0 text-ink-dim">{ORIGIN[o][1]}</dd>
          </div>
        ))}
      </dl>
      {d.stack.length > 0 && (
        <>
          <div className="eyebrow mt-3">
            Stack{SEP}the latest one{d.type && `, ${d.type}`}
          </div>
          <ol
            aria-label="Stack"
            className="mt-1.5 mb-0 list-none p-0 font-mono text-[12.5px] text-ink-dim"
          >
            {d.stack.map((f, i) => (
              <li key={i} className="wrap-anywhere">
                {f}
              </li>
            ))}
          </ol>
        </>
      )}
      <div className="mt-3 grid grid-cols-1 gap-x-4 gap-y-3 min-[700px]:grid-cols-2">
        <Breakdown
          title="Where"
          of={d.places}
          label={(r) => mono(r.where ? whereText(r.where) : r.key)}
          className="min-[700px]:col-span-2"
        />
        <Breakdown title="Page" of={d.pages} label={(r) => mono(r.key)} />
        <Breakdown
          title="Client"
          of={d.clients}
          label={(r) => r.key || "unknown"}
        />
        <Breakdown
          title="Class"
          of={d.classes}
          label={(r) => (
            <>
              <i
                className="sw"
                style={{ background: classColor(r.key as ClassKey) }}
              />
              {CLASSES.find((x) => x.key === r.key)?.label ?? r.key}
            </>
          )}
        />
      </div>
      <div className="eyebrow mt-4">
        Latest{SEP}open a visitor for what came before
      </div>
      <div className="overflow-x-auto">
        <table className="t" aria-label="Latest occurrences">
          <tbody>
            {d.recent.map((r, i) => (
              <tr key={i}>
                <td className="font-mono text-[12.5px] whitespace-nowrap">
                  {when(r.t)}
                </td>
                <td>
                  <button
                    type="button"
                    className="link font-mono !text-[12.5px] !text-ink"
                    title="This visitor's trail"
                    onClick={() => goPanel("visitors", { key: r.key })}
                  >
                    {r.addr}
                  </button>
                </td>
                <td className="text-[12px] whitespace-nowrap text-muted">
                  {r.client}
                </td>
                <td className="font-mono text-[12.5px] wrap-anywhere">
                  {r.page}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}

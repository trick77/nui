// The watch's two cards: probes that got an answer (Traffic) and the hosts
// the pages load scripts from (Content). One answer feeds both
// (Dashboard); the backend judges, the cards only show.

import { type ReactNode, useState } from "react";
import type { Watch, WatchHost, WatchPath } from "./api";
import { date, dateTime, num } from "./format";
import type { Loaded } from "./hooks";
import { type Col, TableHead, TableRow } from "./lists";
import { CardHead, Empty, ErrorBox, Panel, Skeleton, useDash } from "./ui";

// Rows a card shows before "Show all".
const SHOWN = 6;

const WHAT: Record<WatchPath["kind"], string> = {
  flip: "refused path now answered",
  hit: "a scanner got an answer",
  endpoint: "new target",
};

const mono = (s: string) => (
  <span className="font-mono text-[12.5px] wrap-anywhere">{s}</span>
);

const NewPill = () => (
  <span className="pill ml-1.5 !border-ochre/45 !text-ochre">new</span>
);
const Provisional = () => (
  <span
    className="pill ml-1.5 !text-muted"
    title="Of a day still open: judged on the day so far, it may go again"
  >
    provisional
  </span>
);

// Still learning: the date it ends is past now.
const learning = (until: string | null) =>
  until != null && Date.parse(until) > Date.now();

function Note({ children }: { children: ReactNode }) {
  return (
    <p className="mx-0.5 mt-2.5 mb-0 text-[12px] text-muted">{children}</p>
  );
}

// What the limits left out: the site's whole time, not the range's. A key
// left out on several days counts on each, so the number is no count of
// distinct paths.
function Cut({ n }: { n: number }) {
  if (n <= 0) return null;
  return (
    <Note>
      Past the watch's limits, a path or script host was left out {num(n)}{" "}
      {n === 1 ? "time" : "times"} (at most once a day each).
    </Note>
  );
}

// A card's rows, the first few until "Show all"; all of them when the row
// a jump names is further down.
function Rows<R>({
  rows,
  cols,
  rowKey,
  focused,
  onRow,
}: {
  rows: R[];
  cols: Col<R>[];
  rowKey: (r: R) => string;
  focused: string | null;
  onRow: (r: R) => ((r: R) => void) | undefined;
}) {
  const [all, setAll] = useState(false);
  const deep =
    focused != null && rows.findIndex((r) => rowKey(r) === focused) >= SHOWN;
  const shown = all || deep ? rows : rows.slice(0, SHOWN);
  return (
    <>
      <table className="t">
        <TableHead cols={cols} />
        <tbody>
          {shown.map((r) => (
            <TableRow
              key={rowKey(r)}
              r={r}
              cols={cols}
              onRow={onRow(r)}
              sel={rowKey(r) === focused}
            />
          ))}
        </tbody>
      </table>
      {shown.length < rows.length && (
        <button
          type="button"
          className="link mt-2 ml-2.5"
          onClick={() => setAll(true)}
        >
          Show all {num(rows.length)} →
        </button>
      )}
    </>
  );
}

// Probes that got a 2xx: a refused path now answered, a scanner's address
// answered, a new target. Feed sites only; the rows open their requests
// while they are kept (a scanner's: its address).
export function AnsweredProbes({ watch }: { watch: Loaded<Watch> }) {
  const { openLog, goPanel, focus } = useDash();
  const w = watch.data;
  if (w && !w.feed) return null;
  const cols: Col<WatchPath>[] = [
    {
      head: "Path",
      cell: (p) => (
        <>
          {mono(p.key)}
          {p.detail.map((d, i) => (
            <span key={i} className="block text-[12px] text-muted">
              {d}
            </span>
          ))}
        </>
      ),
    },
    {
      head: "What",
      cell: (p) => (
        <span className="whitespace-nowrap">
          {WHAT[p.kind] ?? p.kind}
          {p.provisional && <Provisional />}
        </span>
      ),
    },
    {
      head: "When",
      num: true,
      cell: (p) => (
        <>
          {dateTime(p.at)}
          {!p.raw && (
            <span className="block text-[12px] text-muted">
              requests no longer kept
            </span>
          )}
        </>
      ),
    },
  ];
  const open = (p: WatchPath) => {
    if (!p.raw) return undefined;
    if (p.kind === "hit" && p.hit_addr)
      return () => goPanel("visitors", { key: p.hit_addr });
    return () => openLog({ kind: "request", path: p.key });
  };
  return (
    <Panel
      id="answered-panel"
      label="Answered probes"
      className="card"
      busy={watch.loading}
    >
      <CardHead
        title="Answered probes"
        right={<span>probes that got a 2xx</span>}
      />
      {watch.error ? (
        <ErrorBox error={watch.error} />
      ) : !w ? (
        <Skeleton h={180} />
      ) : !w.ready ? (
        <Empty>
          Still reading the kept requests: answered probes show once that is
          done.
        </Empty>
      ) : !w.paths.length ? (
        <Empty>
          {learning(w.learning_until)
            ? `Learning which paths this site refuses until ${date(w.learning_until)}: nothing is judged before.`
            : "No probe got an answer in this range."}
        </Empty>
      ) : (
        <Rows
          rows={w.paths}
          cols={cols}
          rowKey={(p) => p.key}
          focused={focus?.panel === "answered" ? focus.key : null}
          onRow={open}
        />
      )}
      {w && <Cut n={w.cut} />}
    </Panel>
  );
}

// The hosts the pages loaded scripts from, the range's new ones first: a
// host nobody added is how injected code shows.
export function ScriptHosts({ watch }: { watch: Loaded<Watch> }) {
  const { focus } = useDash();
  const w = watch.data;
  const cols: Col<WatchHost>[] = [
    {
      head: "Host",
      cell: (h) => (
        <>
          {mono(h.host)}
          {h.new && <NewPill />}
          {h.provisional && <Provisional />}
        </>
      ),
    },
    { head: "First seen", num: true, cell: (h) => dateTime(h.first_seen) },
    { head: "Last seen", num: true, cell: (h) => dateTime(h.last_seen) },
    { head: "Addresses", num: true, cell: (h) => num(h.addrs) },
  ];
  return (
    <Panel
      id="scripts-panel"
      label="Script hosts"
      className="card"
      busy={watch.loading}
    >
      <CardHead
        title="Script hosts"
        right={<span>where the pages load scripts from</span>}
      />
      {watch.error ? (
        <ErrorBox error={watch.error} />
      ) : !w ? (
        <Skeleton h={180} />
      ) : !w.ready ? (
        <Empty>
          Still reading the kept requests: script hosts show once that is done.
        </Empty>
      ) : !w.hosts.length ? (
        <Empty>
          {w.scripts_learning_until == null
            ? "No page has reported its script hosts yet: the script sends them once a visitor interacts."
            : "No page loaded scripts from another host in this range."}
        </Empty>
      ) : (
        <>
          <Rows
            rows={w.hosts}
            cols={cols}
            rowKey={(h) => h.host}
            focused={focus?.panel === "scripts" ? focus.key : null}
            onRow={() => undefined}
          />
          <Note>
            Addresses: the most of one day that reported the host.
            {learning(w.scripts_learning_until) &&
              ` Learning the hosts until ${date(w.scripts_learning_until)}: none is new before.`}
          </Note>
        </>
      )}
      {w && <Cut n={w.cut} />}
    </Panel>
  );
}

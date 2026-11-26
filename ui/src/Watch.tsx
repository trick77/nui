// The watch's card: the hosts the pages load scripts from (Content, an
// API's section when its pages report them). The backend judges, the card
// only shows.

import { type ReactNode, useState } from "react";
import type { Watch, WatchHost } from "./api";
import { date, dateTime, num } from "./format";
import type { Loaded } from "./hooks";
import { type Col, TableHead, TableRow } from "./lists";
import { CardHead, Empty, ErrorBox, Panel, Skeleton, useDash } from "./ui";

// Rows the card shows before "Show all".
const SHOWN = 6;

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

// What the limits left out: the site's whole time, not the range's. A host
// left out on several days counts on each, so the number is no count of
// distinct hosts.
function Cut({ n }: { n: number }) {
  if (n <= 0) return null;
  return (
    <Note>
      Past the watch's limits, a script host was left out {num(n)}{" "}
      {n === 1 ? "time" : "times"} (at most once a day each).
    </Note>
  );
}

// The card's rows, the first few until "Show all"; all of them when the
// row a jump names is further down.
function Rows<R>({
  rows,
  cols,
  rowKey,
  focused,
}: {
  rows: R[];
  cols: Col<R>[];
  rowKey: (r: R) => string;
  focused: string | null;
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

// The hosts the pages loaded scripts from, the range's new ones first: a
// host nobody added is how injected code shows.
export function ScriptHosts({ watch }: { watch: Loaded<Watch> }) {
  const { focus, scope } = useDash();
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
            key={scope.site}
            rows={w.hosts}
            cols={cols}
            rowKey={(h) => h.host}
            focused={focus?.panel === "scripts" ? focus.key : null}
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

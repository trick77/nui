// The top-N rule: every list shows six rows from the
// backend, the rest as "Other", a switch between by volume and by change,
// and "Show all" with the full, searchable table. The backend ranks and
// cuts; the UI only draws what it was given.

import { type ReactNode, useEffect, useState } from "react";
import {
  api,
  type Compare,
  type Dim,
  type List,
  type ListRow,
  listScope,
} from "./api";
import { BarRows } from "./charts";
import { SEP, changeText, joinSep, num, pct, rawSince } from "./format";
import { useApi, useDebounced } from "./hooks";
import {
  CardHead,
  Delta,
  Empty,
  ErrorBox,
  Panel,
  RowButton,
  Skeleton,
  Tabs,
  useDash,
} from "./ui";

export type Sort = "volume" | "change";

export type Col<R = ListRow> = {
  head: string;
  num?: boolean;
  cell: (r: R) => ReactNode;
  // A column only some answers carry.
  show?: (rows: R[]) => boolean;
};

// A row's change against the comparison period. A list gives it a column
// only where changeShown says so.
export const changeCol: Col = {
  head: "Change",
  num: true,
  cell: (r) => <Delta change={r.change} verdict={r.verdict} />,
};

// Whether an answer's rows get a change column, the same for a card and its
// full list: the comparison on, figures in it (prev_since unset) and a
// change in some row (the backend nulls them all for a gap).
export function changeShown(
  cmp: Compare,
  l: Pick<List, "rows" | "prev_since">,
): boolean {
  return cmp !== "off" && !l.prev_since && l.rows.some((r) => r.change);
}

export type ListTab = {
  key: string;
  label: string;
  dim: Dim;
  sub?: string;
  cat?: string;
  // The tab's own columns, in place of the list's.
  cols?: Col[];
  // Ranked by volume only: no order switch.
  volumeOnly?: boolean;
  // What the head says beside the tabs while this one is picked.
  head?: ReactNode;
};

export function scopeKey(name: string, extra: unknown = null): string {
  return `${name}|${JSON.stringify(extra)}`;
}

type Since = Pick<List, "since" | "prev_since">;

// Where an answer's figures begin, when within the range, and its
// comparison's: "" when both cover theirs.
function sinceNote(l: Since): string {
  return joinSep([
    l.since && `figures since ${rawSince(l.since)}`,
    // Up to the comparison's end or short of it: the answer does not say
    // which.
    l.prev_since &&
      `the comparison has no figures before ${rawSince(l.prev_since)}`,
  ]);
}

// sinceNote under a card, nothing when it is "".
export function SinceNote({ of }: { of: Since | null }) {
  const note = of ? sinceNote(of) : "";
  if (!note) return null;
  return <p className="mx-0.5 mt-2.5 mb-0 text-[12px] text-muted">{note}</p>;
}

export function useList(
  dim: Dim,
  sort: Sort,
  opts: {
    sub?: string;
    cat?: string;
    within?: string;
    limit?: number;
    q?: string;
  } = {},
) {
  const { scope } = useDash();
  return useApi<List>(
    scopeKey("list", { scope: listScope(scope, dim), dim, sort, ...opts }),
    (s) => api.list(scope, { dim, sort, ...opts }, s),
    scope.site,
  );
}

export function TopList({
  id,
  title,
  sources,
  tabs,
  head,
  cols,
  label,
  note,
  onRow,
  expand,
  groups,
  above,
  busy = false,
  empty,
}: {
  // Something above the list (busy) is loading too.
  busy?: boolean;
  id?: string;
  title: string;
  sources?: ("browser" | "server")[];
  tabs: ListTab[];
  // A tab after them per group the answer names (groups), labelled so: the
  // first tab's list of that group (cat).
  groups?: (group: string) => string;
  head?: ReactNode;
  // A table when cols are given, ranked bars otherwise.
  cols?: Col[];
  label?: (r: ListRow) => ReactNode;
  // The text label shows after the row's name, for the bar's title: a long
  // label is cut.
  note?: (r: ListRow) => string | null | undefined;
  onRow?: (r: ListRow) => void;
  // Rows the backend marks expandable open into their raw values.
  expand?: boolean;
  above?: ReactNode;
  empty: string;
}) {
  const { scope } = useDash();
  const [tab, setTab] = useState(tabs[0].key);
  const [sort, setSort] = useState<Sort>("volume");
  const [all, setAll] = useState(false);
  const [known, setKnown] = useState<string[]>([]);
  // A group's key holds a colon, which no tab's or group's does.
  const groupTab = (g: string): ListTab => ({
    key: `cat:${g}`,
    label: groups ? groups(g) : g,
    dim: tabs[0].dim,
    cat: g,
  });
  const t =
    tabs.find((x) => x.key === tab) ??
    (groups && tab.startsWith("cat:") ? groupTab(tab.slice(4)) : tabs[0]);
  // The order picked stays for the tabs that have one.
  const order = t.volumeOnly ? "volume" : sort;
  const shownCols = t.cols ?? cols;
  const fetched = useList(t.dim, order, { sub: t.sub, cat: t.cat });
  // Another dim's answer, kept while this tab's loads, is not drawn: its
  // rows would sit under this tab's columns.
  const [dataDim, setDataDim] = useState(t.dim);
  if (fetched.data && !fetched.loading && dataDim !== t.dim) setDataDim(t.dim);
  const l =
    fetched.loading && dataDim !== t.dim ? { ...fetched, data: null } : fetched;
  // A group the last answer does not have: back to the first tab, without
  // asking for the group first where the answer kept while loading tells.
  if (t.cat && l.data && !l.data.groups?.includes(t.cat)) setTab(tabs[0].key);
  // The groups last answered: the bar stays while another domain's answer
  // loads or a request fails, the group picked in it.
  const answered = l.data && (l.data.groups ?? []);
  if (answered && answered.join() !== known.join()) setKnown(answered);
  const bar = answered ?? known;
  const shown = groups
    ? [
        ...tabs,
        ...(t.cat && !bar.includes(t.cat) ? [...bar, t.cat].sort() : bar).map(
          groupTab,
        ),
      ]
    : tabs;
  const compare = scope.cmp !== "off";
  const changes = !!l.data && changeShown(scope.cmp, l.data);
  return (
    <Panel id={id} label={title} className="card" busy={busy || l.loading}>
      <CardHead
        title={title}
        sources={sources}
        right={
          <>
            {head}
            {t.head}
            {shown.length > 1 && (
              <Tabs
                label={`${title} view`}
                options={shown.map((x) => ({ key: x.key, label: x.label }))}
                value={t.key}
                onChange={setTab}
              />
            )}
            {compare && !t.volumeOnly && !l.data?.prev_since && (
              <span
                className={
                  shown.length > 1 ? "border-l border-border pl-3.5" : ""
                }
              >
                <Tabs
                  label={`${title} order`}
                  options={[
                    { key: "volume" as Sort, label: "By volume" },
                    { key: "change" as Sort, label: "By change" },
                  ]}
                  value={sort}
                  onChange={setSort}
                />
              </span>
            )}
          </>
        }
      />
      {above}
      {l.error ? (
        <ErrorBox error={l.error} />
      ) : !l.data || (l.loading && !l.data.rows.length) ? (
        <Skeleton h={180} />
      ) : !l.data.rows.length ? (
        <Empty>{empty}</Empty>
      ) : (
        <>
          {shownCols ? (
            <RowsTable
              // A row left open is not another tab's.
              key={t.key}
              list={l.data}
              cols={shownCols}
              onRow={onRow}
              // A tab of its own shape opens no row.
              expand={expand && !t.volumeOnly ? t.dim : undefined}
              sort={order}
            />
          ) : (
            <BarRows
              rows={l.data.rows.map((r) => ({
                key: r.key,
                label: label ? label(r) : r.label,
                value: r.value,
                share: r.share,
                change: changes ? r.change : null,
                verdict: r.verdict,
                onClick: onRow ? () => onRow(r) : undefined,
                title: joinSep([
                  r.label,
                  note?.(r),
                  num(r.value),
                  pct(r.share),
                  changes && r.change && changeText(r.change),
                  onRow && "click for the log",
                ]),
              }))}
              other={l.data.other}
              changes={changes}
            />
          )}
          {l.data.total > l.data.rows.length && (
            <button
              type="button"
              className="link mt-2 ml-2.5"
              disabled={l.loading}
              onClick={() => setAll(true)}
            >
              Show all {num(l.data.total)} →
            </button>
          )}
        </>
      )}
      <SinceNote of={l.data} />
      {all && (
        <AllRows
          title={t.label === title ? title : `${title}${SEP}${t.label}`}
          tab={t}
          cols={shownCols}
          label={label}
          // What a row opens is behind the dialog, or on another page.
          onRow={
            onRow &&
            ((r) => {
              setAll(false);
              onRow(r);
            })
          }
          initialSort={order}
          onClose={() => setAll(false)}
        />
      )}
    </Panel>
  );
}

function RowsTable({
  list,
  cols: allCols,
  onRow,
  expand,
  sort,
}: {
  list: List;
  cols: Col[];
  onRow?: (r: ListRow) => void;
  expand?: Dim;
  sort: Sort;
}) {
  const { scope } = useDash();
  const [open, setOpen] = useState<string | null>(null);
  const cols = allCols.filter(
    (c) =>
      (!c.show || c.show(list.rows)) &&
      (c !== changeCol || changeShown(scope.cmp, list)),
  );
  return (
    <table className="t ranked">
      <TableHead cols={cols} />
      <tbody>
        {list.rows.map((r, i) => (
          <Row
            key={i}
            r={r}
            cols={cols}
            onRow={onRow}
            expandDim={expand && r.expandable ? expand : undefined}
            open={open === r.key}
            onToggle={() => setOpen(open === r.key ? null : r.key)}
            sort={sort}
          />
        ))}
        {list.other.count > 0 && (
          <tr>
            <td className="text-muted">
              Other{SEP}
              {num(list.other.count)}
            </td>
            {cols.slice(1).map((c, j) => (
              <td key={c.head} className="n text-muted">
                {j + 1 === cols.findIndex((x) => x.num)
                  ? num(list.other.value)
                  : ""}
              </td>
            ))}
          </tr>
        )}
      </tbody>
    </table>
  );
}

export function TableHead<R>({ cols }: { cols: Col<R>[] }) {
  return (
    <thead>
      <tr>
        {cols.map((c) => (
          <th key={c.head} className={c.num ? "n" : ""}>
            {c.head}
          </th>
        ))}
      </tr>
    </thead>
  );
}

// One row of a table: clickable with onRow, its first cell then the row's
// button (the keyboard's way in). lead goes before the first cell's
// content (an expander), indent moves that cell in (a row under another);
// sel marks the row picked or jumped to.
export function TableRow<R>({
  r,
  cols,
  onRow,
  lead,
  indent = false,
  sel = false,
  className = "",
}: {
  r: R;
  cols: Col<R>[];
  onRow?: (r: R) => void;
  lead?: ReactNode;
  indent?: boolean;
  sel?: boolean;
  className?: string;
}) {
  const first = (c: Col<R>) =>
    onRow ? <RowButton>{c.cell(r)}</RowButton> : c.cell(r);
  return (
    <tr
      className={
        [className, onRow && "click", sel && "sel"].filter(Boolean).join(" ") ||
        undefined
      }
      aria-selected={sel || undefined}
      onClick={onRow ? () => onRow(r) : undefined}
    >
      {cols.map((c, j) => (
        <td
          key={c.head}
          className={c.num ? "n" : j === 0 && indent ? "pl-8" : ""}
        >
          {j > 0 ? (
            c.cell(r)
          ) : lead ? (
            <span className="inline-flex items-baseline gap-1.5">
              {lead}
              {first(c)}
            </span>
          ) : (
            first(c)
          )}
        </td>
      ))}
    </tr>
  );
}

function Row({
  r,
  cols,
  onRow,
  expandDim,
  open,
  onToggle,
  sort,
}: {
  r: ListRow;
  cols: Col[];
  onRow?: (r: ListRow) => void;
  expandDim?: Dim;
  open: boolean;
  onToggle: () => void;
  sort: Sort;
}) {
  return (
    <>
      <TableRow
        r={r}
        cols={cols}
        onRow={onRow}
        lead={
          expandDim && (
            <button
              type="button"
              className="link !text-muted"
              aria-expanded={open}
              aria-label={`${open ? "Close" : "Open"} ${r.label}`}
              onClick={(e) => {
                e.stopPropagation();
                onToggle();
              }}
            >
              {open ? "▾" : "▸"}
            </button>
          )
        }
      />
      {open && expandDim && (
        <Within
          dim={expandDim}
          template={r.key}
          cols={cols}
          onRow={onRow}
          sort={sort}
        />
      )}
    </>
  );
}

// The raw values inside a template, as rows under it.
function Within({
  dim,
  template,
  cols,
  onRow,
  sort,
}: {
  dim: Dim;
  template: string;
  cols: Col[];
  onRow?: (r: ListRow) => void;
  sort: Sort;
}) {
  const l = useList(dim, sort, { within: template });
  // The paths are raw events, the template's row the whole range.
  const since = rawSince(l.data?.detail_since);
  if (l.error)
    return (
      <tr>
        <td colSpan={cols.length}>
          <ErrorBox error={l.error} />
        </td>
      </tr>
    );
  // Rows of another template or scope are never shown under this one.
  if (!l.data || l.loading)
    return (
      <tr>
        <td colSpan={cols.length}>
          <Skeleton h={24} />
        </td>
      </tr>
    );
  return (
    <>
      {l.data.rows.map((r, i) => (
        <TableRow
          key={i}
          r={r}
          cols={cols}
          onRow={onRow}
          indent
          className="bg-white/[.02]"
        />
      ))}
      {(l.data.other.count > 0 || since) && (
        <tr className="bg-white/[.02]">
          <td className="pl-8 text-muted" colSpan={cols.length}>
            {!l.data.rows.length
              ? `No paths since ${since}`
              : joinSep([
                  l.data.other.count > 0 &&
                    `and ${num(l.data.other.count)} more`,
                  since && `paths since ${since}`,
                ])}
          </td>
        </tr>
      )}
    </>
  );
}

const ALL_LIMIT = 500;

// The full list: searchable, by volume or by change, in an overlay.
function AllRows({
  title,
  tab,
  cols,
  label,
  onRow,
  initialSort,
  onClose,
}: {
  title: string;
  tab: ListTab;
  cols?: Col[];
  label?: (r: ListRow) => ReactNode;
  onRow?: (r: ListRow) => void;
  initialSort: Sort;
  onClose: () => void;
}) {
  const { scope } = useDash();
  const [sort, setSort] = useState<Sort>(initialSort);
  const [q, setQ] = useState("");
  const dq = useDebounced(q.trim(), 250);
  const l = useList(tab.dim, tab.volumeOnly ? "volume" : sort, {
    sub: tab.sub,
    cat: tab.cat,
    limit: ALL_LIMIT,
    q: dq || undefined,
  });
  useEffect(() => {
    const f = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", f);
    return () => window.removeEventListener("keydown", f);
  }, [onClose]);
  const tableCols: Col[] = cols ?? [
    { head: "Name", cell: (r) => (label ? label(r) : r.label) },
    { head: "Count", num: true, cell: (r) => num(r.value) },
    { head: "Share", num: true, cell: (r) => pct(r.share) },
    changeCol,
  ];
  return (
    <div
      className="overlay"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className="panel card max-h-[calc(100vh-120px)] w-full max-w-[980px] overflow-auto"
      >
        <CardHead
          title={title}
          right={
            <>
              {scope.cmp !== "off" &&
                !tab.volumeOnly &&
                !l.data?.prev_since && (
                  <Tabs
                    label="Order"
                    options={[
                      { key: "volume" as Sort, label: "By volume" },
                      { key: "change" as Sort, label: "By change" },
                    ]}
                    value={sort}
                    onChange={setSort}
                  />
                )}
              <input
                className="field w-[230px]"
                name="search"
                placeholder="Search"
                aria-label="Search"
                value={q}
                onChange={(e) => setQ(e.target.value)}
                autoFocus
              />
              <button type="button" className="link" onClick={onClose}>
                Close
              </button>
            </>
          }
        />
        {l.error ? (
          <ErrorBox error={l.error} />
        ) : !l.data || (l.loading && !l.data.rows.length) ? (
          <Skeleton h={240} />
        ) : !l.data.rows.length ? (
          <Empty>Nothing matches “{dq}”.</Empty>
        ) : (
          <div
            className={l.loading ? "busy" : ""}
            aria-busy={l.loading || undefined}
          >
            <RowsTable
              list={l.data}
              cols={tableCols}
              onRow={onRow}
              sort={sort}
            />
            <p className="mt-2.5 mb-0 text-[12.5px] text-muted">
              {num(l.data.rows.length)} of {num(l.data.total)}
              {l.data.total > l.data.rows.length &&
                `${SEP}the first ${num(ALL_LIMIT)} are shown, search to narrow`}
              {sinceNote(l.data) && `${SEP}${sinceNote(l.data)}`}
            </p>
          </div>
        )}
      </div>
    </div>
  );
}

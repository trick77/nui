// The top-N rule: every list shows six rows from the
// backend, the rest as "Other", a switch between by volume and by change,
// and "Show all" with the full, searchable table. The backend ranks and
// cuts; the UI only draws what it was given.

import { type ReactNode, useEffect, useState } from "react";
import { api, type Dim, type List, type ListRow } from "./api";
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

export type Col = {
  head: string;
  num?: boolean;
  cell: (r: ListRow) => ReactNode;
  // A column only some answers carry.
  show?: (rows: ListRow[]) => boolean;
};

export type ListTab = { key: string; label: string; dim: Dim; sub?: string };

export function scopeKey(name: string, extra: unknown = null): string {
  return `${name}|${JSON.stringify(extra)}`;
}

export function useList(
  dim: Dim,
  sort: Sort,
  opts: { sub?: string; within?: string; limit?: number; q?: string } = {},
  enabled = true,
) {
  const { scope } = useDash();
  return useApi<List>(
    enabled ? scopeKey("list", { scope, dim, sort, ...opts }) : null,
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
  onRow,
  expand,
  above,
  busy = false,
  empty,
  className = "",
}: {
  // Something above the list (busy) is loading too.
  busy?: boolean;
  id?: string;
  title: string;
  sources?: ("browser" | "server")[];
  tabs: ListTab[];
  head?: ReactNode;
  // A table when cols are given, ranked bars otherwise.
  cols?: Col[];
  label?: (r: ListRow) => ReactNode;
  onRow?: (r: ListRow) => void;
  // Rows the backend marks expandable open into their raw values.
  expand?: boolean;
  above?: ReactNode;
  empty: string;
  className?: string;
}) {
  const { scope } = useDash();
  const [tab, setTab] = useState(tabs[0].key);
  const [sort, setSort] = useState<Sort>("volume");
  const [all, setAll] = useState(false);
  const t = tabs.find((x) => x.key === tab) ?? tabs[0];
  const l = useList(t.dim, sort, { sub: t.sub });
  const compare = scope.cmp !== "off";
  return (
    <Panel
      id={id}
      label={title}
      className={`card ${className}`}
      busy={busy || l.loading}
    >
      <CardHead
        title={title}
        sources={sources}
        right={
          <>
            {head}
            {tabs.length > 1 && (
              <Tabs
                label={`${title} view`}
                options={tabs.map((x) => ({ key: x.key, label: x.label }))}
                value={t.key}
                onChange={setTab}
              />
            )}
            {compare && (
              <span
                className={
                  tabs.length > 1 ? "border-l border-border pl-3.5" : ""
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
      ) : !l.data ? (
        <Skeleton h={180} />
      ) : !l.data.rows.length ? (
        <Empty>{empty}</Empty>
      ) : (
        <>
          {cols ? (
            <RowsTable
              list={l.data}
              cols={cols}
              onRow={onRow}
              expand={expand ? t.dim : undefined}
              sort={sort}
            />
          ) : (
            <BarRows
              rows={l.data.rows.map((r) => ({
                key: r.key,
                label: label ? label(r) : r.label,
                value: r.value,
                share: r.share,
                change: compare ? r.change : null,
                verdict: r.verdict,
                onClick: onRow ? () => onRow(r) : undefined,
                title: joinSep([
                  r.label,
                  num(r.value),
                  pct(r.share),
                  r.change && changeText(r.change),
                  onRow && "click for the log",
                ]),
              }))}
              other={l.data.other}
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
      {all && (
        <AllRows
          title={t.label === title ? title : `${title}${SEP}${t.label}`}
          tab={t}
          cols={cols}
          label={label}
          // What a row opens is behind the dialog, or on another page.
          onRow={
            onRow &&
            ((r) => {
              setAll(false);
              onRow(r);
            })
          }
          initialSort={sort}
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
  const [open, setOpen] = useState<string | null>(null);
  const cols = allCols.filter((c) => !c.show || c.show(list.rows));
  return (
    <table className="t">
      <thead>
        <tr>
          {cols.map((c) => (
            <th key={c.head} className={c.num ? "n" : ""}>
              {c.head}
            </th>
          ))}
        </tr>
      </thead>
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

// A cell's content; in a clickable row's first cell, the row's button.
function cell(c: Col, r: ListRow, onRow?: (r: ListRow) => void) {
  return onRow ? <RowButton>{c.cell(r)}</RowButton> : c.cell(r);
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
      <tr
        className={onRow ? "click" : ""}
        onClick={onRow ? () => onRow(r) : undefined}
      >
        {cols.map((c, j) => (
          <td key={c.head} className={c.num ? "n" : ""}>
            {j === 0 && expandDim ? (
              <span className="inline-flex items-baseline gap-1.5">
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
                {cell(c, r, onRow)}
              </span>
            ) : (
              cell(c, r, j === 0 ? onRow : undefined)
            )}
          </td>
        ))}
      </tr>
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
        <tr
          key={i}
          className={`bg-white/[.02] ${onRow ? "click" : ""}`}
          onClick={onRow ? () => onRow(r) : undefined}
        >
          {cols.map((c, j) => (
            <td key={c.head} className={c.num ? "n" : j === 0 ? "pl-8" : ""}>
              {cell(c, r, j === 0 ? onRow : undefined)}
            </td>
          ))}
        </tr>
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
  const l = useList(tab.dim, sort, {
    sub: tab.sub,
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
    {
      head: "Change",
      num: true,
      cell: (r) => <Delta change={r.change} verdict={r.verdict} />,
    },
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
              {scope.cmp !== "off" && (
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
        ) : !l.data ? (
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
            </p>
          </div>
        )}
      </div>
    </div>
  );
}

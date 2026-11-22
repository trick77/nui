import type { ListRow } from "./api";
import { num } from "./format";
import { type Col, TopList } from "./lists";
import { Delta, SectionHead, useDash } from "./ui";

const mono = (s: string) => (
  <span className="font-mono text-[12.5px] wrap-anywhere">{s}</span>
);
const change: Col = {
  head: "Change",
  num: true,
  cell: (r) => <Delta change={r.change} verdict={r.verdict} />,
};

export function Content() {
  const { openLog, scope } = useDash();
  const cmp = scope.cmp !== "off";
  const pageCols: Col[] = [
    { head: "Path", cell: (r) => mono(r.label) },
    { head: "Visitors", num: true, cell: (r) => num(r.value) },
    { head: "Views", num: true, cell: (r) => num(r.second) },
    // Shown only when the backend sends entries per page.
    {
      head: "Entry",
      num: true,
      cell: (r) => num(r.entry),
      show: (rows) => rows.some((r) => r.entry != null),
    },
    ...(cmp ? [change] : []),
  ];
  const eventCols: Col[] = [
    {
      head: "Event",
      cell: (r: ListRow) => (
        <>
          {mono(r.label)}
          {r.sub && (
            <span className="ml-1.5 text-[12px] text-muted">{r.sub}</span>
          )}
        </>
      ),
    },
    { head: "Count", num: true, cell: (r) => num(r.value) },
    { head: "Visitors", num: true, cell: (r) => num(r.second) },
    ...(cmp ? [change] : []),
  ];
  return (
    <>
      <SectionHead
        id="content"
        title="Content"
        lead="What people look at, where they came from and what they clicked."
        sources={["browser"]}
      />
      <div className="grid grid-cols-1 gap-3 min-[1000px]:grid-cols-2">
        <TopList
          id="pages-panel"
          title="Pages"
          tabs={[
            { key: "top", label: "Top", dim: "pages" },
            { key: "entry", label: "Entry", dim: "entry" },
          ]}
          cols={pageCols}
          expand
          onRow={(r) => openLog({ path: r.key })}
          empty="No page views in this range yet."
        />
        <TopList
          id="events-panel"
          title="Events"
          tabs={[
            { key: "all", label: "All", dim: "events" },
            { key: "outbound", label: "Outbound links", dim: "outbound" },
            { key: "custom", label: "Custom", dim: "events", sub: "custom" },
          ]}
          cols={eventCols}
          // An automatic event's key is its kind; a custom one's and an
          // outbound link's, the name the log matches as text.
          onRow={(r) =>
            openLog(r.sub === "auto" ? { kind: r.key } : { path: r.key })
          }
          empty="No events in this range yet."
        />
      </div>
      <div className="mt-3 grid grid-cols-1 gap-3 min-[1000px]:grid-cols-2">
        <TopList
          id="referrers-panel"
          title="Referrers"
          tabs={[{ key: "referrers", label: "Referrers", dim: "referrers" }]}
          empty="No referrers in this range yet."
        />
        <TopList
          id="networks-panel"
          title="Networks"
          tabs={[{ key: "networks", label: "Networks", dim: "networks" }]}
          empty="No networks in this range yet."
        />
      </div>
    </>
  );
}

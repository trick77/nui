import { api, type Classes, type ListRow, type Status } from "./api";
import { StackChart, Strip, type StripPart } from "./charts";
import { SEP, joinSep, num, pct } from "./format";
import { type Loaded, useApi } from "./hooks";
import { TopList, scopeKey } from "./lists";
import {
  AllClasses,
  CLASSES,
  CardHead,
  Delta,
  Empty,
  ErrorBox,
  GenText,
  Panel,
  SectionHead,
  Skeleton,
  shownClasses,
  useDash,
} from "./ui";

// Who is asking: every class's share of all requests, whatever the
// audience switch says (server only).
export function WhoIsAsking({ classes }: { classes: Loaded<Classes> }) {
  const { apiClients } = useDash();
  const c = classes.data;
  const listed = c?.rows.filter((r) => r.class !== "api" || apiClients) ?? [];
  return (
    <Panel label="Who is asking" className="card" busy={classes.loading}>
      <CardHead title="Who is asking" sources={["server"]} />
      {classes.error ? (
        <ErrorBox error={classes.error} />
      ) : !c ? (
        <Skeleton h={220} />
      ) : !c.rows.length ? (
        <Empty>No requests from the server feed in this range.</Empty>
      ) : (
        <>
          <Strip
            label="Requests by class"
            parts={listed.map((r) => ({
              key: r.class,
              label: CLASSES.find((x) => x.key === r.class)?.label ?? r.class,
              share: r.share,
              value: r.requests,
              color:
                CLASSES.find((x) => x.key === r.class)?.color ??
                "var(--color-other)",
            }))}
          />
          <table className="t mt-3">
            <thead>
              <tr>
                <th>Class</th>
                <th className="n">Requests</th>
                <th className="n">Share</th>
                <th className="n">Addresses</th>
                <th className="n">Change</th>
              </tr>
            </thead>
            <tbody>
              {listed.map((r) => {
                const k = CLASSES.find((x) => x.key === r.class);
                return (
                  <tr key={r.class}>
                    <td>
                      <i className="sw" style={{ background: k?.color }} />
                      {k?.label ?? r.class}
                    </td>
                    <td className="n">{num(r.requests)}</td>
                    <td className="n">{pct(r.share)}</td>
                    <td className="n">{num(r.addresses)}</td>
                    <td className="n">
                      <Delta change={r.change} verdict={r.verdict} />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          <p className="mx-0.5 mt-2.5 mb-0 text-[12px] text-muted">
            Classified by: the account in the feed
            {SEP}
            ipverse bot list (CIDR, user agent, reverse DNS)
            {SEP}
            <span className="font-mono">navigator.webdriver</span>
            {SEP}
            {joinSep([
              "no script beacon after a page request",
              "no asset requests",
              "hosting network",
              "no pointer, key or scroll input.",
            ])}
          </p>
        </>
      )}
    </Panel>
  );
}

// Errors: JS errors, and the server's 5xx where there is a feed. Part of
// Traffic, so every class, whatever the audience switch says.
export function ErrorsPanel() {
  return (
    <AllClasses>
      <Errors />
    </AllClasses>
  );
}

function Errors() {
  const { feed, openLog } = useDash();
  return (
    <TopList
      id="errors-panel"
      title="Errors"
      sources={["browser", "server"]}
      head={feed ? undefined : <span>JavaScript errors</span>}
      tabs={[{ key: "errors", label: "Errors", dim: "errors" }]}
      cols={[
        {
          head: "Error",
          cell: (r) => (
            <>
              {r.label}
              {r.change && "new" in r.change && (
                <span className="pill ml-1.5 !border-ochre/45 !text-ochre">
                  new
                </span>
              )}
            </>
          ),
        },
        {
          head: "Where",
          cell: (r) => <span className="text-[12px] text-muted">{r.sub}</span>,
        },
        { head: "Count", num: true, cell: (r) => num(r.value) },
        { head: "Visitors", num: true, cell: (r) => num(r.second) },
      ]}
      onRow={(r) => openLog({ path: r.label })}
      empty="No errors in this range."
    />
  );
}

const STATUS_COLOR: Record<string, string> = {
  "2xx": "var(--color-online)",
  "3xx": "var(--color-other)",
  "4xx": "var(--color-ochre)",
  "5xx": "var(--color-danger)",
};

const mono = (s: string) => (
  <span className="font-mono text-[12.5px]">{s}</span>
);

// The server's view: requests per class, errors, probed paths, refused
// requests by address. Shown only on a domain with a feed, always over
// every class.
export function Traffic(props: { classes: Loaded<Classes> }) {
  return (
    <AllClasses>
      <TrafficPanels {...props} />
    </AllClasses>
  );
}

function TrafficPanels({ classes }: { classes: Loaded<Classes> }) {
  const { scope, openLog, apiClients } = useDash();
  const shown = shownClasses(apiClients);
  const st = useApi<Status>(
    scopeKey("status", scope),
    (s) => api.status(scope, s),
    scope.site,
  );
  const c = classes.data;
  return (
    <>
      <SectionHead
        id="traffic"
        title="Traffic"
        lead={`Who is asking: people, ${apiClients ? "signed-in API clients, " : ""}known bots from the ipverse list, uptime probes, and automation nobody announced.`}
        sources={["server"]}
      />
      <div className="grid grid-cols-1 gap-3 min-[1000px]:grid-cols-2">
        <Panel
          label="Requests by class"
          className="card"
          busy={classes.loading}
        >
          <CardHead
            title="Requests by class"
            right={
              <div className="legend">
                {shown.map((k) => (
                  <span key={k.key}>
                    <i className="sw" style={{ background: k.color }} />
                    {k.label}
                  </span>
                ))}
              </div>
            }
          />
          {classes.error ? (
            <ErrorBox error={classes.error} />
          ) : !c ? (
            <Skeleton h={250} />
          ) : (
            <StackChart
              labels={c.labels}
              series={c.series}
              classes={shown}
              axis={c}
              open={c.open}
              partial={c.partial}
            />
          )}
        </Panel>
        <ErrorsPanel />
      </div>
      <div className="mt-3 grid grid-cols-1 gap-3 min-[1000px]:grid-cols-2">
        <TopList
          id="probed-panel"
          title="Probed paths"
          head={<span>requests answered 401, 403 or 404</span>}
          tabs={[{ key: "probed", label: "Probed paths", dim: "probed" }]}
          label={(r: ListRow) => (
            <>
              {mono(r.label)}{" "}
              <GenText
                gen={r.gen}
                text={r.sub}
                className="ml-2 text-[12px] text-muted"
              />
            </>
          )}
          onRow={(r) => openLog({ path: r.key })}
          empty="Nothing probed in this range."
        />
        <TopList
          id="refused-panel"
          title="Refused requests by address"
          head={<span>not answered 2xx or 3xx</span>}
          tabs={[{ key: "refused", label: "Refused", dim: "refused" }]}
          busy={st.loading}
          above={
            st.error ? (
              <ErrorBox error={st.error} />
            ) : st.data && st.data.rows.length ? (
              <div className="mb-3.5">
                <Strip
                  label="Requests by status class"
                  parts={st.data.rows.map((r): StripPart => ({
                    key: r.class,
                    label: r.class,
                    share: r.share,
                    value: r.value,
                    color: STATUS_COLOR[r.class] ?? "var(--color-other)",
                  }))}
                />
              </div>
            ) : null
          }
          label={(r: ListRow) => (
            <>
              {mono(r.label)}
              {r.sub && (
                <span className="ml-1.5 text-[12px] text-muted">{r.sub}</span>
              )}
            </>
          )}
          onRow={(r) => openLog({ addr: r.key })}
          empty="Nothing refused in this range."
        />
      </div>
    </>
  );
}

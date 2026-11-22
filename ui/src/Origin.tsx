import { api, type Audience, type Clients, type ShareRow } from "./api";
import { Strip } from "./charts";
import { countryName, num } from "./format";
import { useApi } from "./hooks";
import { scopeKey, useList } from "./lists";
import { WorldMap } from "./map";
import { AUD_NOTE } from "./Overview";
import {
  CardHead,
  Empty,
  ErrorBox,
  Panel,
  SectionHead,
  Skeleton,
  useDash,
} from "./ui";

const MAP_TITLE: Record<Audience, string> = {
  all: "Where visitors come from",
  humans: "Where people are",
  bots: "Where bots come from",
};

// Every country with data, for the map; the backend still ranks them.
const MAP_LIMIT = 300;

const RAMP = [
  "var(--color-seq-4)",
  "var(--color-seq-3)",
  "var(--color-seq-2)",
  "var(--color-seq-1)",
];

function shareParts(rows: ShareRow[]) {
  return rows.map((r, i) => ({
    key: r.key,
    label: r.key,
    share: r.share,
    value: r.value,
    color: r.key === "Other" ? "var(--color-other)" : RAMP[Math.min(i, 3)],
  }));
}

export function Origin({ aud }: { aud: Audience }) {
  const { scope } = useDash();
  const countries = useList("countries", "volume", { limit: MAP_LIMIT });
  const cl = useApi<Clients>(
    scopeKey("clients", scope),
    (s) => api.clients(scope, s),
    scope.site,
  );
  const rows = countries.data?.rows ?? [];
  const metric = aud === "bots" ? "Addresses" : "Visitors";
  return (
    <>
      <SectionHead
        id="origin"
        title="Origin and clients"
        lead={AUD_NOTE[aud]}
        sources={["browser"]}
      />
      <div className="grid grid-cols-1 gap-3 min-[1000px]:grid-cols-2">
        <Panel label={MAP_TITLE[aud]} className="card" busy={countries.loading}>
          <CardHead
            title={MAP_TITLE[aud]}
            right={
              <div className="legend items-center">
                <span>fewer</span>
                <span>
                  {RAMP.slice()
                    .reverse()
                    .map((c) => (
                      <i
                        key={c}
                        className="sw !mr-[3px]"
                        style={{ background: c }}
                      />
                    ))}
                </span>
                <span>more</span>
              </div>
            }
          />
          {countries.error ? (
            <ErrorBox error={countries.error} />
          ) : !countries.data ? (
            <Skeleton h={280} />
          ) : (
            <>
              <WorldMap rows={rows} metric={metric} />
              {rows.length === 0 ? (
                <Empty>No visitors with a known country in this range.</Empty>
              ) : (
                <div className="legend mt-2.5">
                  {rows.slice(0, 5).map((r) => (
                    <span key={r.key}>
                      {r.label && r.label !== r.key
                        ? r.label
                        : countryName(r.key)}{" "}
                      <b>{num(r.value)}</b>
                    </span>
                  ))}
                  {countries.data.total > 5 && (
                    <span className="text-muted">
                      + {num(countries.data.total - 5)} more
                    </span>
                  )}
                </div>
              )}
            </>
          )}
        </Panel>
        <Panel label="Clients" className="card" busy={cl.loading}>
          <CardHead title="Clients" />
          {cl.error ? (
            <ErrorBox error={cl.error} />
          ) : !cl.data ? (
            <Skeleton h={280} />
          ) : (
            (
              [
                ["IP version", cl.data.ip_version],
                ["Browsers", cl.data.browsers],
                ["Operating systems", cl.data.os],
                ["Devices", cl.data.devices],
              ] as [string, ShareRow[]][]
            ).map(([title, list]) => (
              <div key={title}>
                <div className="eyebrow mt-3.5 mb-1.5">{title}</div>
                {list.length ? (
                  <Strip label={title} parts={shareParts(list)} />
                ) : (
                  <Empty>Nothing recorded yet.</Empty>
                )}
              </div>
            ))
          )}
        </Panel>
      </div>
    </>
  );
}

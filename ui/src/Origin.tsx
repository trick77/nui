import { api, type Audience, type Clients, type ShareRow } from "./api";
import { Strip } from "./charts";
import { countryName, num } from "./format";
import { useApi } from "./hooks";
import { TopList, scopeKey, useList } from "./lists";
import { WorldMap } from "./map";
import { AUD_NOTE } from "./Overview";
import {
  AllClasses,
  AsAudience,
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
const LEGEND_LIMIT = 5;

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

// An API's callers on the map are its programs, counted by address: the other
// audiences count browsers there, which an API has none of.
export function Origin({ aud, api: isApi }: { aud: Audience; api: boolean }) {
  return isApi ? (
    <AsAudience aud="bots">
      <OriginPanels aud="bots" isApi />
    </AsAudience>
  ) : (
    <OriginPanels aud={aud} isApi={false} />
  );
}

function OriginPanels({ aud, isApi }: { aud: Audience; isApi: boolean }) {
  const title = isApi ? "Where callers are" : MAP_TITLE[aud];
  const countries = useList("countries", "volume", { limit: MAP_LIMIT });
  // The legend's top countries and how many more: the backend's, as asked.
  const top = useList("countries", "volume", { limit: LEGEND_LIMIT });
  const rows = countries.data?.rows ?? [];
  const metric = aud === "bots" ? "Addresses" : "Visitors";
  return (
    <>
      <SectionHead
        id="origin"
        title={isApi ? "Origin" : "Origin and clients"}
        lead={
          isApi
            ? "Its programs on the map, every client's network beside it, by address."
            : AUD_NOTE[aud]
        }
        sources={[isApi ? "server" : "browser"]}
      />
      <div className="grid grid-cols-1 gap-3 min-[1000px]:grid-cols-2">
        <Panel label={title} className="card" busy={countries.loading}>
          <CardHead
            title={title}
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
                <Empty>
                  No {isApi ? "callers" : "visitors"} with a known country in
                  this range.
                </Empty>
              ) : (
                top.data && (
                  <div className="legend mt-2.5">
                    {top.data.rows.map((r) => (
                      <span key={r.key}>
                        {r.label && r.label !== r.key
                          ? r.label
                          : countryName(r.key)}{" "}
                        <b>{num(r.value)}</b>
                      </span>
                    ))}
                    {top.data.other.count > 0 && (
                      <span className="text-muted">
                        + {num(top.data.other.count)} more
                      </span>
                    )}
                  </div>
                )
              )}
            </>
          )}
        </Panel>
        {/* A program's user agent names no browser, system or device. */}
        {isApi ? (
          <AllClasses>
            <NetworksPanel />
          </AllClasses>
        ) : (
          <ClientsPanel />
        )}
      </div>
    </>
  );
}

export function NetworksPanel() {
  return (
    <TopList
      id="networks-panel"
      title="Networks"
      tabs={[{ key: "networks", label: "Networks", dim: "networks" }]}
      empty="No networks in this range yet."
    />
  );
}

function ClientsPanel() {
  const { scope } = useDash();
  const cl = useApi<Clients>(
    scopeKey("clients", scope),
    (s) => api.clients(scope, s),
    scope.site,
  );
  return (
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
  );
}

import type { ReactNode } from "react";
import { api, type Domain, type SourceState } from "./api";
import { SEP, bytes, date, dateTime, joinSep, num } from "./format";
import { useApi } from "./hooks";
import { scopeKey } from "./lists";
import { ErrorBox, Panel, SectionHead, Skeleton, useDash } from "./ui";

// A term list; a false row is left out.
function Dl({ rows }: { rows: ([string, ReactNode] | false)[] }) {
  return (
    <dl className="mt-2 mb-0 grid grid-cols-[auto_1fr] gap-x-3.5 gap-y-[5px] text-[13px]">
      {rows
        .filter((r) => r !== false)
        .map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="whitespace-nowrap text-muted">{k}</dt>
            <dd className="m-0">{v}</dd>
          </div>
        ))}
    </dl>
  );
}

function Source({ s, missing }: { s: SourceState | null; missing: string }) {
  if (!s) return <span className="text-muted">{missing}</span>;
  const ok = s.state === "receiving";
  return (
    <>
      <span className="pill">
        <i
          style={{
            background: ok ? "var(--color-online)" : "var(--color-ochre)",
          }}
        />
        {s.state}
      </span>{" "}
      <span className="text-[12px] text-muted">
        last event {dateTime(s.last)}
      </span>
    </>
  );
}

// How the domain is connected, what Stakeout accepted and refused, what is
// stored, and the rules behind it all.
export function DomainFooter() {
  const { scope, site } = useDash();
  const { range } = scope;
  const d = useApi<Domain>(
    scopeKey("domain", { site: site.site, range }),
    (s) => api.domain(scope, s),
    site.site,
  );
  const x = d.data;
  const counters: [string, number | undefined, boolean][] = x
    ? [
        ["Accepted", x.counters.accepted, false],
        ["Rate-limited", x.counters.rate_limited, x.counters.rate_limited > 0],
        [joinSep(["Rejected", "invalid"]), x.counters.rejected, false],
        [
          joinSep(["Dropped", "queue full"]),
          x.counters.dropped,
          x.counters.dropped > 0,
        ],
        // Counted for the collector, not per site: a body naming no
        // configured domain, or a feed request with a key no site has,
        // belongs to none.
        [
          joinSep(["Unknown site or wrong key", "all sites"]),
          x.counters.unknown_domain,
          x.counters.unknown_domain > 0,
        ],
      ]
    : [];
  return (
    <>
      <SectionHead
        id="domain"
        title="This domain"
        lead="How it is connected, what Stakeout accepted and refused, and what is stored."
      />
      <Panel label="This domain" busy={d.loading}>
        {d.error ? (
          <div className="p-4">
            <ErrorBox error={d.error} />
          </div>
        ) : !x ? (
          <div className="p-4">
            <Skeleton h={200} />
          </div>
        ) : (
          <>
            <div className="grid grid-cols-5">
              {counters.map(([k, v, warn], i) => (
                <div
                  key={k}
                  className={`flex flex-col justify-between px-[18px] py-3.5 ${i ? "border-l border-border" : ""}`}
                >
                  <div className="eyebrow">{k}</div>
                  <div
                    className={`font-serif text-[26px] font-medium tabular-nums ${warn ? "text-ochre" : ""}`}
                  >
                    {num(v)}
                  </div>
                </div>
              ))}
            </div>
            <div className="grid grid-cols-1 border-t border-border min-[1000px]:grid-cols-3">
              <section className="px-[18px] py-4">
                <div className="eyebrow">
                  Sources{SEP}
                  {site.site}
                </div>
                <Dl
                  rows={[
                    [
                      "Browser",
                      <Source
                        s={x.sources.browser}
                        missing={joinSep([
                          "no events yet",
                          "load the script on the site",
                        ])}
                      />,
                    ],
                    [
                      "Server",
                      <Source
                        s={x.sources.server}
                        missing={
                          x.sources.feed
                            ? joinSep([
                                "configured, no events yet",
                                "requests, status codes and bots without JavaScript show once the feed delivers",
                              ])
                            : joinSep([
                                "not connected",
                                "bots without JavaScript, requests and status codes are not visible for this domain",
                              ])
                        }
                      />,
                    ],
                    [
                      "Script",
                      <>
                        <span className="font-mono">{x.sources.script}</span>
                        {x.sources.build && `${SEP}build ${x.sources.build}`}
                      </>,
                    ],
                  ]}
                />
              </section>
              <section className="border-border-soft px-[18px] py-4 max-[999px]:border-t min-[1000px]:border-l">
                <div className="eyebrow">Storage</div>
                <Dl
                  rows={[
                    // Every request with its address, user agent and path:
                    // the log, a visitor's trail and what it does.
                    [
                      "Raw events",
                      joinSep([
                        `${num(x.storage.events)} stored`,
                        x.storage.oldest && `since ${date(x.storage.oldest)}`,
                        `kept ${num(x.storage.raw_days)} days`,
                      ]),
                    ],
                    // The tiles, lists and sums, from the first event on:
                    // they may reach further back than raw. None without data.
                    x.storage.figures_since != null && [
                      "Figures",
                      `since ${date(x.storage.figures_since)}`,
                    ],
                    ["Database, all sites", bytes(x.storage.bytes)],
                    ["Visitor identity", x.storage.identity],
                  ]}
                />
              </section>
              <section className="border-border-soft px-[18px] py-4 max-[999px]:border-t min-[1000px]:border-l">
                <div className="eyebrow">Rules and reference data</div>
                <Dl
                  rows={[
                    ["Rate limit", x.rules.rate],
                    ["Batch limit", x.rules.batch],
                    [
                      "Bot list",
                      joinSep([
                        "ipverse",
                        `${num(x.rules.botlist.services)} services`,
                        `updated ${dateTime(x.rules.botlist.updated)}`,
                      ]),
                    ],
                    [
                      "Networks",
                      joinSep([
                        "ipverse as-ip-blocks",
                        `updated ${dateTime(x.rules.networks.updated)}`,
                      ]),
                    ],
                    ["Times shown in", x.rules.tz],
                    [
                      "Model budget",
                      x.rules.llm.configured
                        ? `${num(x.rules.llm.calls_today)} of ${num(x.rules.llm.budget)} calls today`
                        : joinSep([
                            "no model configured",
                            "computed texts only",
                          ]),
                    ],
                  ]}
                />
              </section>
            </div>
          </>
        )}
      </Panel>
    </>
  );
}

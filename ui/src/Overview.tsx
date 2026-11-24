import { useState } from "react";
import type { Audience, Overview, Tile, TileKey } from "./api";
import { MainChart, Spark, isCount } from "./charts";
import { SEP, num, rawSince } from "./format";
import { Delta, ErrorBox, Panel, SectionHead, Skeleton } from "./ui";

export const AUD_NOTE: Record<Audience, string> = {
  all: "Humans and bots together.",
  humans: "Humans only.",
  bots: "Bots only: API clients, known bots, uptime probes and unknown automation.",
};
const AUD_WORD: Record<Audience, string> = {
  all: "all",
  humans: "humans",
  bots: "bots",
};

function TileValue({ t }: { t: Tile }) {
  return (
    <>
      {num(t.value, t.kind === "rate" ? 1 : 0)}
      {t.unit && t.value != null && (
        <small className="ml-[3px] font-sans text-[14px] font-normal text-muted">
          {t.unit}
        </small>
      )}
    </>
  );
}

function prevText(t: Tile, prevLabel: string): string {
  if (t.prev == null) return "";
  return `${num(t.prev, t.kind === "rate" ? 1 : 0)}${t.unit ? ` ${t.unit}` : ""} in the ${prevLabel}`;
}

export function OverviewSection({
  data,
  error,
  busy,
  aud,
  api,
}: {
  data: Overview | null;
  error: Error | null;
  busy: boolean;
  aud: Audience;
  api: boolean;
}) {
  const [metric, setMetric] = useState<TileKey>("visitors");
  const tiles = data?.tiles ?? [];
  // A tile the domain shown does not have (a server tile without a feed, a
  // website's on an API) falls back to the first.
  const sel = tiles.find((t) => t.key === metric) ?? tiles[0];
  const p = data?.period;
  return (
    <>
      <SectionHead
        id="overview"
        title="Overview"
        lead={AUD_NOTE[aud]}
        sources={api ? ["server"] : ["browser", "server"]}
      />
      <Panel className="hero" label="Overview" busy={busy}>
        {error ? (
          <div className="p-4">
            <ErrorBox error={error} />
          </div>
        ) : !data || !p ? (
          <div className="p-4">
            <Skeleton h={360} label="Loading the overview" />
          </div>
        ) : (
          <>
            <div className={`tiles${tiles.length === 4 ? " four" : ""}`}>
              {tiles.map((t) => (
                <button
                  key={t.key}
                  type="button"
                  className="tile"
                  aria-pressed={t.key === sel?.key}
                  onClick={() => setMetric(t.key)}
                >
                  <div className="eyebrow">{t.label}</div>
                  <div className="flex items-end justify-between gap-2">
                    <div>
                      <div className="tile-value">
                        <TileValue t={t} />
                      </div>
                      {/* With the comparison off, no tile has one. */}
                      <div className={p.prev_label ? "tile-delta" : undefined}>
                        <Delta change={t.change} verdict={t.verdict} />
                      </div>
                    </div>
                    <Spark cur={t.mean} prev={t.prev_mean} />
                  </div>
                  <div className="mt-0.5 text-[12px] text-muted">
                    {prevText(t, p.prev_label)}
                  </div>
                </button>
              ))}
            </div>
            {sel && (
              <div className="card">
                <div className="mb-2.5 flex flex-wrap items-center gap-2.5">
                  <h3 className="m-0 text-[15px] font-medium">
                    {sel.label} per {p.slot}
                    {SEP}
                    {AUD_WORD[aud]}
                  </h3>
                  <div className="legend ml-auto">
                    <span>
                      <i
                        className={
                          isCount(sel.kind, sel.unit)
                            ? "sw pale"
                            : "sw line thin"
                        }
                        style={{ background: "var(--color-s1)" }}
                      />
                      per {p.slot}
                    </span>
                    <span>
                      <i
                        className="sw line"
                        style={{ background: "var(--color-s1)" }}
                      />
                      {p.mean} average
                    </span>
                    {/* [] with nothing to compare: no entry. */}
                    {sel.prev_mean.length > 0 && (
                      <span>
                        <i
                          className="sw line"
                          style={{ background: "var(--color-reference)" }}
                        />
                        {p.prev_label} average
                      </span>
                    )}
                    {p.since && (
                      <span className="text-muted">
                        figures since {rawSince(p.since)}
                      </span>
                    )}
                  </div>
                </div>
                <MainChart
                  labels={p.labels}
                  cur={sel.series}
                  mean={sel.mean}
                  prev={sel.prev_mean}
                  delta={sel.delta}
                  meanLabel={p.mean}
                  axis={p}
                  first={p.first}
                  kind={sel.kind}
                  unit={sel.unit}
                  open={p.open}
                  title={sel.label}
                  prevLabel={p.prev_label}
                />
              </div>
            )}
          </>
        )}
      </Panel>
    </>
  );
}

import { useState } from "react";
import type { Audience, Overview, Tile, TileKey } from "./api";
import { MainChart, Spark } from "./charts";
import { num } from "./format";
import { Delta, ErrorBox, Panel, SectionHead, Skeleton } from "./ui";

export const AUD_NOTE: Record<Audience, string> = {
  all: "Humans and bots together.",
  humans: "Humans only.",
  bots: "Bots only: known bots, uptime probes and unknown automation.",
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
      {t.unit && (
        <small className="ml-[3px] font-sans text-[14px] font-normal text-muted">
          {t.unit}
        </small>
      )}
    </>
  );
}

function prevText(t: Tile, prevLabel: string): string {
  const parts: string[] = [];
  if (t.prev != null)
    parts.push(
      `${num(t.prev, t.kind === "rate" ? 1 : 0)}${t.unit ? ` ${t.unit}` : ""} in the ${prevLabel}`,
    );
  if (t.sub) parts.push(t.sub);
  return parts.join(" · ");
}

export function OverviewSection({
  data,
  error,
  busy,
  aud,
}: {
  data: Overview | null;
  error: Error | null;
  busy: boolean;
  aud: Audience;
}) {
  const [metric, setMetric] = useState<TileKey>("visitors");
  const tiles = data?.tiles ?? [];
  // A server tile picked on a domain without a feed falls back to the first.
  const sel = tiles.find((t) => t.key === metric) ?? tiles[0];
  const p = data?.period;
  return (
    <>
      <SectionHead
        id="overview"
        title="Overview"
        lead={AUD_NOTE[aud]}
        sources={["browser", "server"]}
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
                      <Delta change={t.change} verdict={t.verdict} />
                    </div>
                    <Spark
                      cur={t.series}
                      prev={t.prev_series}
                      open={p.last_bucket_open}
                    />
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
                    {sel.label} per {p.bucket} · {AUD_WORD[aud]}
                  </h3>
                  <div className="legend ml-auto">
                    <span>
                      <i
                        className="sw line"
                        style={{ background: "var(--color-s1)" }}
                      />
                      this period
                    </span>
                    {/* [] with the comparison off: no entry. */}
                    {sel.prev_series?.length ? (
                      <span>
                        <i
                          className="sw line"
                          style={{ background: "var(--color-reference)" }}
                        />
                        {p.prev_label}
                      </span>
                    ) : null}
                    {sel.median != null && (
                      <span>
                        <i className="sw dash" />
                        median
                      </span>
                    )}
                    {p.last_bucket_open && (
                      <span className="text-muted">
                        dashed end = {p.bucket} still filling
                      </span>
                    )}
                  </div>
                </div>
                <MainChart
                  labels={p.labels}
                  cur={sel.series}
                  prev={sel.prev_series}
                  median={sel.median}
                  kind={sel.kind}
                  open={p.last_bucket_open}
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
